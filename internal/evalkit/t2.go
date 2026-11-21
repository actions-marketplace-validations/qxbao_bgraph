package evalkit

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bgraph"
	"bgraph/internal/model"
)

// T2 (mục 10.2 của docs/plan.md): oracle đột biến. Gieo MỘT lỗi vào một dòng của tệp nguồn tại R (commit M = R + lỗi, dựng bằng index tạm,
// không checkout), chạy cả bộ test đơn vị trên cây làm việc có lỗi, lấy tập tệp test ĐỎ (trừ tệp hỏng/flaky sẵn ở R). Mọi tệp đỏ phải nằm
// trong phần chọn của Select(R→M). Oracle KHÔNG phụ thuộc bgraph; kết quả lưu cache (oracle.jsonl) để lần sau chỉ chạy lại bgraph.

//go:embed mutate.cjs
var mutateScript []byte

// T2Repo là một repo trong manifest T2.
type T2Repo struct {
	Name       string   `json:"name"`
	Dir        string   `json:"dir"`    // thư mục checkout, tương đối thư mục gốc T2
	Remote     string   `json:"remote"` // chỉ để tài liệu/clone
	R          string   `json:"r"`      // SHA gốc (cây sạch)
	Runner     string   `json:"runner"` // vitest | jest
	Bin        string   `json:"bin"`    // script JS của runner, tương đối checkout, chạy bằng node
	Args       []string `json:"args"`   // đối số chạy toàn bộ bộ test (trước cờ reporter)
	NativeArgs []string `json:"native_args"`
	NativeNote string   `json:"native_note,omitempty"`
	Install    string   `json:"install,omitempty"`
	Covers     string   `json:"covers,omitempty"`
	Include    []string `json:"include"`
	Exclude    []string `json:"exclude"`
	Mutants    int      `json:"mutants"`
	Seed       string   `json:"seed"`
	Env        []string `json:"env,omitempty"`
}

// T2Manifest là manifest repo T2.
type T2Manifest struct {
	Version int      `json:"version"`
	TS      string   `json:"ts"` // thư mục typescript cho bộ sinh đột biến, tương đối thư mục gốc T2
	Notes   string   `json:"notes,omitempty"`
	Repos   []T2Repo `json:"repos"`
}

// LoadT2Manifest đọc manifest T2.
func LoadT2Manifest(p string) (T2Manifest, error) {
	var m T2Manifest
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// T2Mutant là một đột biến: thay byte [Start,End) của blob tại R bằng Repl.
type T2Mutant struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Op     string `json:"op"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Repl   string `json:"repl"`
	Before string `json:"before"`
	After  string `json:"after"`
	Blob   string `json:"blob"` // OID blob của tệp tại R (phát hiện cache cũ)
}

// T2Record là một dòng của oracle.jsonl.
type T2Record struct {
	Repo   string   `json:"repo"`
	R      string   `json:"r"`
	Mutant T2Mutant `json:"mutant"`
	Commit string   `json:"commit,omitempty"` // M
	// Status: killed | survived | timeout | error
	Status         string   `json:"status"`
	Red            []string `json:"red,omitempty"`             // tệp test đỏ (đã trừ tệp loại)
	Confirmed      bool     `json:"confirmed,omitempty"`       // Red đã được xác nhận bằng chạy lại riêng các tệp đỏ
	RedUnconfirmed []string `json:"red_unconfirmed,omitempty"` // đỏ ở lượt đầy đủ nhưng không tái hiện (flaky do tải)
	RedExcluded    []string `json:"red_excluded,omitempty"`    // đỏ nhưng thuộc tệp hỏng/flaky sẵn
	Native         []string `json:"native,omitempty"`          // tập "related" của công cụ gốc
	NativeErr      string   `json:"native_err,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	TestFiles      int      `json:"test_files,omitempty"`
	Ms             int64    `json:"ms,omitempty"`
}

func (r T2Record) key() string { return r.Repo + "|" + r.R + "|" + r.Mutant.ID }

// T2Baseline là kết quả 3 lượt chạy ở R.
type T2Baseline struct {
	Repo    string   `json:"repo"`
	R       string   `json:"r"`
	Files   int      `json:"files"`
	Failing []string `json:"failing"` // đỏ cả 3 lượt
	Flaky   []string `json:"flaky"`   // kết quả khác nhau giữa các lượt (hoặc vắng mặt)
	RunMs   []int64  `json:"run_ms"`
	Tests   int      `json:"tests,omitempty"`
}

