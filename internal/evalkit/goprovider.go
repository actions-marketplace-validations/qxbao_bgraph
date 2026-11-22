package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/golang"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// GoProviderResult compares bgraph's Go analysis of one commit with `go list -e -json -test ./...` run in every module of the commit.
type GoProviderResult struct {
	Repo    string   `json:"repo"`
	Commit  string   `json:"commit"`
	GOOS    string   `json:"goos"`
	Error   string   `json:"error,omitempty"`  // worktree / analysis failure (nothing compared)
	Modules int      `json:"modules"`          // go.mod files found
	Failed  []string `json:"failed,omitempty"` // modules whose go list failed (reason) — reported, not hidden
	Listed  int      `json:"listed_packages"`  // in-repo packages go list reported
	WithTst int      `json:"packages_w_tests"` // ... of which with _test.go files for the GOOS
	Units   int      `json:"units"`            // bgraph test units
	Edges   int      `json:"edges_checked"`    // in-repo Imports/TestImports/XTestImports edges
	Embeds  int      `json:"embed_files_checked"`
	PkgErrs int      `json:"packages_with_errors,omitempty"`
	// Unlisted: imports of an in-repo module path that go list did not report a package for (build-constraint or load errors).
	Unlisted   int      `json:"unlisted_inrepo_imports,omitempty"`
	Match      bool     `json:"match"`
	Mismatches []string `json:"mismatches,omitempty"`
	// Informational (not mismatches): bgraph has a unit/project that go list does not report.
	Extra []string `json:"extra,omitempty"`
	Ms    int64    `json:"ms"`
}

type goListPkg struct {
	Dir             string
	ImportPath      string
	ForTest         string
	Standard        bool
	GoFiles         []string
	TestGoFiles     []string
	XTestGoFiles    []string
	Imports         []string
	TestImports     []string
	XTestImports    []string
	EmbedFiles      []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
	Module          *struct {
		Path string
		Dir  string
		Main bool
	}
	Error *struct{ Err string }
}

// goFindModules lists the module directories (repo-relative, "." for the root) of the files of a tree: every go.mod outside vendor/,
// testdata/ and directories starting with "." or "_".
func goFindModules(files []string) []string {
	var out []string
	for _, f := range files {
		if path.Base(f) == "go.mod" && !goIgnoredDir(path.Dir(f)) {
			out = append(out, path.Dir(f))
		}
	}
	sort.Strings(out)
	return out
}

// runGoList runs `go list -e -json -test ./...` in dir and returns the base package entries (no test variants, no generated test mains).
func runGoList(ctx context.Context, dir, goos string) ([]goListPkg, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "list", "-e", "-json", "-test", "./...")
	cmd.Dir = dir
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOMODCACHE=D:/cache/go-mod", "GOCACHE=D:/cache/go-build", "GOTOOLCHAIN=auto", "GOWORK=off")
	if goos != "" {
		env = append(env, "GOOS="+goos)
	}
	cmd.Env = env
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	runErr := cmd.Run()
	var out []goListPkg
	dec := json.NewDecoder(&so)
	for {
		var p goListPkg
		if err := dec.Decode(&p); err != nil {
			if err != io.EOF && runErr == nil {
				runErr = err
			}
			break
		}
		if p.ForTest != "" || strings.HasSuffix(p.ImportPath, ".test") || strings.Contains(p.ImportPath, " [") {
			continue
		}
		out = append(out, p)
	}
	if runErr != nil && len(out) == 0 {
		msg := strings.TrimSpace(se.String())
		if len(msg) > 600 {
			msg = msg[:600]
		}
		return nil, fmt.Errorf("%v: %s", runErr, msg)
	}
	return out, nil
}

// goReach answers "is node b reachable from node a" over an analysis' edges (pattern edges matched against the tree's files).
type goReach struct {
	fwd   map[string][]string
	pats  map[string][]*glob.Pattern
	files []string
	match map[string][]string
	memo  map[string]map[string]bool
}

func newGoReach(a *eco.Analysis, files []string) *goReach {
	g := &goReach{fwd: map[string][]string{}, pats: map[string][]*glob.Pattern{}, files: files, match: map[string][]string{}, memo: map[string]map[string]bool{}}
	for _, e := range a.Edges {
		if e.Kind == model.EdgePattern {
			if pt, err := glob.Compile(e.To); err == nil {
				g.pats[e.From] = append(g.pats[e.From], pt)
			}
			continue
		}
		g.fwd[e.From] = append(g.fwd[e.From], e.To)
	}
	return g
}

func (g *goReach) from(start string) map[string]bool {
	if m, ok := g.memo[start]; ok {
		return m
	}
	seen := map[string]bool{start: true}
	q := []string{start}
	for len(q) > 0 {
		c := q[0]
		q = q[1:]
		next := g.fwd[c]
		for _, pt := range g.pats[c] {
			ms, ok := g.match[pt.String()]
			if !ok {
				for _, f := range g.files {
					if pt.Match(f) {
						ms = append(ms, f)
					}
				}
				g.match[pt.String()] = ms
			}
			next = append(next, ms...)
		}
		for _, n := range next {
			if !seen[n] {
				seen[n] = true
				q = append(q, n)
			}
		}
	}
	g.memo[start] = seen
	return seen
}

