// Lệnh bgraph-eval: bộ đánh giá ngoài (docs/plan.md mục 10.2).
//
//	bgraph-eval manifest --tasks tasks.jsonl [--meta meta.json] --replay zod,hono --out evalkit/manifest/node.json
//	bgraph-eval t1       --tasks tasks.jsonl --manifest evalkit/manifest/node.json --work DIR --out DIR [--jobs 4] [--max-per-repo 40]
//	bgraph-eval quick    --tasks tasks.jsonl --work DIR --out DIR [--manifest ...] [--regressions evalkit/manifest/node-regressions.txt] [--per-repo 4] [--commits 20] [--checkout] [--serial]
//	bgraph-eval replay   --repo URL [--repo URL...] --commits 100 --manifest ... --work DIR --out DIR
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bgraph/internal/evalkit"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, strings.Split(s, ",")...); return nil }

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "t1":
		err = cmdT1(ctx, os.Args[2:])
	case "quick":
		var code int
		code, err = cmdQuick(ctx, os.Args[2:])
		if err == nil && code != 0 {
			stop()
			os.Exit(code)
		}
	case "replay":
		err = cmdReplay(ctx, os.Args[2:])
	case "manifest":
		err = cmdManifest(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bgraph-eval:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "dùng: bgraph-eval t1|replay|manifest [cờ]  (xem -h của từng lệnh)")
	os.Exit(2)
}

func logger(out string) func(string, ...any) {
	os.MkdirAll(out, 0o755)
	f, _ := os.OpenFile(filepath.Join(out, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	var mu sync.Mutex
	return func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		line := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, line)
		if f != nil {
			fmt.Fprintln(f, line)
		}
	}
}

func cmdT1(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("t1", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "JSONL nhiệm vụ (đã chuẩn hoá)")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest repo dev/holdout")
	work := fs.String("work", "", "thư mục làm việc (clone, worktree, state)")
	out := fs.String("out", "", "thư mục kết quả")
	jobs := fs.Int("jobs", 4, "số repo chạy song song")
	maxPer := fs.Int("max-per-repo", 40, "tối đa task mỗi repo (0 = không giới hạn)")
	only := fs.String("repos", "", "chỉ các repo này (phân tách bằng dấu phẩy)")
	ids := fs.String("ids", "", "chỉ các instance_id này")
	limit := fs.Int("limit", 0, "tối đa tổng số task (0 = không)")
	resume := fs.Bool("resume", false, "bỏ qua task đã có trong t1.jsonl")
	checkout := fs.Bool("checkout", false, "dựng P/C bằng git worktree (lối cũ) thay vì index tạm; lỗi git apply --cached cũng tự rơi về lối này")
	fs.Parse(args)
	if *tasksPath == "" || *work == "" || *out == "" {
		return fmt.Errorf("cần --tasks, --work, --out")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	man, err := evalkit.LoadManifest(*manPath)
	if err != nil {
		return err
	}
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	tasks = evalkit.SelectTasks(tasks, *maxPer)
	var keep []evalkit.Task
	for _, t := range tasks {
		if _, ok := man.Role(t.Repo); !ok {
			return fmt.Errorf("repo %s không có trong manifest", t.Repo)
		}
		if *only != "" && !contains(*only, t.Repo) {
			continue
		}
		if *ids != "" && !contains(*ids, t.InstanceID) {
			continue
		}
		keep = append(keep, t)
	}
	if *limit > 0 && len(keep) > *limit {
		keep = keep[:*limit]
	}
	lg := logger(*out)
	lg("T1: %d task", len(keep))
	rs, err := evalkit.RunT1(ctx, evalkit.T1Config{Work: *work, Out: *out, Tasks: keep, Manifest: man, Jobs: *jobs, MaxPerRepo: *maxPer, Resume: *resume, Checkout: *checkout, Log: lg})
	if werr := evalkit.WriteT1(*out, rs); werr != nil {
		return werr
	}
	return err
}

// cmdQuick chạy bộ đánh giá nhanh: T1 trên mẫu phân tầng + replay; trả mã thoát 1 nếu có vi phạm T1 hoặc bất biến.
func cmdQuick(ctx context.Context, args []string) (int, error) {
	start := time.Now()
	fs := flag.NewFlagSet("quick", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "JSONL nhiệm vụ (đã chuẩn hoá)")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest repo dev/holdout")
	regPath := fs.String("regressions", "evalkit/manifest/node-regressions.txt", "danh sách instance_id từng vi phạm (luôn chạy)")
	work := fs.String("work", "", "thư mục làm việc (clone, state)")
	out := fs.String("out", "", "thư mục kết quả")
	jobs := fs.Int("jobs", 4, "số repo chạy song song")
	perRepo := fs.Int("per-repo", 4, "số task khác mỗi repo, chọn theo sha256(instance_id)")
	maxPer := fs.Int("max-per-repo", 40, "giới hạn mỗi repo của tập T1 đầy đủ (tập mà mẫu rút ra)")
	commits := fs.Int("commits", 20, "số commit replay mỗi repo")
	checkout := fs.Bool("checkout", false, "dựng P/C bằng git worktree (lối cũ)")
	serial := fs.Bool("serial", false, "chạy T1 rồi replay tuần tự (mặc định song song)")
	fs.Parse(args)
	if *tasksPath == "" || *work == "" || *out == "" {
		return 2, fmt.Errorf("cần --tasks, --work, --out")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return 2, err
	}
	man, err := evalkit.LoadManifest(*manPath)
	if err != nil {
		return 2, err
	}
	all, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return 2, err
	}
	reg, err := evalkit.LoadIDList(*regPath)
	if err != nil {
		return 2, err
	}
	sample, missing := evalkit.SampleQuick(all, evalkit.SelectTasks(all, *maxPer), reg, *perRepo)
	var keep []evalkit.Task
	for _, t := range sample {
		if _, ok := man.Role(t.Repo); !ok {
			return 2, fmt.Errorf("repo %s không có trong manifest", t.Repo)
		}
		keep = append(keep, t)
	}
	var rep []string
	for _, r := range man.Repos {
		if r.Replay {
			rep = append(rep, r.Name)
		}
	}
	lg := logger(*out)
	lg("quick: %d task T1 (%d regression, %d id regression không có trong dữ liệu: %v), replay %d repo x %d commit", len(keep), len(reg)-len(missing), len(missing), missing, len(rep), *commits)
	qr, err := evalkit.RunQuick(ctx, evalkit.QuickConfig{Work: *work, Out: *out, Tasks: keep, Manifest: man, Replay: rep, Commits: *commits, Jobs: *jobs, Checkout: *checkout, Serial: *serial, Log: lg})
	if err != nil {
		return 2, err
	}
	b, _ := os.ReadFile(filepath.Join(*out, "quick.md"))
	fmt.Println(string(b))
	fmt.Printf("Tổng thời gian: %s\n", time.Since(start).Round(time.Second))
	if qr.Violations > 0 {
		return 1, nil
	}
	return 0, nil
}