func (b T2Baseline) excluded() map[string]bool {
	m := map[string]bool{}
	for _, f := range b.Failing {
		m[f] = true
	}
	for _, f := range b.Flaky {
		m[f] = true
	}
	return m
}

// ---- chạy tiến trình ----

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return
	}
	cmd.Process.Kill()
}

// runProc chạy name args trong dir; quá timeout thì giết cả cây tiến trình. Trả stdout, đuôi stderr.
func runProc(ctx context.Context, dir string, env []string, timeout time.Duration, name string, args ...string) (stdout string, tail string, timedOut bool, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err = cmd.Start(); err != nil {
		return "", "", false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err = <-done:
	case <-t.C:
		timedOut = true
		killTree(cmd)
		err = <-done
	case <-ctx.Done():
		killTree(cmd)
		<-done
		return "", "", false, ctx.Err()
	}
	tail = errb.String()
	if len(tail) > 1500 {
		tail = tail[len(tail)-1500:]
	}
	return out.String(), tail, timedOut, err
}

// ---- đọc kết quả runner ----

type runJSON struct {
	TestResults []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		AssertionResults []struct {
			Meta struct {
				Typecheck bool `json:"typecheck"`
			} `json:"meta"`
		} `json:"assertionResults"`
	} `json:"testResults"`
	NumTotalTests int `json:"numTotalTests"`
}

// relPath chuẩn hoá đường dẫn tuyệt đối của runner về dạng tương đối root, dấu /.
func relPath(root, p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	root = strings.TrimSuffix(strings.ReplaceAll(root, "\\", "/"), "/")
	if len(p) > len(root) && strings.EqualFold(p[:len(root)], root) && p[len(root)] == '/' {
		return p[len(root)+1:]
	}
	return p
}

// ParseRunJSON đọc báo cáo JSON của vitest/jest thành map tệp → passed|failed|skipped. Mục typecheck của vitest bị bỏ.
// Một tệp xuất hiện nhiều lần (nhiều project) thì đỏ nếu có lượt đỏ.
func ParseRunJSON(root string, b []byte) (map[string]string, int, error) {
	var r runJSON
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, 0, err
	}
	if len(r.TestResults) == 0 {
		return nil, 0, fmt.Errorf("báo cáo không có testResults")
	}
	out := map[string]string{}
	for _, t := range r.TestResults {
		tc := false
		for _, a := range t.AssertionResults {
			if a.Meta.Typecheck {
				tc = true
			}
		}
		if tc {
			continue
		}
		f := relPath(root, t.Name)
		st := t.Status
		if st != "passed" && st != "failed" {
			st = "skipped"
		}
		switch prev := out[f]; {
		case prev == "failed":
		case st == "failed", prev == "", prev == "skipped":
			out[f] = st
		}
	}
	return out, r.NumTotalTests, nil
}

// runSuite chạy cả bộ test, trả trạng thái từng tệp.
func (r T2Repo) runSuite(ctx context.Context, root, tmp string, timeout time.Duration, only []string) (map[string]string, int, time.Duration, bool, string, error) {
	os.MkdirAll(tmp, 0o755)
	of := filepath.Join(tmp, "run.json")
	os.Remove(of)
	args := []string{r.Bin}
	args = append(args, r.Args...)
	if r.Runner == "jest" {
		args = append(args, "--json", "--outputFile="+of)
		if len(only) > 0 {
			args = append(args, "--runTestsByPath")
		}
	} else {
		args = append(args, "--reporter=json", "--outputFile="+of)
	}
	args = append(args, only...)
	t0 := time.Now()
	_, tail, to, err := runProc(ctx, root, append([]string{"CI=1"}, r.Env...), timeout, "node", args...)
	d := time.Since(t0)
	if ctx.Err() != nil {
		return nil, 0, d, false, "", ctx.Err()
	}
	if to {
		return nil, 0, d, true, "quá thời hạn " + timeout.String(), nil
	}
	b, rerr := os.ReadFile(of)
	if rerr != nil {
		return nil, 0, d, false, "không có báo cáo JSON: " + fmt.Sprint(err) + " " + tail, nil
	}
	m, n, perr := ParseRunJSON(root, b)
	if perr != nil {
		return nil, 0, d, false, perr.Error(), nil
	}
	return m, n, d, false, "", nil
}

