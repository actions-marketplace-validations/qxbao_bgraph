package evalkit

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// JVMTools locates the portable JDKs, Maven and the Gradle user home used by the external checks. Everything lives on the eval drive:
// the Maven local repository, the Gradle home and the temp directory of every child process. Nothing touches PATH or the registry.
type JVMTools struct {
	JDK17, JDK21 string // JAVA_HOME directories
	Maven        string // Maven home
	M2           string // -Dmaven.repo.local
	GradleHome   string // GRADLE_USER_HOME
	Tmp          string // TEMP/TMP of the child processes
}

func (t JVMTools) env(jdk string) []string {
	return append(os.Environ(), "JAVA_HOME="+jdk, "GRADLE_USER_HOME="+t.GradleHome, "TEMP="+t.Tmp, "TMP="+t.Tmp, "TMPDIR="+t.Tmp,
		"MAVEN_OPTS=-Djava.io.tmpdir="+t.Tmp, "GRADLE_OPTS=-Djava.io.tmpdir="+t.Tmp, "JAVA_TOOL_OPTIONS=-Djava.io.tmpdir="+t.Tmp,
		"MAVEN_ARGS=", "CI=true")
}

func (t JVMTools) run(ctx context.Context, jdk, dir, exe string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = t.env(jdk)
	var o bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &o
	err := cmd.Run()
	return o.String(), err
}

func (t JVMTools) mvn(ctx context.Context, jdk, dir string, args ...string) (string, error) {
	base := []string{"-B", "-ntp", "-Dmaven.repo.local=" + t.M2}
	return t.run(ctx, jdk, dir, filepath.Join(t.Maven, "bin", "mvn.cmd"), append(base, args...)...)
}

