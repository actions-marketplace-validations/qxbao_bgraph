// Package resolve is the static import resolver of the Node (JS/TS) ecosystem over tree.Tree.
//
// FAIL-CLOSED principle: when unsure return Unknown, never silently return "no dependency".
// Everywhere uses UNION semantics: collect every plausible target (every exports condition, every tsconfig, every paths candidate);
// extra edges are acceptable, missing edges are a bug.
package resolve

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/qxbao/bgraph/internal/tree"
)

// Class is the classification of a result.
type Class string

const (
	ClassInternal  Class = "internal"
	ClassExternal  Class = "external"
	ClassBuiltin   Class = "builtin"
	ClassUnknown   Class = "unknown"
	ClassGenerated Class = "generated"
)

// Result is the outcome of resolving one specifier.
type Result struct {
	Class        Class
	Files        []string // internal targets (sorted, unique) — union of all plausible targets
	Probes       []string // candidates probed but nonexistent (for incremental re-resolution)
	Package      string   // bare package name for external/generated
	PackageDir   string   // generated: the package directory whose build output is missing
	CaseMismatch bool     // the target matches only when ignoring case (Files is the matching file)
	// AlsoGenerated: Class is internal but some targets (exports/main/imports) are build outputs absent from the tree that cannot be
	// mapped back to source; PackageDir is the package that produces them. At run time the runner may load exactly that output
	// (e.g. exports → dist/, types → src/index.d.ts): the real dependency on the package's source is lost if only Files is kept.
	AlsoGenerated bool
	Detail        string // reason for unknown/generated
}

// Alias from jest moduleNameMapper / vite resolve.alias / bgraph.toml; the caller has already substituted <rootDir>.
type Alias struct {
	Regex   *regexp.Regexp // jest: matches the whole spec, $1.. substituted in Targets
	Prefix  string         // vite: exact match or prefix + "/"
	Targets []string       // repo-root-relative paths
}

// Options configures the resolver.
type Options struct {
	Workspaces map[string]string // workspace package name -> directory
	Aliases    []Alias
}

// Resolver resolves specifiers; caches package.json/tsconfig by path. Safe for concurrent use (one shared lock).
type Resolver struct {
	mu     sync.Mutex
	t      tree.Tree
	opt    Options
	pkgs   map[string]*pkgEntry
	effs   map[string]*effEntry
	errs   map[string]error
	cfgIdx map[string][]string
}

// New creates a resolver over tree t.
func New(t tree.Tree, opt Options) *Resolver {
	return &Resolver{t: t, opt: opt, pkgs: map[string]*pkgEntry{}, effs: map[string]*effEntry{}, errs: map[string]error{}}
}

var exts = []string{".ts", ".tsx", ".d.ts", ".mts", ".d.mts", ".cts", ".d.cts", ".js", ".jsx", ".mjs", ".cjs", ".json", ".node", ".vue", ".svelte"}

var extSubs = map[string][]string{
	".js":  {".ts", ".tsx", ".d.ts"},
	".jsx": {".tsx"},
	".mjs": {".mts", ".d.mts"},
	".cjs": {".cts", ".d.cts"},
}

const sentinel = "\x00"

type genInfo struct{ pkg, dir string }

// acc accumulates the result of one Resolve call.
type acc struct {
	r        *Resolver
	files    map[string]struct{}
	probes   map[string]struct{}
	cands    []string
	cfgErr   string
	pkgFail  string
	gen      *genInfo
	external string
	builtin  bool
}

func newAcc(r *Resolver) *acc {
	return &acc{r: r, files: map[string]struct{}{}, probes: map[string]struct{}{}}
}

func (a *acc) setCfgErr(p string, err error) {
	if a.cfgErr == "" {
		a.cfgErr = fmt.Sprintf("config error: %s: %v", p, err)
	}
}