// runNative tính tập "related" của công cụ gốc cho thay đổi hiện có trên cây làm việc, không chạy test.
func (r T2Repo) runNative(ctx context.Context, root, tmp, file string) ([]string, error) {
	os.MkdirAll(tmp, 0o755)
	args := []string{r.Bin}
	args = append(args, r.NativeArgs...)
	var of string
	if r.Runner == "jest" {
		args = append(args, file)
	} else {
		of = filepath.Join(tmp, "native.json")
		os.Remove(of)
		args = append(args, "--json", of)
	}
	out, tail, to, err := runProc(ctx, root, append([]string{"CI=1"}, r.Env...), 10*time.Minute, "node", args...)
	if to {
		return nil, fmt.Errorf("native quá thời hạn")
	}
	var files []string
	if r.Runner == "jest" {
		if err != nil {
			return nil, fmt.Errorf("jest: %v %s", err, tail)
		}
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l != "" {
				files = append(files, relPath(root, l))
			}
		}
	} else {
		b, rerr := os.ReadFile(of)
		if rerr != nil {
			return nil, fmt.Errorf("vitest list: %v %s", err, tail)
		}
		var items []struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			files = append(files, relPath(root, it.File))
		}
	}
	sort.Strings(files)
	return dedup(files), nil
}

