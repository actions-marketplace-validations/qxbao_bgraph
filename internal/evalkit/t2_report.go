package evalkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// T2Markdown dựng báo cáo T2: vi phạm trước, rồi bảng theo repo, rồi chi tiết "native ⊄ bgraph".
func T2Markdown(q T2Result) string {
	var b strings.Builder
	verdict := "ĐẠT (0 vi phạm T2)"
	if q.Violations > 0 {
		verdict = fmt.Sprintf("KHÔNG ĐẠT: %d vi phạm T2", q.Violations)
	}
	mode := "kiểm lại trên oracle cache"
	if q.RunTests {
		mode = "xây/làm mới oracle (--run-tests)"
	}
	fmt.Fprintf(&b, "# bgraph-eval t2 — %s\n\nChế độ: %s. Thời gian: %s\n\n", verdict, mode, q.Wall.Round(time.Millisecond))
	b.WriteString("## Vi phạm\n\n")
	n := 0
	for _, r := range q.Repos {
		for _, c := range r.Violations {
			n++
			fmt.Fprintf(&b, "- `%s` %s %s:%d (%s) `%s -> %s`: tệp đỏ ngoài phần chọn %v; scope=%s; nới %v\n", r.Repo.Name, c.Rec.Mutant.ID, c.Rec.Mutant.File, c.Rec.Mutant.Line, c.Rec.Mutant.Op, short(c.Rec.Mutant.Before), short(c.Rec.Mutant.After), c.Missed, c.Scope, c.Widenings)
			for _, d := range c.Diag {
				fmt.Fprintf(&b, "  - chẩn đoán %s: role=%s in_unit=%v units=%v imports=%d unresolved=%v reach=%d reached_changed=%v\n", d.File, d.Role, d.InUnit, d.Units, len(d.Imports), d.Unresolved, d.ReachSize, d.ReachedChanged)
			}
			for _, x := range c.Near {
				fmt.Fprintf(&b, "  - gần: %s\n", x)
			}
		}
	}
	if n == 0 {
		b.WriteString("(không có)\n")
	}
	var errs []string
	for _, r := range q.Repos {
		if r.Error != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", r.Repo.Name, r.Error))
		}
		for _, c := range r.Checks {
			if c.Err != "" {
				errs = append(errs, fmt.Sprintf("%s %s: %s", r.Repo.Name, c.Rec.Mutant.ID, c.Err))
			}
		}
	}
	if len(errs) > 0 {
		b.WriteString("\n## Lỗi / cảnh báo\n\n")
		for _, e := range errs {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	b.WriteString("\n## Theo repo\n\n| repo | R | baseline tệp | hỏng sẵn | flaky | đột biến | bị giết | sống sót | timeout/lỗi | nới full | vi phạm | native ⊄ bgraph | median sel/units bgraph | native | all | check ms |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range q.Repos {
		fmt.Fprintf(&b, "| %s | %.10s | %d | %d | %d | %d | %d | %d | %d/%d | %d | %d | %d | %.3f | %.3f | 1 | %d |\n", r.Repo.Name, r.Repo.R, r.Baseline.Files, len(r.Baseline.Failing), len(r.Baseline.Flaky),
			r.Killed+r.Survived+r.Timeout+r.Errors, r.Killed, r.Survived, r.Timeout, r.Errors, r.Full, len(r.Violations), len(r.NativeNotSubset), r.MedBgraph, r.MedNative, r.CheckMs)
	}
	b.WriteString("\n## native ⊄ bgraph\n\n")
	n = 0
	for _, r := range q.Repos {
		for _, c := range r.NativeNotSubset {
			n++
			red := map[string]bool{}
			for _, f := range c.Rec.Red {
				red[f] = true
			}
			fmt.Fprintf(&b, "- `%s` %s %s:%d: native %d tệp, bgraph %d/%d; chỉ native: %v", r.Repo.Name, c.Rec.Mutant.ID, c.Rec.Mutant.File, c.Rec.Mutant.Line, len(c.Rec.Native), c.Selected, c.Units, c.NativeOnly)
			var redOnly []string
			for _, f := range c.NativeOnly {
				if red[f] {
					redOnly = append(redOnly, f)
				}
			}
			fmt.Fprintf(&b, " (trong đó đỏ thật: %v)\n", redOnly)
			for _, d := range c.Diag {
				fmt.Fprintf(&b, "  - %s: in_unit=%v reach=%d reached_changed=%v unresolved=%v\n", d.File, d.InUnit, d.ReachSize, d.ReachedChanged, d.Unresolved)
			}
		}
	}
	if n == 0 {
		b.WriteString("(không có: native ⊆ bgraph trên mọi đột biến không nới full)\n")
	}
	b.WriteString("\n## Đột biến\n\n| repo | id | toán tử | vị trí | trước → sau | trạng thái | đỏ | scope | sel/units | native | native-only |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range q.Repos {
		for _, c := range r.Checks {
			fmt.Fprintf(&b, "| %s | %s | %s | %s:%d | `%s` → `%s` | %s | %d | %s | %d/%d | %d | %d |\n", r.Repo.Dir, c.Rec.Mutant.ID, c.Rec.Mutant.Op, c.Rec.Mutant.File, c.Rec.Mutant.Line, short(c.Rec.Mutant.Before), short(c.Rec.Mutant.After), c.Rec.Status, len(c.Rec.Red), c.Scope, c.Selected, c.Units, len(c.Rec.Native), len(c.NativeOnly))
		}
	}
	return b.String()
}

func short(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "`", "'")
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// WriteT2 ghi t2.json và t2.md vào dir.
func WriteT2(dir string, q T2Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(q, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "t2.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "t2.md"), []byte(T2Markdown(q)), 0o644)
}
