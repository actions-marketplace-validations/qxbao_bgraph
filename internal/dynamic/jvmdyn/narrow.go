package jvmdyn

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/eco/jvm"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// Guard returns "" when the JVM class layer may narrow sel at all (docs/dynamic-layer.md §9.2), otherwise the reason. changes are
// classified as for dynamic.Guard, with OtherRole meaning a role in an ecosystem other than jvm. base and head are the trees
// of the selection (the constant guard reads both versions of every changed code file).
func Guard(sel model.Selection, changes []dynamic.Change, base, head tree.Tree) string {
	if sel.Scope == model.TierFull {
		return "the selection is full (never narrowed)"
	}
	for _, w := range sel.Widening {
		if w.Tier == model.TierFull || w.Reason == model.ReasonGlobalInput || w.Eco == Eco {
			return fmt.Sprintf("widening %s on %s (a build definition or global change)", w.Reason, w.Path)
		}
	}
	if len(changes) == 0 || len(changes) != len(sel.Changed) {
		return "the change set is not classified"
	}
	if base == nil || head == nil {
		return "no base or head tree"
	}
	for _, t := range []tree.Tree{base, head} {
		if why := Lombok(t); why != "" {
			return why + " (Lombok rewrites declarations the constant guard cannot see)"
		}
	}
	for _, c := range changes {
		var why string
		switch {
		case c.Status != 'M':
			why = fmt.Sprintf("status %c (only modifications of existing files are observable)", c.Status)
		case c.OldMode != c.NewMode:
			why = fmt.Sprintf("mode changed %s -> %s", c.OldMode, c.NewMode)
		case c.NewMode != "100644" && c.NewMode != "100755":
			why = "mode " + c.NewMode + " is not a regular file"
		case c.Linked:
			why = "read through a symbolic link (the recorded path may be the link's)"
		case c.Global:
			why = "global input"
		case c.Claimed:
			why = "claimed as a manifest, configuration, lockfile or build input"
		case c.Declared:
			why = "named by a bgraph.toml [[config]] or [[dynamic]] declaration"
		case jvm.DynamicSource(c.Path):
			if !codeRole(c.BaseRole) || !codeRole(c.HeadRole) {
				why = fmt.Sprintf("role %s/%s at base/head is not source or test", c.BaseRole, c.HeadRole)
				break
			}
			b, err1 := base.Read(c.Path)
			h, err2 := head.Read(c.Path)
			if err1 != nil || err2 != nil {
				why = "cannot read both versions"
				break
			}
			why = ConstantGuard(c.Path, b, h)
		case docFile(c):
			if inSrc(c.Path) {
				why = "documentation file under a src/ directory (a resource root: tests read its build-output copy)"
			} else if r := customResourceRoot(base, head); r != "" {
				why = "documentation file, but " + r + " declares a non-conventional resource root (the file may be a resource)"
			}
		default:
			why = "not a Java/Kotlin file of a compiled source root nor an unclaimed documentation file (a non-code change refuses class narrowing for the whole selection)"
		}
		if why != "" {
			if !strings.HasPrefix(why, c.Path) {
				why = c.Path + ": " + why
			}
			return why
		}
	}
	return ""
}

func codeRole(r model.Role) bool { return r == model.RoleSource || r == model.RoleTest }

// docFile: a modified documentation or harmless file that no bgraph.toml entry names, that no other ecosystem gives a role and
// that the JVM analysis sees only as a module file (data). The collector observes reads (JFR), so it stays a changed file for
// the per-class check; graph edges to it (module files, test path literals) do not matter for that reason.
func docFile(c dynamic.Change) bool {
	return (classify.Doc(c.Path) || classify.Harmless(c.Path)) && !c.OtherRole && !c.Rule && !c.Declared && !c.Global && !c.Claimed &&
		(c.BaseRole == "" || c.BaseRole == model.RoleData) && (c.HeadRole == "" || c.HeadRole == model.RoleData)
}