func dedup(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ---- kho oracle ----

// OracleStore là oracle.jsonl: thêm dòng khi xong từng đột biến; khi đọc, dòng sau thắng.
type OracleStore struct {
	Path string
	mu   sync.Mutex
}

// Load đọc mọi bản ghi (dòng sau ghi đè dòng trước cùng khoá), theo thứ tự xuất hiện đầu tiên.
func (s *OracleStore) Load() ([]T2Record, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx := map[string]int{}
	var out []T2Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r T2Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("oracle.jsonl: %w", err)
		}
		if i, ok := idx[r.key()]; ok {
			out[i] = r
		} else {
			idx[r.key()] = len(out)
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Append ghi thêm một bản ghi.
func (s *OracleStore) Append(r T2Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	return err
}

// ---- dựng đột biến ----

// ApplyEdit áp đột biến lên nội dung blob.
func ApplyEdit(blob []byte, m T2Mutant) ([]byte, error) {
	if m.Start < 0 || m.End < m.Start || m.End > len(blob) {
		return nil, fmt.Errorf("đột biến %s ngoài phạm vi blob", m.ID)
	}
	out := make([]byte, 0, len(blob)+len(m.Repl))
	out = append(out, blob[:m.Start]...)
	out = append(out, m.Repl...)
	return append(out, blob[m.End:]...), nil
}

func (r T2Repo) blob(ctx context.Context, root, file string) ([]byte, string, error) {
	oid, err := git(ctx, root, "rev-parse", r.R+":"+file)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", strings.TrimSpace(oid))
	cmd.Dir = root
	b, err := cmd.Output()
	return b, strings.TrimSpace(oid), err
}

// MutantCommit dựng commit M = R + đột biến bằng index tạm (không checkout). Tất định nhờ môi trường git kín.
func (r T2Repo) MutantCommit(ctx context.Context, root, idxDir string, m T2Mutant) (commit string, mutated []byte, err error) {
	blob, oid, err := r.blob(ctx, root, m.File)
	if err != nil {
		return "", nil, err
	}
	if oid != m.Blob {
		return "", nil, fmt.Errorf("%s: blob tại R là %s, đột biến sinh cho %s (cache cũ)", m.File, oid, m.Blob)
	}
	mutated, err = ApplyEdit(blob, m)
	if err != nil {
		return "", nil, err
	}
	os.MkdirAll(idxDir, 0o755)
	ib := newIndexBuilder(root, filepath.Join(idxDir, "mut-"+m.ID+".index"), r.R)
	defer ib.Close()
	c, err := ib.SetFile(ctx, m.File, mutated, "eval: mutant "+m.ID)
	return c, mutated, err
}

// GenMutants chạy bộ sinh đột biến tất định (node + typescript).
func (r T2Repo) GenMutants(ctx context.Context, t2Dir string, ts string, root string) ([]T2Mutant, error) {
	sdir := filepath.Join(t2Dir, "scripts")
	os.MkdirAll(sdir, 0o755)
	sp := filepath.Join(sdir, "mutate.cjs")
	if err := os.WriteFile(sp, mutateScript, 0o644); err != nil {
		return nil, err
	}
	cfg := map[string]any{"ts": filepath.ToSlash(filepath.Join(t2Dir, ts)), "repo": filepath.ToSlash(root), "rev": r.R, "include": r.Include, "exclude": r.Exclude, "count": r.Mutants, "seed": r.Seed}
	cb, _ := json.Marshal(cfg)
	cp := filepath.Join(sdir, "gen-"+r.Dir+".json")
	if err := os.WriteFile(cp, cb, 0o644); err != nil {
		return nil, err
	}
	out, tail, _, err := runProc(ctx, t2Dir, nil, 10*time.Minute, "node", sp, "@"+cp)
	if err != nil {
		return nil, fmt.Errorf("sinh đột biến: %v %s", err, tail)
	}
	var ms []T2Mutant
	if err := json.Unmarshal([]byte(out), &ms); err != nil {
		return nil, err
	}
	for i := range ms {
		ms[i].ID = fmt.Sprintf("%s-%02d", r.Dir, i+1)
	}
	return ms, nil
}

// ---- xây oracle ----

// T2Config cấu hình chạy T2.
type T2Config struct {
	Dir        string // thư mục gốc T2 (checkout, tools, state, oracle.jsonl)
	Out        string // thư mục báo cáo
	Manifest   T2Manifest
	Repos      []string // rỗng = tất cả
	RunTests   bool     // xây/làm mới oracle (chạy test)
	Rebaseline bool
	Rebuild    bool // bỏ cache oracle của repo, chạy lại mọi đột biến
	Reconfirm  bool // xác nhận lại các đột biến killed chưa xác nhận (chạy riêng tệp đỏ), không chạy lại bộ đầy đủ
	Log        func(format string, a ...any)
}

func (c *T2Config) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

func (c *T2Config) oraclePath() string { return filepath.Join(c.Dir, "oracle.jsonl") }

func (c *T2Config) selected() []T2Repo {
	var out []T2Repo
	for _, r := range c.Manifest.Repos {
		if len(c.Repos) == 0 {
			out = append(out, r)
			continue
		}
		for _, n := range c.Repos {
			if strings.EqualFold(n, r.Name) || strings.EqualFold(n, r.Dir) {
				out = append(out, r)
			}
		}
	}
	return out
}

func medianInt64(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func (r T2Repo) ensureClean(ctx context.Context, root string) error {
	h, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(h) != r.R {
		return fmt.Errorf("%s: HEAD %s khác R %s", r.Name, strings.TrimSpace(h), r.R)
	}
	// Phục hồi cây (lần chạy trước có thể bị ngắt giữa chừng).
	if _, err := git(ctx, root, "checkout", "-q", "--", "."); err != nil {
		return err
	}
	st, err := git(ctx, root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(st) != "" {
		return fmt.Errorf("%s: cây làm việc không sạch: %.200s", r.Name, st)
	}
	return nil
}

// Baseline chạy bộ test 3 lần ở R; tệp đỏ cả 3 lượt = hỏng sẵn, kết quả không đồng nhất = flaky.
func (r T2Repo) Baseline(ctx context.Context, cfg *T2Config, root string) (T2Baseline, error) {
	bp := filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")
	if !cfg.Rebaseline {
		if b, err := os.ReadFile(bp); err == nil {
			var bl T2Baseline
			if json.Unmarshal(b, &bl) == nil && bl.R == r.R {
				return bl, nil
			}
		}
	}
	bl := T2Baseline{Repo: r.Name, R: r.R}
	var runs []map[string]string
	for i := 0; i < 3; i++ {
		m, n, d, to, msg, err := r.runSuite(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), 20*time.Minute, nil)
		if err != nil {
			return bl, err
		}
		if to || m == nil {
			return bl, fmt.Errorf("%s: baseline lượt %d lỗi: %s", r.Name, i+1, msg)
		}
		cfg.logf("%s: baseline %d/3 %d tệp, %s", r.Name, i+1, len(m), d.Round(time.Second))
		runs = append(runs, m)
		bl.RunMs = append(bl.RunMs, d.Milliseconds())
		bl.Tests = n
	}
	all := map[string]bool{}
	for _, m := range runs {
		for f := range m {
			all[f] = true
		}
	}
	for f := range all {
		nf, np, nm := 0, 0, 0
		for _, m := range runs {
			switch m[f] {
			case "failed":
				nf++
			case "passed":
				np++
			default:
				nm++
			}
		}
		switch {
		case nf == 3:
			bl.Failing = append(bl.Failing, f)
		case nf > 0 || (np > 0 && nm > 0):
			bl.Flaky = append(bl.Flaky, f)
		}
	}
	sort.Strings(bl.Failing)
	sort.Strings(bl.Flaky)
	bl.Files = len(all)
	b, _ := json.MarshalIndent(bl, "", " ")
	os.WriteFile(bp, b, 0o644)
	return bl, nil
}

// BuildOracle chạy các đột biến chưa có trong oracle.jsonl (tuần tự trong repo: cây làm việc chỉ có một đột biến tại một thời điểm).
func (r T2Repo) BuildOracle(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	bl, err := r.Baseline(ctx, cfg, root)
	if err != nil {
		return err
	}
	cfg.logf("%s: baseline %d tệp test, %d hỏng sẵn, %d flaky", r.Name, bl.Files, len(bl.Failing), len(bl.Flaky))
	excl := bl.excluded()
	mp := filepath.Join(cfg.Dir, "mutants-"+r.Dir+".json")
	var muts []T2Mutant
	if b, err := os.ReadFile(mp); err == nil {
		var c struct {
			R       string     `json:"r"`
			Seed    string     `json:"seed"`
			Count   int        `json:"count"`
			Mutants []T2Mutant `json:"mutants"`
		}
		if json.Unmarshal(b, &c) == nil && c.R == r.R && c.Seed == r.Seed && c.Count == r.Mutants {
			muts = c.Mutants
		}
	}
	if muts == nil {
		muts, err = r.GenMutants(ctx, cfg.Dir, cfg.Manifest.TS, root)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(map[string]any{"r": r.R, "seed": r.Seed, "count": r.Mutants, "mutants": muts}, "", " ")
		os.WriteFile(mp, b, 0o644)
	}
	cfg.logf("%s: %d đột biến", r.Name, len(muts))
	var med []int64
	for _, ms := range bl.RunMs {
		med = append(med, ms)
	}
	timeout := time.Duration(medianInt64(med))*4*time.Millisecond + 2*time.Minute
	for _, m := range muts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rec := T2Record{Repo: r.Name, R: r.R, Mutant: m}
		if old, ok := have[rec.key()]; ok && !cfg.Rebuild && (old.Status == "killed" || old.Status == "survived" || old.Status == "timeout") {
			continue
		}
		t0 := time.Now()
		c, mutated, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), m)
		if err != nil {
			rec.Status, rec.Detail = "error", err.Error()
			store.Append(rec)
			continue
		}
		rec.Commit = c
		wt := filepath.Join(root, filepath.FromSlash(m.File))
		orig, err := os.ReadFile(wt)
		if err != nil {
			rec.Status, rec.Detail = "error", err.Error()
			store.Append(rec)
			continue
		}
		func() {
			defer func() {
				os.WriteFile(wt, orig, 0o644)
				git(context.Background(), root, "checkout", "-q", "--", m.File)
			}()
			if err := os.WriteFile(wt, mutated, 0o644); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			res, _, _, to, msg, err := r.runSuite(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), timeout, nil)
			if err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			if to {
				rec.Status, rec.Detail = "timeout", msg
				return
			}
			if res == nil {
				rec.Status, rec.Detail = "error", msg
				return
			}
			rec.TestFiles = len(res)
			for f, st := range res {
				if st != "failed" {
					continue
				}
				if excl[f] {
					rec.RedExcluded = append(rec.RedExcluded, f)
				} else {
					rec.Red = append(rec.Red, f)
				}
			}
			sort.Strings(rec.RedExcluded)
			if err := r.confirmRed(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), timeout, &rec); err != nil {
				rec.Status, rec.Detail = "error", err.Error()
				return
			}
			rec.Status = "survived"
			if len(rec.Red) > 0 {
				rec.Status = "killed"
			}
			rec.Native, err = r.runNative(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), m.File)
			if err != nil {
				rec.NativeErr = err.Error()
			}
		}()
		rec.Ms = time.Since(t0).Milliseconds()
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s %s %s:%d -> %s (đỏ %d, native %d) %s", r.Name, m.ID, m.Op, m.File, m.Line, rec.Status, len(rec.Red), len(rec.Native), time.Duration(rec.Ms)*time.Millisecond)
	}
	return r.ensureClean(ctx, root)
}

