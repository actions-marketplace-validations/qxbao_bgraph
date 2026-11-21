// Package resolve là bộ phân giải import tĩnh của hệ sinh thái Node (JS/TS) trên tree.Tree.
//
// Nguyên tắc FAIL-CLOSED: không chắc thì trả Unknown, không bao giờ im lặng trả "không phụ thuộc".
// Mọi nơi dùng ngữ nghĩa HỢP: gom mọi đích hợp lý (mọi điều kiện exports, mọi tsconfig, mọi ứng viên paths);
// thừa cạnh được chấp nhận, thiếu cạnh là lỗi.
package resolve

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"bgraph/internal/tree"
)

// Class là phân loại kết quả.
type Class string

const (
	ClassInternal  Class = "internal"
	ClassExternal  Class = "external"
	ClassBuiltin   Class = "builtin"
	ClassUnknown   Class = "unknown"
	ClassGenerated Class = "generated"
)

// Result là kết quả phân giải một specifier.
type Result struct {
	Class        Class
	Files        []string // đích nội bộ (sắp xếp, duy nhất) — hợp mọi đích hợp lý
	Probes       []string // ứng viên đã dò nhưng không tồn tại (cho phân giải lại tăng dần)
	Package      string   // tên package trần cho external/generated
	PackageDir   string   // generated: thư mục package có đầu ra bị thiếu
	CaseMismatch bool     // đích chỉ khớp khi bỏ qua hoa thường (Files là tệp khớp đó)
	// AlsoGenerated: Class internal nhưng một số đích (exports/main/imports) là đầu ra build không có trong cây và không
	// ánh xạ ngược được về nguồn; PackageDir là package sinh ra chúng. Lúc chạy, runner có thể nạp đúng đầu ra đó
	// (ví dụ exports → dist/, types → src/index.d.ts): phụ thuộc thật vào nguồn của package bị mất nếu chỉ giữ Files.
	AlsoGenerated bool
	Detail        string // lý do cho unknown/generated
}

// Alias từ jest moduleNameMapper / vite resolve.alias / bgraph.toml; người gọi đã thay <rootDir>.
type Alias struct {
	Regex   *regexp.Regexp // jest: khớp toàn spec, $1.. thay trong Targets
	Prefix  string         // vite: khớp chính xác hoặc prefix + "/"
	Targets []string       // đường dẫn tương đối gốc repo
}

// Options cấu hình bộ phân giải.
type Options struct {
	Workspaces map[string]string // tên package workspace -> thư mục
	Aliases    []Alias
}

// Resolver phân giải specifier; cache package.json/tsconfig theo đường dẫn. An toàn đồng thời (một khoá chung).
type Resolver struct {
	mu     sync.Mutex
	t      tree.Tree
	opt    Options
	pkgs   map[string]*pkgEntry
	effs   map[string]*effEntry
	errs   map[string]error
	cfgIdx map[string][]string
}

// New tạo bộ phân giải trên cây t.
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

// acc gom kết quả của một lần Resolve.
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

// join nối base/rel; false nếu thoát khỏi gốc repo.
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

// splitPkg tách "name[/sub]" hoặc "@scope/name[/sub]"; sub gồm cả "/" đầu.
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

// ---- dò tệp ----

func (a *acc) tryFile(c string) bool {
	a.cands = append(a.cands, c)
	if a.r.t.Has(c) {
		a.files[c] = struct{}{}
		return true
	}
	a.probes[c] = struct{}{}
	return false
}

// fileCands: chính xác, thêm từng đuôi, thay thế đuôi TS.
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

// pkg đọc package.json của dir; lỗi parse → cfgErr. ok=false khi lỗi.
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

// probePath dò p như tệp rồi như thư mục (luật 3).
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
		for _, f := range []string{pj.Main, pj.Module, pj.Types, pj.Typings, pj.BrowserString} {
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

// probePkgTarget dò một đích trong package pkgDir (main/exports/imports). Nếu không có trong cây:
// ánh xạ ngược outDir → mã nguồn; vẫn không có → ghi nhận "generated".
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

// exportsKV chuẩn hoá exports về bảng khoá con đường. ok=false nếu cấu hình sai (trộn khoá).
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

// matchKey: khoá chính xác trước, sau đó mẫu '*' tốt nhất theo PATTERN_KEY_COMPARE của Node.
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

// collectTargets gom đích từ MỌI khoá điều kiện và mọi phần tử mảng (siêu tập của mọi bộ điều kiện).
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

// resolveInPackage phân giải subpath ("" hoặc "/x") trong package pkgDir bằng exports/main.
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

// importTargets tìm scope package gần nhất và khớp khoá imports (luật 4).
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

// merge hợp kết quả của một lần phân giải con (đích imports không bắt đầu bằng ./).
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

// aliasCandidates trả đường dẫn đích (tương đối gốc repo) của mọi alias khớp spec.
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

// Resolve phân giải spec được import từ tệp from.
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
	// (c) tự tham chiếu: package tổ tiên có name trùng
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
	// (d) workspace theo tên
	if dir, ok := a.r.opt.Workspaces[name]; ok && !seen[dir] {
		pj, _ := a.pkg(dir)
		if pj != nil || a.cfgErr == "" {
			a.resolveInPackage(dir, pj, name, sub)
		}
	}
	if len(a.files) > 0 || a.gen != nil || a.pkgFail != "" {
		return
	}
	// (g) khai báo phụ thuộc
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
		if a.gen != nil {
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

// foldMatch: không ứng viên nào khớp chính xác; thử khớp bỏ qua hoa thường.
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

// ResolvePattern xử lý import() động có tiền tố tĩnh: mọi tệp dưới thư mục của prefix mà đường dẫn có
// prefix và suffix. Tiền tố rỗng/trần không giải được → Unknown. Thư mục không tồn tại → Unknown.
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
	// bare thuần: package đã khai báo → external
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
