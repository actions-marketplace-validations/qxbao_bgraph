package evalkit

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Reports of the PHP layer evaluation (t2phpdyn.go, phpdynreplay.go).

var rePHPReasonPath = regexp.MustCompile(` on [^ ]+`)

// PHPReasonClass reduces a PHP layer refusal to a class: the file prefix, paths and numbers dropped, the explanation in
// parentheses cut; refusals of non-code files keep the file's extension (or name when it has none).
func PHPReasonClass(why string) string {
	file := ""
	if i := strings.Index(why, ": "); i > 0 && !strings.Contains(why[:i], " ") {
		file = why[:i]
	}
	c := rePHPReasonPath.ReplaceAllString(why, " on <path>")
	c = RbReasonClass(c)
	if i := strings.Index(c, " ("); i > 0 {
		c = c[:i]
	}
	if file != "" && strings.HasPrefix(c, "not a PHP code file") {
		ext := path.Ext(file)
		if ext == "" {
			ext = path.Base(file)
		}
		c += " [" + ext + "]"
	}
	return c
}

func phpRatio(n, d int) (float64, bool) {
	if d <= 0 {
		return 0, false
	}
	return float64(n) / float64(d), true
}

func phpHistMD(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	return rbHist(m)
}

// PHPDynT2Markdown renders the hybrid pass.
func PHPDynT2Markdown(rs []PHPDynT2Result) string {
	var b, details strings.Builder
	b.WriteString("# PHP layer: hybrid T2\n\n")
	b.WriteString("Files run / files: test files the selection runs over the listed test files of the PHP units (a whole unit runs " +
		"all of them). File level only: the layer with the function-level refinement off. Violations: test files red at M (not at " +
		"R) that the hybrid selection does not run, confirmed by a second whole-suite run.\n\n")
	b.WriteString("| repo | test files | records complete / files | record s | suite s | killed | narrowed | refused | kept all | errors | " +
		"median files run/files static | hybrid | file level only | function-level mutants | violations | unconfirmed |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		narrowed, refused, keptAll, errs, viol, unconf, fnLevel := 0, 0, 0, 0, 0, 0, 0
		var sv, hv, fv []float64
		reasons, notes, levels := map[string]int{}, map[string]int{}, map[string]int{}
		notKilled := 0
		var maxRatio float64
		suiteMs := int64(0)
		if len(r.Baseline.RunMs) > 0 {
			suiteMs = medianInt64(r.Baseline.RunMs)
		}
		for _, m := range r.Mutants {
			if m.Oracle.Status != "killed" {
				notKilled++
				continue
			}
			if m.Error != "" {
				errs++
				continue
			}
			viol += len(m.Confirmed)
			unconf += len(m.Unconfirmed)
			if x, ok := phpRatio(m.Static.Files, m.Total); ok {
				sv = append(sv, x)
			}
			if x, ok := phpRatio(m.Hybrid.Files, m.Total); ok {
				hv = append(hv, x)
			}
			if x, ok := phpRatio(m.FileLevel, m.Total); ok && m.FileLevel >= 0 {
				fv = append(fv, x)
			} else if x, ok := phpRatio(m.Hybrid.Files, m.Total); ok {
				fv = append(fv, x) // the layer did not run: file level only runs the same
			}
			switch {
			case m.Hybrid.Skip != "":
				refused++
				reasons[PHPReasonClass(m.Hybrid.Skip)]++
			case m.Hybrid.Narrowed:
				narrowed++
			default:
				keptAll++
			}
			for k, n := range m.Hybrid.Notes {
				notes[k] += n
			}
			lv := PHPCommitLevel(m.Levels)
			if lv == "function" {
				fnLevel++
			}
			for _, l := range m.Levels {
				k := l.Level
				if l.Note != "" {
					k += ": " + PHPReasonClass(l.Note)
				}
				levels[k]++
			}
			if suiteMs > 0 {
				if x := float64(m.Oracle.Ms+m.SelectMs) / float64(suiteMs); x > maxRatio {
					maxRatio = x
				}
			}
		}
		n := len(r.Mutants) - notKilled - errs
		rec := fmt.Sprintf("%d/%d (lists %d/%d)", r.Records.Complete, r.Records.Files, r.Records.ListsOK, r.Records.Lists)
		if r.Record.Reused {
			rec += " reused"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %.1f | %.1f | %d | %d | %d | %d | %d | %s | %s | %s | %d | **%d** | %d |\n", r.Repo, sumInts(r.Lists),
			rec, float64(r.Record.Ms)/1000, float64(suiteMs)/1000, n, narrowed, refused, keptAll, errs, med(sv), med(hv), med(fv), fnLevel, viol, unconf)
		fmt.Fprintf(&details, "\n## %s\n\nR %s, %s. ", r.Repo, rbShort(r.R), r.PHP)
		if r.Error != "" {
			fmt.Fprintf(&details, "Error: %s. ", r.Error)
		}
		fmt.Fprintf(&details, "Baseline: %d tests in %d files, red at R: %v, runs %v ms%s. Mutants tried: %d; cached killed mutants not killed here: %d. "+
			"Largest (oracle run + selections) / whole suite: %.2f.\n\n", r.Baseline.Tests, r.Baseline.Files, r.Baseline.Red, r.Baseline.RunMs,
			map[bool]string{true: " (exit: " + r.Baseline.Exit + ")", false: ""}[r.Baseline.Exit != ""], r.Tried, notKilled, maxRatio)
		rc := r.Records
		fmt.Fprintf(&details, "Records: %d test files, %d complete and passed, %d incomplete, %d failed (passed false); incomplete reasons: %s; "+
			"file lists %d of %d complete, runners %v %v; recording %s", rc.Files, rc.Complete, rc.Incomplete, rc.Failed, phpHistMD(rc.Reasons),
			rc.ListsOK, rc.Lists, rc.Runners, rc.ListWhy, time.Duration(r.Record.Ms)*time.Millisecond)
		if s := r.Record.Summary; s != nil && len(s.Warnings) > 0 {
			fmt.Fprintf(&details, "; warnings: %v", s.Warnings)
		}
		details.WriteString("\n\n")
		fmt.Fprintf(&details, "Refusals: %s\n\nUnit notes: %s\n\nChanged-file levels: %s\n\n", phpHistMD(reasons), phpHistMD(notes), phpHistMD(levels))
		details.WriteString("| mutant | file:line | op | red | static | hybrid | file level only | level | removed | violations | skipped / error |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, m := range r.Mutants {
			if m.Oracle.Status != "killed" {
				fmt.Fprintf(&details, "| %s | %s:%d | %s | not killed here: %s | | | | | | | %s |\n", m.ID, m.File, m.Line, m.Op, m.Oracle.Status,
					strings.ReplaceAll(firstN(m.Oracle.Detail, 120), "|", "\\|"))
				continue
			}
			fmt.Fprintf(&details, "| %s | %s:%d | %s | %d | %d/%d | %d | %d | %s | %d | %s | %s |\n", m.ID, m.File, m.Line, m.Op, len(m.Oracle.Red),
				m.Static.Files, m.Total, m.Hybrid.Files, m.FileLevel, PHPCommitLevel(m.Levels), len(m.Hybrid.Removed),
				strings.Join(append(append([]string{}, m.Confirmed...), m.Unconfirmed...), " "),
				strings.ReplaceAll(firstN(m.Hybrid.Skip+m.Error, 160), "|", "\\|"))
		}
	}
	return b.String() + details.String()
}

