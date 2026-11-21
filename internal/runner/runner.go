// Package runner biến một Selection thành lệnh chạy test theo từng test runner (mục 7.4 của docs/plan.md).
//
// Nguyên tắc 11: KHÔNG BAO GIỜ phát lệnh với danh sách rỗng. scope none → không có lệnh; scope full → không có lệnh chọn lọc.
//
// Đối số vị trí có nghĩa khác nhau giữa các runner:
//   - vitest: chuỗi con (substring) của đường dẫn → chọn thừa (aa.test.ts khớp a.test.ts) là chấp nhận được.
//   - jest: dùng --runTestsByPath nên là đường dẫn chính xác, không phải regex.
//   - playwright: regex → escape bằng regexp.QuoteMeta; ưu tiên --test-list khi có ListDir.
//   - mocha, ava: đường dẫn/glob; tên tệp có ký tự glob → lùi về lệnh cả dự án.
//   - node --test: glob (Node >= 21); tên có ký tự glob → lùi về `node --test` cả dự án.
package runner

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"bgraph/internal/model"
)

// Options cấu hình adapter.
type Options struct {
	ListDir       string  // nơi ghi tệp danh sách; "" → không dùng tệp danh sách, truyền đối số trực tiếp
	MaxArgChars   int     // mặc định 8000 (dưới giới hạn 8191 của cmd.exe)
	FullThreshold float64 // 0 = tắt; số đơn vị chọn / tổng đơn vị > ngưỡng → full
}

