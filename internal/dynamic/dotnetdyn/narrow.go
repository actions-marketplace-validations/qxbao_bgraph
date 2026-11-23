package dotnetdyn

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// RunnerDotnet is the runner of the units the class layer narrows (eco/dotnet.Runner).
const RunnerDotnet = "dotnet"

// Source reports whether p is a C# source file the class layer can reason about.
func Source(p string) bool { return strings.EqualFold(path.Ext(p), ".cs") }

var publicAPI = regexp.MustCompile(`^PublicAPI\.(Shipped|Unshipped)\.txt$`)

// Guarded is the result of Guard: the members the changed C# files touch, per file (base line spans), for Instrumented.
type Guarded map[string][]Span

// Guard returns "" when the .NET class layer may narrow sel at all (docs/dynamic-layer.md §9.2, §9.9), otherwise the reason, and
// the members each changed C# file touches. changes are classified as for dynamic.Guard, with OtherRole meaning a role in an
// ecosystem other than dotnet. base and head are the trees of the selection.
func Guard(sel model.Selection, changes []dynamic.Change, base, head tree.Tree) (Guarded, string) {
	if sel.Scope == model.TierFull {
		return nil, "the selection is full (never narrowed)"
	}
	for _, w := range sel.Widening {
		if w.Tier == model.TierFull || w.Reason == model.ReasonGlobalInput || w.Eco == Eco {
			return nil, fmt.Sprintf("widening %s on %s (a build definition or global change)", w.Reason, w.Path)
		}
	}
	if len(changes) == 0 || len(changes) != len(sel.Changed) {
		return nil, "the change set is not classified"
	}
	if base == nil || head == nil {
		return nil, "no base or head tree"
	}
	for _, t := range []tree.Tree{base, head} {
		if f := FloatingVersions(t); f != "" {
			return nil, "floating or ranged NuGet version in " + f + " (packages can change without a file change)"
		}
	}
	g := Guarded{}
	for _, c := range changes {
		var why string
		switch {
		case c.Status != 'M' && !(publicAPI.MatchString(path.Base(c.Path)) && !c.Rule && !c.Declared):
			why = fmt.Sprintf("status %c (only modifications of existing files are observable)", c.Status)
		case c.Status == 'M' && c.OldMode != c.NewMode:
			why = fmt.Sprintf("mode changed %s -> %s", c.OldMode, c.NewMode)
		case c.Status == 'M' && c.NewMode != "100644" && c.NewMode != "100755":
			why = "mode " + c.NewMode + " is not a regular file"
		case c.Linked:
			why = "read through a symbolic link"
		case c.Global:
			why = "global input"
		case c.Declared:
			why = "named by a bgraph.toml [[config]] or [[dynamic]] declaration"
		case c.Rule:
			why = "named by a bgraph.toml [[rule]]"
		case publicAPI.MatchString(path.Base(c.Path)):
			// Public API baselines are read by an analyzer at build time only (the build still runs).
		case c.Claimed:
			why = "claimed as a manifest, configuration, lockfile or build input"
		case Source(c.Path):
			if c.BaseRole != model.RoleSource && c.BaseRole != model.RoleTest || c.HeadRole != model.RoleSource && c.HeadRole != model.RoleTest {
				why = fmt.Sprintf("role %s/%s at base/head is not source or test", c.BaseRole, c.HeadRole)
				break
			}
			b, err1 := base.Read(c.Path)
			h, err2 := head.Read(c.Path)
			if err1 != nil || err2 != nil {
				why = "cannot read both versions"
				break
			}
			if bt := buildTimeProject(base, c.Path); bt != "" {
				why = bt
				break
			}
			var spans []Span
			spans, why = BodyGuard(c.Path, b, h)
			g[c.Path] = spans
		case docFile(c):
			if d := projectDirOf(base, c.Path); d != "" {
				why = "documentation file inside the project directory " + d + " (tests may read it; reads are not observed)"
			} else if f := mentioned(head, c.Path); f != "" {
				why = "documentation file named by " + f + " (tests may read it; reads are not observed)"
			}
		default:
			why = "not a C# file of a project nor an unclaimed documentation file (file reads are not observed: a non-code change refuses class narrowing for the whole selection)"
		}
		if why != "" {
			if !strings.HasPrefix(why, c.Path) {
				why = c.Path + ": " + why
			}
			return nil, why
		}
	}
	return g, ""
}

var (
	analyzerRef      = regexp.MustCompile(`(?is)<ProjectReference\b[^>]*\bInclude\s*=\s*"([^"]+)"[^>]*?(?:/>|>(.*?)</ProjectReference>)`)
	analyzerItemType = regexp.MustCompile(`(?i)OutputItemType\s*(?:=\s*"|>)\s*Analyzer`)
	buildTimeMarker  = regexp.MustCompile(`(?i)<IsRoslynComponent>\s*true|Include\s*=\s*"Microsoft\.Build\.(?:Utilities|Framework|Tasks)`)
)