func sumInts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// PHPGuardMarkdown renders the guard rate.
func PHPGuardMarkdown(rs []PHPGuardResult) string {
	var b, details strings.Builder
	b.WriteString("# PHP layer: guard over first-parent commits\n\n")
	b.WriteString("Every first-parent commit against its first parent, the selection with the PHP layer on and no records. Full: the static " +
		"selection is full (never narrowed). Inactive: no narrowable PHP unit selected (nothing to narrow). Accepted: the guard passed. Levels of the accepted " +
		"commits: function (every changed non-test PHP file refined to functions), mixed, file (none refined), none (only test files " +
		"and documentation changed).\n\n")
	b.WriteString("| repo | examined | inactive | full | refused | accepted | accepted / (accepted + refused + full) | accepted / examined | function | mixed | file | none |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		cnt, reasons, inactive, lv, notes := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
		for _, c := range r.Commits {
			cnt[c.Stage]++
			switch c.Stage {
			case "refused", "error", "full":
				reasons[c.Stage+": "+PHPReasonClass(c.Reason)]++
			case "inactive":
				inactive[reInactive.ReplaceAllString(c.Reason, "N")]++
			case "accepted":
				lv[c.Level]++
				for _, l := range c.Levels {
					if l.Level == "file" {
						notes[PHPReasonClass(l.Note)]++
					}
				}
			}
		}
		guarded := cnt["accepted"] + cnt["refused"] + cnt["full"]
		share, all := "-", "-"
		if guarded > 0 {
			share = fmt.Sprintf("%.2f", float64(cnt["accepted"])/float64(guarded))
		}
		if len(r.Commits) > 0 {
			all = fmt.Sprintf("%.2f", float64(cnt["accepted"])/float64(len(r.Commits)))
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %s | %s | %d | %d | %d | %d |\n", r.Repo, r.Examined, cnt["inactive"], cnt["full"], cnt["refused"], cnt["accepted"],
			share, all, lv["function"], lv["mixed"], lv["file"], lv["none"])
		fmt.Fprintf(&details, "\n## %s\n\nHead %s, %s. Errors: %d.\n\nRefusals:\n\n", r.Repo, rbShort(r.Head), time.Duration(r.Ms)*time.Millisecond, cnt["error"])
		writeHist(&details, reasons)
		details.WriteString("\nInactive:\n\n")
		writeHist(&details, inactive)
		details.WriteString("\nWhy changed source files of accepted commits stayed at file level:\n\n")
		writeHist(&details, notes)
	}
	return b.String() + details.String()
}

var reInactive = regexp.MustCompile(`[0-9]+`)