// ---- kiểm bgraph ----

// T2Check là kết quả kiểm một đột biến với bgraph.
type T2Check struct {
	Rec        T2Record         `json:"rec"`
	Scope      string           `json:"scope,omitempty"`
	Selected   int              `json:"selected"`
	Units      int              `json:"units"`
	Missed     []string         `json:"missed,omitempty"`      // tệp đỏ ngoài phần chọn (vi phạm)
	NativeOnly []string         `json:"native_only,omitempty"` // tệp của công cụ gốc không có trong phần chọn của bgraph
	Widenings  map[string]int   `json:"widenings,omitempty"`
	WideList   []model.Widening `json:"widening_list,omitempty"`
	Diag       []FileDiag       `json:"diag,omitempty"` // chẩn đoán tệp bị thiếu (vi phạm) hoặc native-only
	Near       []string         `json:"near,omitempty"` // chuỗi lý do của các test gần tệp bị thiếu
	Err        string           `json:"err,omitempty"`
	Ms         int64            `json:"ms"`
}

func (c T2Check) Full() bool { return c.Scope == string(model.TierFull) }

// selCovers cho biết phần chọn có chứa tệp test f: là Files/ID của một đơn vị, hoặc một đơn vị Whole bao dự án chứa f.
func selCovers(sel model.Selection, f string) bool {
	for _, u := range sel.Tests {
		if u.Unit == f {
			return true
		}
		for _, uf := range u.Files {
			if uf == f {
				return true
			}
		}
		if u.Whole && (u.Project == "." || u.Project == "" || under(f, u.Project)) {
			return true
		}
	}
	return false
}

