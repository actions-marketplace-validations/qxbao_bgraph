// Lệnh bgraph: CLI chọn test bị ảnh hưởng (mục 7.2 của docs/plan.md).
//
// Mã thoát: 0 thành công, 1 lỗi công cụ, 2 dùng sai.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"bgraph"
	"bgraph/internal/model"
	"bgraph/internal/runner"
)

const version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `bgraph - chọn test bị ảnh hưởng từ đồ thị import mức tệp (đóng khi lỗi)

Dùng:
  bgraph affected  [--root DIR] [--base REF] [--head REF] [--json] [--failed ID,...]
  bgraph explain   [--root DIR] [--base REF] <test|tệp>
  bgraph unresolved [--root DIR] [--class unknown|dynamic|ambiguous|uncertain_scan]
  bgraph command   [--root DIR] [--base REF] [--list-dir DIR] [--json]
  bgraph index     [--root DIR]
  bgraph audit | doctor        (chưa cài đặt)
  bgraph version | help
`

// run là điểm vào có thể test: trả mã thoát.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "bgraph", version)
		return 0
	case "audit", "doctor":
		fmt.Fprintf(stderr, "bgraph %s: chưa cài đặt (giai đoạn sau)\n", cmd)
		return 2
	case "affected", "explain", "unresolved", "command", "index":
	default:
		fmt.Fprintf(stderr, "lệnh không hợp lệ: %q\n\n%s", cmd, usage)
		return 2
	}

	fs := flag.NewFlagSet("bgraph "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "thư mục trong repo")
	var base, head, failed, class, listDir string
	var asJSON bool
	switch cmd {
	case "affected":
		fs.StringVar(&base, "base", "", "ref mốc (mặc định: merge-base với origin/HEAD, origin/main, main, master)")
		fs.StringVar(&head, "head", "", "commit đích (mặc định: cây làm việc)")
		fs.StringVar(&failed, "failed", "", "id đơn vị test hỏng ở lượt trước, cách nhau bởi dấu phẩy")
		fs.BoolVar(&asJSON, "json", false, "in JSON")
	case "explain":
		fs.StringVar(&base, "base", "", "ref mốc")
	case "unresolved":
		fs.StringVar(&class, "class", "", "lọc theo lớp: unknown|dynamic|ambiguous|uncertain_scan")
	case "command":
		fs.StringVar(&base, "base", "", "ref mốc")
		fs.StringVar(&listDir, "list-dir", "", "thư mục ghi tệp danh sách")
		fs.BoolVar(&asJSON, "json", false, "in JSON")
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if cmd == "explain" && fs.NArg() != 1 {
		fmt.Fprintln(stderr, "explain cần đúng một đối số: <test|tệp>")
		return 2
	}
	if cmd != "explain" && fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: đối số thừa: %v\n", cmd, fs.Args())
		return 2
	}
	if cmd == "unresolved" && class != "" && !slices.Contains([]string{"unknown", "dynamic", "ambiguous", "uncertain_scan"}, class) {
		fmt.Fprintf(stderr, "--class không hợp lệ: %q\n", class)
		return 2
	}

	ctx := context.Background()
	ix, err := bgraph.Open(ctx, *root, bgraph.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "lỗi:", err)
		return 1
	}
	defer ix.Close()

	fail := func(err error) int { fmt.Fprintln(stderr, "lỗi:", err); return 1 }

	switch cmd {
	case "index":
		st, err := ix.Analyze(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "tệp: %d\ncạnh: %d\nđơn vị test: %d\nchưa phân giải: %d\nthời gian: %s\n",
			st.Files, st.Edges, st.Units, len(st.Unresolved), st.Elapsed.Round(1e6))
		return 0
	case "unresolved":
		list, err := ix.Unresolved(ctx)
		if err != nil {
			return fail(err)
		}
		n := 0
		for _, u := range list {
			if class != "" && string(u.Class) != class {
				continue
			}
			n++
			fmt.Fprintf(stdout, "%s:%d  %q  [%s]  nới tới %s %s\n", u.File, u.Line, u.Spec, u.Class, u.ScopeTier, scopeDir(u.ScopeDir))
			if u.Detail != "" {
				fmt.Fprintf(stdout, "    %s\n", u.Detail)
			}
			fmt.Fprintf(stdout, "    gợi ý bgraph.toml:\n%s\n", indent(suggestRule(u), "      "))
		}
		fmt.Fprintf(stdout, "tổng: %d\n", n)
		return 0
	}

	// affected / explain / command: cần Selection.
	if base == "" {
		b, err := ix.DefaultBase(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "cảnh báo:", err, "→ coi như không có base (chạy full)")
		}
		base = b // rỗng → Select trả full (no_base)
	}
	ch := bgraph.Change{Base: base, Head: head, Worktree: head == ""}
	if failed != "" {
		ch.PreviouslyFailed = strings.Split(failed, ",")
	}
	sel, err := ix.Select(ctx, ch)
	if err != nil {
		return fail(err)
	}

	switch cmd {
	case "affected":
		if asJSON {
			return printJSON(stdout, stderr, sel)
		}
		printSelection(stdout, sel)
	case "explain":
		explain(stdout, sel, fs.Arg(0))
	case "command":
		plan, err := runner.Commands(sel, runner.Options{ListDir: listDir})
		if err != nil {
			return fail(err)
		}
		if asJSON {
			return printJSON(stdout, stderr, plan)
		}
		fmt.Fprintf(stdout, "# scope: %s\n", plan.Scope)
		for _, n := range plan.Notes {
			fmt.Fprintf(stdout, "# %s\n", n)
		}
		for _, c := range plan.Commands {
			line := shellLine(c.Argv)
			if c.Project != "." && c.Project != "" {
				line = "cd " + shellQuote(c.Project) + " && " + line
			}
			fmt.Fprintln(stdout, line)
		}
	}
	return 0
}

func printJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "lỗi:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

func scopeDir(d string) string {
	if d == "" {
		return "(repo)"
	}
	return d
}

func printSelection(w io.Writer, sel model.Selection) {
	fmt.Fprintf(w, "scope: %s\n", sel.Scope)
	if sel.Base != "" {
		fmt.Fprintf(w, "base: %s\n", sel.Base)
	}
	fmt.Fprintf(w, "tệp đổi: %d\n", len(sel.Changed))
	if len(sel.Widening) > 0 {
		fmt.Fprintln(w, "\nnới:")
		for _, x := range sel.Widening {
			fmt.Fprintf(w, "  - %s  %s  scope=%s %s\n", x.Reason, x.Path, x.Tier, x.Scope)
			if x.Detail != "" {
				fmt.Fprintf(w, "      %s\n", x.Detail)
			}
			if x.Suggest != "" {
				fmt.Fprintf(w, "      gợi ý:\n%s\n", indent(x.Suggest, "        "))
			}
		}
	}
	if len(sel.Warnings) > 0 {
		fmt.Fprintln(w, "\ncảnh báo:")
		for _, x := range sel.Warnings {
			fmt.Fprintf(w, "  - %s  %s  %s\n", x.Reason, x.Path, x.Detail)
		}
	}
	fmt.Fprintf(w, "\ntest được chọn: %d\n", len(sel.Tests))
	for _, t := range sel.Tests {
		first := ""
		if len(t.Reasons) > 0 {
			first = t.Reasons[0]
		}
		fmt.Fprintf(w, "  %s  (%s)  %s\n", t.Unit, t.Runner, first)
	}
}

func explain(w io.Writer, sel model.Selection, target string) {
	target = strings.TrimPrefix(strings.ReplaceAll(target, "\\", "/"), "./")
	found := false
	for _, t := range sel.Tests {
		if t.Unit == target || slices.Contains(t.Files, target) {
			found = true
			fmt.Fprintf(w, "test %s được chọn vì:\n", t.Unit)
			for _, r := range t.Reasons {
				fmt.Fprintf(w, "  - %s\n", r)
			}
		}
	}
	if found {
		return
	}
	n := 0
	for _, t := range sel.Tests {
		for _, r := range t.Reasons {
			if strings.Contains(r, target) {
				if n == 0 {
					fmt.Fprintf(w, "tệp %s xuất hiện trong lý do của:\n", target)
				}
				n++
				fmt.Fprintf(w, "  %s: %s\n", t.Unit, r)
				break
			}
		}
	}
	if slices.Contains(sel.Changed, target) {
		fmt.Fprintf(w, "%s là tệp đã đổi", target)
		if n == 0 {
			fmt.Fprint(w, " nhưng không test nào được chọn do nó")
		}
		fmt.Fprintln(w)
	} else if n == 0 {
		fmt.Fprintf(w, "không có test nào được chọn liên quan tới %s\n", target)
	}
}

func suggestRule(u model.Unresolved) string {
	dir := u.ScopeDir
	if dir == "" {
		dir = "."
	}
	return fmt.Sprintf("[[rule]]\npaths = [%q]\naffects = [%q]  # hoặc glob test; \"none\" nếu chắc chắn vô hại", u.File, dir)
}

func indent(s, pre string) string {
	return pre + strings.ReplaceAll(s, "\n", "\n"+pre)
}

// shellLine ghép argv thành một dòng shell (chỉ để hiển thị; dùng nháy đơn kiểu POSIX).
func shellLine(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