// inSrc: p lies under a src/ directory of any module (conventional source and resource roots; a documentation file there may
// be a resource that tests read from build output or a module jar).
func inSrc(p string) bool { return strings.HasPrefix(p, "src/") || strings.Contains(p, "/src/") }

var (
	mavenResources = regexp.MustCompile(`(?s)<(?:testR|r)esources>(.*?)</(?:testR|r)esources>`)
	mavenDirectory = regexp.MustCompile(`(?s)<directory>\s*(.*?)\s*</directory>`)
	gradleResource = regexp.MustCompile(`(?s)resources.{0,200}?srcDirs?`)
)

// customResourceRoot returns the build file that declares a resource root outside src/ (Maven <resources>/<testResources>
// <directory>, Gradle resources srcDir/srcDirs), "" when every resource root is conventional. Lexical and conservative:
// any Gradle resources srcDir counts.
func customResourceRoot(trees ...tree.Tree) string {
	for _, t := range trees {
		for _, p := range t.Files() {
			b := path.Base(p)
			if b != "pom.xml" && !strings.HasSuffix(b, ".gradle") && !strings.HasSuffix(b, ".gradle.kts") {
				continue
			}
			src, err := t.Read(p)
			if err != nil {
				return p + " (unreadable)"
			}
			if b == "pom.xml" {
				for _, blk := range mavenResources.FindAllSubmatch(src, -1) {
					for _, d := range mavenDirectory.FindAllSubmatch(blk[1], -1) {
						dir := string(d[1])
						for _, pre := range []string{"${project.basedir}/", "${basedir}/"} {
							dir = strings.TrimPrefix(dir, pre)
						}
						// The source roots and the build output (derived files only) are not new roots of tracked files.
						for _, pre := range []string{"${project.build.sourceDirectory}", "${project.build.testSourceDirectory}",
							"${project.build.directory}", "${project.build.outputDirectory}", "${project.build.testOutputDirectory}"} {
							if strings.HasPrefix(dir, pre) {
								dir = "src/"
							}
						}
						if !strings.HasPrefix(dir, "src/") {
							return p
						}
					}
				}
			} else if gradleResource.Match(src) {
				return p
			}
		}
	}
	return ""
}

// Params is what Narrow needs besides the selection.
type Params struct {
	Collector, Env string
	Records        []dynamic.Record
	Base, Head     tree.Tree
	UnitEco        map[string]string
}

// unitDir returns the module directory of a selected JVM unit ("." for the root).
func unitDir(t model.SelectedTest) string {
	d := strings.Trim(t.Project, "/")
	if d == "" {
		if _, dir, _, ok := jvm.ParseUnitID(t.Unit); ok && strings.HasPrefix(t.Unit, "maven:") {
			return dir
		}
		return "."
	}
	return d
}

