package evalkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/model"
)

// ReplayConfig configures replay.
type ReplayConfig struct {
	Work      string
	Out       string
	Repos     []string // "owner/name" or https URL
	Commits   int
	Depth     int // shallow clone depth; 0 → 3*Commits+20
	ColdEvery int // every ColdEvery commits, check determinism with a fresh Index without the durable cache (0 → 25)
	Jobs      int
	Manifest  Manifest
	Timeout   time.Duration
	Log       func(format string, a ...any)
}

// Violation is one invariant violation.
type Violation struct {
	Inv    string `json:"invariant"` // I1 | I2 | I3
	Commit string `json:"commit"`
	Detail string `json:"detail"`
}

// CommitResult is the result for one commit.
type CommitResult struct {
	Commit    string           `json:"commit"`
	Parent    string           `json:"parent"`
	Subject   string           `json:"subject,omitempty"`
	Class     string           `json:"class"` // docs_only | config | ordinary
	Files     int              `json:"files"`
	Scope     string           `json:"scope"`
	Widenings map[string]int   `json:"widenings,omitempty"`
	WideList  []model.Widening `json:"widening_list,omitempty"`
	Selected  int              `json:"selected"`
	Units     int              `json:"units"`
	Ms        int64            `json:"ms"`
	Err       string           `json:"error,omitempty"`
}

// RepoReplay is the result for one repo.
type RepoReplay struct {
	Repo       string         `json:"repo"`
	Role       string         `json:"role"`
	Head       string         `json:"head"`
	Commits    []CommitResult `json:"commits"`
	Violations []Violation    `json:"violations"`
	Error      string         `json:"error,omitempty"`
	ColdChecks int            `json:"cold_determinism_checks"`
	FirstMs    int64          `json:"first_select_ms"`
}

func urlOf(repo string) string {
	if strings.HasPrefix(repo, "http") {
		return repo
	}
	return "https://github.com/" + repo + ".git"
}

func nameOf(repo string) string {
	r := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repo, "https://github.com/"), "http://github.com/"), ".git")
	return r
}

// RunReplay replays every repo (in parallel across repos).
func RunReplay(ctx context.Context, cfg ReplayConfig) ([]RepoReplay, error) {
	if cfg.Jobs <= 0 {
		cfg.Jobs = 4
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Minute
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	res := make([]RepoReplay, len(cfg.Repos))
	sem := make(chan struct{}, cfg.Jobs)
	var wg sync.WaitGroup
	for i, repo := range cfg.Repos {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
			rr := replayRepo(rctx, &cfg, repo)
			res[i] = rr
		}()
	}
	wg.Wait()
	return res, ctx.Err()
}

func (c *ReplayConfig) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