// join joins base/rel; false if it escapes the repo root.
func join(base, rel string) (string, bool) {
	p := path.Join(base, rel)
	if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

func isRelative(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

func hasSeg(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// splitPkg splits "name[/sub]" or "@scope/name[/sub]"; sub includes the leading "/".
func splitPkg(spec string) (name, sub string, ok bool) {
	parts := strings.SplitN(spec, "/", 3)
	if strings.HasPrefix(spec, "@") {
		if len(parts) < 2 || parts[0] == "@" || parts[1] == "" {
			return "", "", false
		}
		name = parts[0] + "/" + parts[1]
		if len(parts) == 3 {
			sub = "/" + parts[2]
		}
		return name, sub, true
	}
	if parts[0] == "" {
		return "", "", false
	}
	name = parts[0]
	if len(parts) > 1 {
		sub = "/" + strings.Join(parts[1:], "/")
	}
	return name, sub, true
}

// ---- file probing ----

func (a *acc) tryFile(c string) bool {
	a.cands = append(a.cands, c)
	if a.r.t.Has(c) {
		a.files[c] = struct{}{}
		return true
	}
	a.probes[c] = struct{}{}
	return false
}

// fileCands: exact, each extension appended, TS extension substitution.
func (a *acc) fileCands(p string) bool {
	if p == "." {
		return false
	}
	found := a.tryFile(p)
	for _, e := range exts {
		if a.tryFile(p + e) {
			found = true
		}
	}
	ext := path.Ext(p)
	if subs, ok := extSubs[ext]; ok {
		base := strings.TrimSuffix(p, ext)
		for _, s := range subs {
			if a.tryFile(base + s) {
				found = true
			}
		}
	}
	return found
}

func (a *acc) probeIndex(dir string) bool {
	found := false
	for _, e := range exts {
		if a.tryFile(path.Join(dir, "index"+e)) {
			found = true
		}
	}
	return found
}

// pkg reads the package.json of dir; parse error → cfgErr. ok=false on error.
func (a *acc) pkg(dir string) (*PackageJSON, bool) {
	pj, err := a.r.pkgJSON(dir)
	if err != nil {
		p := "package.json"
		if dir != "." {
			p = dir + "/package.json"
		}
		a.setCfgErr(p, err)
		return nil, false
	}
	return pj, true
}

// probePath probes p as a file, then as a directory (rule 3).
func (a *acc) probePath(p string) bool {
	found := a.fileCands(p)
	if a.probeDir(p) {
		found = true
	}
	return found
}

func (a *acc) probeDir(p string) bool {
	found := false
	pj, ok := a.pkg(p)
	if ok && pj == nil {
		pp := path.Join(p, "package.json")
		if !a.r.t.Has(pp) {
			a.probes[pp] = struct{}{}
		}
	}
	if pj != nil {
		for _, f := range []string{pj.Main, pj.Module, pj.Types, pj.Typings, pj.BrowserString, pj.Source} {
			if f != "" && a.probePkgTarget(p, pj.Name, f) {
				found = true
			}
		}
		if pj.Exports != nil {
			if kv, ok := exportsKV(pj.Exports); ok {
				if v, subst, pat, ok := matchKey(kv, "."); ok {
					var t tgt
					collectTargets(v, subst, pat, &t)
					for _, s := range t.strs {
						if strings.HasPrefix(s, "./") && a.probePkgTarget(p, pj.Name, s) {
							found = true
						}
					}
				}
			}
		}
	}
	if a.probeIndex(p) {
		found = true
	}
	return found
}

// probePkgTarget probes a target inside package pkgDir (main/exports/imports). If absent from the tree:
// reverse-map outDir → source; if still absent, record "generated".
func (a *acc) probePkgTarget(pkgDir, name, rel string) bool {
	p, ok := join(pkgDir, rel)
	if !ok {
		a.pkgFail = "path escapes repository root"
		return false
	}
	found := a.fileCands(p)
	if a.probeIndex(p) {
		found = true
	}
	if found {
		return true
	}
	for _, c := range a.mapOutDir(pkgDir, p) {
		if a.fileCands(c) {
			found = true
		}
		if a.probeIndex(c) {
			found = true
		}
	}
	if !found && a.gen == nil {
		if name == "" {
			name = path.Base(pkgDir)
		}
		a.gen = &genInfo{name, pkgDir}
	}
	return found
}

// ---- exports / imports ----

type kv struct {
	keys []string
	vals []*JSONValue
}

// exportsKV normalizes exports into a table of subpath keys. ok=false if the config is invalid (mixed keys).
func exportsKV(v *JSONValue) (kv, bool) {
	if v.Kind == KObject && len(v.Keys) > 0 {
		dot := 0
		for _, k := range v.Keys {
			if strings.HasPrefix(k, ".") {
				dot++
			}
		}
		if dot == len(v.Keys) {
			return kv{v.Keys, v.Vals}, true
		}
		if dot > 0 {
			return kv{}, false
		}
	}
	return kv{[]string{"."}, []*JSONValue{v}}, true
}

// matchKey: exact key first, then the best '*' pattern per Node's PATTERN_KEY_COMPARE.
func matchKey(m kv, sub string) (val *JSONValue, subst string, pat, ok bool) {
	for i, k := range m.keys {
		if k == sub && !strings.Contains(k, "*") {
			return m.vals[i], "", false, true
		}
	}
	bestKey, bestPre := "", -1
	for i, k := range m.keys {
		pi := strings.IndexByte(k, '*')
		if pi < 0 || strings.LastIndexByte(k, '*') != pi {
			continue
		}
		pre, suf := k[:pi], k[pi+1:]
		if !strings.HasPrefix(sub, pre) || len(sub) < len(k) || !strings.HasSuffix(sub, suf) {
			continue
		}
		if pi > bestPre || (pi == bestPre && len(k) > len(bestKey)) {
			bestKey, bestPre = k, pi
			val, subst, pat, ok = m.vals[i], sub[pi:len(sub)-len(suf)], true, true
		}
	}
	return
}

type tgt struct {
	strs  []string
	nulls int
}

// collectTargets gathers targets from EVERY condition key and every array element (superset of any condition set).
func collectTargets(v *JSONValue, subst string, pat bool, t *tgt) {
	switch v.Kind {
	case KString:
		s := v.Str
		if pat {
			s = strings.ReplaceAll(s, "*", subst)
		}
		t.strs = append(t.strs, s)
	case KArray:
		for _, e := range v.Arr {
			collectTargets(e, subst, pat, t)
		}
	case KObject:
		for _, e := range v.Vals {
			collectTargets(e, subst, pat, t)
		}
	case KNull:
		t.nulls++
	}
}

// resolveInPackage resolves a subpath ("" or "/x") in package pkgDir via exports/main.
func (a *acc) resolveInPackage(pkgDir string, pj *PackageJSON, name, sub string) {
	if pj != nil && pj.Exports != nil {
		kvs, ok := exportsKV(pj.Exports)
		if !ok {
			a.pkgFail = fmt.Sprintf("invalid exports in %s (mixed keys)", pkgDir)
			return
		}
		key := "."
		if sub != "" {
			key = "." + sub
		}
		v, subst, pat, ok := matchKey(kvs, key)
		if !ok {
			a.pkgFail = fmt.Sprintf("%q is not exported by package %q (%s)", key, name, pkgDir)
			return
		}
		var t tgt
		collectTargets(v, subst, pat, &t)
		used := 0
		for _, s := range t.strs {
			if !strings.HasPrefix(s, "./") || hasSeg(s, "..") {
				continue
			}
			used++
			a.probePkgTarget(pkgDir, name, s)
		}
		if used == 0 {
			if t.nulls > 0 {
				a.pkgFail = fmt.Sprintf("%q excluded by null export in %q", key, name)
			} else {
				a.pkgFail = fmt.Sprintf("%q has no valid export target in %q", key, name)
			}
		}
		return
	}
	if sub == "" {
		a.probeDir(pkgDir)
		return
	}
	if p, ok := join(pkgDir, sub[1:]); ok {
		a.probePath(p)
	}
}

type impItem struct{ raw, pkgDir, name string }

// importTargets finds the nearest package scope and matches the imports keys (rule 4).
func (a *acc) importTargets(from, spec string) ([]impItem, string) {
	var scope *PackageJSON
	for d := dirOf(from); ; d = path.Dir(d) {
		if hasSeg(d, "node_modules") {
			return nil, "importer inside node_modules"
		}
		pj, ok := a.pkg(d)
		if !ok {
			return nil, a.cfgErr
		}
		if pj != nil {
			scope = pj
			break
		}
		if d == "." {
			break
		}
	}
	if scope == nil {
		return nil, "no package.json scope for subpath import"
	}
	if scope.Imports == nil || scope.Imports.Kind != KObject {
		return nil, fmt.Sprintf("package scope %s has no imports", scope.Path)
	}
	v, subst, pat, ok := matchKey(kv{scope.Imports.Keys, scope.Imports.Vals}, spec)
	if !ok {
		return nil, fmt.Sprintf("no imports key of %s matches %q", scope.Path, spec)
	}
	var t tgt
	collectTargets(v, subst, pat, &t)
	var items []impItem
	for _, s := range t.strs {
		if hasSeg(s, "..") {
			continue
		}
		items = append(items, impItem{s, scope.Dir, scope.Name})
	}
	if len(items) == 0 {
		if t.nulls > 0 {
			return nil, fmt.Sprintf("%q excluded by null target in %s", spec, scope.Path)
		}
		return nil, fmt.Sprintf("%q has no valid target in %s", spec, scope.Path)
	}
	return items, ""
}

func (a *acc) imports(from, spec string, depth int) {
	items, fail := a.importTargets(from, spec)
	if fail != "" {
		if a.pkgFail == "" {
			a.pkgFail = fail
		}
		return
	}
	for _, it := range items {
		if strings.HasPrefix(it.raw, "./") {
			a.probePkgTarget(it.pkgDir, it.name, it.raw)
		} else if depth < 8 && !strings.HasPrefix(it.raw, "#") {
			a.merge(a.r.resolve(from, it.raw, depth+1))
		}
	}
}

// merge unions the result of a sub-resolution (an imports target not starting with ./).
func (a *acc) merge(res Result) {
	for _, f := range res.Files {
		a.files[f] = struct{}{}
	}
	for _, p := range res.Probes {
		a.probes[p] = struct{}{}
	}
	switch res.Class {
	case ClassExternal:
		a.external = res.Package
	case ClassBuiltin:
		a.builtin = true
	case ClassGenerated:
		if a.gen == nil {
			a.gen = &genInfo{res.Package, res.PackageDir}
		}
	case ClassUnknown:
		if strings.HasPrefix(res.Detail, "config error") && a.cfgErr == "" {
			a.cfgErr = res.Detail
		} else if a.pkgFail == "" {
			a.pkgFail = res.Detail
		}
	}
}

// ---- alias ----

func expandTarget(spec string, m []int, t string) string {
	var sb strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] != '$' {
			sb.WriteByte(t[i])
			continue
		}
		j := i + 1
		for j < len(t) && t[j] >= '0' && t[j] <= '9' {
			j++
		}
		if j == i+1 {
			sb.WriteByte('$')
			continue
		}
		n := 0
		for _, c := range t[i+1 : j] {
			n = n*10 + int(c-'0')
		}
		if 2*n+1 < len(m) && m[2*n] >= 0 {
			sb.WriteString(spec[m[2*n]:m[2*n+1]])
		}
		i = j - 1
	}
	return sb.String()
}

// aliasCandidates returns the target paths (repo-root-relative) of every alias matching spec.
func (a *acc) aliasCandidates(spec string) []string {
	var out []string
	rel := isRelative(spec)
	for _, al := range a.r.opt.Aliases {
		var raw []string
		if al.Regex != nil {
			m := al.Regex.FindStringSubmatchIndex(spec)
			if m == nil {
				continue
			}
			for _, t := range al.Targets {
				raw = append(raw, expandTarget(spec, m, t))
			}
		} else if al.Prefix != "" && !rel {
			pre := strings.TrimSuffix(al.Prefix, "/")
			var rest string
			switch {
			case spec == pre:
			case strings.HasPrefix(spec, pre+"/"):
				rest = spec[len(pre):]
			default:
				continue
			}
			for _, t := range al.Targets {
				raw = append(raw, t+rest)
			}
		}
		for _, t := range raw {
			if p, ok := join(".", t); ok {
				out = append(out, p)
			} else if a.pkgFail == "" {
				a.pkgFail = "alias target escapes repository root"
			}
		}
	}
	return out
}

// ---- Resolve ----

// Resolve resolves the spec imported from file from.
func (r *Resolver) Resolve(from, spec string) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resolve(from, spec, 0)
}