func contains(list, v string) bool {
	for _, x := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

func cmdReplay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	var repos multi
	fs.Var(&repos, "repo", "URL hoặc owner/name (lặp được)")
	n := fs.Int("commits", 100, "số commit gần nhất (first-parent, bỏ merge)")
	manPath := fs.String("manifest", "evalkit/manifest/node.json", "manifest repo dev/holdout")
	work := fs.String("work", "", "thư mục làm việc")
	out := fs.String("out", "", "thư mục kết quả")
	jobs := fs.Int("jobs", 4, "số repo song song")
	depth := fs.Int("depth", 0, "độ sâu clone nông (0 = 3*commits+20)")
	resum := fs.Bool("resummarize", false, "chỉ đọc lại replay.json trong --out, gán vai trò từ manifest và ghi lại báo cáo")
	fs.Parse(args)
	if *resum {
		man, err := evalkit.LoadManifest(*manPath)
		if err != nil {
			return err
		}
		return evalkit.Resummarize(*out, man)
	}
	if len(repos) == 0 || *work == "" || *out == "" {
		return fmt.Errorf("cần --repo, --work, --out")
	}
	if err := evalkit.SetupHermeticEnv(filepath.Join(*work, "gitcfg")); err != nil {
		return err
	}
	man, _ := evalkit.LoadManifest(*manPath)
	lg := logger(*out)
	rs, err := evalkit.RunReplay(ctx, evalkit.ReplayConfig{Work: *work, Out: *out, Repos: repos, Commits: *n, Depth: *depth, Jobs: *jobs, Manifest: man, Log: lg})
	if werr := evalkit.WriteReplay(*out, rs); werr != nil {
		return werr
	}
	return err
}

func cmdManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	tasksPath := fs.String("tasks", "", "JSONL nhiệm vụ")
	metaPath := fs.String("meta", "", "meta.json do fetch_datasets.py ghi (revision các bộ dữ liệu)")
	var replay multi
	fs.Var(&replay, "replay", "repo replay (phân tách bằng dấu phẩy)")
	maxPer := fs.Int("max-per-repo", 40, "")
	out := fs.String("out", "evalkit/manifest/node.json", "")
	fs.Parse(args)
	tasks, err := evalkit.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	tasks = evalkit.SelectTasks(tasks, *maxPer)
	t1 := map[string]*evalkit.ManifestRepo{}
	for _, t := range tasks {
		r := t1[strings.ToLower(t.Repo)]
		if r == nil {
			r = &evalkit.ManifestRepo{Name: t.Repo}
			t1[strings.ToLower(t.Repo)] = r
		}
		r.T1++
		if !contains(strings.Join(r.Sources, ","), t.Source) {
			r.Sources = append(r.Sources, t.Source)
		}
	}
	// Repo trùng tên nhưng khác hoa/thường giữa replay và T1: giữ tên của T1.
	var rep []string
	for _, r := range replay {
		if x, ok := t1[strings.ToLower(r)]; ok {
			r = x.Name
		}
		rep = append(rep, r)
	}
	sort.Strings(rep)
	ds := []evalkit.DatasetInfo{
		{Name: "SWE-bench/SWE-bench_Multilingual", URL: "https://huggingface.co/datasets/SWE-bench/SWE-bench_Multilingual", Notes: "split test; giữ repo JS/TS"},
		{Name: "ByteDance-Seed/Multi-SWE-bench", URL: "https://huggingface.co/datasets/ByteDance-Seed/Multi-SWE-bench", Notes: "thư mục js/ và ts/ (JSONL); FAIL_TO_PASS = khoá f2p_tests"},
	}
	if *metaPath != "" {
		if b, err := os.ReadFile(*metaPath); err == nil {
			var m map[string]string
			if json.Unmarshal(b, &m) == nil {
				for i := range ds {
					ds[i].Revision = m[ds[i].Name]
				}
			}
		}
	}
	man := evalkit.BuildManifest(t1, rep, ds, *maxPer)
	b, _ := json.MarshalIndent(man, "", " ")
	os.MkdirAll(filepath.Dir(*out), 0o755)
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	for _, r := range man.Repos {
		fmt.Printf("%-8s t1=%-3d replay=%-5v %s\n", r.Role, r.T1, r.Replay, r.Name)
	}
	return nil
}
