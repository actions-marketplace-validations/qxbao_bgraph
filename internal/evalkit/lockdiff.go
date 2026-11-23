package evalkit

// Lockfile differential check (gate of level L1, docs/lockfiles.md): for every sampled lockfile commit, the dependency closure
// that internal/lockfile computes for each project must be a superset of the package set the package manager itself
// reports for that project (pnpm list, npm ls, cargo metadata, uv export, poetry export).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/lockfile"
	"gopkg.in/yaml.v3"

	"github.com/pelletier/go-toml/v2"
)

// LockdiffOpts configures one format's run.
type LockdiffOpts struct {
	Format  string // pnpm | npm | cargo | uv | poetry
	Work    string // work dir holding repos/<owner__name>
	Tmp     string // scratch dir for materialized trees and worktrees (removed after use)
	Commits int    // lockfile-changing commits per repo
	Until   string // only commits before this date (git log --until), to sample older lockfile versions
	Pnpm    []string
	// PnpmConvert: a pnpm 9 command that rewrites a 6.x lockfile as 9.0 (install --lockfile-only --frozen-lockfile)
	// before Pnpm lists it: pnpm list --lockfile-only exists only in pnpm 10, which cannot read 6.x.
	PnpmConvert []string
	Uv, Poetry  string   // uv and poetry commands (poetry with poetry-plugin-export)
	Env         []string // extra environment for the tool
	Log         func(string, ...any)
}

// LockdiffMismatch: packages the tool reports for a project that our closure lacks.
type LockdiffMismatch struct {
	Project string   `json:"project"`
	Count   int      `json:"count"`
	Missing []string `json:"missing"` // name@version, at most 20
}

// LockdiffCommit is the check at one commit.
type LockdiffCommit struct {
	Commit      string `json:"commit"`
	LockVersion string `json:"lock_version"`
	// Status: ok | unsupported (format version deliberately not parsed) | parse-error | tool-unreadable | tool-error | absent
	Status       string             `json:"status"`
	Err          string             `json:"err,omitempty"`
	OurProjects  int                `json:"our_projects"`
	ToolProjects int                `json:"tool_projects"`
	Checked      int                `json:"checked"` // projects present on both sides
	ToolOnly     []string           `json:"tool_only,omitempty"`
	OursOnly     []string           `json:"ours_only,omitempty"`
	ToolPkgs     int                `json:"tool_pkgs"` // sum over checked projects of |T(P)|
	Mismatches   []LockdiffMismatch `json:"mismatches,omitempty"`
	Seconds      float64            `json:"seconds"`
}

// LockdiffPair is Compare between two consecutive sampled commits.
type LockdiffPair struct {
	Base string `json:"base"`
	Head string `json:"head"`
	Err  string `json:"err,omitempty"`
	// L0: why Compare could not compare the pair (lockfile.IncomparableError reasons: top-level keys, "version"...).
	L0      []string `json:"l0,omitempty"`
	Changed int      `json:"changed"`
	Total   int      `json:"total"`
	// Patches: patch selectors that changed between the sides (their packages count as changed).
	Patches []string `json:"patches,omitempty"`
	// Missed: projects whose package set reported by the tool (name@version) differs between the sides while Compare
	// says their closure is unchanged (a soundness failure of the pair comparison). Checked only when the tool read both.
	Missed []string `json:"missed,omitempty"`
	// KindOnly: projects whose tool package set changed only by packages our closure holds on both sides (a dependency
	// kind change, e.g. dependencies → devDependencies of a linked workspace package): not a miss.
	KindOnly []string `json:"kind_only,omitempty"`
	// FormerL0: the reasons the pair would have had before overrides and patches were compared (L0 plus "overrides" and
	// "patchedDependencies" when they differ), for the before/after count.
	FormerL0    []string `json:"former_l0,omitempty"`
	ToolChecked int      `json:"tool_checked"` // projects compared against the tool for Missed
}

// LockdiffRepo is one repo's result.
type LockdiffRepo struct {
	Repo     string           `json:"repo"`
	Lockfile string           `json:"lockfile"`
	Skipped  string           `json:"skipped,omitempty"`
	Commits  []LockdiffCommit `json:"commits"`
	Pairs    []LockdiffPair   `json:"pairs"`
}

func lockBase(format string) []string {
	switch format {
	case lockfile.FormatPnpm:
		return []string{"pnpm-lock.yaml"}
	case lockfile.FormatNpm:
		return []string{"package-lock.json"}
	case lockfile.FormatCargo:
		return []string{"Cargo.lock"}
	case lockfile.FormatUv:
		return []string{"uv.lock"}
	case lockfile.FormatPoetry:
		return []string{"poetry.lock"}
	}
	return nil
}