func replayRepo(ctx context.Context, cfg *ReplayConfig, repo string) (rr RepoReplay) {
	rr.Repo = nameOf(repo)
	rr.Role, _ = cfg.Manifest.Role(rr.Repo)
	defer func() {
		if p := recover(); p != nil {
			rr.Error = fmt.Sprint("panic: ", p)
		}
	}()
	dir := filepath.Join(cfg.Work, "replay", repoDirName(rr.Repo))
	depth := cfg.Depth
	if depth == 0 {
		depth = 3*cfg.Commits + 20
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		os.MkdirAll(filepath.Dir(dir), 0o755)
		os.RemoveAll(dir)
		if _, err := git(ctx, "", "clone", "--no-checkout", "--single-branch", fmt.Sprintf("--depth=%d", depth), urlOf(repo), dir); err != nil {
			rr.Error = err.Error()
			return
		}
	}
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		rr.Error = err.Error()
		return
	}
	rr.Head = strings.TrimSpace(head)
	out, err := git(ctx, dir, "rev-list", "--first-parent", "--parents", "HEAD")
	if err != nil {
		rr.Error = err.Error()
		return
	}
	type cp struct{ c, p string }
	var list []cp
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 { // root or merge
			continue
		}
		if _, err := git(ctx, dir, "cat-file", "-e", f[1]+"^{commit}"); err != nil {
			continue
		}
		list = append(list, cp{f[0], f[1]})
		if len(list) >= cfg.Commits {
			break
		}
	}
	state := filepath.Join(cfg.Work, "state", repoDirName(rr.Repo)+"__replay")
	ix, err := bgraph.Open(ctx, dir, bgraph.Options{StateDir: state})
	if err != nil {
		rr.Error = err.Error()
		return
	}
	defer ix.Close()
	coldEvery := cfg.ColdEvery
	if coldEvery == 0 {
		coldEvery = 25
	}
	vio := func(inv, c, d string) {
		rr.Violations = append(rr.Violations, Violation{inv, c, d})
		cfg.logf("%s %s VIOLATION %s %s", rr.Repo, c[:10], inv, d)
	}
	// Run from oldest to newest so the cache is warm as in real use.
	for k := len(list) - 1; k >= 0; k-- {
		if ctx.Err() != nil {
			rr.Error = "ctx: " + ctx.Err().Error()
			break
		}
		c, p := list[k].c, list[k].p
		cr := CommitResult{Commit: c, Parent: p}
		subj, _ := git(ctx, dir, "show", "-s", "--format=%s", c)
		cr.Subject = strings.TrimSpace(subj)
		ns, err := git(ctx, dir, "diff-tree", "-r", "--no-renames", "-z", "--name-status", p, c)
		if err != nil {
			cr.Err = err.Error()
			rr.Commits = append(rr.Commits, cr)
			continue
		}
		type fc struct{ st, path string }
		var changed []fc
		parts := strings.Split(ns, "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			changed = append(changed, fc{parts[i], parts[i+1]})
		}
		var paths []string
		for _, f := range changed {
			paths = append(paths, f.path)
		}
		cr.Files = len(paths)
		cr.Class = ClassifyCommit(paths)

		t0 := time.Now()
		sel, err := ix.Select(ctx, bgraph.Change{Base: p, Head: c})
		cr.Ms = time.Since(t0).Milliseconds()
		if k == len(list)-1 {
			rr.FirstMs = cr.Ms
		}
		if err != nil {
			cr.Err = err.Error()
			rr.Commits = append(rr.Commits, cr)
			continue
		}
		cr.Scope = string(sel.Scope)
		cr.Widenings = map[string]int{}
		for _, w := range sel.Widening {
			cr.Widenings[string(w.Tier)+":"+w.Reason]++
		}
		cr.WideList = sel.Widening
		if len(cr.WideList) > 8 {
			cr.WideList = cr.WideList[:8]
		}
		cr.Selected, cr.Units = len(sel.Tests), sel.IndexStats["units"]

		// I1: two identical revisions → empty.
		s1, err := ix.Select(ctx, bgraph.Change{Base: c, Head: c})
		switch {
		case err != nil:
			vio("I1", c, "error: "+err.Error())
		case s1.Scope != model.TierNone || len(s1.Tests) > 0 || len(s1.Widening) > 0 || len(s1.Changed) > 0:
			vio("I1", c, fmt.Sprintf("Select(C,C): scope=%s tests=%d widenings=%d changed=%d %v", s1.Scope, len(s1.Tests), len(s1.Widening), len(s1.Changed), s1.Widening))
		}
		// I3: deterministic (same Index, warm cache).
		s2, err := ix.Select(ctx, bgraph.Change{Base: p, Head: c})
		if err != nil || !reflect.DeepEqual(canon(sel), canon(s2)) {
			vio("I3", c, fmt.Sprintf("two Select calls differ (err=%v)", err))
		}
		// Strong I3: a fresh in-memory-only Index (no durable cache) must give the same result.
		if k%coldEvery == 0 {
			rr.ColdChecks++
			if cix, err := bgraph.Open(ctx, dir, bgraph.Options{MemoryOnly: true}); err == nil {
				s3, err := cix.Select(ctx, bgraph.Change{Base: p, Head: c})
				cix.Close()
				if err != nil || !reflect.DeepEqual(canon(sel), canon(s3)) {
					vio("I3", c, fmt.Sprintf("cold Index (MemoryOnly) differs from warm Index (err=%v) scope %s vs %s, tests %d vs %d", err, sel.Scope, s3.Scope, len(sel.Tests), len(s3.Tests)))
				}
			}
		}
		// I2: added/modified test files must be selected (unless full).
		if sel.Scope != model.TierFull {
			have := map[string]bool{}
			for _, st := range sel.Tests {
				have[st.Unit] = true
				for _, f := range st.Files {
					have[f] = true
				}
			}
			for _, f := range changed {
				if (f.st == "A" || f.st == "M") && IsTestPath(f.path) && !have[f.path] {
					vio("I2", c, fmt.Sprintf("test file %s (%s) not selected; scope=%s widenings=%v", f.path, f.st, sel.Scope, cr.Widenings))
				}
			}
		}
		rr.Commits = append(rr.Commits, cr)
		cfg.logf("%s %s %-8s %-6s sel=%d/%d %dms", rr.Repo, c[:10], cr.Class, cr.Scope, cr.Selected, cr.Units, cr.Ms)
	}
	return
}