func (r *Resolver) resolve(from, spec string, depth int) Result {
	if i := strings.IndexByte(spec, '?'); i >= 0 {
		spec = spec[:i]
	}
	if spec == "" {
		return Result{Class: ClassUnknown, Detail: "empty specifier"}
	}
	a := newAcc(r)
	fold := false
	for _, c := range a.aliasCandidates(spec) {
		a.probePath(c)
	}
	switch {
	case isRelative(spec):
		fold = true
		if p, ok := join(dirOf(from), spec); ok {
			a.probePath(p)
		} else {
			a.pkgFail = "path escapes repository root"
		}
	case strings.HasPrefix(spec, "/"):
		fold = true
		bases := []string{"."}
		for d := dirOf(from); ; d = path.Dir(d) {
			pj, ok := a.pkg(d)
			if pj != nil {
				if d != "." {
					bases = append(bases, d)
				}
				break
			}
			if !ok || d == "." {
				break
			}
		}
		for _, b := range bases {
			if p, ok := join(b, spec[1:]); ok {
				a.probePath(p)
			}
		}
	case strings.HasPrefix(spec, "#"):
		fold = true
		// tsconfig paths may map "#x" keys too (svelte: "#compiler"); union with package.json "imports".
		for _, c := range a.tsCandidates(from, spec) {
			a.probePath(c)
		}
		a.imports(from, spec, depth)
	default:
		a.bare(from, spec)
	}
	return a.finish(spec, fold)
}