// LockdiffPick returns the lockfile of the format with the most commits in the last 200 of history (ties: shortest path).
func LockdiffPick(ctx context.Context, repoDir, format string) (string, error) {
	out, err := git(ctx, repoDir, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		return "", err
	}
	best, bestN := "", -1
	for _, p := range strings.Split(out, "\n") {
		p = strings.TrimSpace(p)
		if p == "" || strings.Contains("/"+p, "/node_modules/") {
			continue
		}
		ok := false
		for _, b := range lockBase(format) {
			ok = ok || path.Base(p) == b
		}
		if !ok {
			continue
		}
		l, _ := git(ctx, repoDir, "log", "--format=%H", "-n", "200", "--", p)
		n := len(strings.Fields(l))
		if n > bestN || (n == bestN && len(p) < len(best)) {
			best, bestN = p, n
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s lockfile at HEAD", format)
	}
	return best, nil
}

// LockdiffRun checks one repo. spec is "owner__name" or "owner__name:path/to/lockfile".
func LockdiffRun(ctx context.Context, o LockdiffOpts, spec string) LockdiffRepo {
	repo, lockPath, _ := strings.Cut(spec, ":")
	res := LockdiffRepo{Repo: repo}
	repoDir := filepath.Join(o.Work, "repos", repo)
	if _, err := os.Stat(repoDir); err != nil {
		res.Skipped = "no clone"
		return res
	}
	if lockPath == "" {
		p, err := LockdiffPick(ctx, repoDir, o.Format)
		if err != nil {
			res.Skipped = err.Error()
			return res
		}
		lockPath = p
	}
	res.Lockfile = lockPath
	lockDir := path.Dir(lockPath)
	logArgs := []string{"log", "--format=%H", "-n", strconv.Itoa(o.Commits)}
	if o.Until != "" {
		logArgs = append(logArgs, "--until="+o.Until)
	}
	out, err := git(ctx, repoDir, append(logArgs, "--", lockPath)...)
	if err != nil {
		res.Skipped = err.Error()
		return res
	}
	commits := strings.Fields(out)
	if len(commits) == 0 {
		res.Skipped = "no lockfile commits"
		return res
	}
	slices_reverse(commits) // chronological
	var wt string
	if o.Format == lockfile.FormatCargo {
		wt = filepath.Join(o.Tmp, "wt-"+repo)
		os.RemoveAll(wt)
		git(ctx, repoDir, "worktree", "prune")
		if _, err := git(ctx, repoDir, "worktree", "add", "--detach", "--no-checkout", wt, commits[len(commits)-1]); err != nil {
			res.Skipped = "worktree: " + err.Error()
			return res
		}
		defer func() {
			git(context.Background(), repoDir, "worktree", "remove", "--force", wt)
			os.RemoveAll(wt)
			git(context.Background(), repoDir, "worktree", "prune")
		}()
	}
	var prev *lockfile.Graph
	var prevC string
	var prevTool map[string]map[string]bool // our project key → tool package set of prev (nil: not read)
	var prevData string
	for _, c := range commits {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		cr := LockdiffCommit{Commit: c}
		data, err := git(ctx, repoDir, "show", c+":"+lockPath)
		if err != nil {
			cr.Status, cr.Err = "absent", firstLine(err.Error())
			res.Commits = append(res.Commits, cr)
			prev, prevTool = nil, nil
			continue
		}
		cr.LockVersion = sniffLockVersion(o.Format, data)
		g, perr := lockfile.Parse(path.Base(lockPath), lockDir, []byte(data))
		if perr != nil {
			cr.Status, cr.Err = "parse-error", perr.Error()
			if strings.Contains(perr.Error(), "not supported") {
				cr.Status = "unsupported"
			}
			res.Commits = append(res.Commits, cr)
			prev, prevTool = nil, nil
			continue
		}
		var pr *LockdiffPair
		var pd *lockfile.Diff
		if prev != nil {
			pr = &LockdiffPair{Base: prevC, Head: c}
			if d, err := lockfile.Compare(prev, g); err != nil {
				pr.Err = err.Error()
				if ie, ok := err.(*lockfile.IncomparableError); ok {
					pr.L0 = ie.Reasons
				} else {
					pr.L0 = []string{"other"}
				}
			} else {
				pd = d
				all := map[string]bool{}
				for p := range prev.Projects {
					all[p] = true
				}
				for p := range g.Projects {
					all[p] = true
				}
				pr.Changed, pr.Total = len(d.Changed), len(all)
				pr.Patches = changedPatches(prev, g)
			}
			pr.FormerL0 = formerL0(o.Format, pr.L0, prevData, data)
		}
		prevG := prev
		prev, prevC, prevData = g, c, data
		var tool map[string]map[string]bool
		var terr error
		var unreadable bool
		switch o.Format {
		case lockfile.FormatPnpm, lockfile.FormatNpm:
			var projects []string
			for p := range g.Projects {
				projects = append(projects, p)
			}
			sort.Strings(projects)
			tool, unreadable, terr = lockdiffJS(ctx, o, repo, repoDir, c, lockPath, projects)
		case lockfile.FormatCargo:
			tool, unreadable, terr = lockdiffCargo(ctx, o, repoDir, wt, c, lockDir)
		case lockfile.FormatUv, lockfile.FormatPoetry:
			tool, unreadable, terr = lockdiffPy(ctx, o, repo, repoDir, c, lockPath, g)
		}
		if terr != nil {
			cr.Status, cr.Err = "tool-error", terr.Error()
			if unreadable {
				cr.Status = "tool-unreadable"
			}
			cr.Seconds = time.Since(start).Seconds()
			res.Commits = append(res.Commits, cr)
			o.Log("%s %s %.10s %s: %s", o.Format, repo, c, cr.Status, firstLine(cr.Err))
			if pr != nil {
				res.Pairs = append(res.Pairs, *pr)
			}
			prevTool = nil
			continue
		}
		curTool := lockdiffCheck(o.Format, g, tool, &cr, lockDir)
		cr.Status = "ok"
		cr.Seconds = time.Since(start).Seconds()
		res.Commits = append(res.Commits, cr)
		if pr != nil {
			if pd != nil && prevTool != nil {
				lockdiffPairCheck(prevG, g, pd, prevTool, curTool, pr, lockDir)
			}
			res.Pairs = append(res.Pairs, *pr)
			if len(pr.Missed) > 0 {
				o.Log("%s %s %.10s→%.10s MISSED projects: %s", o.Format, repo, pr.Base, pr.Head, strings.Join(pr.Missed, ", "))
			}
		}
		prevTool = curTool
		o.Log("%s %s %.10s v%s ours=%d tool=%d checked=%d mismatching=%d tool-only=%d (%.0fs)", o.Format, repo, c, cr.LockVersion, cr.OurProjects, cr.ToolProjects, cr.Checked, len(cr.Mismatches), len(cr.ToolOnly), cr.Seconds)
	}
	return res
}

func slices_reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

var (
	rePnpmVer  = regexp.MustCompile(`(?m)^lockfileVersion:\s*'?"?([0-9.]+)`)
	reNpmVer   = regexp.MustCompile(`"lockfileVersion"\s*:\s*([0-9]+)`)
	reCargoVer = regexp.MustCompile(`(?m)^version\s*=\s*([0-9]+)`)
	reUvRev    = regexp.MustCompile(`(?m)^revision\s*=\s*([0-9]+)`)
	rePoetry   = regexp.MustCompile(`(?m)^lock-version\s*=\s*"([^"]*)"`)
)

func sniffLockVersion(format, data string) string {
	var m []string
	switch format {
	case lockfile.FormatPnpm:
		all := rePnpmVer.FindAllStringSubmatch(data, -1)
		if len(all) > 0 {
			m = all[len(all)-1]
		}
	case lockfile.FormatNpm:
		m = reNpmVer.FindStringSubmatch(data)
	case lockfile.FormatCargo:
		m = reCargoVer.FindStringSubmatch(data)
		if m == nil {
			return "1-2"
		}
	case lockfile.FormatUv:
		m = reCargoVer.FindStringSubmatch(data)
		if m != nil {
			r := "0"
			if rm := reUvRev.FindStringSubmatch(data); rm != nil {
				r = rm[1]
			}
			return m[1] + "." + r
		}
	case lockfile.FormatPoetry:
		m = rePoetry.FindStringSubmatch(data)
	}
	if m == nil {
		return "?"
	}
	return m[1]
}

// formerL0 adds to the L0 reasons of a pnpm pair the top-level keys that used to force level L0 and no longer do.
func formerL0(format string, l0 []string, base, head string) []string {
	out := append([]string(nil), l0...)
	switch format {
	case lockfile.FormatUv, lockfile.FormatPoetry:
		// Keys compared through the resolved graph rather than as settings (uv [options], [manifest], requires-python
		// and markers; poetry content-hash): a pair where they differ would be at L0 if they were settings.
		keys := map[string][]string{lockfile.FormatUv: {"options", "manifest", "requires-python", "resolution-markers", "supported-markers", "required-markers"},
			lockfile.FormatPoetry: {"metadata.content-hash", "metadata.python-versions"}}[format]
		b, h := tomlKeys(base), tomlKeys(head)
		for _, k := range keys {
			if b != nil && h != nil && !reflect.DeepEqual(b[k], h[k]) && !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	if format != lockfile.FormatPnpm {
		return out
	}
	b, h := pnpmLastDoc(base), pnpmLastDoc(head)
	for _, k := range []string{"overrides", "patchedDependencies"} {
		if !reflect.DeepEqual(b[k], h[k]) && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// pnpmLastDoc decodes the last YAML document of a pnpm-lock.yaml (the project lockfile); nil when it does not decode.
func pnpmLastDoc(data string) map[string]any {
	dec := yaml.NewDecoder(strings.NewReader(data))
	var last map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			return last
		}
		last = d
	}
}

// changedPatches returns the sorted patch selectors whose entry differs between the graphs.
func changedPatches(base, head *lockfile.Graph) []string {
	var out []string
	for k, v := range base.Patches {
		if hv, ok := head.Patches[k]; !ok || hv != v {
			out = append(out, k)
		}
	}
	for k := range head.Patches {
		if _, ok := base.Patches[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// lockdiffPairCheck checks Compare's verdict on a pair against the tool: a project present on both sides whose tool
// package set changed must be in d.Changed.
//
// uv: a member's tool set includes the workspace root's dependency groups, and a change of the root's closure widens
// every project (docs/lockfiles.md): the member counts as changed when the root does, and its closure is taken
// together with the root's.
func lockdiffPairCheck(base, head *lockfile.Graph, d *lockfile.Diff, bt, ht map[string]map[string]bool, pr *LockdiffPair, lockDir string) {
	for p := range head.Projects {
		if _, ok := base.Projects[p]; !ok {
			continue
		}
		bs, ok1 := bt[p]
		hs, ok2 := ht[p]
		if !ok1 || !ok2 {
			continue
		}
		pr.ToolChecked++
		if maps.Equal(bs, hs) || d.Changed[p] || (base.Format == lockfile.FormatUv && d.Changed[lockDir]) {
			continue
		}
		// A package the tool adds or drops while our closure holds it on both sides changed only its dependency kind
		// (e.g. a workspace package's dependency moved to devDependencies, which the tool does not follow through the
		// link, while the closure includes every kind): nothing installed changed.
		bo, ho := lockdiffEffective(base, p, lockDir), lockdiffEffective(head, p, lockDir)
		kindOnly := true
		for _, m := range []struct{ a, b map[string]bool }{{bs, hs}, {hs, bs}} {
			for pkg := range m.a {
				if !m.b[pkg] && (!bo[pkg] || !ho[pkg]) {
					kindOnly = false
				}
			}
		}
		if kindOnly {
			pr.KindOnly = append(pr.KindOnly, p)
		} else {
			pr.Missed = append(pr.Missed, p)
		}
	}
	sort.Strings(pr.Missed)
	sort.Strings(pr.KindOnly)
}

// lockdiffEffective returns lockdiffOurs of project p, with the closure of the lockfile's root project added for uv
// (the root's dependency groups are installed with every member; a change there widens every project).
func lockdiffEffective(g *lockfile.Graph, p, lockDir string) map[string]bool {
	ours := lockdiffOurs(g, g.Projects[p])
	if g.Format == lockfile.FormatUv {
		if r, ok := g.Projects[lockDir]; ok {
			maps.Copy(ours, lockdiffOurs(g, r))
		}
	}
	return ours
}

// lockdiffOurs returns the name@version set of the closure of node id (with name@* for every name).
func lockdiffOurs(g *lockfile.Graph, id string) map[string]bool {
	ours := map[string]bool{}
	for _, nid := range g.Packages(id) {
		n, ok := g.Nodes[nid]
		if !ok || n.Project != "" {
			continue
		}
		ours[n.Package+"@"+n.Version] = true
		ours[n.Name+"@"+n.Version] = true
		ours[n.Package+"@*"] = true
		ours[n.Name+"@*"] = true
	}
	return ours
}

// lockdiffCheck fills the superset comparison of one commit. tool maps a project key to its package set (name@version).
// It returns the tool's package set by our project key.
func lockdiffCheck(format string, g *lockfile.Graph, tool map[string]map[string]bool, cr *LockdiffCommit, lockDir string) map[string]map[string]bool {
	cr.OurProjects, cr.ToolProjects = len(g.Projects), len(tool)
	byOurs := map[string]map[string]bool{}
	matched := map[string]bool{}
	keys := make([]string, 0, len(tool))
	for k := range tool {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		ourKey := p
		if format == lockfile.FormatCargo {
			k, ok := g.CargoProject(p)
			if !ok {
				cr.ToolOnly = append(cr.ToolOnly, p)
				continue
			}
			ourKey = k
		}
		if _, ok := g.Projects[ourKey]; !ok {
			cr.ToolOnly = append(cr.ToolOnly, p)
			continue
		}
		matched[ourKey] = true
		byOurs[ourKey] = tool[p]
		cr.Checked++
		ours := lockdiffEffective(g, ourKey, lockDir)
		var miss []string
		for pkg := range tool[p] {
			if !ours[pkg] {
				miss = append(miss, pkg)
			}
		}
		cr.ToolPkgs += len(tool[p])
		if len(miss) > 0 {
			sort.Strings(miss)
			m := LockdiffMismatch{Project: p, Count: len(miss), Missing: miss}
			if len(m.Missing) > 20 {
				m.Missing = m.Missing[:20]
			}
			cr.Mismatches = append(cr.Mismatches, m)
		}
	}
	if format != lockfile.FormatCargo { // cargo: path dependencies are source-less packages that are not members
		for p := range g.Projects {
			if !matched[p] {
				cr.OursOnly = append(cr.OursOnly, p)
			}
		}
		sort.Strings(cr.OursOnly)
	}
	return byOurs
}

// lockdiffMaterialize writes the files of commit c the JS package manager reads (lockfile, manifests, workspace and
// npm settings, patches) under dst.
func lockdiffMaterialize(ctx context.Context, repoDir, c, lockPath, dst string) error {
	return lockdiffMaterializeKeep(ctx, repoDir, c, lockPath, dst, func(rel string) bool {
		if strings.Contains("/"+rel, "/node_modules/") {
			return false
		}
		return path.Base(rel) == "package.json" || rel == path.Base(lockPath) || rel == "pnpm-workspace.yaml" || rel == ".npmrc" ||
			(strings.HasPrefix(rel, "patches/") || strings.Contains(rel, "/patches/")) && strings.HasSuffix(rel, ".patch")
	})
}

// lockdiffMaterializeKeep writes the files of commit c under the lockfile's directory that keep accepts (by path
// relative to that directory) under dst.
func lockdiffMaterializeKeep(ctx context.Context, repoDir, c, lockPath, dst string, keep func(rel string) bool) error {
	lockDir := path.Dir(lockPath)
	args := []string{"ls-tree", "-r", c}
	if lockDir != "." {
		args = append(args, "--", lockDir+"/")
	}
	out, err := git(ctx, repoDir, args...)
	if err != nil {
		return err
	}
	type ent struct{ oid, rel string }
	var ents []ent
	for _, line := range strings.Split(out, "\n") {
		meta, p, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		rel := p
		if lockDir != "." {
			rel = strings.TrimPrefix(p, lockDir+"/")
		}
		if keep(rel) {
			ents = append(ents, ent{f[2], rel})
		}
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = repoDir
	var in strings.Builder
	for _, e := range ents {
		in.WriteString(e.oid + "\n")
	}
	cmd.Stdin = strings.NewReader(in.String())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	r := bufio.NewReader(stdout)
	for _, e := range ents {
		hdr, err := r.ReadString('\n')
		if err != nil {
			cmd.Wait()
			return fmt.Errorf("cat-file: %v %s", err, errb.String())
		}
		f := strings.Fields(hdr)
		if len(f) != 3 {
			cmd.Wait()
			return fmt.Errorf("cat-file: %q", hdr)
		}
		n, _ := strconv.Atoi(f[2])
		buf := make([]byte, n+1)
		if _, err := io.ReadFull(r, buf); err != nil {
			cmd.Wait()
			return err
		}
		fp := filepath.Join(dst, filepath.FromSlash(e.rel))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			continue
		}
		os.WriteFile(fp, buf[:n], 0o644)
	}
	return cmd.Wait()
}

// stripPnpmEnvDocument drops a leading env-lockfile YAML document (pnpm 10 reads single-document lockfiles only).
func stripPnpmEnvDocument(data []byte) []byte {
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return data
	}
	lines := strings.SplitAfter(s, "\n")
	seps := 0
	for i, l := range lines {
		if strings.TrimRight(l, "\r\n") == "---" {
			seps++
			if seps == 2 {
				return []byte(strings.Join(lines[i+1:], ""))
			}
		}
	}
	return data
}

func runTool(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// lockdiffJS runs pnpm list / npm ls on the materialized tree of commit c and returns each project's package set, keyed
// by repo-relative project directory.
func lockdiffJS(ctx context.Context, o LockdiffOpts, repo, repoDir, c, lockPath string, projects []string) (map[string]map[string]bool, bool, error) {
	dst := filepath.Join(o.Tmp, o.Format+"-"+repo+"-"+c[:10])
	os.RemoveAll(dst)
	defer os.RemoveAll(dst)
	if err := lockdiffMaterialize(ctx, repoDir, c, lockPath, dst); err != nil {
		return nil, false, err
	}
	lockDir := path.Dir(lockPath)
	if o.Format == lockfile.FormatPnpm {
		lp := filepath.Join(dst, "pnpm-lock.yaml")
		if b, err := os.ReadFile(lp); err == nil {
			os.WriteFile(lp, stripPnpmEnvDocument(b), 0o644)
		}
		// pnpm 10 refuses manifests that require a newer node or pnpm; the engines do not change the lockfile's graph.
		filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "package.json" {
				return nil
			}
			b, _ := os.ReadFile(p)
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return nil
			}
			_, e1 := m["engines"]
			_, e2 := m["packageManager"]
			_, e3 := m["devEngines"]
			if e1 || e2 || e3 {
				delete(m, "engines")
				delete(m, "packageManager")
				delete(m, "devEngines")
				nb, _ := json.Marshal(m)
				os.WriteFile(p, nb, 0o644)
			}
			return nil
		})
		if b, err := os.ReadFile(lp); err == nil && len(o.PnpmConvert) > 0 && strings.HasPrefix(sniffLockVersion(lockfile.FormatPnpm, string(b)), "6.") {
			cargs := append(append([]string(nil), o.PnpmConvert[1:]...), "install", "--lockfile-only", "--frozen-lockfile", "--ignore-scripts")
			if out, errb, err := runTool(ctx, dst, o.Env, o.PnpmConvert[0], cargs...); err != nil {
				return nil, false, fmt.Errorf("pnpm 9 conversion of the 6.x lockfile: %v: %s", err, firstLine(strings.Join(strings.Fields(string(out)+" "+string(errb)), " ")))
			}
		}
		args := append(append([]string(nil), o.Pnpm[1:]...), "list", "-r", "--depth", "Infinity", "--json", "--lockfile-only")
		out, errb, err := runTool(ctx, dst, o.Env, o.Pnpm[0], args...)
		var projs []pnpmProject
		if jerr := json.Unmarshal(out, &projs); jerr != nil {
			msg := strings.Join(strings.Fields(string(out)+" "+string(errb)), " ")
			if strings.Contains(msg, "Invalid string length") || strings.Contains(msg, "heap out of memory") || strings.Contains(msg, "allocation failure") {
				// The JSON tree exceeds Node's string or heap limit: list each project's deduplicated package paths instead.
				return pnpmParseable(ctx, o, dst, lockDir, projects)
			}
			msg = firstLine(msg)
			unreadable := strings.Contains(msg, "LOCKFILE") || strings.Contains(msg, "lockfile") || strings.Contains(msg, "reading 'resolution'")
			return nil, unreadable, fmt.Errorf("pnpm list: %v: %s", err, msg)
		}
		res := map[string]map[string]bool{}
		for _, p := range projs {
			rel, err := filepath.Rel(dst, p.Path)
			if err != nil {
				continue
			}
			key := path.Join(lockDir, filepath.ToSlash(rel))
			set := map[string]bool{}
			for _, m := range []map[string]*pnpmDep{p.Dependencies, p.DevDependencies, p.OptionalDependencies} {
				for name, d := range m {
					walkPnpm(name, d, set, 0)
				}
			}
			res[key] = set
		}
		return res, false, nil
	}
	out, errb, err := runTool(ctx, dst, o.Env, "npm", "ls", "--all", "--json", "--long", "--package-lock-only")
	var top npmDep
	if jerr := json.Unmarshal(out, &top); jerr != nil || top.Dependencies == nil {
		msg := firstLine(string(errb))
		return nil, jerr != nil, fmt.Errorf("npm ls: %v %v: %s", err, jerr, msg)
	}
	// Workspace and file: projects by package name, from the materialized manifests.
	names := map[string]string{}
	local := map[string]bool{} // name@version of every manifest: a project, never a registry package
	filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "package.json" {
			return nil
		}
		b, _ := os.ReadFile(p)
		var pj struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &pj) == nil && pj.Name != "" {
			local[pj.Name+"@"+pj.Version] = true
			rel, _ := filepath.Rel(dst, filepath.Dir(p))
			if rel != "." { // the root is not a workspace entry; a name shared by several manifests is ambiguous
				if _, dup := names[pj.Name]; dup {
					names[pj.Name] = "\x00"
				} else {
					names[pj.Name] = filepath.ToSlash(rel)
				}
			}
		}
		return nil
	})
	full := map[string][]map[string]*npmDep{}
	var index func(name string, d *npmDep)
	index = func(name string, d *npmDep) {
		if d.Path != "" && len(d.Dependencies) > 0 {
			full[d.Path] = append(full[d.Path], d.Dependencies)
		}
		for n, c := range d.Dependencies {
			index(n, c)
		}
	}
	index("", &top)
	res := map[string]map[string]bool{}
	rootSet := map[string]bool{}
	for name, d := range top.Dependencies {
		if strings.HasPrefix(d.Resolved, "file:") {
			dir, ok := names[name]
			if !ok || dir == "\x00" {
				continue
			}
			set := map[string]bool{}
			for n, c := range d.Dependencies {
				walkNpm(n, c, set, full, map[string]bool{})
			}
			res[path.Join(lockDir, dir)] = set
			continue
		}
		walkNpm(name, d, rootSet, full, map[string]bool{})
	}
	res[lockDir] = rootSet
	for _, set := range res {
		for k := range set {
			if local[k] {
				delete(set, k)
			}
		}
	}
	return res, false, nil
}