// CheckMutant chạy Select(R→M) và so với oracle.
func (r T2Repo) CheckMutant(ctx context.Context, ix *bgraph.Index, root, idxDir string, rec T2Record) T2Check {
	ck := T2Check{Rec: rec}
	t0 := time.Now()
	defer func() { ck.Ms = time.Since(t0).Milliseconds() }()
	m, _, err := r.MutantCommit(ctx, root, idxDir, rec.Mutant)
	if err != nil {
		ck.Err = err.Error()
		return ck
	}
	sel, err := ix.Select(ctx, bgraph.Change{Base: r.R, Head: m})
	if err != nil {
		ck.Err = err.Error()
		return ck
	}
	ck.Scope = string(sel.Scope)
	ck.Selected = len(sel.Tests)
	ck.Units = sel.IndexStats["units"]
	ck.Widenings = map[string]int{}
	for _, w := range sel.Widening {
		ck.Widenings[string(w.Tier)+":"+w.Reason]++
	}
	ck.WideList = sel.Widening
	if len(ck.WideList) > 12 {
		ck.WideList = ck.WideList[:12]
	}
	if ck.Full() {
		return ck
	}
	for _, f := range rec.Red {
		if !selCovers(sel, f) {
			ck.Missed = append(ck.Missed, f)
		}
	}
	for _, f := range rec.Native {
		if !selCovers(sel, f) {
			ck.NativeOnly = append(ck.NativeOnly, f)
		}
	}
	diagFor := ck.Missed
	if len(diagFor) == 0 && len(ck.NativeOnly) > 0 {
		diagFor = ck.NativeOnly
		if len(diagFor) > 5 {
			diagFor = diagFor[:5]
		}
	}
	if len(diagFor) > 0 {
		ck.Diag = diagnose(ctx, root, r.R, diagFor, sel.Changed)
	}
	if len(ck.Missed) > 0 {
		for _, f := range ck.Missed {
			dir := path.Dir(f)
			n := 0
			for _, u := range sel.Tests {
				if n < 4 && (path.Dir(u.Unit) == dir || strings.HasPrefix(u.Unit, path.Dir(dir)+"/")) && len(u.Reasons) > 0 {
					ck.Near = append(ck.Near, u.Unit+": "+strings.Join(u.Reasons, " | "))
					n++
				}
			}
		}
	}
	return ck
}