func (a *acc) bare(from, spec string) {
	for _, c := range a.tsCandidates(from, spec) {
		a.probePath(c)
	}
	if isBuiltin(spec) {
		if len(a.files) == 0 {
			a.builtin = true
		}
		return
	}
	name, sub, ok := splitPkg(spec)
	if !ok {
		a.pkgFail = "invalid package specifier"
		return
	}
	seen := map[string]bool{}
	// (c) self-reference: an ancestor package has the same name
	for d := dirOf(from); ; d = path.Dir(d) {
		if pj, ok := a.pkg(d); pj != nil && pj.Name == name && !seen[d] {
			seen[d] = true
			a.resolveInPackage(d, pj, name, sub)
		} else if !ok {
			break
		}
		if d == "." {
			break
		}
	}
	// (d) workspace by name
	if dir, ok := a.r.opt.Workspaces[name]; ok && !seen[dir] {
		pj, _ := a.pkg(dir)
		if pj != nil || a.cfgErr == "" {
			a.resolveInPackage(dir, pj, name, sub)
		}
	}
	if len(a.files) > 0 || a.gen != nil || a.pkgFail != "" {
		return
	}
	// (g) declared dependency
	if a.declared(from, name) {
		a.external = name
	}
}

func (a *acc) declared(from, name string) bool {
	types := "@types/" + name
	if strings.HasPrefix(name, "@") {
		types = "@types/" + strings.Replace(name[1:], "/", "__", 1)
	}
	for d := dirOf(from); ; d = path.Dir(d) {
		pj, _ := a.pkg(d)
		if pj != nil && (pj.HasDep(name) || pj.HasDep(types)) {
			return true
		}
		if d == "." {
			return false
		}
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (a *acc) finish(spec string, fold bool) Result {
	res := Result{Files: sortedKeys(a.files), Probes: sortedKeys(a.probes)}
	switch {
	case a.cfgErr != "":
		res.Class, res.Detail = ClassUnknown, a.cfgErr
	case len(res.Files) > 0:
		res.Class = ClassInternal
		// Build output missing next to a real target: if the real target includes CODE files (not only .d.ts) — the "source condition" pattern
		// ("@pkg/source": "./src/index.ts" next to "import": "./dist/index.js") — the output is compiled from that very entry,
		// so the edge to it already covers the dependency. With only .d.ts (preact: types → src/index.d.ts, main → dist/) the real code lives
		// in the build output: keep the signal so the importer depends on the whole package.
		if a.gen != nil && !hasCodeFile(res.Files) {
			res.AlsoGenerated, res.Package, res.PackageDir = true, a.gen.pkg, a.gen.dir
		}
	case fold && a.foldMatch(&res):
	case a.gen != nil:
		res.Class, res.Package, res.PackageDir = ClassGenerated, a.gen.pkg, a.gen.dir
		res.Detail = fmt.Sprintf("build output of package %q is not in the tree", a.gen.pkg)
	case a.pkgFail != "":
		res.Class, res.Detail = ClassUnknown, a.pkgFail
	case a.external != "":
		res.Class, res.Package, res.Probes = ClassExternal, a.external, nil
	case a.builtin:
		res.Class, res.Probes = ClassBuiltin, nil
	default:
		res.Class = ClassUnknown
		if isRelative(spec) || strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "#") {
			res.Detail = fmt.Sprintf("missing: no file matches %q", spec)
		} else {
			res.Detail = fmt.Sprintf("undeclared package %q", spec)
		}
	}
	return res
}

// foldMatch: no candidate matches exactly; try a case-insensitive match.
func (a *acc) foldMatch(res *Result) bool {
	set := map[string]struct{}{}
	for _, c := range a.cands {
		for _, m := range a.r.t.FoldLookup(c) {
			set[m] = struct{}{}
		}
	}
	if len(set) == 0 {
		return false
	}
	res.Class, res.CaseMismatch, res.Files = ClassInternal, true, sortedKeys(set)
	res.Detail = "matched ignoring case only"
	return true
}

// ---- ResolvePattern ----

// ResolvePattern handles dynamic import() with a static prefix: every file under the prefix's directory whose path has
// the prefix and suffix. An empty/bare prefix cannot be resolved → Unknown. A nonexistent directory → Unknown.
func (r *Resolver) ResolvePattern(from, spec, suffix string) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	if spec == "" {
		return Result{Class: ClassUnknown, Detail: "dynamic specifier without static prefix"}
	}
	if strings.HasPrefix(spec, "node:") {
		return Result{Class: ClassBuiltin}
	}
	a := newAcc(r)
	full := spec + sentinel + suffix
	var prefixes []string
	cut := func(p string) {
		if i := strings.Index(p, sentinel); i >= 0 {
			prefixes = append(prefixes, p[:i])
		}
	}
	switch {
	case isRelative(spec):
		if p, ok := join(dirOf(from), full); ok {
			cut(p)
		} else {
			return Result{Class: ClassUnknown, Detail: "path escapes repository root"}
		}
	case strings.HasPrefix(spec, "/"):
		return Result{Class: ClassUnknown, Detail: "absolute dynamic specifier"}
	case strings.HasPrefix(spec, "#"):
		items, fail := a.importTargets(from, full)
		if fail != "" {
			return Result{Class: ClassUnknown, Detail: fail}
		}
		for _, it := range items {
			if strings.HasPrefix(it.raw, "./") {
				if p, ok := join(it.pkgDir, it.raw); ok {
					cut(p)
				}
			}
		}
	}
	if !strings.HasPrefix(spec, "#") && !strings.HasPrefix(spec, "/") && !isRelative(spec) {
		for _, c := range a.aliasCandidates(full) {
			cut(c)
		}
		for _, c := range a.tsCandidates(from, full) {
			cut(c)
		}
	} else if isRelative(spec) {
		for _, c := range a.aliasCandidates(full) {
			cut(c)
		}
	}
	if a.cfgErr != "" {
		return Result{Class: ClassUnknown, Detail: a.cfgErr}
	}
	var res Result
	any := false
	for _, pfx := range prefixes {
		pfx = strings.TrimPrefix(pfx, "./")
		if pfx == "." {
			pfx = ""
		}
		dir := "."
		if strings.HasSuffix(pfx, "/") {
			dir = strings.TrimSuffix(pfx, "/")
		} else if pfx != "" {
			dir = path.Dir(pfx)
		}
		if dir == "" {
			dir = "."
		}
		if dir != "." && !r.t.IsDir(dir) {
			a.probes[dir] = struct{}{}
			continue
		}
		any = true
		for _, f := range r.t.Files() {
			if strings.HasPrefix(f, pfx) && strings.HasSuffix(f, suffix) && len(f) >= len(pfx)+len(suffix) {
				a.files[f] = struct{}{}
			}
		}
	}
	res.Files, res.Probes = sortedKeys(a.files), sortedKeys(a.probes)
	if any {
		res.Class = ClassInternal
		return res
	}
	// bare only: declared package → external
	if !isRelative(spec) && !strings.HasPrefix(spec, "#") && len(prefixes) == 0 {
		if name, sub, ok := splitPkg(strings.TrimSuffix(spec, "/")); ok && sub != "" || ok && strings.HasSuffix(spec, "/") {
			if isBuiltin(name) {
				return Result{Class: ClassBuiltin}
			}
			if a.declared(from, name) {
				return Result{Class: ClassExternal, Package: name}
			}
		}
	}
	res.Class, res.Detail = ClassUnknown, fmt.Sprintf("dynamic prefix %q does not map to an existing directory", spec)
	return res
}

// hasCodeFile: there is a JS/TS code file (not a .d.ts/.d.mts/.d.cts declaration, not JSON/asset).
func hasCodeFile(files []string) bool {
	for _, f := range files {
		if strings.HasSuffix(f, ".d.ts") || strings.HasSuffix(f, ".d.mts") || strings.HasSuffix(f, ".d.cts") {
			continue
		}
		switch path.Ext(f) {
		case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
			return true
		}
	}
	return false
}