// pnpmParseable runs pnpm list --parseable in every project directory (our graph's importers: the tool cannot tell
// which projects exist in this mode) and reads name@version from the virtual store paths
// (".pnpm/<name with + for />@<version>[_peers|(peers)]/node_modules/<name>").
func pnpmParseable(ctx context.Context, o LockdiffOpts, dst, lockDir string, projects []string) (map[string]map[string]bool, bool, error) {
	res := map[string]map[string]bool{}
	for _, p := range projects {
		rel := p
		if lockDir != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, lockDir), "/")
		}
		if rel == "" {
			rel = "."
		}
		args := append(append([]string(nil), o.Pnpm[1:]...), "list", "--depth", "Infinity", "--parseable", "--lockfile-only")
		out, errb, err := runTool(ctx, filepath.Join(dst, filepath.FromSlash(rel)), o.Env, o.Pnpm[0], args...)
		if err != nil {
			return nil, false, fmt.Errorf("pnpm list --parseable in %s: %v: %s", rel, err, firstLine(string(out)+" "+string(errb)))
		}
		set := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			line = filepath.ToSlash(strings.TrimSpace(line))
			dir, ok := strings.CutPrefix(line, ".pnpm/")
			if !ok {
				continue // a project path
			}
			dir, name, ok := strings.Cut(dir, "/node_modules/")
			if !ok {
				continue
			}
			v, ok := strings.CutPrefix(dir, strings.ReplaceAll(name, "/", "+")+"@")
			if !ok {
				continue
			}
			if reTruncated.MatchString(v) {
				// A long directory name is truncated and hashed: the version may be cut; check the name only.
				set[name+"@*"] = true
				continue
			}
			if i := strings.IndexAny(v, "_("); i >= 0 {
				v = v[:i]
			}
			set[name+"@"+v] = true
		}
		res[p] = set
	}
	return res, false, nil
}