// T2RepoResult là kết quả một repo.
type T2RepoResult struct {
	Repo                 T2Repo     `json:"repo"`
	Baseline             T2Baseline `json:"baseline"`
	Checks               []T2Check  `json:"checks"`
	Killed               int        `json:"killed"`
	Survived             int        `json:"survived"`
	Timeout              int        `json:"timeout"`
	Errors               int        `json:"errors"`
	Full                 int        `json:"full"` // đột biến bị nới full (không tính bằng chứng)
	Violations           []T2Check  `json:"violations"`
	NativeNotSubset      []T2Check  `json:"native_not_subset"`
	MedBgraph, MedNative float64
	OracleMs, CheckMs    int64
	Error                string `json:"error,omitempty"`
}

// T2Result là kết quả mọi repo.
type T2Result struct {
	Repos      []T2RepoResult `json:"repos"`
	Wall       time.Duration  `json:"wall"`
	RunTests   bool           `json:"run_tests"`
	Violations int            `json:"violations"`
}

// RunT2 xây oracle (nếu RunTests) rồi kiểm bgraph trên oracle đã cache. Các repo chạy song song.
func RunT2(ctx context.Context, cfg T2Config) (T2Result, error) {
	start := time.Now()
	store := &OracleStore{Path: cfg.oraclePath()}
	repos := cfg.selected()
	res := T2Result{RunTests: cfg.RunTests, Repos: make([]T2RepoResult, len(repos))}
	have := map[string]T2Record{}
	if recs, err := store.Load(); err == nil {
		for _, r := range recs {
			have[r.key()] = r
		}
	}
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := &res.Repos[i]
			rr.Repo = r
			root := filepath.Join(cfg.Dir, r.Dir)
			if cfg.Reconfirm {
				if err := r.Reconfirm(ctx, &cfg, store, have); err != nil {
					rr.Error = err.Error()
					return
				}
			}
			if cfg.RunTests {
				t0 := time.Now()
				if err := r.BuildOracle(ctx, &cfg, store, have); err != nil {
					rr.Error = err.Error()
					cfg.logf("%s: lỗi oracle: %v", r.Name, err)
					return
				}
				rr.OracleMs = time.Since(t0).Milliseconds()
			}
			if b, err := os.ReadFile(filepath.Join(cfg.Dir, "baseline-"+r.Dir+".json")); err == nil {
				json.Unmarshal(b, &rr.Baseline)
			}
			recs, err := store.Load()
			if err != nil {
				rr.Error = "chưa có oracle: " + err.Error() + " (chạy `bgraph-eval t2 --run-tests`)"
				return
			}
			var mine []T2Record
			for _, x := range recs {
				if x.Repo == r.Name && x.R == r.R {
					mine = append(mine, x)
				}
			}
			if len(mine) == 0 {
				rr.Error = "oracle không có đột biến cho " + r.Name + "@" + r.R
				return
			}
			t0 := time.Now()
			r.checkAll(ctx, &cfg, root, mine, rr)
			rr.CheckMs = time.Since(t0).Milliseconds()
		}()
	}
	wg.Wait()
	for _, r := range res.Repos {
		res.Violations += len(r.Violations)
	}
	res.Wall = time.Since(start)
	return res, ctx.Err()
}

func (r T2Repo) checkAll(ctx context.Context, cfg *T2Config, root string, recs []T2Record, rr *T2RepoResult) {
	ix, err := bgraph.Open(ctx, root, bgraph.Options{StateDir: filepath.Join(cfg.Dir, "state", r.Dir)})
	if err != nil {
		rr.Error = err.Error()
		return
	}
	defer ix.Close()
	var bg, nv []float64
	for _, rec := range recs {
		switch rec.Status {
		case "killed":
			rr.Killed++
		case "survived":
			rr.Survived++
		case "timeout":
			rr.Timeout++
			continue
		default:
			rr.Errors++
			continue
		}
		c := r.CheckMutant(ctx, ix, root, filepath.Join(cfg.Dir, "idx"), rec)
		rr.Checks = append(rr.Checks, c)
		switch {
		case c.Err != "":
			rr.Errors++
		case c.Full():
			rr.Full++
		default:
			if c.Units > 0 {
				bg = append(bg, float64(c.Selected)/float64(c.Units))
				if rec.NativeErr == "" {
					nv = append(nv, float64(len(rec.Native))/float64(c.Units))
				}
			}
		}
		if len(c.Missed) > 0 {
			rr.Violations = append(rr.Violations, c)
		}
		if len(c.NativeOnly) > 0 {
			rr.NativeNotSubset = append(rr.NativeNotSubset, c)
		}
	}
	rr.MedBgraph, rr.MedNative = Percentile(bg, 50), Percentile(nv, 50)
}

