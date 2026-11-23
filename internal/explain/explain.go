// Package explain answers "why was this test selected / where does this file lead" from a Selection; shared by the CLI and the MCP server.
package explain

import (
	"fmt"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// Lines explains target (a test unit or a file) against sel, one line per element (no trailing newlines).
func Lines(sel model.Selection, target string) []string {
	target = strings.TrimPrefix(strings.ReplaceAll(target, "\\", "/"), "./")
	var out []string
	found := false
	for _, t := range sel.Tests {
		if t.Unit == target || slices.Contains(t.Files, target) {
			found = true
			out = append(out, fmt.Sprintf("test %s is selected because:", t.Unit))
			for _, r := range t.Reasons {
				out = append(out, fmt.Sprintf("  - %s", r))
			}
			if t.Dynamic != "" {
				out = append(out, "  "+t.Dynamic)
			} else if t.Widened {
				for _, w := range sel.Warnings {
					if w.Reason == model.ReasonDynamicSkipped {
						out = append(out, "  the dynamic layer did not narrow this selection: "+w.Detail)
					}
				}
			}
		}
	}
	for _, n := range sel.Narrowed {
		if n.Unit == target {
			found = true
			line := "  - " + n.Reason
			if !n.Recorded.IsZero() {
				line += " (recorded " + n.Recorded.UTC().Format("2006-01-02 15:04 MST")
				if n.Commit != "" {
					line += " at " + short(n.Commit)
				}
				line += ")"
			}
			out = append(out, fmt.Sprintf("test %s was selected but removed by the dynamic layer:", n.Unit), line)
		}
	}
	if found {
		return out
	}
	n := 0
	for _, t := range sel.Tests {
		for _, r := range t.Reasons {
			if strings.Contains(r, target) {
				if n == 0 {
					out = append(out, fmt.Sprintf("file %s appears in the reasons of:", target))
				}
				n++
				out = append(out, fmt.Sprintf("  %s: %s", t.Unit, r))
				break
			}
		}
	}
	if slices.Contains(sel.Changed, target) {
		line := fmt.Sprintf("%s is a changed file", target)
		if n == 0 {
			line += " but no test was selected because of it"
		}
		out = append(out, line)
	} else if n == 0 {
		out = append(out, fmt.Sprintf("no selected test is related to %s", target))
	}
	return out
}