// exportTree extracts the tree of rev into dir (git archive; works on a partial clone: missing blobs are fetched). Symlinks are skipped.
func exportTree(ctx context.Context, mirror, rev, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// One batched blob fetch instead of git archive fetching the missing blobs one by one.
	if gt, err := newGitTreeWant(ctx, mirror, rev, func(string) bool { return true }); err == nil {
		gt.Close()
	}
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", rev)
	cmd.Dir = mirror
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	tr := tar.NewReader(out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		f, err := os.Create(dst)
		if err != nil {
			continue // path too long or reserved name: the build does not need it
		}
		io.Copy(f, tr)
		f.Close()
	}
	io.Copy(io.Discard, out) // the tar padding after the end marker: git blocks on a full pipe otherwise
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %v: %s", rev, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// initStandaloneRepo makes dir a stand-alone git repository whose objects come from the mirror (alternates): JGit, and so GIB, does not
// support the linked worktrees of git. HEAD is rev; refs/heads/gib-ref is ref.
func initStandaloneRepo(ctx context.Context, mirror, dir, rev, ref string) error {
	for _, a := range [][]string{{"init", "-q"}, {"config", "diff.renames", "false"}, {"config", "core.autocrlf", "false"}} {
		if _, err := git(ctx, dir, a...); err != nil {
			return err
		}
	}
	alt := filepath.Join(dir, ".git", "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.ToSlash(filepath.Join(mirror, ".git", "objects"))+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := git(ctx, dir, "update-ref", "refs/heads/gib-ref", ref); err != nil {
		return err
	}
	_, err := git(ctx, dir, "update-ref", "HEAD", rev)
	return err
}

// ---- GIB superset check ----

const gibVersion = "4.5.4"

// JVMGibRow is the outcome of the GIB comparison for one task.
type JVMGibRow struct {
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
	GIB      int      `json:"gib_impacted"`
	GIBList  []string `json:"gib_modules,omitempty"` // the impacted modules, kept for violations
	GIBOut   string   `json:"gib_log,omitempty"`     // the GIB lines of the Maven output, kept for violations
	Missing  []string `json:"missing,omitempty"` // impacted by GIB, not selected by bgraph
	Extra    int      `json:"extra"`             // selected by bgraph, not impacted by GIB
	Changed  []string `json:"changed,omitempty"`
	ToolMs   int64    `json:"tool_ms,omitempty"`
}

// JVMGibConfig configures JVMGibTask.
type JVMGibConfig struct {
	Work    string
	Tools   JVMTools
	Timeout time.Duration
}

const gibExtension = `<extension>
    <groupId>io.github.gitflow-incremental-builder</groupId>
    <artifactId>gitflow-incremental-builder</artifactId>
    <version>` + gibVersion + `</version>
  </extension>`

// writeGibExtension adds GIB to .mvn/extensions.xml of the temporary copy (creating or extending the file).
func writeGibExtension(dir string) error {
	p := filepath.Join(dir, ".mvn", "extensions.xml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "</extensions>") {
		return os.WriteFile(p, []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<extensions>\n  "+gibExtension+"\n</extensions>\n"), 0o644)
	}
	s := strings.Replace(string(b), "</extensions>", "  "+gibExtension+"\n</extensions>", 1)
	return os.WriteFile(p, []byte(s), 0o644)
}

// JVMGibTask runs bgraph and GIB on P -> C of one task and checks GIB's impacted modules (changed + downstream) are a subset of bgraph's
// selection. Only Maven builds with a root pom.xml at C are compared.
func JVMGibTask(ctx context.Context, cfg JVMGibConfig, mirror string, seq int, t Task, role string) JVMGibRow {
	row := JVMGibRow{ID: t.InstanceID, Repo: t.Repo, Role: role}
	skip := func(why string, err error) JVMGibRow {
		row.Status = "skipped:" + why
		if err != nil {
			row.Detail = firstN(err.Error(), 300)
		}
		return row
	}
	idx := filepath.Join(cfg.Work, "idx", fmt.Sprintf("gib_%s_%d.index", repoDirName(t.Repo), seq))
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
	if _, err := git(ctx, mirror, "cat-file", "-e", c+":pom.xml"); err != nil {
		return skip("no_root_pom", nil)
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
	dir := filepath.Join(cfg.Work, "wt", fmt.Sprintf("gib_%s_%d", repoDirName(t.Repo), seq))
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	if err := exportTree(ctx, mirror, c, dir); err != nil {
		return skip("checkout", err)
	}
	if err := initStandaloneRepo(ctx, mirror, dir, c, p); err != nil {
		return skip("repo", err)
	}
	if err := writeGibExtension(dir); err != nil {
		return skip("extension", err)
	}
	log := filepath.Join(cfg.Tools.Tmp, fmt.Sprintf("gib_%s_%d.txt", repoDirName(t.Repo), seq))
	os.Remove(log)
	defer os.Remove(log)
	t0 := time.Now()
	tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	out, err := cfg.Tools.mvn(tctx, cfg.Tools.JDK17, dir, "-Dgib.referenceBranch=refs/heads/gib-ref", "-Dgib.compareToMergeBase=false",
		"-Dgib.uncommitted=false", "-Dgib.untracked=false", "-Dgib.buildUpstream=never", "-Dgib.logImpactedTo="+log,
		// A goal without a lifecycle phase: lifecycle plugins (checkstyle, enforcer...) need built reactor artifacts and are not needed, GIB acts when the projects are read.
		"org.apache.maven.plugins:maven-help-plugin:3.4.0:evaluate", "-Dexpression=project.artifactId", "-DforceStdout")
	row.ToolMs = time.Since(t0).Milliseconds()
	if err != nil {
		row.Status, row.Detail = "tool_failed", mvnError(out)
		return row
	}
	if !strings.Contains(out, "gitflow-incremental-builder "+gibVersion+" starting") || strings.Contains(out, "execution skipped") {
		row.Status, row.Detail = "tool_failed", "GIB did not run: "+mvnError(out)
		return row
	}
	impacted := map[string]bool{}
	if b, err := os.ReadFile(log); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			ln = strings.TrimSpace(strings.ReplaceAll(ln, "\\", "/"))
			if ln == "" {
				continue
			}
			if ln == "." {
				impacted[jvm.UnitID(jvm.RunnerMaven, ".", "")] = true
			} else {
				impacted[jvm.UnitID(jvm.RunnerMaven, path.Clean(ln), "")] = true
			}
		}
	}
	row.GIB = len(impacted)
	for u := range impacted {
		if !selected[u] {
			row.Missing = append(row.Missing, u)
		}
	}
	for u := range selected {
		if strings.HasPrefix(u, "maven:") && !impacted[u] {
			row.Extra++
		}
	}
	sort.Strings(row.Missing)
	row.Status = "ok"
	if len(row.Missing) > 0 {
		row.Status = "violation"
		for u := range impacted {
			row.GIBList = append(row.GIBList, u)
		}
		sort.Strings(row.GIBList)
		var gl []string
		for _, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, "gitflow-incremental-builder") || strings.Contains(ln, "GIB") || strings.HasPrefix(ln, "[INFO] - ") {
				gl = append(gl, ln)
			}
		}
		row.GIBOut = firstN(strings.Join(gl, "\n"), 6000)
	}
	return row
}

var mvnErrRe = regexp.MustCompile(`(?m)^\[ERROR\] (.+)$`)

