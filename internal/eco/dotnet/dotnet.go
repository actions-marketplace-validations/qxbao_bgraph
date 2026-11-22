// Package dotnet is bgraph's .NET ecosystem, at MSBuild-project granularity (docs/ecosystems/dotnet.md).
//
// Inside a project the file/namespace graph is not a sound dependency relation (partial classes, source generators, globbed items,
// reflection), so the safe unit is the project. File-level information is used only to decide which projects OWN a file.
//
// # Test units
//
// One model.TestUnit per MSBuild project (.csproj, .fsproj, .vbproj). Every project is a unit: test projects are NOT told apart by their
// PackageReference, because the reference can come from a props file and `dotnet test` skips projects that are not test projects.
// Eco is "dotnet", Runner "dotnet", Project is the project directory, Package the project file path and Config the global.json that
// selects the Microsoft.Testing.Platform runner (empty for VSTest). The unit ID is built by UnitID:
//
//	dotnet:<project file path>
//
// The unit's Files hold a single virtual node, "<dotnet:<project file path>>", so selection is reachability of that node, and the same
// node is the dependency target of every project that references it.
//
// # Graph (From depends on To)
//
//   - a project depends on its project file and on every file under its directory (the SDK default globs), so a file belongs to EVERY
//     ancestor project whose directory contains it. bin/ and obj/ of the project, other project files, solutions, *.user and docs
//     (*.md, LICENSE...) are not owned;
//   - a project depends on the file of every Compile/Link/None/Content/EmbeddedResource/AdditionalFiles/Analyzer/... item that its
//     Include names, wherever it lives (a wildcard is a pattern edge), and on HintPath files;
//   - a project depends on the project node of every ProjectReference (including OutputItemType="Analyzer" and
//     ReferenceOutputAssembly="false"); a reference to a project that is not an SDK .NET project (.vcxproj, .wapproj...) depends on
//     the project file and everything under its directory;
//   - a project depends on the props/targets that apply to it: the nearest Directory.Build.props, Directory.Build.targets and
//     Directory.Packages.props at or above its directory (the nearest wins; the chain continues only because the file imports its
//     parent, which is an ordinary <Import> followed here), every file named by an <Import Project="..."> (transitively; a shared
//     project's .projitems is such an import and the files it lists are items), and every .editorconfig/.globalconfig/*.rsp above it.
//     A changed props file therefore selects exactly the projects below it, with no widening;
//   - an imported file depends on the files it imports.
//
// Properties are evaluated for each project over the union of every definition found in the project and its import closure
// ($(MSBuildThisFileDirectory), $(MSBuildProjectDirectory), $(SolutionDir) when a .sln/.slnx sits in an ancestor directory,
// [MSBuild]::GetPathOfFileAbove/NormalizePath/NormalizeDirectory/EnsureTrailingSlash, System.IO.Path.Combine...). $(Configuration) and
// the like become "*", build output properties drop the item, toolchain locations are ignored. Under a Condition, a property defined
// nowhere in the repository (SDK, command line, environment) is "*" inside a path segment and "any directories" ("**/*") when it starts
// a segment glued to a name ($(RepositoryEngineeringDir)x.targets); a whole-segment one stays Unresolved. When the nearest global.json
// names the Arcade SDK, its layout properties are known: $(RepoRoot) is the global.json directory, $(RepositoryEngineeringDir) its eng/
// and $(ArtifactsDir) and the like are build output. A Condition is never evaluated: every
// conditional ProjectReference, Import and item counts as present. Forcing a full run for a Condition on a
// ProjectReference/Import would be sound too; the union is a superset of every branch, so it cannot miss a dependency and keeps conditional references
// (TargetFramework-specific ones are everywhere) from widening everything.
//
// # Widening
//
//   - an expression that cannot be evaluated, an import or ProjectReference that names no file (without a Condition) and a path
//     outside the repository are Unresolved (reason unknown_import) seeded at the file that contains them, for ANY change in the
//     repository: the project (or, for a props file, every project that imports it) and its dependents are selected. This is a
//     "full" run restricted to the .NET ecosystem and to the projects the unknown construct belongs to: a project that does not
//     contain it cannot depend on what it names;
//   - global.json (classify's global table), nuget.config, .config/dotnet-tools.json, Directory.Solution.props/targets and
//     *.runsettings widen to every .NET unit (module scope "." restricted to Eco "dotnet");
//   - packages.lock.json widens its directory (lock_changed);
//   - .sln/.slnx/.slnf and .shproj are claimed without widening: the projects they list bring their own files.
//
// # Strings read by tasks
//
// A task can read a file that no item declares (Copy SourceFiles, Exec Command, ReadLinesFromFile File...). Like the string literals
// of a Rust build script, every attribute value and element text of the project and of the props/targets that apply to it (inside
// <Target> too) is split into words, evaluated with the property substitution above, and, when a word resolves to an existing file, it
// is a dependency of the project; an existing directory (named with a path separator) becomes a `dir/**` pattern edge and a path with
// wildcards a pattern edge. Words that are not path-like are ignored.
//
// Known gaps: such a string that cannot be evaluated (undefined property, item list or metadata reference, unsupported property
// function) is ignored; a path assembled at build time by an inline task or a script; and test data reached through absolute or
// environment-derived paths.
package dotnet

