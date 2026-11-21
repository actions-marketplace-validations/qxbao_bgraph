package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/eco/dotnet"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// DotnetTools locates the portable .NET SDK and dotnet-affected used by the external checks. Nothing here touches PATH or the registry:
// every child process gets its own environment.
type DotnetTools struct {
	Dotnet   string // dotnet executable
	Affected string // dotnet-affected executable
	Root     string // DOTNET_ROOT
}

func (t DotnetTools) env() []string {
	return append(os.Environ(), "DOTNET_ROOT="+t.Root, "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_ROLL_FORWARD=Major",
		"DOTNET_MULTILEVEL_LOOKUP=0", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1", "MSBUILDDISABLENODEREUSE=1", "DOTNET_CLI_UI_LANGUAGE=en")
}

func (t DotnetTools) run(ctx context.Context, dir string, exe string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = t.env()
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// patchGlobalJSON lets every global.json of the worktree roll forward to the installed SDK (the pinned SDK is rarely the one we have).
// The worktree is temporary, so the change is never part of a repository; the commit the tools compare (--from) is unaffected.
func patchGlobalJSON(wt string) []string {
	var patched []string
	filepath.Walk(wt, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		if info.IsDir() || info.Name() != "global.json" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var g map[string]any
		if json.Unmarshal(stripJSONComments(b), &g) != nil {
			return nil
		}
		sdk, _ := g["sdk"].(map[string]any)
		if sdk == nil {
			return nil
		}
		// The pinned version may be newer than the installed SDK (a preview), which no roll-forward policy resolves: pin the installed band.
		sdk["version"], sdk["rollForward"], sdk["allowPrerelease"] = "10.0.100", "latestMajor", true
		g["sdk"] = sdk
		out, _ := json.MarshalIndent(g, "", " ")
		if os.WriteFile(p, out, 0o644) == nil {
			rel, _ := filepath.Rel(wt, p)
			patched = append(patched, filepath.ToSlash(rel))
		}
		return nil
	})
	return patched
}

var jsonLineComment = regexp.MustCompile(`(?m)^\s*//.*$`)

func stripJSONComments(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	return jsonLineComment.ReplaceAll(b, nil)
}

// addWorktree checks out rev in a new temporary worktree of the mirror and returns it with a cleanup function.
func addWorktree(ctx context.Context, mirror, wt, rev string) (func(), error) {
	os.RemoveAll(wt)
	git(ctx, mirror, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return func() {}, err
	}
	cleanup := func() {
		git(context.Background(), mirror, "worktree", "remove", "--force", wt)
		os.RemoveAll(wt)
		git(context.Background(), mirror, "worktree", "prune")
	}
	if _, err := git(ctx, mirror, "worktree", "add", "--detach", wt, rev); err != nil {
		cleanup()
		return func() {}, err
	}
	return cleanup, nil
}

// ---- dotnet-affected superset check ----

// DotnetAffectedRow is the outcome of the dotnet-affected comparison for one task.
type DotnetAffectedRow struct {
	ID       string   `json:"instance_id"`
	Repo     string   `json:"repo"`
	Role     string   `json:"role,omitempty"`
	Status   string   `json:"status"` // ok | violation | full | tool_failed | skipped:<why>
	Detail   string   `json:"detail,omitempty"`
	P        string   `json:"p_commit,omitempty"`
	C        string   `json:"c_commit,omitempty"`
	Scope    string   `json:"scope,omitempty"`
	Units    int      `json:"units"`
	Selected int      `json:"selected"`
	Affected int      `json:"affected"` // projects reported by dotnet-affected (changed + dependents)
	Missing  []string `json:"missing,omitempty"`
	Extra    int      `json:"extra"` // selected by bgraph, not reported by dotnet-affected
	Changed  []string `json:"changed,omitempty"`
	Patched  []string `json:"global_json_patched,omitempty"`
	Excluded []string `json:"excluded_from_discovery,omitempty"` // projects the tool could not load (a reference to a missing project file)
	ToolMs   int64    `json:"tool_ms,omitempty"`
}

// DotnetAffectedConfig configures RunDotnetAffected.
type DotnetAffectedConfig struct {
	Work        string
	Tools       DotnetTools
	MaxProjects int // skip the tool for repos with more projects than this at C (MSBuild evaluates every project)
	Timeout     time.Duration
	Log         func(string, ...any)
}

var brokenRefRe = regexp.MustCompile(`could not be loaded when referenced by ([^\r\n]+?\.\w+proj)`)

var affectedPathRe = regexp.MustCompile(`"FilePath":\s*"((?:[^"\\]|\\.)*)"`)

// DotnetAffectedTask runs bgraph and dotnet-affected on P -> C of one task and checks that bgraph's selection is a superset of the tool's
// affected set. mirror is the partial clone.
func DotnetAffectedTask(ctx context.Context, cfg DotnetAffectedConfig, mirror string, seq int, t Task, role string) DotnetAffectedRow {
	row := DotnetAffectedRow{ID: t.InstanceID, Repo: t.Repo, Role: role}
	skip := func(why string, err error) DotnetAffectedRow {
		row.Status = "skipped:" + why
		if err != nil {
			row.Detail = err.Error()
			if len(row.Detail) > 300 {
				row.Detail = row.Detail[:300]
			}
		}
		return row
	}
	idx := filepath.Join(cfg.Work, "idx", fmt.Sprintf("dnaff_%s_%d.index", repoDirName(t.Repo), seq))
	os.MkdirAll(filepath.Dir(idx), 0o755)
	ib := newIndexBuilder(mirror, idx, t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		return skip("test_patch", err)
	}
	c, err := ib.Apply(ctx, t.Patch, "eval: gold patch")
	if err != nil {
		return skip("patch", err)
	}
	row.P, row.C = p, c
	ls, err := git(ctx, mirror, "ls-tree", "-r", "--name-only", c)
	if err != nil {
		return skip("ls-tree", err)
	}
	nproj := 0
	for _, f := range strings.Split(ls, "\n") {
		if dotnet.IsProjectFile(f) {
			nproj++
		}
	}
	if cfg.MaxProjects > 0 && nproj > cfg.MaxProjects {
		return skip("too_large", fmt.Errorf("%d projects > %d", nproj, cfg.MaxProjects))
	}

	ix, err := bgraph.Open(ctx, mirror, bgraph.Options{StateDir: filepath.Join(cfg.Work, "state", repoDirName(t.Repo))})
	if err != nil {
		return skip("open", err)
	}
	sel, err := ix.Select(ctx, bgraph.Change{Base: p, Head: c})
	ix.Close()
	if err != nil {
		return skip("select", err)
	}
	row.Scope, row.Units, row.Selected = string(sel.Scope), sel.IndexStats["units"], len(sel.Tests)
	row.Changed = sel.Changed
	if len(row.Changed) > 40 {
		row.Changed = row.Changed[:40]
	}
	if sel.Scope == model.TierFull {
		row.Status = "full"
		return row
	}
	selected := map[string]bool{}
	for _, s := range sel.Tests {
		selected[s.Unit] = true
	}

	wt := filepath.Join(cfg.Work, "wt", fmt.Sprintf("dnaff_%s_%d", repoDirName(t.Repo), seq))
	cleanup, err := addWorktree(ctx, mirror, wt, c)
	if err != nil {
		return skip("checkout", err)
	}
	defer cleanup()
	row.Patched = patchGlobalJSON(wt)
	t0 := time.Now()
	tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	args := []string{"-p", wt, "--from", p, "--uncommitted", "None", "-f", "json", "--dry-run"}
	var out, errOut string
	var excluded []string
	for attempt := 0; ; attempt++ {
		out, errOut, err = cfg.Tools.run(tctx, wt, cfg.Tools.Affected, args...)
		if err == nil || attempt >= 4 {
			break
		}
		// A project that references a project file the repository itself lacks cannot be loaded; leave the referencing project out of the
		// discovery (nothing can depend on it) and retry, so the rest of the repository is still compared.
		m := brokenRefRe.FindStringSubmatch(errOut + out)
		if m == nil || strings.Contains(strings.Join(excluded, "|"), m[1]) {
			break
		}
		excluded = append(excluded, m[1])
		args = append(args[:len(args):len(args)], "--exclude-discovery", "^"+regexp.QuoteMeta(m[1])+"$")
		row.Excluded = append(row.Excluded, filepath.ToSlash(strings.TrimPrefix(m[1], wt+string(filepath.Separator))))
	}
	row.ToolMs = time.Since(t0).Milliseconds()
	if err != nil && strings.Contains(errOut+out, "No affected projects") {
		err, out = nil, "" // an empty affected set is a result, not a failure
	}
	if err != nil {
		row.Status = "tool_failed"
		msg := strings.TrimSpace(errOut + "\n" + out)
		if i := strings.Index(msg, "Unhandled exception"); i >= 0 {
			msg = msg[i:]
		}
		if len(msg) > 700 {
			msg = msg[:700]
		}
		row.Detail = msg
		return row
	}
	aff := map[string]bool{}
	i := strings.Index(out, "CONTENTS:")
	if i < 0 {
		i = 0
	}
	for _, m := range affectedPathRe.FindAllStringSubmatch(out[i:], -1) {
		var fp string
		if json.Unmarshal([]byte(`"`+m[1]+`"`), &fp) != nil {
			continue
		}
		rel, err := filepath.Rel(wt, fp)
		if err != nil {
			continue
		}
		aff[dotnet.UnitID(filepath.ToSlash(rel))] = true
	}
	row.Affected = len(aff)
	for u := range aff {
		if !selected[u] {
			row.Missing = append(row.Missing, u)
		}
	}
	for u := range selected {
		if !aff[u] {
			row.Extra++
		}
	}
	sort.Strings(row.Missing)
	row.Status = "ok"
	if len(row.Missing) > 0 {
		row.Status = "violation"
	}
	return row
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// ---- provider check: ProjectReference edges against MSBuild evaluation ----

// DotnetProviderRow is the provider comparison of one repo at the sampled base commits.
type DotnetProviderRow struct {
	Repo      string   `json:"repo"`
	Commits   int      `json:"commits"`
	Projects  int      `json:"projects"` // projects compared (msbuild evaluation succeeded)
	Failed    int      `json:"failed"`   // msbuild evaluation failed
	Edges     int      `json:"msbuild_edges"`
	Covered   int      `json:"covered"` // msbuild edges that bgraph has too
	Missing   []string `json:"missing"` // "commit:from -> to" edges msbuild has and bgraph lacks
	Errors    []string `json:"errors,omitempty"`
	Commit    []string `json:"commit_list,omitempty"`
	ElapsedMs int64    `json:"elapsed_ms"`
}

// DotnetProviderConfig configures DotnetProviderRepo.
type DotnetProviderConfig struct {
	Work        string
	Tools       DotnetTools
	MaxCommits  int
	MaxProjects int // per commit
	Timeout     time.Duration
	Jobs        int // parallel msbuild evaluations
}

type msbuildItems struct {
	Items      map[string][]map[string]string `json:"Items"`
	Properties map[string]string              `json:"Properties"`
}

// DotnetProviderRepo compares, at up to MaxCommits base commits of the repo's tasks, bgraph's ProjectReference edges with the items that
// `dotnet msbuild -getItem:ProjectReference` evaluates (no restore, no build). The check is msbuild subset-of bgraph: bgraph never evaluates a
// Condition, so it may have more edges; it must not lack one.
func DotnetProviderRepo(ctx context.Context, cfg DotnetProviderConfig, mirror, repo string, bases []string) (row DotnetProviderRow) {
	row = DotnetProviderRow{Repo: repo}
	t0 := time.Now()
	defer func() { row.ElapsedMs = time.Since(t0).Milliseconds() }()
	if cfg.MaxCommits > 0 && len(bases) > cfg.MaxCommits {
		bases = bases[:cfg.MaxCommits]
	}
	for _, base := range bases {
		row.Commit = append(row.Commit, base[:10])
		row.Commits++
		gt, err := dotnetGitTree(ctx, mirror, base)
		if err != nil {
			row.Errors = append(row.Errors, base[:10]+": "+err.Error())
			continue
		}
		a, err := dotnet.New(nil).Analyze(ctx, gt, scancache.New())
		gt.Close()
		if err != nil {
			row.Errors = append(row.Errors, base[:10]+": analyze: "+err.Error())
			continue
		}
		refs := map[string]map[string]bool{} // project file -> referenced node / path
		var projects []string
		for _, u := range a.Tests {
			if pf, ok := dotnet.ParseUnitID(u.ID); ok {
				projects = append(projects, pf)
				refs[pf] = map[string]bool{}
			}
		}
		for _, e := range a.Edges {
			if from, ok := strings.CutPrefix(e.From, "<dotnet:"); ok && refs[strings.TrimSuffix(from, ">")] != nil {
				refs[strings.TrimSuffix(from, ">")][e.To] = true
			}
		}
		sort.Strings(projects)
		// Deterministic sample: every k-th project.
		if cfg.MaxProjects > 0 && len(projects) > cfg.MaxProjects {
			step := float64(len(projects)) / float64(cfg.MaxProjects)
			var s []string
			for i := 0; i < cfg.MaxProjects; i++ {
				s = append(s, projects[int(float64(i)*step)])
			}
			projects = s
		}
		wt := filepath.Join(cfg.Work, "wt", "dnprov_"+repoDirName(repo)+"_"+base[:8])
		cleanup, err := addWorktree(ctx, mirror, wt, base)
		if err != nil {
			row.Errors = append(row.Errors, base[:10]+": checkout: "+err.Error())
			continue
		}
		patchGlobalJSON(wt)
		type res struct {
			proj  string
			edges []string
			err   string
		}
		jobs := cfg.Jobs
		if jobs <= 0 {
			jobs = 4
		}
		in := make(chan string)
		outc := make(chan res)
		for w := 0; w < jobs; w++ {
			go func() {
				for pf := range in {
					edges, err := msbuildProjectRefs(ctx, cfg, wt, pf)
					r := res{proj: pf, edges: edges}
					if err != nil {
						r.err = err.Error()
					}
					outc <- r
				}
			}()
		}
		go func() {
			for _, pf := range projects {
				in <- pf
			}
			close(in)
		}()
		for range projects {
			r := <-outc
			if r.err != "" {
				row.Failed++
				if len(row.Errors) < 12 {
					row.Errors = append(row.Errors, base[:10]+":"+r.proj+": "+r.err)
				}
				continue
			}
			row.Projects++
			have := refs[r.proj]
			for _, to := range r.edges {
				row.Edges++
				if have[dotnet.Node(to)] || have[to] {
					row.Covered++
				} else {
					row.Missing = append(row.Missing, fmt.Sprintf("%s:%s -> %s", base[:8], r.proj, to))
				}
			}
		}
		cleanup()
	}
	return row
}

// msbuildProjectRefs evaluates the ProjectReference items of one project (repo-relative, forward slashes) and returns the referenced
// project files relative to the worktree. A multi-targeted project is evaluated for its first and last target framework as well (the
// outer evaluation does not see references conditioned on $(TargetFramework)).
func msbuildProjectRefs(ctx context.Context, cfg DotnetProviderConfig, wt, proj string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	eval := func(extra ...string) (*msbuildItems, error) {
		tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		args := append([]string{"msbuild", proj, "-getItem:ProjectReference", "-getProperty:TargetFrameworks,TargetFramework", "-nologo", "-nodeReuse:false", "-verbosity:quiet"}, extra...)
		so, se, err := cfg.Tools.run(tctx, wt, cfg.Tools.Dotnet, args...)
		if err != nil {
			return nil, fmt.Errorf("%s", tail(strings.TrimSpace(se+"\n"+so), 300))
		}
		var mi msbuildItems
		i := strings.Index(so, "{")
		if i < 0 {
			return nil, fmt.Errorf("no JSON: %s", tail(so, 200))
		}
		if err := json.Unmarshal([]byte(so[i:]), &mi); err != nil {
			return nil, fmt.Errorf("parse: %v: %s", err, tail(so, 200))
		}
		return &mi, nil
	}
	add := func(mi *msbuildItems) {
		for _, it := range mi.Items["ProjectReference"] {
			fp := it["FullPath"]
			if fp == "" {
				continue
			}
			rel, err := filepath.Rel(wt, fp)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			rel = path.Clean(filepath.ToSlash(rel))
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	mi, err := eval()
	if err != nil {
		return nil, err
	}
	add(mi)
	if tfs := strings.Split(strings.Trim(mi.Properties["TargetFrameworks"], ";"), ";"); len(tfs) > 1 && mi.Properties["TargetFramework"] == "" {
		for _, tf := range []string{tfs[0], tfs[len(tfs)-1]} {
			if m2, err := eval("-p:TargetFramework=" + tf); err == nil {
				add(m2)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// DotnetProjectCount counts the project files of commit rev (cheap: trees only).
func DotnetProjectCount(ctx context.Context, mirror, rev string) (int, error) {
	ls, err := git(ctx, mirror, "ls-tree", "-r", "--name-only", rev)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range strings.Split(ls, "\n") {
		if dotnet.IsProjectFile(f) {
			n++
		}
	}
	return n, nil
}