// GoProviderCompare runs the comparison for repo (a clone under work/repos) at each commit, using a temporary worktree under work/wt.
// The first commit is also compared under GOOS=linux.
func GoProviderCompare(ctx context.Context, work, repo string, commits []string) []GoProviderResult {
	var out []GoProviderResult
	dir := RustRepoDir(work, repo)
	for i, c := range commits {
		wt := filepath.Join(work, "wt", fmt.Sprintf("gp_%s_%d", repoDirName(repo), i))
		oses := []string{""}
		if i == 0 {
			oses = append(oses, "linux")
		}
		res := goProviderOne(ctx, dir, wt, repo, c, oses)
		out = append(out, res...)
	}
	return out
}

func goProviderOne(ctx context.Context, dir, wt, repo, c string, oses []string) []GoProviderResult {
	fail := func(e string) []GoProviderResult {
		var rs []GoProviderResult
		for _, o := range oses {
			rs = append(rs, GoProviderResult{Repo: repo, Commit: c, GOOS: goosName(o), Error: e})
		}
		return rs
	}
	os.RemoveAll(wt)
	git(ctx, dir, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return fail(err.Error())
	}
	defer func() {
		git(context.Background(), dir, "worktree", "remove", "--force", wt)
		os.RemoveAll(wt)
		git(context.Background(), dir, "worktree", "prune")
	}()
	if _, err := git(ctx, dir, "worktree", "add", "--detach", wt, c); err != nil {
		// A Windows-invalid name in the tree fails the checkout after writing everything else; the compare proceeds when go.mod is there.
		if _, serr := os.Stat(filepath.Join(wt, "go.mod")); serr != nil {
			return fail("worktree: " + err.Error())
		}
	}
	gt, err := newGitTreeWant(ctx, dir, c, goPrefetchWant)
	if err != nil {
		return fail("tree: " + err.Error())
	}
	defer gt.Close()
	t0 := time.Now()
	a, err := golang.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return fail("analyze: " + err.Error())
	}
	ms := goFindModules(gt.Files())
	reach := newGoReach(a, gt.Files())
	anaMs := time.Since(t0).Milliseconds()
	var rs []GoProviderResult
	for _, o := range oses {
		r := GoProviderResult{Repo: repo, Commit: c, GOOS: goosName(o), Modules: len(ms), Units: len(a.Tests)}
		t1 := time.Now()
		var pkgs []goListPkg
		for _, m := range ms {
			ps, err := runGoList(ctx, filepath.Join(wt, filepath.FromSlash(m)), o)
			if err != nil {
				r.Failed = append(r.Failed, m+": "+err.Error())
				continue
			}
			pkgs = append(pkgs, ps...)
		}
		r.Mismatches, r.Extra = compareGoList(wt, pkgs, ms, a, reach, &r)
		r.Match = len(r.Mismatches) == 0
		r.Ms = anaMs + time.Since(t1).Milliseconds()
		rs = append(rs, r)
	}
	return rs
}

func goosName(o string) string {
	if o == "" {
		return "default"
	}
	return o
}