import (
	"context"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/qxbao/bgraph/internal/classify"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "dotnet"

// Runner is the runner name of the units.
const Runner = "dotnet"

// AnalyzerVersion: bump whenever the analysis rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "dotnet-3"

// Eco is the .NET ecosystem.
type Eco struct{}

// New creates the .NET ecosystem. The rules file is accepted for symmetry with the other ecosystems; .NET has no declarations yet.
func New(_ *rules.File) *Eco { return &Eco{} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// UnitID returns the test-unit ID of a project: "dotnet:<project file path>".
func UnitID(projectFile string) string { return "dotnet:" + projectFile }

// ParseUnitID is the inverse of UnitID. ok=false when id is not a .NET unit ID.
func ParseUnitID(id string) (projectFile string, ok bool) {
	p, found := strings.CutPrefix(id, "dotnet:")
	if !found || p == "" || !IsProjectFile(p) {
		return "", false
	}
	return p, true
}

// Node returns the virtual graph node of a project file.
func Node(projectFile string) string { return "<dotnet:" + projectFile + ">" }

// IsProjectFile: an MSBuild project that `dotnet test` can run.
func IsProjectFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	}
	return false
}

var implicitImports = []string{"Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props"}

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, _ eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	var projects []string
	var slnDirs []string
	for _, f := range t.Files() {
		switch strings.ToLower(path.Ext(f)) {
		case ".csproj", ".fsproj", ".vbproj":
			if !skipProject(f) {
				projects = append(projects, f)
			}
		case ".sln", ".slnx":
			slnDirs = append(slnDirs, path.Dir(f))
		}
	}
	if len(projects) == 0 {
		return a, nil
	}
	b := &builder{t: t, a: a, seen: map[edgeKey]bool{}, files: map[string]*msFile{}, seenU: map[string]bool{},
		projSet: map[string]bool{}, projDirs: map[string][]string{}, mtp: map[string]string{}, globs: map[string][]string{}, arc: map[string]string{}, strs: map[string]string{}}
	for _, p := range projects {
		b.projSet[p] = true
		d := path.Dir(p)
		b.projDirs[d] = append(b.projDirs[d], p)
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: d, Name: strings.TrimSuffix(path.Base(p), path.Ext(p))})
		a.Roles[p] = model.RoleManifest
	}
	b.assignFiles()
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.analyzeProject(p, slnDirs)
	}
	for _, f := range t.Files() {
		if path.Base(f) == "packages.lock.json" {
			if _, ok := a.Roles[f]; !ok {
				a.Roles[f] = model.RoleLock
			}
		}
	}
	slices.SortFunc(a.Edges, func(x, y model.Edge) int {
		if c := strings.Compare(x.From, y.From); c != 0 {
			return c
		}
		return strings.Compare(x.To, y.To)
	})
	return a, nil
}