// confirmRed xác nhận các tệp đỏ ở lượt đầy đủ: chạy lại RIÊNG các tệp đó (2 lượt, trên cây đang có đột biến); chỉ tệp đỏ ở cả hai lượt
// mới là đỏ. Tệp đỏ một lần rồi xanh (hết thời hạn do tải máy, thứ tự chạy...) vào RedUnconfirmed và không dùng làm oracle.
func (r T2Repo) confirmRed(ctx context.Context, root, tmp string, timeout time.Duration, rec *T2Record) error {
	cand := append(append([]string(nil), rec.Red...), rec.RedUnconfirmed...)
	sort.Strings(cand)
	cand = dedup(cand)
	rec.Red, rec.RedUnconfirmed, rec.Confirmed = nil, nil, true
	if len(cand) == 0 {
		return nil
	}
	still := map[string]bool{}
	for _, f := range cand {
		still[f] = true
	}
	for round := 0; round < 2; round++ {
		res, _, _, to, msg, err := r.runSuite(ctx, root, tmp, timeout, cand)
		if err != nil {
			return err
		}
		if to || res == nil {
			return fmt.Errorf("xác nhận lượt %d lỗi: %s", round+1, msg)
		}
		for f := range still {
			if res[f] != "failed" {
				delete(still, f)
			}
		}
	}
	for _, f := range cand {
		if still[f] {
			rec.Red = append(rec.Red, f)
		} else {
			rec.RedUnconfirmed = append(rec.RedUnconfirmed, f)
		}
	}
	return nil
}

// Reconfirm xác nhận lại các đột biến "killed" chưa xác nhận trong oracle cache (không chạy lại bộ đầy đủ).
func (r T2Repo) Reconfirm(ctx context.Context, cfg *T2Config, store *OracleStore, have map[string]T2Record) error {
	root := filepath.Join(cfg.Dir, r.Dir)
	if err := r.ensureClean(ctx, root); err != nil {
		return err
	}
	bl, err := r.Baseline(ctx, cfg, root)
	if err != nil {
		return err
	}
	timeout := time.Duration(medianInt64(bl.RunMs))*4*time.Millisecond + 2*time.Minute
	var recs []T2Record
	for _, x := range have {
		if x.Repo == r.Name && x.R == r.R && x.Status == "killed" && !x.Confirmed {
			recs = append(recs, x)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Mutant.ID < recs[j].Mutant.ID })
	for _, rec := range recs {
		_, mutated, err := r.MutantCommit(ctx, root, filepath.Join(cfg.Dir, "idx"), rec.Mutant)
		if err != nil {
			return err
		}
		wt := filepath.Join(root, filepath.FromSlash(rec.Mutant.File))
		orig, err := os.ReadFile(wt)
		if err != nil {
			return err
		}
		before := len(rec.Red)
		err = func() error {
			defer func() {
				os.WriteFile(wt, orig, 0o644)
				git(context.Background(), root, "checkout", "-q", "--", rec.Mutant.File)
			}()
			if err := os.WriteFile(wt, mutated, 0o644); err != nil {
				return err
			}
			return r.confirmRed(ctx, root, filepath.Join(cfg.Dir, "tmp", r.Dir), timeout, &rec)
		}()
		if err != nil {
			cfg.logf("%s: %s xác nhận lỗi: %v", r.Name, rec.Mutant.ID, err)
			continue
		}
		rec.Status = "survived"
		if len(rec.Red) > 0 {
			rec.Status = "killed"
		}
		if err := store.Append(rec); err != nil {
			return err
		}
		cfg.logf("%s: %s xác nhận: đỏ %d -> %d (không tái hiện: %d) %s", r.Name, rec.Mutant.ID, before, len(rec.Red), len(rec.RedUnconfirmed), rec.Status)
	}
	return r.ensureClean(ctx, root)
}
