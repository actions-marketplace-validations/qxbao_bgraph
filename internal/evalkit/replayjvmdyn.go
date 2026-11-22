package evalkit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic/jvmdyn"
	"github.com/qxbao/bgraph/internal/eco/jvm"
)

// JVM class-layer replay (docs/dynamic-layer.md §9.8): first-parent commits C whose diff from its parent P only modifies Java/Kotlin
// files of compiled source roots; records made fresh at P (`bgraph record --eco jvm`), the selection P→C with the class layer,
// and every removed class run at C (and at P when it fails at C) to verify that removing it was right.

// JVMReplayCommit is the outcome of one replayed commit.
type JVMReplayCommit struct {
	C             string   `json:"c"`
	P             string   `json:"p"`
	Files         []string `json:"files"`
	Skipped       string   `json:"skipped,omitempty"`
	ModuleClasses int      `json:"module_classes"`
	HybridClasses int      `json:"hybrid_classes"`
	Removed       []string `json:"removed,omitempty"`
	FailAtC       []string `json:"fail_at_c,omitempty"` // removed classes red at C
	FailAtP       []string `json:"fail_at_p,omitempty"` // of those, red at P too (pre-broken: not a miss)
	Misses        []string `json:"misses,omitempty"`    // red at C, green at P: the class layer removed a class the commit broke
	RecordMs      int64    `json:"record_ms"`
	VerifyMs      int64    `json:"verify_ms"`
	Error         string   `json:"error,omitempty"`
}

// JVMReplayConfig configures RunJVMDynReplay.
type JVMReplayConfig struct {
	JVMDynConfig
	Units   string // --units glob of the recordings
	Commits int    // commits examined (first parent, newest first, ending at Repo.R)
	Max     int    // commits replayed at most (guard-passing candidates)
}

// RunJVMDynReplay replays up to cfg.Max candidate commits.
func RunJVMDynReplay(ctx context.Context, cfg JVMReplayConfig) ([]JVMReplayCommit, error) {
	if err := localClone(ctx, cfg.Clone, cfg.Clone, cfg.Repo.R); err != nil {
		return nil, err
	}
	log, err := git(ctx, cfg.Clone, "rev-list", "--first-parent", fmt.Sprintf("-n%d", cfg.Commits), cfg.Repo.R)
	if err != nil {
		return nil, err
	}
	var out []JVMReplayCommit
	for _, c := range strings.Fields(log) {
		if len(out) >= cfg.Max {
			break
		}
		raw, err := git(ctx, cfg.Clone, "diff", "--raw", "--no-renames", c+"^", c)
		if err != nil {
			continue // root commit
		}
		var files []string
		ok := true
		for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
			meta, p, found := strings.Cut(l, "\t")
			fs := strings.Fields(strings.TrimPrefix(meta, ":"))
			if !found || len(fs) < 5 || fs[4] != "M" || fs[0] != fs[1] || !jvm.DynamicSource(p) {
				ok = false
				break
			}
			files = append(files, p)
		}
		if !ok || len(files) == 0 {
			continue
		}
		p, _ := git(ctx, cfg.Clone, "rev-parse", c+"^")
		rc := JVMReplayCommit{C: c, P: strings.TrimSpace(p), Files: files}
		cfg.replayOne(ctx, &rc)
		cfg.logf("replay %s: %d files, %d/%d classes run, %d removed, misses %v %s%s", c[:10], len(files), rc.HybridClasses, rc.ModuleClasses,
			len(rc.Removed), rc.Misses, rc.Error, map[bool]string{true: " skipped: " + rc.Skipped, false: ""}[rc.Skipped != ""])
		out = append(out, rc)
	}
	return out, nil
}