var reTruncated = regexp.MustCompile(`_[0-9a-f]{32}$`)

type pnpmProject struct {
	Name                 string              `json:"name"`
	Path                 string              `json:"path"`
	Dependencies         map[string]*pnpmDep `json:"dependencies"`
	DevDependencies      map[string]*pnpmDep `json:"devDependencies"`
	OptionalDependencies map[string]*pnpmDep `json:"optionalDependencies"`
}

type pnpmDep struct {
	From                 string              `json:"from"`
	Version              string              `json:"version"`
	Dependencies         map[string]*pnpmDep `json:"dependencies"`
	OptionalDependencies map[string]*pnpmDep `json:"optionalDependencies"`
}

func walkPnpm(name string, d *pnpmDep, set map[string]bool, depth int) {
	if d == nil || depth > 200 {
		return
	}
	if !strings.HasPrefix(d.Version, "link:") && d.Version != "" {
		n := d.From
		if n == "" {
			n = name
		}
		set[n+"@"+d.Version] = true
	}
	for n, c := range d.Dependencies {
		walkPnpm(n, c, set, depth+1)
	}
	for n, c := range d.OptionalDependencies {
		walkPnpm(n, c, set, depth+1)
	}
}

type npmDep struct {
	Version      string             `json:"version"`
	Resolved     string             `json:"resolved"`
	Missing      bool               `json:"missing"`
	Path         string             `json:"path"`
	Dependencies map[string]*npmDep `json:"dependencies"`
}

