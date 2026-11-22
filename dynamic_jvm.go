package bgraph

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/jvmdyn"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/selector"
	"github.com/qxbao/bgraph/internal/tree"
)

// jvmEnv computes the JVM environment fingerprint for tree t (docs/dynamic-layer.md §9.3). env overrides the process environment
// (JAVA_HOME, MAVEN_HOME, GRADLE_HOME).
func (ix *Index) jvmEnv(rf *rules.File, t tree.Tree, env []string) (fp, collector, javaHome string, err error) {
	_, _, trust := ix.dynamicConfig(rf)
	home, err := jvmdyn.JavaHome(env)
	if err != nil {
		return "", "", "", err
	}
	collector = jvmdyn.Collector(trust)
	return jvmdyn.Fingerprint(collector, home, env, t), collector, home, nil
}

// applyJVMClasses runs the JVM class layer (docs/dynamic-layer.md §9) on a selection: selected Maven modules / Gradle projects run
// only the test classes whose records reach a changed file. Anything that prevents it leaves the selection unchanged with a
// dynamic_skipped warning.
func (ix *Index) applyJVMClasses(ctx context.Context, in selector.Input, res selector.Result, sel Selection) Selection {
	candidate := slices.ContainsFunc(sel.Tests, func(t model.SelectedTest) bool {
		return res.UnitEco[t.Unit] == jvmdyn.Eco && !t.Whole && (t.Runner == jvm.RunnerMaven || t.Runner == jvm.RunnerGradle)
	})
	if !candidate {
		return sel
	}
	changes := make([]dynamic.Change, 0, len(res.Changes))
	for _, c := range res.Changes {
		roles := c.Roles[jvmdyn.Eco]
		other := false
		for e, r := range c.Roles {
			other = other || e != jvmdyn.Eco && (r[0] != "" || r[1] != "")
		}
		changes = append(changes, dynamic.Change{Path: c.Path, Status: c.Status, OldMode: c.OldMode, NewMode: c.NewMode,
			BaseRole: roles[0], HeadRole: roles[1], Global: c.Global, Claimed: len(c.Claimed) > 0,
			Declared: dynamic.Declared(in.Rules, c.Path), Rule: c.Rule, Referenced: c.Dependents, OtherRole: other, Linked: c.Linked})
	}
	if why := jvmdyn.Guard(sel, changes, in.BaseTree, in.HeadTree); why != "" {
		return dynamic.Skip(sel, jvmdyn.Eco, why)
	}
	if ix.stateDir == "" {
		return dynamic.Skip(sel, jvmdyn.Eco, "no state directory: no records")
	}
	recs, warns := dynamic.Load(dynamic.StorePath(ix.stateDir, jvmdyn.Eco))
	for _, w := range warns {
		sel.Warnings = append(sel.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonDynamicSkipped, Detail: w})
	}
	if len(recs) == 0 {
		return dynamic.Skip(sel, jvmdyn.Eco, "no records (run `bgraph record --eco jvm`)")
	}
	env, collector, _, err := ix.jvmEnv(in.Rules, in.BaseTree, nil)
	if err != nil {
		return dynamic.Skip(sel, jvmdyn.Eco, "environment fingerprint: "+err.Error())
	}
	return jvmdyn.Narrow(sel, jvmdyn.Params{Collector: collector, Env: env, Records: recs, Base: in.BaseTree, Head: in.HeadTree,
		UnitEco: res.UnitEco})
}

// recordJVM records the JVM units (docs/dynamic-layer.md §9.4): one build invocation per Maven reactor / Gradle build, every test
// class in a fresh JVM with the collector agent.
func (ix *Index) recordJVM(ctx context.Context, opt RecordOptions, rf *rules.File, an *analyzed, wt tree.Tree, head string,
	now time.Time, keep func(string) bool, sum *RecordSummary) ([]dynamic.Record, []string, error) {
	var units []jvmdyn.Unit
	var skipped []string
	all := map[string]int{}
	for _, a := range an.Analyses {
		for _, u := range a.Tests {
			if u.Eco != jvm.Name {
				continue
			}
			bf, _ := jvm.ParseLaunchConfig(u.Config)
			bdir := "."
			if bf != "" {
				bdir = pathDir(bf)
			}
			all[u.Runner+"|"+bdir]++
			if u.Whole {
				skipped = append(skipped, u.ID+": whole-project unit")
				continue
			}
			if !keep(u.ID) {
				continue
			}
			dir := u.Project
			if dir == "" {
				dir = "."
			}
			units = append(units, jvmdyn.Unit{ID: u.ID, Dir: dir, Runner: u.Runner, Config: u.Config, Package: u.Package})
		}
	}
	if len(units) == 0 {
		return nil, skipped, nil
	}
	_, _, trust := ix.dynamicConfig(rf)
	env, collector, home, err := ix.jvmEnv(rf, wt, opt.Env)
	if err != nil {
		return nil, skipped, fmt.Errorf("jvm: %w", err)
	}
	tmp := ix.tmpDir()
	if tmp == "" {
		return nil, skipped, fmt.Errorf("jvm: no state directory for the collector")
	}
	agent, err := jvmdyn.BuildAgent(ctx, home, tmp)
	if err != nil {
		return nil, skipped, err
	}
	sum.Units += len(units)
	sum.Env = env
	jobs := opt.Jobs
	if jobs <= 0 {
		jobs = jvmdyn.DefaultJobs()
	}
	m := &jvmdyn.Mapper{Root: ix.repo.Root, Tree: wt, Index: jvmdyn.NewSourceIndex(wt), Trust: trust, SystemDirs: jvmdyn.DefaultSystemDirs()}
	var recs []dynamic.Record
	for _, b := range jvmdyn.Builds(units, all) {
		ropt := jvmdyn.RunOptions{Root: ix.repo.Root, TmpDir: tmp, JavaHome: home, Agent: agent, Jobs: jobs, Timeout: opt.Timeout,
			Env: opt.Env, BuildArgs: opt.BuildArgs}
		o, err := jvmdyn.Run(ctx, ropt, b)
		if o.LogDir != "" && !opt.KeepLogs {
			defer removeAll(o.LogDir)
		}
		if err != nil {
			return nil, skipped, err
		}
		forks, err := jvmdyn.LoadForks(ctx, o.LogDir, home, agent, opt.Env)
		if err != nil {
			return nil, skipped, err
		}
		rs := jvmdyn.Records(m, o.Units, forks, env, collector, head, now)
		if opt.Progress != nil {
			for i, r := range rs {
				opt.Progress(i+1, len(rs), r, o.Elapsed, o.Tail)
			}
		}
		if o.Exit != 0 || o.TimedOut {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s build in %s: exit %d (timed out %v): %s", b.Kind, b.Dir, o.Exit, o.TimedOut,
				lastLines(o.Tail, 8)))
		}
		recs = append(recs, rs...)
	}
	return recs, skipped, nil
}

func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

func removeAll(dir string) { os.RemoveAll(dir) }
