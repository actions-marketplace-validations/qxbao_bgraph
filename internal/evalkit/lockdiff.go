package evalkit

// Lockfile differential check (gate L1 of docs/plan.md §6.8): for every sampled lockfile commit, the dependency closure
// that internal/lockfile computes for each project must be a superset of the package set the package manager itself
// reports for that project (pnpm list, npm ls, cargo metadata).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/lockfile"
)

// LockdiffOpts configures one format's run.
type LockdiffOpts struct {
	Format  string // pnpm | npm | cargo
	Work    string // work dir holding repos/<owner__name>
	Tmp     string // scratch dir for materialized trees and worktrees (removed after use)
	Commits int    // lockfile-changing commits per repo
	Until   string // only commits before this date (git log --until), to sample older lockfile versions
	Pnpm    []string
	// PnpmConvert: a pnpm 9 command that rewrites a 6.x lockfile as 9.0 (install --lockfile-only --frozen-lockfile)
	// before Pnpm lists it: pnpm list --lockfile-only exists only in pnpm 10, which cannot read 6.x.
	PnpmConvert []string
	Env     []string // extra environment for the tool
	Log     func(string, ...any)
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
	Base    string `json:"base"`
	Head    string `json:"head"`
	Err     string `json:"err,omitempty"`
	Changed int    `json:"changed"`
	Total   int    `json:"total"`
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
			prev = nil
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
			prev = nil
			continue
		}
		if prev != nil {
			pr := LockdiffPair{Base: prevC, Head: c}
			if d, err := lockfile.Compare(prev, g); err != nil {
				pr.Err = err.Error()
			} else {
				all := map[string]bool{}
				for p := range prev.Projects {
					all[p] = true
				}
				for p := range g.Projects {
					all[p] = true
				}
				pr.Changed, pr.Total = len(d.Changed), len(all)
			}
			res.Pairs = append(res.Pairs, pr)
		}
		prev, prevC = g, c
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
		}
		if terr != nil {
			cr.Status, cr.Err = "tool-error", terr.Error()
			if unreadable {
				cr.Status = "tool-unreadable"
			}
			cr.Seconds = time.Since(start).Seconds()
			res.Commits = append(res.Commits, cr)
			o.Log("%s %s %.10s %s: %s", o.Format, repo, c, cr.Status, firstLine(cr.Err))
			continue
		}
		lockdiffCheck(o.Format, g, tool, &cr)
		cr.Status = "ok"
		cr.Seconds = time.Since(start).Seconds()
		res.Commits = append(res.Commits, cr)
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
	}
	if m == nil {
		return "?"
	}
	return m[1]
}

// lockdiffCheck fills the superset comparison of one commit. tool maps a project key to its package set (name@version).
func lockdiffCheck(format string, g *lockfile.Graph, tool map[string]map[string]bool, cr *LockdiffCommit) {
	cr.OurProjects, cr.ToolProjects = len(g.Projects), len(tool)
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
		id, ok := g.Projects[ourKey]
		if !ok {
			cr.ToolOnly = append(cr.ToolOnly, p)
			continue
		}
		matched[ourKey] = true
		cr.Checked++
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
}

// lockdiffMaterialize writes the files of commit c the JS package manager reads (lockfile, manifests, workspace and
// npm settings, patches) under dst.
func lockdiffMaterialize(ctx context.Context, repoDir, c, lockPath, dst string) error {
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
		if strings.Contains("/"+rel, "/node_modules/") {
			continue
		}
		b := path.Base(rel)
		keep := b == "package.json" || rel == path.Base(lockPath) || rel == "pnpm-workspace.yaml" || rel == ".npmrc" ||
			(strings.HasPrefix(rel, "patches/") || strings.Contains(rel, "/patches/")) && strings.HasSuffix(rel, ".patch")
		if keep {
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