// walkNpm adds a node of npm ls's tree and its subtree; a deduplicated node (printed without its subtree) is expanded
// from the full occurrence at the same node_modules path (npm ls --long prints paths).
func walkNpm(name string, d *npmDep, set map[string]bool, full map[string][]map[string]*npmDep, expanded map[string]bool) {
	if d == nil || d.Missing || d.Version == "" {
		return
	}
	if !strings.HasPrefix(d.Resolved, "file:") {
		set[name+"@"+d.Version] = true
	}
	if len(d.Dependencies) > 0 {
		for n, c := range d.Dependencies {
			walkNpm(n, c, set, full, expanded)
		}
		return
	}
	key := d.Path
	if key == "" || expanded[key] {
		return
	}
	expanded[key] = true
	for _, deps := range full[key] {
		for n, c := range deps {
			walkNpm(n, c, set, full, expanded)
		}
	}
}

// tomlKeys decodes a TOML lockfile into its top-level keys, with [metadata] keys flattened as "metadata.<key>"; nil
// when it does not decode.
func tomlKeys(data string) map[string]any {
	var m map[string]any
	if toml.Unmarshal([]byte(data), &m) != nil {
		return nil
	}
	if md, ok := m["metadata"].(map[string]any); ok {
		for k, v := range md {
			m["metadata."+k] = v
		}
	}
	return m
}