// Command là một lệnh chạy test.
type Command struct {
	Runner   string   `json:"runner"`
	Project  string   `json:"project"` // thư mục làm việc, tương đối gốc repo
	Argv     []string `json:"argv"`
	ListFile string   `json:"list_file,omitempty"`
	Batch    int      `json:"batch,omitempty"` // 1..Batches khi chia lô
	Batches  int      `json:"batches,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// Plan là kế hoạch chạy.
type Plan struct {
	Scope    model.Tier `json:"scope"`
	Commands []Command  `json:"commands"`
	Notes    []string   `json:"notes,omitempty"`
}

const defaultMaxArgChars = 8000

const outsideMark = "\x00outside"

var globMeta = regexp.MustCompile(`[*?\[\]{}!()]`)

// Commands tạo kế hoạch lệnh từ phần chọn.
func Commands(sel model.Selection, opt Options) (Plan, error) {
	if opt.MaxArgChars <= 0 {
		opt.MaxArgChars = defaultMaxArgChars
	}
	plan := Plan{Scope: sel.Scope, Commands: []Command{}}
	full := func(note string) (Plan, error) {
		plan.Scope = model.TierFull
		plan.Commands = []Command{}
		plan.Notes = append(plan.Notes, note)
		return plan, nil
	}
	switch sel.Scope {
	case model.TierNone:
		plan.Notes = append(plan.Notes, "không có test nào bị ảnh hưởng: không chạy gì")
		return plan, nil
	case model.TierFull:
		return full("run the full suite")
	}
	if len(sel.Tests) == 0 {
		// Phạm vi khác none mà không có test: không thể phát lệnh rỗng, cũng không được im lặng bỏ qua → full.
		return full("phạm vi " + string(sel.Scope) + " nhưng không có test được chọn: run the full suite")
	}
	if total := sel.IndexStats["units"]; opt.FullThreshold > 0 && total > 0 &&
		float64(len(sel.Tests))/float64(total) > opt.FullThreshold {
		return full(fmt.Sprintf("đã chọn %d/%d đơn vị, vượt ngưỡng %.2f: run the full suite", len(sel.Tests), total, opt.FullThreshold))
	}

	type key struct{ project, runner string }
	groups := map[key][]string{}
	var keys []key
	for _, t := range sel.Tests {
		proj := cleanProject(t.Project)
		k := key{proj, t.Runner}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		files := t.Files
		if len(files) == 0 {
			files = []string{t.Unit}
		}
		for _, f := range files {
			rel, ok := relTo(proj, f)
			if !ok {
				// Không biểu diễn được tệp tương đối dự án: lùi về cả dự án (rộng hơn, an toàn).
				groups[k] = append(groups[k], outsideMark)
				continue
			}
			groups[k] = append(groups[k], rel)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].runner < keys[j].runner
	})

	for _, k := range keys {
		paths := dedupe(groups[k])
		sp, ok := specs[k.runner]
		if !ok {
			return full(fmt.Sprintf("runner %q (dự án %s) chưa được hỗ trợ: run the full suite", k.runner, k.project))
		}
		whole := false
		for _, p := range paths {
			if p == outsideMark || (sp.globby && globMeta.MatchString(p)) {
				whole = true
			}
		}
		if whole {
			plan.Commands = append(plan.Commands, Command{Runner: k.runner, Project: k.project, Argv: append([]string(nil), sp.whole...),
				Note: "tên tệp có ký tự glob hoặc nằm ngoài dự án: chạy cả dự án thay vì danh sách"})
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s/%s: lùi về lệnh cả dự án", k.project, k.runner))
			continue
		}
		cmds, notes, err := sp.build(k.project, k.runner, paths, opt)
		if err != nil {
			return Plan{}, err
		}
		plan.Commands = append(plan.Commands, cmds...)
		plan.Notes = append(plan.Notes, notes...)
	}
	return plan, nil
}

type spec struct {
	whole  []string // lệnh chạy cả dự án
	globby bool     // đối số vị trí là glob: tên có ký tự glob không đưa vào được an toàn
	build  func(project, runner string, paths []string, opt Options) ([]Command, []string, error)
}

func positional(prefix []string, note string) func(string, string, []string, Options) ([]Command, []string, error) {
	return func(project, runner string, paths []string, opt Options) ([]Command, []string, error) {
		cmds, notes := batch(runner, project, prefix, paths, opt.MaxArgChars)
		if note != "" {
			notes = append(notes, note)
		}
		return cmds, notes, nil
	}
}

var specs = map[string]spec{
	"vitest": {whole: []string{"npx", "vitest", "run"},
		build: positional([]string{"npx", "vitest", "run"},
			"vitest: đối số là chuỗi con của đường dẫn, nên a.test.ts cũng khớp aa.test.ts (chọn thừa, an toàn)")},
	"jest": {whole: []string{"npx", "jest"},
		build: positional([]string{"npx", "jest", "--runTestsByPath"}, "")},
	"mocha": {whole: []string{"npx", "mocha"}, globby: true,
		build: positional([]string{"npx", "mocha"}, "")},
	"ava": {whole: []string{"npx", "ava"}, globby: true,
		build: positional([]string{"npx", "ava"}, "")},
	"node-test": {whole: []string{"node", "--test"}, globby: true,
		build: positional([]string{"node", "--test"}, "node --test: đối số vị trí là glob (Node >= 21); các đường dẫn đã chọn không có ký tự glob")},
	"playwright": {whole: []string{"npx", "playwright", "test"}, build: buildPlaywright},
}

func buildPlaywright(project, runner string, paths []string, opt Options) ([]Command, []string, error) {
	if opt.ListDir != "" {
		if err := os.MkdirAll(opt.ListDir, 0o755); err != nil {
			return nil, nil, err
		}
		file := filepath.Join(opt.ListDir, "playwright-"+sanitize(project)+".txt")
		if err := os.WriteFile(file, []byte(strings.Join(paths, "\n")+"\n"), 0o644); err != nil {
			return nil, nil, err
		}
		return []Command{{Runner: runner, Project: project, Argv: []string{"npx", "playwright", "test", "--test-list", file}, ListFile: file}}, nil, nil
	}
	args := make([]string, len(paths))
	for i, p := range paths {
		args[i] = regexp.QuoteMeta(p) // đối số vị trí của Playwright là regex
	}
	cmds, notes := batch(runner, project, []string{"npx", "playwright", "test"}, args, opt.MaxArgChars)
	return cmds, append(notes, "playwright: đối số là regex (đã QuoteMeta, không neo): có thể chọn thừa"), nil
}

// batch chia args thành các lô sao cho độ dài dòng lệnh <= max (một đối số quá dài vẫn chiếm một lô riêng, không bị bỏ).
func batch(runner, project string, prefix, args []string, max int) ([]Command, []string) {
	base := 0
	for _, a := range prefix {
		base += len(a) + 1
	}
	var groups [][]string
	var cur []string
	size := base
	for _, a := range args {
		n := len(a) + 1
		if len(cur) > 0 && size+n > max {
			groups = append(groups, cur)
			cur, size = nil, base
		}
		cur = append(cur, a)
		size += n
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	cmds := make([]Command, 0, len(groups))
	for i, g := range groups {
		c := Command{Runner: runner, Project: project, Argv: append(append([]string(nil), prefix...), g...)}
		if len(groups) > 1 {
			c.Batch, c.Batches = i+1, len(groups)
			c.Note = "chia lô: fixture phạm vi session chạy lại mỗi lô; mã thoát phải gộp (lấy mã lỗi lớn nhất)"
		}
		cmds = append(cmds, c)
	}
	var notes []string
	if len(groups) > 1 {
		notes = append(notes, fmt.Sprintf("%s/%s: chia %d lô vì dòng lệnh vượt %d ký tự; chia lô đổi fixture phạm vi session và cách gộp mã thoát", project, runner, len(groups), max))
	}
	return cmds, notes
}

func cleanProject(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(p)
	if p == "" || p == "/" {
		return "."
	}
	return strings.TrimPrefix(p, "./")
}

// relTo trả f (tương đối gốc repo, gạch xuôi hoặc ngược) tương đối thư mục dự án.
func relTo(project, f string) (string, bool) {
	f = path.Clean(strings.ReplaceAll(f, "\\", "/"))
	if project == "." {
		return f, !strings.HasPrefix(f, "../") && !strings.HasPrefix(f, "/")
	}
	if !strings.HasPrefix(f, project+"/") {
		return "", false
	}
	return strings.TrimPrefix(f, project+"/"), true
}

func dedupe(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}
