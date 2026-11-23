package evalkit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/jvmdyn"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
)

// Phase 7 (docs/dynamic-layer.md §9.8): the JVM class layer checked against the Phase 5 JVM T2 oracle. The oracle (unit verdicts of
// the whole suite on R+mutant, oracle.jsonl of the T2 set, never the collector) gives the killed mutants and their red units;
// for every killed mutant whose hybrid selection removes classes from a red unit, the red units are run again whole on the
// mutated tree in a shared clone and their class verdicts are read from the JUnit reports. A violation is a red class the
// hybrid selection does not run. Records are made beforehand with `bgraph record --eco jvm` in the clone at R.

// JVMDynConfig configures RunJVMDynT2 and RunJVMDynReplay.
type JVMDynConfig struct {
	T2Dir     string // the JVM T2 set (oracle.jsonl); read only
	Clone     string // shared clone of the repo used for runs (its working tree is modified and restored)
	StateDir  string // bgraph state directory holding dynamic/jvm.jsonl
	Repo      T2Repo
	BuildArgs []string // extra Maven/Gradle arguments (Maven: -Dmaven.repo.local=...)
	Timeout   time.Duration
	Logf      func(string, ...any)
}

// JVMDynMutant is the outcome of one killed mutant.
type JVMDynMutant struct {
	ID            string   `json:"id"`
	File          string   `json:"file"`
	Op            string   `json:"op"`
	Red           []string `json:"red"`                     // red units of the oracle
	Skipped       string   `json:"skipped,omitempty"`       // why the class layer did not narrow (dynamic_skipped)
	ModuleClasses int      `json:"module_classes"`          // test classes of the selected units
	StaticClasses int      `json:"static_classes"`          // classes the static selection runs (every class of its units)
	HybridClasses int      `json:"hybrid_classes"`          // classes the hybrid selection runs
	AllClasses    int      `json:"all_classes"`             // classes of every recorded unit
	Removed       []string `json:"removed,omitempty"`       // "<unit>#<class>" removed by the class layer
	RemovedRed    int      `json:"removed_from_red"`        // removed classes that belong to red units
	RedClasses    []string `json:"red_classes,omitempty"`   // class oracle: red classes of the narrowed red units
	Violations    []string `json:"violations,omitempty"`    // red classes the hybrid selection does not run
	StaticMissed  []string `json:"static_missed,omitempty"` // red units the static selection misses (must be none: Phase 5)
	OracleMs      int64    `json:"oracle_ms,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// JVMDynResult is the outcome of one repo.
type JVMDynResult struct {
	Repo    string         `json:"repo"`
	R       string         `json:"r"`
	Records map[string]int `json:"records"`
	Mutants []JVMDynMutant `json:"mutants"`
}

func (c JVMDynConfig) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

func loadOracle(file string) ([]T2Record, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T2Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		var r T2Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// classLists returns the class list of every recorded JVM unit (class-list records of the store).
func classLists(stateDir string) (map[string][]string, map[string]int) {
	return ecoClassLists(stateDir, jvmdyn.Eco)
}

// ecoClassLists returns the class list of every recorded unit of a per-test-class ecosystem (class-list records of the store).
func ecoClassLists(stateDir, eco string) (map[string][]string, map[string]int) {
	recs, _ := dynamic.Load(dynamic.StorePath(stateDir, eco))
	lists := map[string][]string{}
	stat := map[string]int{}
	for _, r := range recs {
		if !strings.Contains(r.Unit, "#") {
			lists[r.Unit] = r.Classes
			stat["class lists"]++
			continue
		}
		stat["class records"]++
		switch {
		case !r.Pass:
			stat["failed in isolation"]++
		case !r.Complete:
			stat["incomplete"]++
		default:
			stat["complete"]++
		}
	}
	return lists, stat
}

// classCounts measures a selection in test classes: every class of the selected JVM units (static), what runs (hybrid).
func classCounts(sel model.Selection, lists map[string][]string) (module, run int) {
	for _, t := range sel.Tests {
		n := len(lists[t.Unit])
		module += n
		if t.ClassFilter {
			run += len(t.Classes)
		} else {
			run += n
		}
	}
	return module, run
}

func skipReason(sel model.Selection) string { return ecoSkipReason(sel, jvmdyn.Eco) }

func ecoSkipReason(sel model.Selection, eco string) string {
	for _, w := range sel.Warnings {
		if w.Reason == model.ReasonDynamicSkipped && strings.HasPrefix(w.Detail, eco+": ") {
			return strings.TrimPrefix(w.Detail, eco+": ")
		}
	}
	return ""
}

// jvmUnitOf returns the jvmdyn.Unit of a unit ID in the clone (directory from the ID or the selection).
func jvmUnitOf(id, project, runner string) jvmdyn.Unit {
	dir := strings.Trim(project, "/")
	if dir == "" {
		if _, d, _, ok := jvm.ParseUnitID(id); ok && runner == jvm.RunnerMaven {
			dir = d
		} else {
			dir = "."
		}
	}
	return jvmdyn.Unit{ID: id, Dir: dir, Runner: runner}
}

// runClasses runs the tests of units in the clone (only the listed classes when classes is non-empty) and returns the class
// verdicts per unit read from the JUnit reports written by this run.
func (c JVMDynConfig) runClasses(ctx context.Context, units []jvmdyn.Unit, classes []string) (map[string]jvmdyn.Reports, string, error) {
	start := time.Now()
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	var out, tl string
	var to bool
	var err error
	var env []string
	if c.Repo.Runner == "maven" {
		var pl []string
		for _, u := range units {
			pl = append(pl, u.Dir)
		}
		args := []string{"-B", "-ntp", "-Dmaven.test.failure.ignore=true", "-Dsurefire.failIfNoSpecifiedTests=false", "-DfailIfNoTests=false"}
		args = append(args, c.BuildArgs...)
		if len(classes) > 0 {
			var pats []string
			for _, cl := range classes {
				pats = append(pats, cl, cl+"$*")
			}
			// In the environment: mvn.cmd runs through cmd.exe (8,191 characters); Maven >= 3.9 reads MAVEN_ARGS.
			env = []string{"MAVEN_ARGS=-Dtest=" + strings.Join(pats, ",")}
		}
		args = append(args, "-pl", strings.Join(pl, ","), "-am", "test")
		out, tl, to, err = runProc(ctx, c.Clone, env, timeout, mvnExe(jvmT2Tools.Maven), args...)
	} else {
		args := []string{"--console=plain", "--continue", "-q"}
		args = append(args, c.BuildArgs...)
		var init string
		if len(classes) > 0 {
			f, ferr := os.CreateTemp("", "bgraph-eval-filter-*.gradle")
			if ferr != nil {
				return nil, "", ferr
			}
			var qs []string
			for _, cl := range classes {
				qs = append(qs, "'"+cl+"'", "'"+cl+"$*'")
			}
			fmt.Fprintf(f, "allprojects { tasks.withType(Test).configureEach { filter.failOnNoMatchingTests = false; [%s].each { filter.includeTestsMatching(it) } } }\n", strings.Join(qs, ", "))
			f.Close()
			init = f.Name()
			defer os.Remove(init)
			args = append(args, "-I", init)
		}
		for _, u := range units {
			_, _, pp, _ := jvm.ParseUnitID(u.ID)
			task := strings.TrimSuffix(pp, ":")
			args = append(args, task+":cleanTest", task+":test")
		}
		out, tl, to, err = runProc(ctx, c.Clone, nil, timeout, filepath.Join(c.Clone, "gradlew.bat"), args...)
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if to {
		return nil, "", fmt.Errorf("timed out")
	}
	res := map[string]jvmdyn.Reports{}
	for _, u := range units {
		rep, rerr := jvmdyn.ReadReports(c.Clone, u, start)
		if rerr != nil {
			return nil, "", rerr
		}
		res[u.ID] = rep
	}
	detail := ""
	if err != nil {
		detail = firstN(tail(out+"\n"+tl, 600), 600)
	}
	return res, detail, nil
}

// RunJVMDynT2 evaluates the class layer on the killed mutants of one repo.
func RunJVMDynT2(ctx context.Context, cfg JVMDynConfig, onlyIDs map[string]bool) (JVMDynResult, error) {
	res := JVMDynResult{Repo: cfg.Repo.Name, R: cfg.Repo.R}
	oracle, err := loadOracle(filepath.Join(cfg.T2Dir, "oracle.jsonl"))
	if err != nil {
		return res, err
	}
	lists, stat := classLists(cfg.StateDir)
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
	var baseline map[string]jvmdyn.Reports // class verdicts at R of the units run so far
	for _, o := range oracle {
		if o.Repo != cfg.Repo.Name || o.R != cfg.Repo.R || o.Status != "killed" || onlyIDs != nil && !onlyIDs[o.Mutant.ID] {
			continue
		}
		m := JVMDynMutant{ID: o.Mutant.ID, File: o.Mutant.File, Op: o.Mutant.Op, Red: o.Red, AllClasses: all}
		st, err := ix.Select(ctx, bgraph.Change{Base: o.R, Head: o.Commit, Dynamic: bgraph.DynamicOff})
		if err != nil {
			m.Error = err.Error()
			res.Mutants = append(res.Mutants, m)
			continue
		}
		hy, err := ix.Select(ctx, bgraph.Change{Base: o.R, Head: o.Commit, Dynamic: bgraph.DynamicOn})
		if err != nil {
			m.Error = err.Error()
			res.Mutants = append(res.Mutants, m)
			continue
		}
		m.Skipped = skipReason(hy)
		m.ModuleClasses, m.StaticClasses = classCounts(st, lists)
		_, m.HybridClasses = classCounts(hy, lists)
		sel := map[string]model.SelectedTest{}
		for _, t := range st.Tests {
			sel[t.Unit] = t
		}
		for _, u := range o.Red {
			if _, ok := sel[u]; !ok {
				m.StaticMissed = append(m.StaticMissed, u)
			}
		}
		red := map[string]bool{}
		for _, u := range o.Red {
			red[u] = true
		}
		var narrowedRed []jvmdyn.Unit
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
				narrowedRed = append(narrowedRed, jvmUnitOf(t.Unit, t.Project, t.Runner))
			}
		}
		for _, n := range hy.Narrowed {
			if !strings.Contains(n.Unit, "#") {
				continue
			}
			m.Removed = append(m.Removed, n.Unit)
			if u, _, _ := strings.Cut(n.Unit, "#"); red[u] {
				m.RemovedRed++
			}
		}
		cfg.logf("%s %s: static %d/%d classes, hybrid %d (removed %d, %d from red units)%s", cfg.Repo.Name, m.ID, m.StaticClasses,
			m.ModuleClasses, m.HybridClasses, len(m.Removed), m.RemovedRed, map[bool]string{true: "; skipped: " + m.Skipped, false: ""}[m.Skipped != ""])
		if m.RemovedRed > 0 {
			t0 := time.Now()
			if baseline == nil {
				baseline = map[string]jvmdyn.Reports{}
			}
			var need []jvmdyn.Unit
			for _, u := range narrowedRed {
				if _, ok := baseline[u.ID]; !ok {
					need = append(need, u)
				}
			}
			if len(need) > 0 {
				b, detail, err := cfg.runClasses(ctx, need, nil)
				if err != nil {
					return res, fmt.Errorf("baseline: %v", err)
				}
				for id, r := range b {
					baseline[id] = r
					if len(r) == 0 {
						return res, fmt.Errorf("baseline of %s: no reports: %s", id, detail)
					}
				}
			}
			reps, detail, err := cfg.withMutant(ctx, o.Mutant, func() (map[string]jvmdyn.Reports, string, error) {
				return cfg.runClasses(ctx, narrowedRed, nil)
			})
			m.OracleMs = time.Since(t0).Milliseconds()
			if err != nil {
				m.Error = err.Error()
			}
			for _, u := range narrowedRed {
				rep := reps[u.ID]
				if len(rep) == 0 && m.Error == "" {
					m.Error = "no class verdicts for " + u.ID + ": " + detail
				}
				for c, pass := range rep {
					if pass || !baseline[u.ID][c] {
						continue // passed, or already red/absent at R
					}
					m.RedClasses = append(m.RedClasses, u.ID+"#"+c)
					if !kept[u.ID][c] {
						m.Violations = append(m.Violations, u.ID+"#"+c)
					}
				}
			}
			sort.Strings(m.RedClasses)
			sort.Strings(m.Violations)
			cfg.logf("%s %s: class oracle: %d red classes, %d violations %v (%s)", cfg.Repo.Name, m.ID, len(m.RedClasses), len(m.Violations),
				m.Violations, time.Duration(m.OracleMs)*time.Millisecond)
		}
		res.Mutants = append(res.Mutants, m)
	}
	return res, nil
}

// withMutant writes the mutated file into the clone, runs f and restores the file.
func (c JVMDynConfig) withMutant(ctx context.Context, mu T2Mutant, f func() (map[string]jvmdyn.Reports, string, error)) (map[string]jvmdyn.Reports, string, error) {
	p := filepath.Join(c.Clone, filepath.FromSlash(mu.File))
	orig, err := os.ReadFile(p)
	if err != nil {
		return nil, "", err
	}
	mut, err := ApplyEdit(orig, mu)
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(p, mut, 0o644); err != nil {
		return nil, "", err
	}
	defer os.WriteFile(p, orig, 0o644)
	return f()
}

// JVMDynMarkdown renders the T2 results.
func JVMDynMarkdown(rs []JVMDynResult) string {
	var b strings.Builder
	b.WriteString("| repo | killed | narrowed (classes removed) | narrowed in a red unit | refused by a guard | violations | median classes static/module | median hybrid/module | mean hybrid/static |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		n, nr, refused, viol := 0, 0, 0, 0
		var st, hy, ratio []float64
		for _, m := range r.Mutants {
			if len(m.Removed) > 0 {
				n++
			}
			if m.RemovedRed > 0 {
				nr++
			}
			if m.Skipped != "" {
				refused++
			}
			viol += len(m.Violations)
			if m.ModuleClasses > 0 {
				st = append(st, float64(m.StaticClasses)/float64(m.ModuleClasses))
				hy = append(hy, float64(m.HybridClasses)/float64(m.ModuleClasses))
			}
			if m.StaticClasses > 0 {
				ratio = append(ratio, float64(m.HybridClasses)/float64(m.StaticClasses))
			}
		}
		mean := 0.0
		for _, x := range ratio {
			mean += x
		}
		if len(ratio) > 0 {
			mean /= float64(len(ratio))
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %s | %s | %.3f |\n", r.Repo, len(r.Mutants), n, nr, refused, viol, med(st), med(hy), mean)
	}
	b.WriteString("\n| repo | mutant | file | red units | static classes | hybrid classes | removed | red classes | violations | guard |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		for _, m := range r.Mutants {
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d/%d | %d | %d | %d | %d | %s |\n", r.Repo, m.ID, m.File, len(m.Red), m.StaticClasses, m.ModuleClasses,
				m.HybridClasses, len(m.Removed), len(m.RedClasses), len(m.Violations), strings.ReplaceAll(firstN(m.Skipped, 120), "|", "\\|"))
		}
	}
	return b.String()
}