// lockdiffPy runs the Python package manager on the materialized tree of commit c (every pyproject.toml under the
// lockfile's directory and the lockfile) and returns each project's package set (normalized name@version), keyed by
// repo-relative project directory.
//
// uv: uv export --frozen --all-extras --all-groups for every workspace member (our graph's local packages; one that
// uv does not know as a member is left out). poetry: poetry export --all-groups --all-extras for the lockfile's project.
// Both read the lockfile only (no resolution, no install) and keep every marker (universal lockfiles).
func lockdiffPy(ctx context.Context, o LockdiffOpts, repo, repoDir, c, lockPath string, g *lockfile.Graph) (map[string]map[string]bool, bool, error) {
	dst := filepath.Join(o.Tmp, o.Format+"-"+repo+"-"+c[:10])
	os.RemoveAll(dst)
	defer os.RemoveAll(dst)
	base := path.Base(lockPath)
	err := lockdiffMaterializeKeep(ctx, repoDir, c, lockPath, dst, func(rel string) bool {
		return rel == base || path.Base(rel) == "pyproject.toml" || rel == "poetry.toml" || rel == "uv.toml"
	})
	if err != nil {
		return nil, false, err
	}
	lockDir := path.Dir(lockPath)
	res := map[string]map[string]bool{}
	if o.Format == lockfile.FormatPoetry {
		out, errb, err := runTool(ctx, dst, o.Env, o.Poetry, "export", "--all-groups", "--all-extras", "--without-hashes", "-f", "requirements.txt",
			"--no-interaction", "--no-ansi")
		if err != nil {
			msg := firstLine(strings.Join(strings.Fields(string(out)+" "+string(errb)), " "))
			return nil, strings.Contains(msg, "poetry.lock"), fmt.Errorf("poetry export: %v: %s", err, msg)
		}
		res[lockDir] = parseRequirements(string(out))
		return res, false, nil
	}
	dirs := make([]string, 0, len(g.Projects))
	for d := range g.Projects {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		n := g.Nodes[g.Projects[d]]
		if n == nil || n.Package == "" { // a synthetic virtual workspace root
			continue
		}
		export := func(sel ...string) ([]byte, []byte, error) {
			args := append([]string{"export", "--frozen", "--no-hashes", "--no-header", "--no-annotate", "--package", n.Package}, sel...)
			return runTool(ctx, dst, o.Env, o.Uv, args...)
		}
		out, errb, err := export("--all-extras", "--all-groups")
		if err != nil && strings.Contains(string(errb), "declared conflicts") {
			// Declared conflicts forbid exporting every extra with every group at once: the union of every group without
			// extras and of sets of extras without groups, split at each conflict uv names until every set exports.
			o1, e1, err1 := export("--all-groups")
			out, errb, err = o1, e1, err1
			var extras []string
			for k := range n.Deps {
				if e, ok := strings.CutPrefix(k, "optional-dependencies:"); ok {
					e, _, _ = strings.Cut(e, " ")
					if !slices.Contains(extras, e) {
						extras = append(extras, e)
					}
				}
			}
			sort.Strings(extras)
			queue, seen := [][]string{extras}, map[string]bool{}
			for len(queue) > 0 && err == nil {
				set := queue[0]
				queue = queue[1:]
				key := strings.Join(set, ",")
				if seen[key] {
					continue
				}
				seen[key] = true
				args := []string{"--no-default-groups"}
				for _, e := range set {
					args = append(args, "--extra", e)
				}
				o2, e2, err2 := export(args...)
				if m := reConflictExtras.FindStringSubmatch(string(e2)); err2 != nil && m != nil && (slices.Contains(set, m[1]) || slices.Contains(set, m[2])) {
					queue = append(queue, without(set, m[2]), without(set, m[1]))
					continue
				}
				out, errb, err = append(out, o2...), append(errb, e2...), err2
			}
		}
		if err != nil {
			msg := firstLine(strings.Join(strings.Fields(string(out)+" "+string(errb)), " "))
			if strings.Contains(msg, "not found in workspace") || strings.Contains(msg, "is not a workspace member") {
				continue // a path dependency, not a member
			}
			return nil, strings.Contains(msg, "uv.lock") || strings.Contains(msg, "lockfile"), fmt.Errorf("uv export --package %s: %v: %s", n.Package, err, msg)
		}
		res[d] = parseRequirements(string(out))
	}
	return res, false, nil
}