// buildTimeProject returns why the C# file p belongs to a project that runs at build time (a source generator, an analyzer,
// an MSBuild task): what it generates or checks in other projects is not attributed to its files by any record ("" when
// none of the projects that own p is such a project).
func buildTimeProject(t tree.Tree, p string) string {
	var owners []string
	for _, f := range t.Files() {
		if projectFile(f) {
			if d := path.Dir(f); d == "." || strings.HasPrefix(p, d+"/") {
				owners = append(owners, f)
			}
		}
	}
	for _, o := range owners {
		src, err := t.Read(o)
		if err != nil {
			return "cannot read the owning project " + o
		}
		if buildTimeMarker.Match(src) {
			return "the owning project " + o + " runs at build time (Roslyn component or MSBuild task)"
		}
		if ref := UnsafeReference(src); strings.HasPrefix(strings.ToLower(ref), "microsoft.codeanalysis") {
			return "the owning project " + o + " references " + ref + " (a source generator or analyzer)"
		}
	}
	for _, f := range t.Files() {
		if !projectFile(f) {
			continue
		}
		src, err := t.Read(f)
		if err != nil {
			continue
		}
		for _, m := range analyzerRef.FindAllSubmatch(src, -1) {
			if !analyzerItemType.Match(m[0]) {
				continue
			}
			target := path.Clean(path.Join(path.Dir(f), strings.ReplaceAll(string(m[1]), "\\", "/")))
			for _, o := range owners {
				if strings.EqualFold(target, o) {
					return "the owning project " + o + " is referenced as an analyzer by " + f
				}
			}
		}
	}
	return ""
}

// docFile: a documentation or harmless file that no ecosystem gives a role, that nothing in the graph references and that no
// bgraph.toml entry names.
func docFile(c dynamic.Change) bool {
	return (classify.Doc(c.Path) || classify.Harmless(c.Path)) && !c.OtherRole && !c.Referenced && c.BaseRole == "" && c.HeadRole == ""
}

func projectFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	}
	return false
}

// projectDirOf returns the directory of a project whose directory contains p ("" when none).
func projectDirOf(t tree.Tree, p string) string {
	for _, f := range t.Files() {
		if !projectFile(f) {
			continue
		}
		d := path.Dir(f)
		if d == "." || strings.HasPrefix(p, d+"/") {
			return d
		}
	}
	return ""
}

// mentioned returns a C# file of t whose text contains the base name of p (a test that reads a file usually names it).
func mentioned(t tree.Tree, p string) string {
	name := path.Base(p)
	for _, f := range t.Files() {
		if !Source(f) {
			continue
		}
		if b, err := t.Read(f); err == nil && strings.Contains(string(b), name) {
			return f
		}
	}
	return ""
}

// LineMaps indexes the instrumented lines of the class-list records: file → base blob OID → instrumented lines.
type LineMaps map[string]map[string][]int

// NewLineMaps collects the line maps of the usable class-list records.
func NewLineMaps(recs []dynamic.Record) LineMaps {
	m := LineMaps{}
	for _, r := range recs {
		for f, v := range r.Lines {
			oid, rs, ok := strings.Cut(v, "|")
			if !ok {
				continue
			}
			ls, ok := ParseRanges(rs)
			if !ok {
				continue
			}
			if m[f] == nil {
				m[f] = map[string][]int{}
			}
			cur := m[f][oid]
			cur = append(cur, ls...)
			sort.Ints(cur)
			m[f][oid] = slices.Compact(cur)
		}
	}
	return m
}

// Instrumented returns "" when every member a changed C# file touches has at least one line the coverage instrumented (in the
// file's base content), otherwise the reason: code the coverage does not see ([ExcludeFromCodeCoverage], [DebuggerHidden],
// generated or excluded modules, files no recorded test host loaded) cannot be judged by the records.
func Instrumented(g Guarded, maps LineMaps, base tree.Tree) string {
	files := make([]string, 0, len(g))
	for f := range g {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		spans := g[f]
		if len(spans) == 0 {
			continue // comment, whitespace or using-directive changes only
		}
		ls, ok := maps[f][base.Key(f)]
		if !ok {
			return f + ": no record lists the file's instrumented lines at the base (not loaded by any recorded test host, excluded from coverage, or records older than the file)"
		}
		for _, s := range spans {
			i := sort.SearchInts(ls, s.From)
			if i >= len(ls) || ls[i] > s.To {
				return fmt.Sprintf("%s:%d: %s has no instrumented line (excluded from coverage or never compiled in a recorded configuration)", f, s.From, s.What)
			}
		}
	}
	return ""
}

// Params is what Narrow needs besides the selection.
type Params struct {
	Env        string
	Records    []dynamic.Record
	Base, Head tree.Tree
	UnitEco    map[string]string
	// Guarded: the members each changed C# file touches (Guard); each narrowed unit's own line maps must instrument them.
	Guarded Guarded
}