// mvnError is the first Maven [ERROR] line(s) of a failed run.
func mvnError(out string) string {
	var ls []string
	for _, m := range mvnErrRe.FindAllStringSubmatch(out, 3) {
		ls = append(ls, strings.TrimSpace(m[1]))
	}
	if len(ls) == 0 {
		return firstN(strings.TrimSpace(tail(out, 300)), 300)
	}
	return firstN(strings.Join(ls, " | "), 500)
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- provider check ----

// JVMProviderRow is the provider comparison of one repo at the sampled base commits.
type JVMProviderRow struct {
	Repo    string   `json:"repo"`
	Tool    string   `json:"tool"` // maven | gradle
	Commits int      `json:"commits"`
	Builds  int      `json:"builds"` // commits for which the build tool ran
	Failed  int      `json:"failed"`
	Modules int      `json:"build_modules"` // modules/projects the build tool lists
	InUnits int      `json:"in_units"`      // ... that bgraph has as a unit
	Missing []string `json:"missing_modules,omitempty"`
	Extra   int      `json:"bgraph_extra_units"` // units bgraph has that the build tool does not list (informational)
	Edges   int      `json:"build_edges"`        // Maven: dependency edges between reactor modules
	Covered int      `json:"edges_covered"`
	MissEdg []string `json:"missing_edges,omitempty"`
	Errors  []string `json:"errors,omitempty"`
	// ReadOK: commits at which every build module is a bgraph unit and every build edge is a bgraph edge ("read correctly as text").
	ReadOK int   `json:"read_ok"`
	Ms     int64 `json:"elapsed_ms"`
}

// JVMProviderConfig configures JVMProviderRepo.
type JVMProviderConfig struct {
	Work    string
	Tools   JVMTools
	Timeout time.Duration
}

type effPom struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Parent     struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
	} `xml:"parent"`
	Dependencies struct {
		Dep []struct {
			GroupID    string `xml:"groupId"`
			ArtifactID string `xml:"artifactId"`
		} `xml:"dependency"`
	} `xml:"dependencies"`
}