// skipProject: project files that are build output or vendored, not repository projects.
func skipProject(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "obj" || seg == "node_modules" {
			return true
		}
	}
	return false
}

type edgeKey struct {
	from, to string
	kind     model.EdgeKind
}

type builder struct {
	t        tree.Tree
	a        *eco.Analysis
	seen     map[edgeKey]bool
	seenU    map[string]bool
	files    map[string]*msFile  // parsed MSBuild files
	projSet  map[string]bool     // project file → true
	projDirs map[string][]string // directory → project files in it
	mtp      map[string]string   // directory → global.json selecting MTP (memo)
	globs    map[string][]string // glob → matching files (memo)
	arc      map[string]string   // directory → global.json selecting the Arcade SDK (memo)
	strs     map[string]string   // interned strings
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string) {
	k := edgeKey{from, to, kind}
	if b.seen[k] {
		return
	}
	b.seen[k] = true
	// Specs and targets repeat across the projects that share a props file: keep one copy of each string.
	b.a.Edges = append(b.a.Edges, model.Edge{From: b.intern(from), To: b.intern(to), Kind: kind, Spec: b.intern(spec), Provenance: model.ProvScan})
}

func (b *builder) intern(s string) string {
	if v, ok := b.strs[s]; ok {
		return v
	}
	b.strs[s] = s
	return s
}

func (b *builder) role(p string, r model.Role) {
	if old, ok := b.a.Roles[p]; !ok || old == model.RoleData || old == model.RoleUnknown {
		b.a.Roles[p] = r
	}
}

// unresolved records a construct bgraph cannot evaluate, seeded at file for any change in the repository.
func (b *builder) unresolved(file, spec, detail string) {
	k := file + "\x00" + spec
	if b.seenU[k] {
		return
	}
	b.seenU[k] = true
	b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: file, Spec: spec, Class: model.UnresolvedUnknown,
		ScopeTier: model.TierModule, ScopeDir: "", Detail: detail})
}

// unresolved records a construct found in f while evaluating project c.proj: it is seeded at the project's own file, so only that
// project (and its dependents) is selected, not every project that imports f.
func (c *closure) unresolved(f *msFile, spec, detail string) {
	c.b.unresolved(c.proj, f.path+": "+spec, detail+" (in "+f.path+")")
}

func (b *builder) parse(p string) *msFile {
	if f, ok := b.files[p]; ok {
		return f
	}
	f := parseMSBuild(b.t, p)
	b.files[p] = f
	if f.err != nil {
		b.unresolved(p, p, "MSBuild file unreadable: "+f.err.Error())
	}
	return f
}

// notOwned: files below a project directory that the SDK default globs do not include (or that cannot affect a test).
func notOwned(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	if first == "bin" || first == "obj" {
		return true
	}
	base := path.Base(rel)
	switch strings.ToLower(path.Ext(base)) {
	case ".csproj", ".fsproj", ".vbproj", ".vcxproj", ".sln", ".slnx", ".user", ".vssscc":
		return true
	}
	return classify.Doc(rel) || classify.Harmless(rel)
}

// assignFiles gives every file to every project whose directory contains it.
func (b *builder) assignFiles() {
	for _, f := range b.t.Files() {
		for d := path.Dir(f); ; d = path.Dir(d) {
			for _, p := range b.projDirs[d] {
				rel := f
				if d != "." {
					rel = strings.TrimPrefix(f, d+"/")
				}
				if f == p || notOwned(rel) {
					continue
				}
				b.edge(Node(p), f, model.EdgeImplicit, "SDK default items")
				b.role(f, roleOf(f))
			}
			if d == "." || d == "/" || d == "" {
				break
			}
		}
	}
}