// reConflictExtras matches uv's refusal to export two extras declared as conflicting.
var reConflictExtras = regexp.MustCompile("Extras `([^`]+)` and `([^`]+)` are incompatible")

// without returns s without x.
func without(s []string, x string) []string {
	return slices.DeleteFunc(slices.Clone(s), func(e string) bool { return e == x })
}

// parseRequirements reads an exported requirements file: "name==version ; marker" gives name@version, "name @ url"
// gives name@* (normalized names); editable and local path lines (projects) and comments are skipped.
func parseRequirements(s string) map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, ".") || strings.HasPrefix(line, "/") {
			continue
		}
		req, _, _ := strings.Cut(line, ";")
		req = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(req), `\`))
		if name, ver, ok := strings.Cut(req, "=="); ok {
			if i := strings.IndexByte(name, '['); i >= 0 {
				name = name[:i]
			}
			set[lockfile.NormalizeName(strings.TrimSpace(name))+"@"+strings.TrimSpace(ver)] = true
			continue
		}
		if name, _, ok := strings.Cut(req, " @ "); ok {
			if i := strings.IndexByte(name, '['); i >= 0 {
				name = name[:i]
			}
			set[lockfile.NormalizeName(strings.TrimSpace(name))+"@*"] = true
		}
	}
	return set
}

type cargoMetaOut struct {
	Packages []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"packages"`
	WorkspaceMembers []string `json:"workspace_members"`
	Resolve          struct {
		Nodes []struct {
			ID   string `json:"id"`
			Deps []struct {
				Pkg string `json:"pkg"`
			} `json:"deps"`
		} `json:"nodes"`
	} `json:"resolve"`
}

// lockdiffCargo checks out commit c into the worktree and returns every workspace member's package set (by crate name).
func lockdiffCargo(ctx context.Context, o LockdiffOpts, repoDir, wt, c, lockDir string) (map[string]map[string]bool, bool, error) {
	if _, err := git(ctx, wt, "checkout", "-q", "--detach", "--force", c); err != nil {
		return nil, false, err
	}
	dir := filepath.Join(wt, filepath.FromSlash(lockDir))
	args := []string{"metadata", "--format-version", "1", "--locked", "--all-features"}
	out, errb, err := runTool(ctx, dir, o.Env, "cargo", append(args, "--offline")...)
	if err != nil {
		out, errb, err = runTool(ctx, dir, o.Env, "cargo", args...)
	}
	if err != nil {
		msg := string(errb)
		lines := strings.Split(strings.TrimSpace(msg), "\n")
		var keep []string
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "error") || strings.HasPrefix(strings.TrimSpace(l), "Caused by") || len(keep) > 0 && len(keep) < 4 {
				keep = append(keep, strings.TrimSpace(l))
			}
		}
		return nil, false, fmt.Errorf("cargo metadata: %v: %s", err, strings.Join(keep, " | "))
	}
	var m cargoMetaOut
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, false, err
	}
	pkg := map[string]string{}
	for _, p := range m.Packages {
		pkg[p.ID] = p.Name + "@" + p.Version
	}
	deps := map[string][]string{}
	for _, n := range m.Resolve.Nodes {
		for _, d := range n.Deps {
			deps[n.ID] = append(deps[n.ID], d.Pkg)
		}
	}
	members := map[string]bool{}
	for _, id := range m.WorkspaceMembers {
		members[id] = true
	}
	res := map[string]map[string]bool{}
	for _, id := range m.WorkspaceMembers {
		name, _, _ := strings.Cut(pkg[id], "@")
		set := map[string]bool{}
		seen := map[string]bool{id: true}
		queue := []string{id}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, d := range deps[cur] {
				if !seen[d] {
					seen[d] = true
					queue = append(queue, d)
					if !members[d] {
						set[pkg[d]] = true
					}
				}
			}
		}
		res[name] = set
	}
	return res, false, nil
}

