package evalkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic/dotnetdyn"
	"github.com/qxbao/bgraph/internal/model"
)

// Phase 8 (docs/dynamic-layer.md §9.8): the .NET class layer checked against the Phase 5 .NET T2 oracle, like RunJVMDynT2. For every
// killed mutant whose hybrid selection removes classes from a red unit, the red test projects are run again whole on the mutated
// tree (`dotnet test <project> <args> --logger trx`, the oracle's own arguments) and their class verdicts read from the TRX
// reports. A violation is a red class the hybrid selection does not run. Records are made beforehand with
// `bgraph record --eco dotnet` in the clone at R.

// DotnetDynConfig configures RunDotnetDynT2.
type DotnetDynConfig struct {
	T2Dir    string // the .NET T2 set (oracle.jsonl); read only
	Clone    string // clone of the repo at R used for runs (its working tree is modified and restored)
	StateDir string // bgraph state directory holding dynamic/dotnet.jsonl
	Repo     T2Repo
	Timeout  time.Duration
	Logf     func(string, ...any)
}

func (c DotnetDynConfig) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

// runProjects runs whole test projects in the clone and returns their class verdicts (class → passed) read from TRX.
func (c DotnetDynConfig) runProjects(ctx context.Context, projects []string) (map[string]map[string]bool, string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	dir, err := os.MkdirTemp("", "bgraph-eval-trx-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	res := map[string]map[string]bool{}
	var detail string
	for i, p := range projects {
		name := fmt.Sprintf("p%d.trx", i)
		args := append([]string{"test", p}, c.Repo.Args...)
		args = append(args, "--logger", "trx;LogFileName="+name, "--results-directory", dir)
		out, tl, to, rerr := runProc(ctx, c.Clone, nil, timeout, "dotnet", args...)
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		if to {
			return nil, "", fmt.Errorf("%s: timed out", p)
		}
		trx, perr := dotnetdyn.ParseTRXFile(filepath.Join(dir, name))
		if perr != nil {
			detail = firstN(tail(out+"\n"+tl, 600), 600)
			if rerr != nil {
				detail = rerr.Error() + ": " + detail
			}
			res["dotnet:"+p] = nil
			continue
		}
		v := map[string]bool{}
		for cl, o := range trx {
			v[cl] = o.Failed == 0
		}
		res["dotnet:"+p] = v
	}
	return res, detail, nil
}

// RunDotnetDynT2 evaluates the .NET class layer on the killed mutants of one repo.
func RunDotnetDynT2(ctx context.Context, cfg DotnetDynConfig, onlyIDs map[string]bool) (JVMDynResult, error) {
	res := JVMDynResult{Repo: cfg.Repo.Name, R: cfg.Repo.R}
	oracle, err := loadOracle(filepath.Join(cfg.T2Dir, "oracle.jsonl"))
	if err != nil {
		return res, err
	}
	lists, stat := ecoClassLists(cfg.StateDir, dotnetdyn.Eco)
	res.Records = stat
	all := 0
	for _, l := range lists {
		all += len(l)
	}
	ix, err := bgraph.Open(ctx, cfg.Clone, bgraph.Options{StateDir: cfg.StateDir})
	if err != nil {
		return res, err
	}
	defer ix.Close()
	baseline := map[string]map[string]bool{} // class verdicts at R of the projects run so far
	for _, o := range oracle {
		if o.Repo != cfg.Repo.Name || o.R != cfg.Repo.R || o.Status != "killed" || onlyIDs != nil && !onlyIDs[o.Mutant.ID] {
			continue
		}
		m := JVMDynMutant{ID: o.Mutant.ID, File: o.Mutant.File, Op: o.Mutant.Op, Red: o.Red, AllClasses: all}
		st, err := ix.Select(ctx, bgraph.Change{Base: o.R, Head: o.Commit, Dynamic: bgraph.DynamicOff})
		if err == nil {
			var hy model.Selection
			hy, err = ix.Select(ctx, bgraph.Change{Base: o.R, Head: o.Commit, Dynamic: bgraph.DynamicOn})
			if err == nil {
				err = cfg.check(ctx, o, &m, st, hy, lists, baseline)
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			m.Error = err.Error()
		}
		res.Mutants = append(res.Mutants, m)
	}
	return res, nil
}

func (cfg DotnetDynConfig) check(ctx context.Context, o T2Record, m *JVMDynMutant, st, hy model.Selection, lists map[string][]string,
	baseline map[string]map[string]bool) error {
	m.Skipped = ecoSkipReason(hy, dotnetdyn.Eco)
	m.ModuleClasses, m.StaticClasses = classCounts(st, lists)
	_, m.HybridClasses = classCounts(hy, lists)
	sel := map[string]bool{}
	for _, t := range st.Tests {
		sel[t.Unit] = true
	}
	red := map[string]bool{}
	for _, u := range o.Red {
		red[u] = true
		if !sel[u] {
			m.StaticMissed = append(m.StaticMissed, u)
		}
	}
	var narrowedRed []string
	kept := map[string]map[string]bool{}
	for _, t := range hy.Tests {
		if !t.ClassFilter {
			continue
		}
		k := map[string]bool{}
		for _, c := range t.Classes {
			k[c] = true
		}
		kept[t.Unit] = k
		if red[t.Unit] {
			narrowedRed = append(narrowedRed, t.Package)
		}
	}
	for _, n := range hy.Narrowed {
		if u, _, ok := strings.Cut(n.Unit, "#"); ok {
			m.Removed = append(m.Removed, n.Unit)
			if red[u] {
				m.RemovedRed++
			}
		}
	}
	cfg.logf("%s %s: static %d/%d classes, hybrid %d (removed %d, %d from red units)%s", cfg.Repo.Name, m.ID, m.StaticClasses,
		m.ModuleClasses, m.HybridClasses, len(m.Removed), m.RemovedRed, map[bool]string{true: "; skipped: " + m.Skipped, false: ""}[m.Skipped != ""])
	if m.RemovedRed == 0 {
		return nil
	}
	t0 := time.Now()
	var need []string
	for _, p := range narrowedRed {
		if _, ok := baseline["dotnet:"+p]; !ok {
			need = append(need, p)
		}
	}
	if len(need) > 0 {
		b, detail, err := cfg.runProjects(ctx, need)
		if err != nil {
			return fmt.Errorf("baseline: %v", err)
		}
		for id, r := range b {
			if len(r) == 0 {
				return fmt.Errorf("baseline of %s: no TRX: %s", id, detail)
			}
			baseline[id] = r
		}
	}
	p := filepath.Join(cfg.Clone, filepath.FromSlash(o.Mutant.File))
	orig, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	mut, err := ApplyEdit(orig, o.Mutant)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, mut, 0o644); err != nil {
		return err
	}
	reps, detail, err := cfg.runProjects(ctx, narrowedRed)
	if werr := os.WriteFile(p, orig, 0o644); werr != nil && err == nil {
		err = werr
	}
	m.OracleMs = time.Since(t0).Milliseconds()
	if err != nil {
		return err
	}
	for _, proj := range narrowedRed {
		u := "dotnet:" + proj
		rep := reps[u]
		if len(rep) == 0 {
			m.Error = "no class verdicts for " + u + ": " + detail
		}
		for c, pass := range rep {
			if pass || !baseline[u][c] {
				continue // passed, or already red/absent at R
			}
			m.RedClasses = append(m.RedClasses, u+"#"+c)
			if !kept[u][c] {
				m.Violations = append(m.Violations, u+"#"+c)
			}
		}
	}
	sort.Strings(m.RedClasses)
	sort.Strings(m.Violations)
	cfg.logf("%s %s: class oracle: %d red classes, %d violations %v (%s)", cfg.Repo.Name, m.ID, len(m.RedClasses), len(m.Violations),
		m.Violations, time.Duration(m.OracleMs)*time.Millisecond)
	return nil
}