func roleOf(f string) model.Role {
	switch strings.ToLower(path.Ext(f)) {
	case ".cs", ".fs", ".vb", ".fsi", ".fsx", ".razor", ".cshtml", ".xaml", ".resx":
		return model.RoleSource
	case ".props", ".targets", ".projitems", ".editorconfig", ".globalconfig", ".rsp", ".ruleset", ".config":
		return model.RoleConfig
	}
	switch path.Base(f) {
	case ".editorconfig", ".globalconfig", "Directory.Build.rsp", "MSBuild.rsp":
		return model.RoleConfig
	}
	return model.RoleData
}

// nearestAbove returns the nearest dir/name at or above dir.
func (b *builder) nearestAbove(dir, name string) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if p := path.Join(d, name); b.t.Has(p) {
			return p, true
		}
		if d == "." || d == "/" || d == "" {
			return "", false
		}
	}
}

type impKey struct {
	file string
	idx  int
}

// closure is the set of MSBuild files evaluated for one project.
type closure struct {
	b     *builder
	proj  string
	node  string
	env   *env
	files []string
	in    map[string]bool
	done  map[impKey]bool
}

func (c *closure) add(p string) {
	if c.in[p] {
		return
	}
	c.in[p] = true
	c.files = append(c.files, p)
	f := c.b.parse(p)
	for _, pd := range f.props {
		key := strings.ToLower(pd.name)
		c.env.props[key] = append(c.env.props[key], propVal{raw: pd.value, file: p})
	}
}