// Narrowable reports whether the class layer handles selected unit t (VSTest projects; Microsoft.Testing.Platform is not
// supported yet).
func Narrowable(t model.SelectedTest, eco string) bool {
	return eco == Eco && t.Runner == RunnerDotnet && !t.Whole && t.Config == "" && t.Package != ""
}

// Narrow applies the class layer to sel (docs/dynamic-layer.md §9.1): every selected .NET test project with a usable class list runs
// only the classes without a usable record, the classes whose record reaches a changed file, and the top-level types declared
// by its changed C# files. The project stays selected (it is still built); removed classes are listed in Narrowed as
// "<unit>#<class>". Call it only after Guard and Instrumented passed.
func Narrow(sel model.Selection, p Params) model.Selection {
	bp := dynamic.Params{Eco: Eco, Collector: CollectorID, Env: p.Env, BaseOID: func(f string) string {
		if !p.Base.Has(f) {
			return ""
		}
		return p.Base.Key(f)
	}}
	byUnit := Latest(p.Records, p.Env)
	changed := map[string]bool{}
	for _, c := range sel.Changed {
		changed[c] = true
	}
	out := sel
	out.Tests = slices.Clone(sel.Tests)
	out.Narrowed = slices.Clone(sel.Narrowed)
	removedTotal := 0
	for i, t := range out.Tests {
		if !Narrowable(t, p.UnitEco[t.Unit]) {
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
		dir := path.Dir(t.Package)
		if why := sameFiles(cl, p.Base, dir); why != "" {
			keep("class list not usable: " + why)
			continue
		}
		// The unit's own hosts must have instrumented every changed member: a file instrumented elsewhere may be loaded
		// without instrumentation here (another assembly compiling the same file, an assembly without symbols).
		if why := Instrumented(p.Guarded, NewLineMaps([]dynamic.Record{cl}), p.Base); why != "" {
			keep(why)
			continue
		}
		run := map[string]bool{}
		declaredOK := true
		for _, c := range sel.Changed {
			if !Source(c) || !(dir == "." || strings.HasPrefix(c, dir+"/")) || !p.Head.Has(c) {
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
			keep("cannot extract the types declared by a changed C# file of the project")
			continue
		}
		var removed []model.NarrowedTest
		unproven := ""
		for _, c := range cl.Classes {
			r, ok := byUnit[t.Unit+"#"+c]
			if !ok || strings.Contains(r.Incomplete, "not reported in its own run") {
				// No proof that the class filter selects this class's tests: the narrowed filter might not run it either.
				unproven = c
				break
			}
			if dynamic.Usable(r, nil, bp) != "" {
				run[c] = true // not usable: incomplete, failed, stale
				continue
			}
			hit := false
			for d := range r.Deps {
				if changed[d] {
					hit = true
					break
				}
			}
			if hit || run[c] {
				run[c] = true
				continue
			}
			removed = append(removed, model.NarrowedTest{Unit: t.Unit + "#" + c, Recorded: r.Recorded, Commit: r.Commit,
				Reason: fmt.Sprintf("record by %s: none of the %d changed files is among its %d recorded dependencies", r.Collector,
					len(sel.Changed), len(r.Deps))})
		}
		if unproven != "" {
			keep("no proof that the class filter runs " + unproven + " (no record, or its own recording run did not report it)")
			continue
		}
		if len(removed) == 0 {
			out.Tests[i].Dynamic = fmt.Sprintf("class layer: all %d test classes run (each has a changed file among its dependencies or no usable record)", len(cl.Classes))
			continue
		}
		classes := make([]string, 0, len(run))
		for c := range run {
			classes = append(classes, c) // declared types not in the list too: a contains filter can only run more
		}
		sort.Strings(classes)
		out.Tests[i].ClassFilter = true
		out.Tests[i].Classes = classes
		out.Tests[i].Dynamic = fmt.Sprintf("class layer: %d of %d test classes run (the project is still built)", len(classes), len(cl.Classes))
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

// Latest returns the latest .NET record per unit for environment env.
func Latest(recs []dynamic.Record, env string) map[string]dynamic.Record {
	byUnit := map[string]dynamic.Record{}
	for _, r := range recs {
		if r.Eco != Eco || r.Env != env {
			continue
		}
		if cur, ok := byUnit[r.Unit]; !ok || !r.Recorded.Before(cur.Recorded) {
			byUnit[r.Unit] = r
		}
	}
	return byUnit
}

// sameFiles: the class-list record lists exactly the files under the project directory at the base (a file added or removed
// since the recording could add or remove a test class).
func sameFiles(cl dynamic.Record, base tree.Tree, dir string) string {
	files := FilesUnder(base, dir)
	if len(files) != len(cl.Deps) {
		return fmt.Sprintf("the project directory has %d files at the base, %d when recorded", len(files), len(cl.Deps))
	}
	for _, f := range files {
		if _, ok := cl.Deps[f]; !ok {
			return "file " + f + " was not there when recorded"
		}
	}
	return ""
}