// compareGoList checks: every in-repo package with tests is a unit; every in-repo import edge is reachable from the importer's library
// (Imports) or unit (test imports) node to the imported package's library node; every embedded file reaches its package.
func compareGoList(root string, pkgs []goListPkg, mods []string, a *eco.Analysis, g *goReach, r *GoProviderResult) (diffs, extra []string) {
	rel := func(p string) string { return relToRoot(root, p) }
	units := map[string]bool{}
	for _, u := range a.Tests {
		units[u.ID] = true
	}
	byImp := map[string][]string{} // import path -> repo-relative dirs (several when two modules declare the same path)
	modOf := func(dir string) string {
		best := ""
		for _, m := range mods {
			if (m == "." || dir == m || strings.HasPrefix(dir, m+"/")) && len(m) >= len(best) {
				best = m
			}
		}
		return best
	}
	var inRepo []goListPkg
	for _, p := range pkgs {
		if p.Standard || p.Dir == "" {
			continue
		}
		d := rel(p.Dir)
		if filepath.IsAbs(d) || strings.HasPrefix(d, "../") || strings.Contains(d, ":") {
			continue // outside the worktree (module cache)
		}
		if d == "" {
			d = "."
		}
		if p.Error != nil {
			r.PkgErrs++
		}
		byImp[p.ImportPath] = append(byImp[p.ImportPath], d)
		inRepo = append(inRepo, p)
	}
	modPaths := map[string]bool{}
	for _, p := range a.Projects {
		modPaths[p.Name] = true
	}
	inRepoPath := func(imp string) bool {
		for m := range modPaths {
			if m != "" && (imp == m || strings.HasPrefix(imp, m+"/")) {
				return true
			}
		}
		return false
	}
	r.Listed = len(inRepo)
	listedDirs := map[string]bool{}
	for _, p := range inRepo {
		d := rel(p.Dir)
		if d == "" {
			d = "."
		}
		listedDirs[d] = true
		if len(p.TestGoFiles)+len(p.XTestGoFiles) > 0 {
			r.WithTst++
			if !units[golang.UnitID(d)] {
				diffs = append(diffs, fmt.Sprintf("package %s (%s) has tests (%v) but bgraph has no unit", d, p.ImportPath, append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...)))
			}
		}
		check := func(kind string, start string, imps []string) {
			for _, imp := range imps {
				dqs := byImp[imp]
				if len(dqs) == 0 {
					if inRepoPath(imp) {
						r.Unlisted++
					}
					continue
				}
				// Prefer the declaration in the importer's own module; with several candidates reaching any of them is enough.
				var same []string
				for _, dq := range dqs {
					if modOf(dq) == modOf(d) {
						same = append(same, dq)
					}
				}
				if len(same) > 0 {
					dqs = same
				}
				r.Edges++
				ok := false
				for _, dq := range dqs {
					ok = ok || g.from(start)[golang.LibNode(dq)]
				}
				if !ok {
					diffs = append(diffs, fmt.Sprintf("%s edge %s -> %v (%s) not reachable from %s", kind, d, dqs, imp, start))
				}
			}
		}
		check("import", golang.LibNode(d), p.Imports)
		if units[golang.UnitID(d)] {
			check("test-import", golang.Node(d), p.TestImports)
			check("xtest-import", golang.Node(d), p.XTestImports)
		}
		emb := func(kind, start string, fs []string) {
			for _, f := range fs {
				r.Embeds++
				fp := path.Join(d, f)
				if d == "." {
					fp = path.Clean(f)
				}
				if !g.from(start)[fp] {
					diffs = append(diffs, fmt.Sprintf("%s file %s of package %s not reachable from %s", kind, fp, d, start))
				}
			}
		}
		emb("embed", golang.LibNode(d), p.EmbedFiles)
		if units[golang.UnitID(d)] {
			emb("test-embed", golang.Node(d), p.TestEmbedFiles)
			emb("xtest-embed", golang.Node(d), p.XTestEmbedFiles)
		}
	}
	for id := range units {
		if d, ok := golang.ParseUnitID(id); ok && !listedDirs[d] {
			extra = append(extra, "bgraph unit "+id+" not reported by go list (all files excluded by constraints, load error or module failed)")
		}
	}
	bgMods := map[string]bool{}
	for _, p := range a.Projects {
		bgMods[p.Dir] = true
	}
	for _, m := range mods {
		if !bgMods[m] {
			diffs = append(diffs, "module "+m+" has a go.mod but is not a bgraph project")
		}
	}
	listedMods := map[string]bool{}
	for _, m := range mods {
		listedMods[m] = true
	}
	for m := range bgMods {
		if !listedMods[m] {
			extra = append(extra, "bgraph project "+m+" has no go.mod outside vendor/testdata")
		}
	}
	sort.Strings(diffs)
	sort.Strings(extra)
	return diffs, extra
}

// GoProviderRepos picks up to n repos for the provider check: repos with several modules or packages first, skipping repos whose tree
// holds more than maxGo Go files (a checkout + go list of those is too slow). Counting uses only trees (no blobs).
func GoProviderRepos(ctx context.Context, work string, tasks []Task, n, maxGo int) []string {
	type rc struct {
		repo      string
		mods, go_ int
		dirs      int
	}
	seen := map[string]bool{}
	var cs []rc
	for _, t := range tasks {
		k := strings.ToLower(t.Repo)
		if seen[k] {
			continue
		}
		seen[k] = true
		out, err := git(ctx, RustRepoDir(work, t.Repo), "ls-tree", "-r", "--name-only", t.BaseCommit)
		if err != nil {
			continue
		}
		x := rc{repo: t.Repo}
		dirs := map[string]bool{}
		for _, f := range strings.Split(out, "\n") {
			switch {
			case strings.HasSuffix(f, ".go") && !goIgnoredDir(path.Dir(f)):
				x.go_++
				dirs[path.Dir(f)] = true
			case path.Base(f) == "go.mod" && !goIgnoredDir(path.Dir(f)):
				x.mods++
			}
		}
		x.dirs = len(dirs)
		if x.go_ > maxGo || x.go_ == 0 {
			continue
		}
		cs = append(cs, x)
	}
	sort.Slice(cs, func(i, j int) bool {
		if (cs[i].mods > 1) != (cs[j].mods > 1) {
			return cs[i].mods > 1
		}
		if cs[i].dirs != cs[j].dirs {
			return cs[i].dirs > cs[j].dirs
		}
		return cs[i].repo < cs[j].repo
	})
	var repos []string
	for i := 0; i < len(cs) && i < n; i++ {
		repos = append(repos, cs[i].repo)
	}
	return repos
}