func (cfg JVMReplayConfig) replayOne(ctx context.Context, rc *JVMReplayCommit) {
	if err := localClone(ctx, cfg.Clone, cfg.Clone, rc.P); err != nil {
		rc.Error = err.Error()
		return
	}
	ix, err := bgraph.Open(ctx, cfg.Clone, bgraph.Options{StateDir: cfg.StateDir})
	if err != nil {
		rc.Error = err.Error()
		return
	}
	defer ix.Close()
	// The guard first: a refused commit needs no records.
	pre, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: rc.C, Dynamic: bgraph.DynamicOn})
	if err != nil {
		rc.Error = err.Error()
		return
	}
	if s := skipReason(pre); s != "" && !strings.Contains(s, "no records") {
		rc.Skipped = s
		return
	}
	t0 := time.Now()
	if _, err := ix.Record(ctx, bgraph.RecordOptions{Eco: jvmdyn.Eco, Units: cfg.Units, Jobs: 4, Timeout: 40 * time.Minute, BuildArgs: cfg.BuildArgs}); err != nil {
		rc.Error = "record: " + err.Error()
		return
	}
	rc.RecordMs = time.Since(t0).Milliseconds()
	hy, err := ix.Select(ctx, bgraph.Change{Base: rc.P, Head: rc.C, Dynamic: bgraph.DynamicOn})
	if err != nil {
		rc.Error = err.Error()
		return
	}
	rc.Skipped = skipReason(hy)
	lists, _ := classLists(cfg.StateDir)
	rc.ModuleClasses, rc.HybridClasses = classCounts(hy, lists)
	byUnit := map[string][]string{}
	for _, n := range hy.Narrowed {
		if u, c, ok := strings.Cut(n.Unit, "#"); ok {
			rc.Removed = append(rc.Removed, n.Unit)
			byUnit[u] = append(byUnit[u], c)
		}
	}
	if len(rc.Removed) == 0 {
		return
	}
	t1 := time.Now()
	defer func() { rc.VerifyMs = time.Since(t1).Milliseconds() }()
	failing := func(rev string, units map[string][]string) ([]string, error) {
		if err := localClone(ctx, cfg.Clone, cfg.Clone, rev); err != nil {
			return nil, err
		}
		var us []jvmdyn.Unit
		var classes []string
		for u, cs := range units {
			for _, t := range hy.Tests {
				if t.Unit == u {
					us = append(us, jvmUnitOf(u, t.Project, t.Runner))
				}
			}
			classes = append(classes, cs...)
		}
		reps, detail, err := cfg.runClasses(ctx, us, classes)
		if err != nil {
			return nil, err
		}
		var red []string
		for u, cs := range units {
			for _, c := range cs {
				pass, ran := reps[u][c]
				if !ran || !pass {
					red = append(red, u+"#"+c+map[bool]string{true: "", false: " (no report)"}[ran])
				}
			}
		}
		if len(red) > 0 && detail != "" {
			cfg.logf("replay %s: %s", rev[:10], detail)
		}
		sort.Strings(red)
		return red, nil
	}
	red, err := failing(rc.C, byUnit)
	if err != nil {
		rc.Error = "verify at C: " + err.Error()
		return
	}
	rc.FailAtC = red
	if len(red) == 0 {
		return
	}
	again := map[string][]string{}
	for _, r := range red {
		u, c, _ := strings.Cut(strings.TrimSuffix(r, " (no report)"), "#")
		again[u] = append(again[u], c)
	}
	atP, err := failing(rc.P, again)
	if err != nil {
		rc.Error = "verify at P: " + err.Error()
		return
	}
	rc.FailAtP = atP
	pre2 := map[string]bool{}
	for _, r := range atP {
		pre2[strings.TrimSuffix(r, " (no report)")] = true
	}
	for _, r := range red {
		if !pre2[strings.TrimSuffix(r, " (no report)")] {
			rc.Misses = append(rc.Misses, r)
		}
	}
}

// JVMReplayMarkdown renders the replay.
func JVMReplayMarkdown(repo string, cs []JVMReplayCommit) string {
	var b strings.Builder
	n, refused, narrowed, misses := 0, 0, 0, 0
	var share []float64
	for _, c := range cs {
		n++
		if c.Skipped != "" {
			refused++
			continue
		}
		if len(c.Removed) > 0 {
			narrowed++
		}
		misses += len(c.Misses)
		if c.ModuleClasses > 0 {
			share = append(share, float64(c.HybridClasses)/float64(c.ModuleClasses))
		}
	}
	fmt.Fprintf(&b, "%s: %d code-only commits replayed, %d refused by a guard, %d narrowed, median classes run/module classes %s, misses %d\n\n",
		repo, n, refused, narrowed, med(share), misses)
	b.WriteString("| C | files | classes run / module | removed | red at C | red at P too | misses | record s | guard / error |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range cs {
		fmt.Fprintf(&b, "| %s | %d | %d/%d | %d | %d | %d | %d | %d | %s |\n", c.C[:10], len(c.Files), c.HybridClasses, c.ModuleClasses, len(c.Removed),
			len(c.FailAtC), len(c.FailAtP), len(c.Misses), c.RecordMs/1000, strings.ReplaceAll(firstN(c.Skipped+c.Error, 140), "|", "\\|"))
	}
	return b.String()
}