var codeExt = map[string]bool{".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true}

// IsTestPath: the file matches common test patterns (*.test.*, *.spec.*, __tests__/) and is JS/TS source; excludes fixtures/snapshots/mocks.
func IsTestPath(p string) bool {
	if !codeExt[strings.ToLower(path.Ext(p))] {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "__snapshots__", "__fixtures__", "__mocks__", "fixtures", "fixture", "__mock__":
			return false
		}
	}
	b := path.Base(p)
	if strings.Contains(b, ".test.") || strings.Contains(b, ".spec.") {
		return true
	}
	return strings.Contains("/"+p, "/__tests__/")
}

// ClassifyCommit classifies independently of the tool: docs_only, config, ordinary.
func ClassifyCommit(paths []string) string {
	if len(paths) == 0 {
		return "docs_only"
	}
	docs := true
	cfg := false
	for _, p := range paths {
		if !isDoc(p) {
			docs = false
		}
		if isConfig(p) {
			cfg = true
		}
	}
	if docs {
		return "docs_only"
	}
	if cfg {
		return "config"
	}
	return "ordinary"
}

func isDoc(p string) bool {
	l := strings.ToLower(p)
	b := path.Base(l)
	switch path.Ext(l) {
	case ".md", ".mdx", ".rst":
		return true
	}
	if strings.HasPrefix(l, "docs/") || strings.HasPrefix(l, "doc/") {
		return true
	}
	for _, pre := range []string{"license", "changelog", "authors", "contributing", "code_of_conduct", "security"} {
		if strings.HasPrefix(b, pre) {
			return true
		}
	}
	return false
}

var lockNames = map[string]bool{"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"bun.lock": true, "bun.lockb": true, "pnpm-workspace.yaml": true, "package.json": true, ".nvmrc": true, ".node-version": true,
	".npmrc": true, ".yarnrc": true, ".yarnrc.yml": true, ".tool-versions": true, ".browserslistrc": true, ".babelrc": true,
	".editorconfig": true, "lerna.json": true, "nx.json": true, "turbo.json": true, ".gitattributes": true, ".npmignore": true}

func isConfig(p string) bool {
	l := strings.ToLower(p)
	b := path.Base(l)
	if lockNames[b] || strings.HasPrefix(l, ".github/") || strings.HasPrefix(b, "dockerfile") || strings.HasPrefix(b, "tsconfig") ||
		strings.HasPrefix(b, "jsconfig") || strings.Contains(b, ".config.") || strings.HasPrefix(b, ".eslintrc") ||
		strings.HasPrefix(b, ".prettierrc") || strings.HasPrefix(b, ".babelrc") || strings.HasPrefix(b, "docker-compose") {
		return true
	}
	return false
}

// ---- Summary ----

// ReplaySummary aggregates one repo.
type ReplaySummary struct {
	Repo        string         `json:"repo"`
	Role        string         `json:"role"`
	Commits     int            `json:"commits"`
	Docs        int            `json:"docs_only"`
	Config      int            `json:"config"`
	Ordinary    int            `json:"ordinary"`
	OrdFull     int            `json:"ordinary_full"`
	FullRate    float64        `json:"ordinary_full_rate"`
	FullReasons map[string]int `json:"ordinary_full_reasons"`
	CfgFull     int            `json:"config_full"`
	NonFullN    int            `json:"ordinary_non_full"`
	MedRatio    float64        `json:"median_selected_over_units"`
	P90Ratio    float64        `json:"p90_selected_over_units"`
	MedMs       float64        `json:"median_ms"`
	P95Ms       float64        `json:"p95_ms"`
	FirstMs     int64          `json:"first_select_ms"`
	Errors      int            `json:"errors"`
	Viol        map[string]int `json:"violations"`
}

// SummarizeReplay computes the summary per repo and per role group ("" = all; Repo="ALL").
func SummarizeReplay(rrs []RepoReplay, role string) ReplaySummary {
	s := ReplaySummary{Repo: "ALL", Role: role, FullReasons: map[string]int{}, Viol: map[string]int{}}
	if role == "" {
		s.Role = "all"
	}
	var rs []RepoReplay
	for _, r := range rrs {
		if role == "" || r.Role == role {
			rs = append(rs, r)
		}
	}
	return foldReplay(s, rs)
}