func (b *builder) analyzeProject(p string, slnDirs []string) {
	dir := path.Dir(p)
	node := Node(p)
	b.a.Tests = append(b.a.Tests, model.TestUnit{Eco: Name, ID: UnitID(p), Project: dir, Files: []string{node}, Runner: Runner,
		Package: p, Config: b.mtpConfig(dir)})
	b.edge(node, p, model.EdgeImplicit, "project file")

	c := &closure{b: b, proj: p, node: node, in: map[string]bool{}, done: map[impKey]bool{},
		env: &env{t: b.t, project: p, props: map[string][]propVal{}}}
	for d := dir; ; d = path.Dir(d) {
		for _, s := range slnDirs {
			if s == d {
				c.env.slnDirs = append(c.env.slnDirs, d)
				break
			}
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	c.add(p)
	if g := b.arcade(dir); g != "" {
		// The Arcade SDK (RepoLayout.props) defines the repository layout from the global.json that names it.
		for k, v := range map[string]string{"reporoot": "$(MSBuildThisFileDirectory)", "repositoryengineeringdir": "$(MSBuildThisFileDirectory)eng/",
			"repositoryengineeringcommondir": "$(MSBuildThisFileDirectory)eng/common/"} {
			c.env.props[k] = append(c.env.props[k], propVal{raw: v, file: g})
		}
	}
	for _, name := range implicitImports {
		if f, ok := b.nearestAbove(dir, name); ok {
			b.edge(node, f, model.EdgeImplicit, name)
			b.role(f, model.RoleConfig)
			c.add(f)
		}
	}
	// Configuration files that apply to every project below them.
	for d := dir; ; d = path.Dir(d) {
		for _, name := range []string{".editorconfig", ".globalconfig", "Directory.Build.rsp", "MSBuild.rsp"} {
			if f := path.Join(d, name); b.t.Has(f) {
				b.edge(node, f, model.EdgeImplicit, name)
				b.role(f, model.RoleConfig)
			}
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	// Imports to a fixpoint: resolving one can define the properties another needs.
	for progress := true; progress; {
		progress = false
		for i := 0; i < len(c.files); i++ {
			f := b.parse(c.files[i])
			for j, imp := range f.imports {
				k := impKey{f.path, j}
				if c.done[k] {
					continue
				}
				if _, err := c.resolveImport(f, imp); err == nil {
					c.done[k] = true
					progress = true
				}
			}
		}
	}
	for i := 0; i < len(c.files); i++ {
		f := b.parse(c.files[i])
		for j, imp := range f.imports {
			if c.done[impKey{f.path, j}] {
				continue
			}
			if _, err := c.resolveImport(f, imp); err != nil {
				c.unresolved(f, "Import "+imp.project, err.Error())
			}
		}
	}
	for _, fp := range c.files {
		f := b.parse(fp)
		c.items(f)
		c.literals(f)
	}
}

// literals turns every string of f that, after property substitution, names an existing file or directory (or a wildcard path) into a
// dependency of the project, the way a Rust build script's string literals are: a task (Copy, Exec, ReadLinesFromFile...) can read
// it at build time even though no item declares it. Strings that cannot be evaluated are ignored (documented gap).
func (c *closure) literals(f *msFile) {
	bases := []string{path.Dir(c.proj)}
	if f.dir != bases[0] {
		bases = append(bases, f.dir)
	}
	seen := map[string]bool{}
	for _, sv := range f.strs {
		if seen[sv.value] || strings.Contains(sv.value, "@(") || strings.Contains(sv.value, "%(") {
			continue
		}
		seen[sv.value] = true
		c.env.lenient = false
		vals, err := c.env.eval(sv.value, f.path, 0)
		if err != nil {
			continue
		}
		for _, v := range vals {
			for _, tok := range pathTokens(v) {
				for _, tg := range toTarget(tok, bases) {
					c.literal(tg, tok)
				}
			}
		}
	}
}

func (c *closure) literal(tg target, tok string) {
	b := c.b
	rel := tg.rel
	if tg.outside || rel == "." || rel == ".." {
		return
	}
	spec := "path literal " + tok
	switch {
	case hasWildcard(rel):
		if strings.Contains(rel, "/") && strings.Trim(rel, "*?/.") != "" {
			b.edge(c.node, globOf(rel), model.EdgePattern, spec)
		}
	case b.t.Has(rel):
		b.edge(c.node, rel, model.EdgeInclude, spec)
		b.role(rel, roleOf(rel))
	case b.t.IsDir(rel) && strings.ContainsAny(tok, "/\\"):
		b.edge(c.node, globOf(rel)+"/**", model.EdgePattern, spec)
	}
}

// pathTokens splits an evaluated string into the candidate paths in it: separated by ';' and whitespace, with quotes (and a leading
// option value such as -File=x) stripped, so that `powershell -File "scripts/x.ps1" a` yields scripts/x.ps1.
func pathTokens(v string) []string {
	var out []string
	isSep := func(r rune) bool { return unicode.IsSpace(r) }
	for _, part := range strings.Split(v, ";") {
		for _, w := range strings.FieldsFunc(part, isSep) {
			w = strings.Trim(w, "\"'`")
			if w == "" {
				continue
			}
			out = append(out, w)
			if k := strings.LastIndex(w, "="); k >= 0 && k+1 < len(w) {
				out = append(out, strings.Trim(w[k+1:], "\"'`"))
			}
		}
	}
	return out
}

// resolveImport tries to resolve one import; an error means "not yet" (an undefined property may be defined by a file added later).
func (c *closure) resolveImport(f *msFile, imp importDef) (bool, error) {
	if imp.sdk {
		return true, nil
	}
	c.env.lenient = imp.cond
	vals, err := c.env.eval(imp.project, f.path, 0)
	c.env.lenient = false
	if err != nil {
		if err == errExternal || err == errIgnore {
			return true, nil
		}
		return false, err
	}
	b := c.b
	for _, v := range vals {
		if strings.TrimSpace(v) == "" {
			if !imp.cond {
				c.unresolved(f, "Import "+imp.project, "the import expression evaluates to an empty path")
			}
			continue
		}
		for _, tg := range toTarget(v, []string{f.dir}) {
			switch {
			case tg.outside:
				c.unresolved(f, "Import "+imp.project, "imports a file outside the repository: "+v)
			case hasWildcard(tg.rel):
				g := globOf(tg.rel)
				b.edge(f.path, g, model.EdgePattern, "Import "+imp.project)
				b.edge(c.node, g, model.EdgePattern, "Import "+imp.project)
				for _, x := range b.matchFiles(g) {
					c.imported(f, x, imp)
				}
			case b.t.Has(tg.rel):
				c.imported(f, tg.rel, imp)
			default:
				// A file that does not exist: fine behind a Condition (Exists(...)); a later-added file is still a dependency.
				b.edge(f.path, tg.rel, model.EdgeInclude, "Import "+imp.project)
				b.edge(c.node, tg.rel, model.EdgeInclude, "Import "+imp.project)
				if !imp.cond {
					c.unresolved(f, "Import "+imp.project, "imports a file that does not exist: "+tg.rel)
				}
			}
		}
	}
	return true, nil
}

// matchFiles returns the files of the tree that glob g matches (memoized: the same import pattern recurs in every project below it).
func (b *builder) matchFiles(g string) []string {
	if m, ok := b.globs[g]; ok {
		return m
	}
	var m []string
	if gp, err := glob.Compile(g); err == nil {
		for _, x := range b.t.Files() {
			if gp.Match(x) {
				m = append(m, x)
			}
		}
	}
	b.globs[g] = m
	return m
}

func (c *closure) imported(from *msFile, to string, imp importDef) {
	if to == from.path {
		return
	}
	c.b.edge(from.path, to, model.EdgeImplicit, "Import "+imp.project)
	c.b.edge(c.node, to, model.EdgeImplicit, "Import "+imp.project)
	c.b.role(to, roleOf(to))
	c.add(to)
}

// itemDeny: item types whose Include is a name, not a path.
var itemDeny = map[string]bool{"packagereference": true, "packageversion": true, "packagedownload": true, "globalpackagereference": true,
	"frameworkreference": true, "using": true, "internalsvisibleto": true, "projectcapability": true, "sdkreference": true,
	"assemblyattribute": true, "assemblymetadata": true, "runtimehostconfigurationoption": true, "trimmerrootassembly": true,
	"compilervisibleproperty": true, "compilervisibleitemmetadata": true, "dotnetclitoolreference": true, "watch": true}

func (c *closure) items(f *msFile) {
	for _, it := range f.items {
		lt := strings.ToLower(it.typ)
		if itemDeny[lt] || strings.Contains(it.include, "@(") || strings.Contains(it.include, "%(") {
			continue
		}
		c.env.lenient = it.cond
		vals, err := c.env.eval(it.include, f.path, 0)
		c.env.lenient = false
		if err != nil {
			// Computed at build time, a build output, or a toolchain location: not a repository input.
			if err == errIgnore || err == errExternal || it.inTarget || lt == "hintpath" {
				continue
			}
			c.unresolved(f, it.typ+" "+it.include, err.Error())
			continue
		}
		for _, v := range vals {
			for _, part := range strings.Split(v, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				c.item(f, it, lt, part)
			}
		}
	}
}

// fileItems: item types whose Include always names files.
var fileItems = map[string]bool{"compile": true, "none": true, "content": true, "embeddedresource": true, "additionalfiles": true,
	"analyzer": true, "reference": true, "page": true, "resource": true, "apptxcontent": true, "editorconfigfiles": true,
	"globalanalyzerconfigfiles": true, "codeanalysisdictionary": true, "trimmerrootdescriptor": true, "compilerinputs": true}

func (c *closure) item(f *msFile, it itemDef, lt, v string) {
	b := c.b
	spec := it.typ + " " + it.include
	bases := []string{path.Dir(c.proj)}
	if f.dir != bases[0] {
		bases = append(bases, f.dir)
	}
	for _, tg := range toTarget(v, bases) {
		if tg.outside {
			if lt != "hintpath" {
				c.unresolved(f, spec, "names a path outside the repository: "+v)
			}
			continue
		}
		if lt == "projectreference" {
			c.projectRef(f, tg.rel, it, spec)
			continue
		}
		// The written value decides whether it is a path (tg.rel always has a separator once joined with the base): a word of a
		// non-file item (<SequenceOfIntegers Include="0;1;2" />) that names no file is not an input; a file item type keeps a
		// not-yet-existing bare name (a later-added file).
		pathLike := strings.ContainsAny(v, `/\`) || hasWildcard(v) || b.t.Has(tg.rel) || lt == "hintpath" || fileItems[lt]
		if !pathLike {
			continue
		}
		if hasWildcard(tg.rel) {
			b.edge(c.node, globOf(tg.rel), model.EdgePattern, spec)
			continue
		}
		b.edge(c.node, tg.rel, model.EdgeInclude, spec)
		if b.t.Has(tg.rel) {
			b.role(tg.rel, roleOf(tg.rel))
		}
	}
}

func (c *closure) projectRef(f *msFile, rel string, it itemDef, spec string) {
	b := c.b
	link := func(q string) {
		b.edge(c.node, Node(q), model.EdgeImplicit, "ProjectReference "+q)
	}
	switch {
	case hasWildcard(rel):
		b.edge(c.node, globOf(rel), model.EdgePattern, spec)
		if gp, err := glob.Compile(globOf(rel)); err == nil {
			for p := range b.projSet {
				if gp.Match(p) && p != c.proj {
					link(p)
				}
			}
		}
	case b.projSet[rel]:
		link(rel)
	case b.t.Has(rel):
		// A project this ecosystem does not model (.vcxproj, .wapproj, .proj...): its file and everything below its directory.
		b.edge(c.node, rel, model.EdgeInclude, spec)
		d := path.Dir(rel)
		if d == "." {
			b.edge(c.node, "**", model.EdgePattern, spec)
		} else {
			b.edge(c.node, globOf(d)+"/**", model.EdgePattern, spec)
		}
	default:
		// Missing: the project may be added later (the edge makes the new file a dependency). Without a Condition this is a broken
		// reference.
		b.edge(c.node, rel, model.EdgeInclude, spec)
		if IsProjectFile(rel) {
			b.edge(c.node, Node(rel), model.EdgeImplicit, spec)
		}
		if !it.cond {
			c.unresolved(f, spec, "ProjectReference to a file that does not exist: "+rel)
		}
	}
}

var arcadeRe = regexp.MustCompile(`"Microsoft\.DotNet\.Arcade\.Sdk"\s*:`)

// arcade returns the global.json at or above dir when it selects the Arcade SDK (msbuild-sdks), else "".
func (b *builder) arcade(dir string) string {
	if v, ok := b.arc[dir]; ok {
		return v
	}
	res := ""
	if g, ok := b.nearestAbove(dir, "global.json"); ok {
		if src, err := b.t.Read(g); err == nil && arcadeRe.Match(src) {
			res = g
		}
	}
	b.arc[dir] = res
	return res
}

var mtpRe = regexp.MustCompile(`(?i)"runner"\s*:\s*"Microsoft\.Testing\.Platform"`)

// mtpConfig returns the global.json at or above dir when it selects the Microsoft.Testing.Platform runner for `dotnet test`.
func (b *builder) mtpConfig(dir string) string {
	if v, ok := b.mtp[dir]; ok {
		return v
	}
	res := ""
	if g, ok := b.nearestAbove(dir, "global.json"); ok {
		if src, err := b.t.Read(g); err == nil && mtpRe.Match(src) {
			res = g
		}
	}
	b.mtp[dir] = res
	return res
}