func writeHist(b *strings.Builder, m map[string]int) {
	type kv struct {
		k string
		n int
	}
	var s []kv
	for k, n := range m {
		s = append(s, kv{k, n})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].n > s[j].n || s[i].n == s[j].n && s[i].k < s[j].k })
	if len(s) == 0 {
		b.WriteString("- none\n")
	}
	for _, x := range s {
		fmt.Fprintf(b, "- %d × %s\n", x.n, x.k)
	}
}

// PHPReplayMarkdown renders a replay.
func PHPReplayMarkdown(r PHPReplayResult) string {
	var b strings.Builder
	cnt := map[string]int{}
	narrowed, removed, misses, asserts, errs, fnLevel := 0, 0, 0, 0, 0, 0
	var sv, hv []float64
	units, complete, incomplete, failed, lists, listsOK := 0, 0, 0, 0, 0, 0
	reasons := map[string]int{}
	var recMs []int64
	for _, c := range r.Commits {
		cnt[c.Stage]++
		if c.Error != "" {
			errs++
		}
		if c.AssertFail != "" {
			asserts++
		}
		misses += len(c.Misses)
		if c.Stage != "accepted" || c.Error != "" {
			continue
		}
		if c.Hybrid.Narrowed {
			narrowed++
		}
		if c.Level == "function" {
			fnLevel++
		}
		removed += len(c.Hybrid.Removed)
		if x, ok := phpRatio(c.Static.Files, c.Total); ok {
			sv = append(sv, x)
		}
		if x, ok := phpRatio(c.Hybrid.Files, c.Total); ok {
			hv = append(hv, x)
		}
		if s := c.Records; s != nil {
			units += s.Files
			complete += s.Complete
			incomplete += s.Incomplete
			failed += s.Failed
			for k, n := range s.Reasons {
				reasons[k] += n
			}
			lists += s.Lists
			listsOK += s.ListsOK
		}
		if c.Record != nil {
			recMs = append(recMs, c.Record.Ms)
		}
	}
	fmt.Fprintf(&b, "## %s\n\nRev %s, %s, %s. %d commits: %d inactive, %d full, %d refused by the guard, %d accepted (%d errors). Accepted: %d narrowed "+
		"(%d at function level), %d removed test files run at their commit, **misses %d**, hybrid != static where nothing was narrowed: "+
		"**%d**; median files run / files over accepted commits: static %s, hybrid %s.\n\n", r.Repo, rbShort(r.Rev), r.PHP,
		time.Duration(r.Ms)*time.Millisecond, len(r.Commits), cnt["inactive"], cnt["full"], cnt["refused"], cnt["accepted"], errs, narrowed, fnLevel, removed,
		misses, asserts, med(sv), med(hv))
	if units > 0 {
		fmt.Fprintf(&b, "Records at the parents: %d test files, %d complete and passed (%.1f %%), %d incomplete, %d failed (passed false); "+
			"file lists %d of %d complete; median recording %s; incomplete reasons: %s\n\n", units, complete, 100*float64(complete)/float64(units),
			incomplete, failed, listsOK, lists, time.Duration(medianInt64(recMs))*time.Millisecond, phpHistMD(reasons))
	}
	if p := r.Plain; p != nil {
		fmt.Fprintf(&b, "Timing at %s (%d jobs): recording %s; the %d listed test files alone without the collector %s; whole suite in one "+
			"process %s%s.\n\n", rbShort(p.At), p.Jobs, time.Duration(p.RecordMs)*time.Millisecond, p.Files, time.Duration(p.PlainMs)*time.Millisecond,
			time.Duration(p.WholeMs)*time.Millisecond, map[bool]string{true: " (" + p.WholeErr + ")", false: ""}[p.WholeErr != ""])
	}
	b.WriteString("| C | files | level | static / files | hybrid | removed | red at C | red at P too | misses | record (complete/files, s) | composer | skipped / error |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range r.Commits {
		if c.Stage != "accepted" && c.AssertFail == "" && c.Error == "" {
			continue
		}
		rec := "-"
		if c.Records != nil && c.Record != nil {
			rec = fmt.Sprintf("%d/%d, %.0f", c.Records.Complete, c.Records.Files, float64(c.Record.Ms)/1000)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %d/%d | %d | %d | %d | %d | %d | %s | %s | %s |\n", rbShort(c.C), len(c.Files), c.Level, c.Static.Files, c.Total,
			c.Hybrid.Files, len(c.Hybrid.Removed), len(c.FailAtC), len(c.FailAtP), len(c.Misses), rec, c.Composer,
			strings.ReplaceAll(firstN(c.Hybrid.Skip+c.AssertFail+" "+c.Error, 160), "|", "\\|"))
	}
	reasonsR := map[string]int{}
	for _, c := range r.Commits {
		if c.Stage == "refused" || c.Stage == "full" {
			reasonsR[c.Stage+": "+PHPReasonClass(c.Skip)]++
		}
	}
	b.WriteString("\nRefusals:\n\n")
	writeHist(&b, reasonsR)
	for _, c := range r.Commits {
		for _, d := range c.MissDetail {
			fmt.Fprintf(&b, "\nMISS at %s: %s\n", c.C, d)
		}
	}
	return b.String()
}