// parseEffectivePoms reads the output of help:effective-pom (<projects><project>... for a reactor, a single <project> otherwise).
func parseEffectivePoms(b []byte) ([]effPom, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var out []effPom
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "project" {
			var p effPom
			if err := dec.DecodeElement(&p, &se); err != nil {
				return out, err
			}
			if p.GroupID == "" {
				p.GroupID = p.Parent.GroupID
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// JVMProviderRepo compares, at the given base commits, the modules of the build tool with bgraph's units. For Maven the reactor is the
// effective-pom of the root (every module, with its resolved dependencies) and the dependency edges between reactor modules must be
// bgraph edges; for Gradle the projects of `gradlew projects` must be units.
func JVMProviderRepo(ctx context.Context, cfg JVMProviderConfig, mirror, repo, tool string, bases []string) (row JVMProviderRow) {
	row = JVMProviderRow{Repo: repo, Tool: tool}
	t0 := time.Now()
	defer func() { row.Ms = time.Since(t0).Milliseconds() }()
	for _, base := range bases {
		row.Commits++
		gt, err := jvmGitTreeP(ctx, mirror, base)
		if err != nil {
			row.Errors = append(row.Errors, base[:10]+": "+err.Error())
			row.Failed++
			continue
		}
		a, err := jvm.New(nil).Analyze(ctx, gt, scancache.New())
		gt.Close()
		if err != nil {
			row.Errors = append(row.Errors, base[:10]+": analyze: "+err.Error())
			row.Failed++
			continue
		}
		units := map[string]string{} // unit ID -> package
		byGA := map[string]string{}  // maven GA or artifactId -> unit ID
		edges := map[string]bool{}
		for _, u := range a.Tests {
			if (tool == "maven") == strings.HasPrefix(u.ID, "maven:") {
				units[u.ID] = u.Package
				byGA[u.Package] = u.ID
				if i := strings.IndexByte(u.Package, ':'); i >= 0 {
					byGA["*:"+u.Package[i+1:]] = u.ID
				}
			}
		}
		for _, e := range a.Edges {
			from, ok1 := strings.CutPrefix(e.From, "<jvm:")
			to, ok2 := strings.CutPrefix(e.To, "<jvm:")
			if ok1 && ok2 {
				edges[strings.TrimSuffix(from, ">")+"->"+strings.TrimSuffix(to, ">")] = true
			}
		}
		dir := filepath.Join(cfg.Work, "wt", fmt.Sprintf("jvmprov_%s_%s", repoDirName(repo), base[:8]))
		os.RemoveAll(dir)
		if err := exportTree(ctx, mirror, base, dir); err != nil {
			os.RemoveAll(dir)
			row.Errors = append(row.Errors, base[:10]+": checkout: "+err.Error())
			row.Failed++
			continue
		}
		var mods []string // unit IDs the build tool lists
		var builtEdges []string
		var berr error
		tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		if tool == "maven" {
			mods, builtEdges, berr = mavenReactor(tctx, cfg.Tools, dir, byGA)
		} else {
			mods, berr = gradleProjects(tctx, cfg.Tools, dir)
		}
		cancel()
		os.RemoveAll(dir)
		if berr != nil {
			row.Failed++
			row.Errors = append(row.Errors, base[:10]+": "+firstN(berr.Error(), 400))
			continue
		}
		row.Builds++
		ok := true
		listed := map[string]bool{}
		for _, m := range mods {
			row.Modules++
			listed[m] = true
			if _, has := units[m]; has {
				row.InUnits++
			} else {
				ok = false
				row.Missing = append(row.Missing, base[:8]+":"+m)
			}
		}
		for u := range units {
			if !listed[u] {
				row.Extra++
			}
		}
		for _, e := range builtEdges {
			row.Edges++
			if edges[e] {
				row.Covered++
			} else {
				ok = false
				row.MissEdg = append(row.MissEdg, base[:8]+":"+e)
			}
		}
		if ok {
			row.ReadOK++
		}
	}
	return row
}

// mavenReactor returns the unit IDs of the modules of the reactor at dir and the dependency edges ("maven:a->maven:b") between them.
func mavenReactor(ctx context.Context, tl JVMTools, dir string, byGA map[string]string) ([]string, []string, error) {
	out := filepath.Join(tl.Tmp, "effpom_"+filepath.Base(dir)+".xml")
	os.Remove(out)
	defer os.Remove(out)
	o, err := tl.mvn(ctx, tl.JDK17, dir, "-q", "org.apache.maven.plugins:maven-help-plugin:3.4.0:effective-pom", "-Doutput="+out)
	if err != nil {
		return nil, nil, fmt.Errorf("effective-pom: %s", mvnError(o))
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, nil, err
	}
	poms, err := parseEffectivePoms(b)
	if len(poms) == 0 {
		return nil, nil, fmt.Errorf("effective-pom: no project (%v)", err)
	}
	// Reactor module key -> unit. The effective pom has no directory: match on groupId:artifactId, then on artifactId alone.
	unitOf := func(g, a string) string {
		if u, ok := byGA[g+":"+a]; ok {
			return u
		}
		return byGA["*:"+a]
	}
	var mods, edges []string
	seen := map[string]bool{}
	for _, p := range poms {
		u := unitOf(p.GroupID, p.ArtifactID)
		if u == "" {
			u = "maven:?" + p.GroupID + ":" + p.ArtifactID // listed by Maven, unknown to bgraph
		}
		mods = append(mods, u)
		for _, d := range p.Dependencies.Dep {
			if v := unitOf(d.GroupID, d.ArtifactID); v != "" && v != u && !seen[u+"->"+v] {
				seen[u+"->"+v] = true
				edges = append(edges, u+"->"+v)
			}
		}
	}
	return mods, edges, nil
}

var gradleProjRe = regexp.MustCompile(`(?m)^[|+\\ -]*Project '(:[^']*)'`)

// gradleProjects returns the unit IDs ("gradle::a:b") of the projects of the root build at dir (included builds are not listed).
func gradleProjects(ctx context.Context, tl JVMTools, dir string) ([]string, error) {
	wrapper := filepath.Join(dir, "gradlew.bat")
	exe := wrapper
	if _, err := os.Stat(wrapper); err != nil {
		return nil, fmt.Errorf("no gradle wrapper")
	}
	var out string
	var err error
	for _, jdk := range []string{tl.JDK17, tl.JDK21} {
		out, err = tl.run(ctx, jdk, dir, exe, "projects", "--console=plain", "--no-daemon", "-q", "--no-configuration-cache")
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gradlew projects: %s", firstN(strings.TrimSpace(gradleErr(out)), 300))
	}
	ids := []string{jvm.UnitID(jvm.RunnerGradle, ".", ":")}
	for _, m := range gradleProjRe.FindAllStringSubmatch(out, -1) {
		ids = append(ids, jvm.UnitID(jvm.RunnerGradle, ".", m[1]))
	}
	return ids, nil
}

var gradleErrRe = regexp.MustCompile(`(?s)\* What went wrong:\s*(.*?)\n\n`)

func gradleErr(out string) string {
	if m := gradleErrRe.FindStringSubmatch(out); m != nil {
		return strings.ReplaceAll(m[1], "\n", " ")
	}
	return tail(out, 300)
}

// JVMRootTools lists the build tools whose root build files exist at rev: "maven" (root pom.xml), "gradle" (root settings script and
// wrapper).
func JVMRootTools(ctx context.Context, mirror, rev string) []string {
	var out []string
	has := func(f string) bool { _, err := git(ctx, mirror, "cat-file", "-e", rev+":"+f); return err == nil }
	if has("pom.xml") {
		out = append(out, "maven")
	}
	if (has("settings.gradle") || has("settings.gradle.kts") || has("build.gradle") || has("build.gradle.kts")) && has("gradlew.bat") {
		out = append(out, "gradle")
	}
	return out
}