// LockdiffSummary aggregates one format's results as markdown.
func LockdiffSummary(format string, all []LockdiffRepo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Lockfile differential check: %s\n\n", format)
	b.WriteString("Superset check: for every project the package manager reports, every (name, version) in its dependency tree must be in bgraph's lockfile closure.\n\n")
	b.WriteString("| repo | lockfile | commits | ok | unsupported | parse-error | tool-unreadable | tool-error | project checks | mismatching checks | tool-only projects | ours-only projects | pairs | Compare errors | median changed |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	type tot struct{ commits, ok, unsup, perr, tunread, terr, checks, mism, toolOnly, oursOnly, pairs, cerr int }
	var T tot
	var allFrac []float64
	versions := map[string]int{}
	for _, r := range all {
		var t tot
		var fr []float64
		for _, c := range r.Commits {
			t.commits++
			versions[c.LockVersion]++
			switch c.Status {
			case "ok":
				t.ok++
			case "unsupported":
				t.unsup++
			case "parse-error":
				t.perr++
			case "tool-unreadable":
				t.tunread++
			case "tool-error":
				t.terr++
			}
			t.checks += c.Checked
			t.mism += len(c.Mismatches)
			t.toolOnly += len(c.ToolOnly)
			t.oursOnly += len(c.OursOnly)
		}
		for _, p := range r.Pairs {
			t.pairs++
			if p.Err != "" {
				t.cerr++
			} else if p.Total > 0 {
				fr = append(fr, float64(p.Changed)/float64(p.Total))
			}
		}
		allFrac = append(allFrac, fr...)
		if r.Skipped != "" {
			fmt.Fprintf(&b, "| %s | %s | skipped: %s |||||||||||||\n", r.Repo, r.Lockfile, r.Skipped)
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s |\n", r.Repo, r.Lockfile, t.commits, t.ok, t.unsup, t.perr, t.tunread, t.terr, t.checks, t.mism, t.toolOnly, t.oursOnly, t.pairs, t.cerr, fmtMedian(fr))
		T.commits += t.commits
		T.ok += t.ok
		T.unsup += t.unsup
		T.perr += t.perr
		T.tunread += t.tunread
		T.terr += t.terr
		T.checks += t.checks
		T.mism += t.mism
		T.toolOnly += t.toolOnly
		T.oursOnly += t.oursOnly
		T.pairs += t.pairs
		T.cerr += t.cerr
	}
	fmt.Fprintf(&b, "| **total** | | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s |\n", T.commits, T.ok, T.unsup, T.perr, T.tunread, T.terr, T.checks, T.mism, T.toolOnly, T.oursOnly, T.pairs, T.cerr, fmtMedian(allFrac))
	vk := make([]string, 0, len(versions))
	for k := range versions {
		vk = append(vk, k)
	}
	sort.Strings(vk)
	b.WriteString("\nLockfile versions seen:")
	for _, k := range vk {
		fmt.Fprintf(&b, " %s×%d", k, versions[k])
	}
	b.WriteString("\n\n## Mismatches (tool package missing from our closure)\n\n")
	n := 0
	for _, r := range all {
		for _, c := range r.Commits {
			for _, m := range c.Mismatches {
				if n < 60 {
					fmt.Fprintf(&b, "- %s @ %.10s project %s: %d missing, e.g. %s\n", r.Repo, c.Commit, m.Project, m.Count, strings.Join(m.Missing[:min(5, len(m.Missing))], ", "))
				}
				n++
			}
		}
	}
	if n == 0 {
		b.WriteString("none\n")
	}
	b.WriteString("\n## Pair comparison against the tool\n\n")
	b.WriteString("For every pair compared without error, a project whose package set reported by the tool changed must be among the changed projects.\n\n")
	var checked, missed, kindOnly, patchPairs int
	reasons, formerReasons := map[string]int{}, map[string]int{}
	combos := map[string]int{}
	var l0Pairs, formerPairs int
	for _, r := range all {
		for _, p := range r.Pairs {
			checked += p.ToolChecked
			missed += len(p.Missed)
			kindOnly += len(p.KindOnly)
			if len(p.Patches) > 0 {
				patchPairs++
			}
			for _, k := range p.L0 {
				reasons[k]++
			}
			for _, k := range p.FormerL0 {
				formerReasons[k]++
			}
			if len(p.L0) > 0 {
				l0Pairs++
			}
			if len(p.FormerL0) > 0 {
				formerPairs++
			}
			if len(p.L0) > 0 {
				combos[strings.Join(p.L0, " + ")]++
			}
			if len(p.Missed) > 0 {
				fmt.Fprintf(&b, "- MISSED %s %.10s→%.10s: %s\n", r.Repo, p.Base, p.Head, strings.Join(p.Missed[:min(8, len(p.Missed))], ", "))
			}
		}
	}
	fmt.Fprintf(&b, "Project checks: %d; missed: %d; dependency-kind-only changes (not misses): %d. Pairs with a changed patch (compared, patched packages marked): %d.\n", checked, missed, kindOnly, patchPairs)
	b.WriteString("\n## Pairs at level L0 by reason\n\n")
	fmt.Fprintf(&b, "Pairs at L0: %d (if the keys compared through the graph were settings — pnpm overrides and patches, uv options, manifest, requires-python and markers, poetry content-hash: %d). A pair counts under every reason it has.\n\n", l0Pairs, formerPairs)
	b.WriteString("| reason | pairs at L0 | before |\n|---|---|---|\n")
	keys := map[string]int{}
	for k := range formerReasons {
		keys[k] = 0
	}
	for k := range reasons {
		keys[k] = 0
	}
	for _, k := range sortedIntKeys(keys) {
		fmt.Fprintf(&b, "| %s | %d | %d |\n", k, reasons[k], formerReasons[k])
	}
	if len(combos) > 0 {
		b.WriteString("\n| reasons of a pair at L0 | pairs |\n|---|---|\n")
		for _, k := range sortedIntKeys(combos) {
			fmt.Fprintf(&b, "| %s | %d |\n", k, combos[k])
		}
	}
	b.WriteString("\n## Errors and project-set differences\n\n")
	for _, r := range all {
		for _, c := range r.Commits {
			if c.Status != "ok" {
				fmt.Fprintf(&b, "- %s @ %.10s v%s %s: %s\n", r.Repo, c.Commit, c.LockVersion, c.Status, firstLine(c.Err))
			}
			if len(c.ToolOnly) > 0 {
				fmt.Fprintf(&b, "- %s @ %.10s tool-only projects: %s\n", r.Repo, c.Commit, strings.Join(c.ToolOnly[:min(8, len(c.ToolOnly))], ", "))
			}
			if len(c.OursOnly) > 0 {
				fmt.Fprintf(&b, "- %s @ %.10s ours-only projects: %s\n", r.Repo, c.Commit, strings.Join(c.OursOnly[:min(8, len(c.OursOnly))], ", "))
			}
		}
		for _, p := range r.Pairs {
			if p.Err != "" {
				fmt.Fprintf(&b, "- %s %.10s→%.10s Compare: %s\n", r.Repo, p.Base, p.Head, p.Err)
			}
		}
	}
	return b.String()
}

func sortedIntKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fmtMedian(f []float64) string {
	if len(f) == 0 {
		return "-"
	}
	s := append([]float64(nil), f...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return fmt.Sprintf("%.2f", m)
}