// Narrow applies the class layer to sel (docs/dynamic-layer.md §9.1): every selected JVM unit with a usable class list runs only
// the classes without a usable record, the classes whose record reaches a changed file, and the top-level types declared by
// its changed test files. The unit stays selected (it is still built); removed classes are listed in Narrowed as
// "<unit>#<class>". Call it only after Guard passed.
func Narrow(sel model.Selection, p Params) model.Selection {
	bp := dynamic.Params{Eco: Eco, Collector: p.Collector, Env: p.Env, BaseOID: func(f string) string {
		if !p.Base.Has(f) {
			return ""
		}
		return p.Base.Key(f)
	}}
	byUnit := map[string]dynamic.Record{}
	for _, r := range p.Records {
		if r.Eco != Eco || r.Env != p.Env {
			continue
		}
		if cur, ok := byUnit[r.Unit]; !ok || !r.Recorded.Before(cur.Recorded) {
			byUnit[r.Unit] = r
		}
	}
	changed := map[string]bool{}
	for _, c := range sel.Changed {
		changed[c] = true
	}
	out := sel
	out.Tests = slices.Clone(sel.Tests)
	out.Narrowed = slices.Clone(sel.Narrowed)
	removedTotal := 0
	for i, t := range out.Tests {
		if p.UnitEco[t.Unit] != Eco || t.Whole || (t.Runner != jvm.RunnerMaven && t.Runner != jvm.RunnerGradle) {
			continue
		}
		keep := func(why string) { out.Tests[i].Dynamic = "class layer kept the whole unit: " + why }
		cl, ok := byUnit[t.Unit]
		if !ok {
			keep("no class-list record for the current environment")
			continue
		}
		if why := dynamic.Usable(cl, nil, bp); why != "" {
			keep("class list not usable: " + why)
			continue
		}
		dir := unitDir(t)
		if why := sameTestRoots(cl, p.Base, dir); why != "" {
			keep("class list not usable: " + why)
			continue
		}
		run := map[string]bool{}
		declaredOK := true
		for _, c := range sel.Changed {
			rel, ok := strings.CutPrefix(c, dirPrefix(dir))
			if !ok || !jvm.TestRoot(rel) {
				continue
			}
			if ext := path.Ext(c); ext != ".java" && ext != ".kt" {
				continue
			}
			b, err := p.Head.Read(c)
			if err != nil {
				declaredOK = false
				break
			}
			names, ok := TopLevelTypes(c, b)
			if !ok {
				declaredOK = false
				break
			}
			for _, n := range names {
				run[n] = true
			}
		}
		if !declaredOK {
			keep("cannot extract the types declared by a changed test file")
			continue
		}
		var removed []model.NarrowedTest
		for _, c := range cl.Classes {
			r, ok := byUnit[t.Unit+"#"+c]
			if !ok {
				run[c] = true
				continue
			}
			if dynamic.Usable(r, nil, bp) != "" || len(r.Artifacts) == 0 || ArtifactsUnchanged(r.Artifacts) != "" {
				run[c] = true // not usable: incomplete, failed, stale, or an external artifact missing or changed
				continue
			}
			hit := false
			for d := range r.Deps {
				if changed[d] {
					hit = true
					break
				}
			}
			if hit {
				run[c] = true
				continue
			}
			if run[c] {
				continue // declared by a changed test file
			}
			removed = append(removed, model.NarrowedTest{Unit: t.Unit + "#" + c, Recorded: r.Recorded, Commit: r.Commit,
				Reason: fmt.Sprintf("record by %s: none of the %d changed files is among its %d recorded dependencies", r.Collector,
					len(sel.Changed), len(r.Deps))})
		}
		if len(removed) == 0 {
			out.Tests[i].Dynamic = fmt.Sprintf("class layer: all %d test classes run (each has a changed file among its dependencies or no usable record)", len(cl.Classes))
			continue
		}
		classes := make([]string, 0, len(run))
		for c := range run {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		out.Tests[i].ClassFilter = true
		out.Tests[i].Classes = classes
		out.Tests[i].Dynamic = fmt.Sprintf("class layer: %d of %d test classes run (the module is still built)", len(classes), len(cl.Classes))
		out.Narrowed = append(out.Narrowed, removed...)
		removedTotal += len(removed)
	}
	if removedTotal > 0 && out.IndexStats != nil {
		st := make(map[string]int, len(out.IndexStats)+1)
		for k, v := range out.IndexStats {
			st[k] = v
		}
		st["narrowed_classes"] = removedTotal
		out.IndexStats = st
	}
	return out
}

func dirPrefix(dir string) string {
	if dir == "." || dir == "" {
		return ""
	}
	return dir + "/"
}

// sameTestRoots: the class-list record lists exactly the files of the unit's test roots at the base (a file added or removed
// since the recording could add or remove a test class).
func sameTestRoots(cl dynamic.Record, base tree.Tree, dir string) string {
	files := TestRootFiles(base, dir)
	if len(files) != len(cl.Deps) {
		return fmt.Sprintf("the test roots have %d files at the base, %d when recorded", len(files), len(cl.Deps))
	}
	for _, f := range files {
		if _, ok := cl.Deps[f]; !ok {
			return "test file " + f + " was not there when recorded"
		}
	}
	return ""
}