func foldReplay(s ReplaySummary, rs []RepoReplay) ReplaySummary {
	var ratios, ms []float64
	for _, r := range rs {
		for _, v := range r.Violations {
			s.Viol[v.Inv]++
		}
		for _, c := range r.Commits {
			if c.Err != "" {
				s.Errors++
				continue
			}
			s.Commits++
			ms = append(ms, float64(c.Ms))
			switch c.Class {
			case "docs_only":
				s.Docs++
			case "config":
				s.Config++
				if c.Scope == "full" {
					s.CfgFull++
				}
			default:
				s.Ordinary++
				if c.Scope == "full" {
					s.OrdFull++
					for k := range c.Widenings {
						if strings.HasPrefix(k, "full:") {
							s.FullReasons[strings.TrimPrefix(k, "full:")]++
						}
					}
				} else {
					s.NonFullN++
					if c.Units > 0 {
						ratios = append(ratios, float64(c.Selected)/float64(c.Units))
					}
				}
			}
		}
		if len(rs) == 1 {
			s.FirstMs = r.FirstMs
		}
	}
	if s.Ordinary > 0 {
		s.FullRate = float64(s.OrdFull) / float64(s.Ordinary)
	}
	s.MedRatio, s.P90Ratio = Percentile(ratios, 50), Percentile(ratios, 90)
	s.MedMs, s.P95Ms = Percentile(ms, 50), Percentile(ms, 95)
	return s
}

// WriteReplay writes replay.json and replay.md.
func WriteReplay(dir string, rrs []RepoReplay) error {
	var per []ReplaySummary
	for _, r := range rrs {
		s := ReplaySummary{Repo: r.Repo, Role: r.Role, FullReasons: map[string]int{}, Viol: map[string]int{}}
		per = append(per, foldReplay(s, []RepoReplay{r}))
	}
	groups := []ReplaySummary{SummarizeReplay(rrs, "dev"), SummarizeReplay(rrs, "holdout"), SummarizeReplay(rrs, "")}
	b, _ := json.MarshalIndent(map[string]any{"per_repo": per, "groups": groups, "repos": rrs}, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "replay.json"), b, 0o644); err != nil {
		return err
	}
	var md strings.Builder
	md.WriteString("# Commit history replay\n\n")
	md.WriteString("| repo | role | commits | docs | config | ordinary | full% (ordinary) | full reasons | median sel/units | p90 | ms median | ms p95 | errors | violations |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	row := func(s ReplaySummary) {
		fmt.Fprintf(&md, "| %s | %s | %d | %d | %d | %d | %.0f%% (%d/%d) | %s | %.3f | %.3f | %.0f | %.0f | %d | %s |\n", s.Repo, s.Role, s.Commits, s.Docs, s.Config,
			s.Ordinary, s.FullRate*100, s.OrdFull, s.Ordinary, mapStr(s.FullReasons), s.MedRatio, s.P90Ratio, s.MedMs, s.P95Ms, s.Errors, mapStr(s.Viol))
	}
	for _, s := range per {
		row(s)
	}
	for _, s := range groups {
		row(s)
	}
	md.WriteString("\n## Invariant violations\n\n")
	n := 0
	for _, r := range rrs {
		if r.Error != "" {
			fmt.Fprintf(&md, "- ERROR repo %s: %s\n", r.Repo, r.Error)
		}
		for _, v := range r.Violations {
			n++
			fmt.Fprintf(&md, "- %s %s `%s`: %s\n", r.Repo, v.Inv, v.Commit[:10], v.Detail)
		}
	}
	if n == 0 {
		md.WriteString("(none)\n")
	}
	sort.Strings(nil)
	return os.WriteFile(filepath.Join(dir, "replay.md"), []byte(md.String()), 0o644)
}

// Resummarize reads replay.json, reassigns roles from the manifest and rewrites the report (re-runs nothing).
func Resummarize(dir string, m Manifest) error {
	b, err := os.ReadFile(filepath.Join(dir, "replay.json"))
	if err != nil {
		return err
	}
	var doc struct {
		Repos []RepoReplay `json:"repos"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	for i := range doc.Repos {
		doc.Repos[i].Role, _ = m.Role(doc.Repos[i].Repo)
	}
	return WriteReplay(dir, doc.Repos)
}
