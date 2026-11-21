// Package node là hệ sinh thái Node (JS/TS, CJS/ESM) của bgraph (mục 6.1).
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/node/resolve"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name là tên hệ sinh thái.
const Name = "node"

// AnalyzerVersion: tăng mỗi khi đổi luật phân tích/phân giải để vô hiệu snapshot phân tích đã lưu.
const AnalyzerVersion = "node-18"

// Eco là hệ sinh thái Node.
type Eco struct {
	Rules *rules.File // [[generated]]; có thể nil
}

// New tạo hệ sinh thái Node.
func New(r *rules.File) *Eco { return &Eco{Rules: r} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// pkgInfo là phần package.json mà lớp hệ sinh thái cần (resolver tự đọc phần phân giải).
type pkgInfo struct {
	Name            string            `json:"name"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Jest            json.RawMessage   `json:"jest"`
}

func readPkg(t tree.Tree, p string) (*pkgInfo, error) {
	b, err := t.Read(p)
	if err != nil {
		return nil, err
	}
	var pi pkgInfo
	if err := json.Unmarshal(b, &pi); err != nil {
		return nil, err
	}
	return &pi, nil
}

// Analyze phân tích cây t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	files := t.Files()

	// 1. Dự án = mọi package.json ngoài node_modules.
	pkgs := map[string]*pkgInfo{}
	workspaces := map[string]string{}
	for _, p := range files {
		if path.Base(p) != "package.json" || inNodeModules(p) {
			continue
		}
		dir := path.Dir(p)
		a.Roles[p] = model.RoleManifest
		pi, err := readPkg(t, p)
		if err != nil {
			// package.json hỏng: dự án vẫn tồn tại; mọi tệp trong đó coi như không chắc.
			pi = &pkgInfo{}
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: p, Spec: "package.json", Class: model.UnresolvedUnknown,
				ScopeTier: model.TierModule, ScopeDir: dir, Detail: "package.json không đọc được: " + err.Error()})
		}
		pkgs[dir] = pi
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: dir, Name: pi.Name})
		if pi.Name != "" {
			// Tập cha: mọi package có tên trong repo đều là ứng viên workspace (chọn thừa không hại). Trùng tên: giữ thư mục
			// nông nhất rồi theo thứ tự chữ (tất định).
			if old, ok := workspaces[pi.Name]; !ok || shallower(dir, old) {
				workspaces[pi.Name] = dir
			}
		}
	}
	if len(pkgs) == 0 {
		return a, nil
	}
	projDirs := make([]eco.Project, len(a.Projects))
	copy(projDirs, a.Projects)

	// 2. Cấu hình runner theo dự án: alias, phụ thuộc ngầm, phần không đọc được.
	// Tệp cấu hình runner ở MỌI thư mục, gán cho dự án gần nhất (cấu hình lồng như tests/unit/jest.config.mjs).
	cfgByProj := map[string][]string{}
	for _, p := range files {
		if isRunnerConfig(p) && !inNodeModules(p) {
			if pr, ok := eco.NearestProject(projDirs, p); ok {
				cfgByProj[pr.Dir] = append(cfgByProj[pr.Dir], p)
			}
		}
	}
	configs := map[string]testConfig{}
	var aliases []resolve.Alias
	for dir, pi := range pkgs {
		tc := readTestConfig(t, dir, pi.Jest, cfgByProj[dir])
		configs[dir] = tc
		aliases = append(aliases, tc.aliases...)
		for _, f := range tc.files {
			a.Roles[f] = model.RoleConfig
		}
	}
	slices.SortFunc(aliases, func(x, y resolve.Alias) int { return strings.Compare(aliasKey(x), aliasKey(y)) })
	r := resolve.New(t, resolve.Options{Workspaces: workspaces, Aliases: aliases})

	projCache := map[string]string{} // thư mục → dự án gần nhất
	projectOf := func(p string) string {
		d := path.Dir(p)
		if v, ok := projCache[d]; ok {
			return v
		}
		v := ""
		if pr, ok := eco.NearestProject(projDirs, p); ok {
			v = pr.Dir
		}
		projCache[d] = v
		return v
	}

	// Mock gốc của Jest: <dự án hoặc thư mục roots>/__mocks__/<tên module>.<đuôi> thay module ngoài cùng tên khi chạy test.
	jestMocks := map[string]map[string][]string{}
	for dir := range pkgs {
		if detectRunner(pkgs, dir) != "jest" {
			continue
		}
		mockDirs := []string{path.Join(dir, "__mocks__")}
		for _, rt := range configs[dir].roots {
			mockDirs = append(mockDirs, path.Join(rt, "__mocks__"))
		}
		for _, f := range files {
			if !isJS(f) {
				continue
			}
			for _, md := range mockDirs {
				if strings.HasPrefix(f, md+"/") {
					name := strings.TrimPrefix(f, md+"/")
					name = strings.TrimSuffix(name, path.Ext(name))
					name = strings.TrimSuffix(name, "/index")
					if jestMocks[dir] == nil {
						jestMocks[dir] = map[string][]string{}
					}
					jestMocks[dir][name] = append(jestMocks[dir][name], f)
				}
			}
		}
	}

	// 3. Quét và phân giải mọi tệp JS/TS thuộc một dự án.
	var tests []string
	for _, p := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if inNodeModules(p) {
			continue
		}
		proj := projectOf(p)
		if owner, ok := snapshotOwner(p); ok && proj != "" {
			a.Roles[p] = model.RoleData
			a.Edges = append(a.Edges, model.Edge{From: owner, To: p, Kind: model.EdgeImplicit, Provenance: model.ProvScan, Spec: "snapshot"})
			continue
		}
		if isTSConfig(p) {
			a.Roles[p] = model.RoleConfig
			continue
		}
		if isLockfile(p) {
			a.Roles[p] = model.RoleLock
			continue
		}
		if !isJS(p) && !isSFC(p) {
			continue
		}
		if proj == "" {
			continue // JS ngoài mọi package.json: không thuộc hệ sinh thái này (bộ chọn coi là tệp lạ)
		}
		// Cấu hình runner ở dự án tổ tiên (jest.config.js ở gốc monorepo) cũng áp cho tệp trong package con: hợp.
		inConfig := false
		for d := proj; ; d = path.Dir(d) {
			if tc, ok := configs[d]; ok && tc.isTest(p) {
				inConfig = true
				break
			}
			if d == "." || d == "/" || d == "" {
				break
			}
		}
		if isTestFile(p, detectRunner(pkgs, proj)) || inConfig {
			a.Roles[p] = model.RoleTest
			tests = append(tests, p)
		} else if _, ok := a.Roles[p]; !ok {
			a.Roles[p] = model.RoleSource
		}
		res, err := sc.Scan(t, p)
		if err != nil {
			return nil, fmt.Errorf("quét %s: %w", p, err)
		}
		if res.Uncertain {
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: p, Spec: "*", Class: model.UnresolvedUncertainScan,
				ScopeTier: model.TierModule, ScopeDir: "", Detail: res.UncertainReason})
		}
		for _, imp := range res.Imports {
			e.addImport(a, r, t, p, proj, imp, jestMocks[proj])
		}
	}
	for _, ce := range r.ConfigErrors() {
		dir := projectOf(ce.Path)
		a.Unresolved = append(a.Unresolved, model.Unresolved{File: ce.Path, Spec: path.Base(ce.Path), Class: model.UnresolvedUnknown,
			ScopeTier: model.TierModule, ScopeDir: dir, Detail: "cấu hình không đọc được: " + ce.Err.Error()})
		// Mọi tệp của dự án có thể phân giải sai: gắn tệp cấu hình hỏng làm phụ thuộc của mọi test trong dự án.
	}

	// 4. Đơn vị test và phụ thuộc ngầm.
	for _, tf := range tests {
		dir := projectOf(tf)
		a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: tf, Project: dir, Files: []string{tf}, Runner: detectRunner(pkgs, dir)})
	}
	// Mẫu tệp test do code tính (template/biến): không biết tệp nào là test → đơn vị "cả dự án" (runner chạy cả dự án).
	// Không có nó, test chưa nhận ra sẽ không bao giờ được chọn (babel: testRegex là template có biểu thức).
	for dir, tc := range configs {
		if tc.testsOpaque {
			a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: wholeUnitID(dir), Project: dir, Runner: detectRunner(pkgs, dir), Whole: true})
		}
	}
	for dir, tc := range configs {
		var projTests []string
		for _, tf := range tests {
			if projectOf(tf) == dir || (eco.Within(tf, dir) && dir == ".") {
				projTests = append(projTests, tf)
			}
		}
		targets := append([]string{}, tc.files...)
		targets = append(targets, tc.implicit...)
		// __mocks__ ở gốc dự án hoặc kề module: phụ thuộc của mọi test trong dự án (tập cha).
		for _, p := range files {
			if strings.Contains("/"+p, "/__mocks__/") && projectOf(p) == dir {
				targets = append(targets, p)
				if _, ok := a.Roles[p]; !ok {
					a.Roles[p] = model.RoleSource
				}
			}
		}
		slices.Sort(targets)
		targets = slices.Compact(targets)
		globs := slices.Clone(tc.implicitGlobs)
		slices.Sort(globs)
		globs = slices.Compact(globs)
		for _, tf := range projTests {
			// Đường dẫn chính xác (có thể chứa "[id]", "{x}") là cạnh thường; glob (đã thoát phần chữ nghĩa) là cạnh pattern.
			for _, tg := range targets {
				if tg != tf {
					a.Edges = append(a.Edges, model.Edge{From: tf, To: tg, Kind: model.EdgeImplicit, Provenance: model.ProvScan, Spec: "test config"})
				}
			}
			for _, g := range globs {
				a.Edges = append(a.Edges, model.Edge{From: tf, To: g, Kind: model.EdgePattern, Provenance: model.ProvScan, Spec: "test config"})
			}
			for _, op := range tc.opaque {
				a.Unresolved = append(a.Unresolved, model.Unresolved{File: tf, Spec: op, Class: model.UnresolvedUnknown,
					ScopeTier: model.TierModule, ScopeDir: dir, Detail: model.ReasonConfigIsCode + ": " + op})
			}
		}
	}
	return a, nil
}

func aliasKey(a resolve.Alias) string {
	if a.Regex != nil {
		return "r:" + a.Regex.String() + strings.Join(a.Targets, ",")
	}
	return "p:" + a.Prefix + strings.Join(a.Targets, ",")
}

func shallower(a, b string) bool {
	da, db := strings.Count(a, "/"), strings.Count(b, "/")
	if da != db {
		return da < db
	}
	return a < b
}

func edgeKind(k model.ImportKind) model.EdgeKind {
	switch k {
	case model.ImportTypeOnly:
		return model.EdgeTypeOnly
	case model.ImportReExport:
		return model.EdgeReExport
	case model.ImportMock:
		return model.EdgeMock
	}
	return model.EdgeImport
}

// addImport phân giải một import và ghi cạnh hoặc Unresolved.
func (e *Eco) addImport(a *eco.Analysis, r *resolve.Resolver, t tree.Tree, from, proj string, imp model.ImportRef, mocks map[string][]string) {
	unres := func(class model.UnresolvedClass, scope, detail string) {
		a.Unresolved = append(a.Unresolved, model.Unresolved{File: from, Spec: imp.Spec, Class: class, Line: imp.Line,
			ScopeTier: model.TierModule, ScopeDir: scope, Detail: detail})
	}
	switch {
	case imp.Kind == model.ImportGlob:
		spec := imp.Spec
		if strings.HasPrefix(spec, "!") {
			return // phủ định chỉ bớt tệp: bỏ qua là tập cha
		}
		var g string
		switch {
		case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
			g = path.Join(path.Dir(from), spec)
		case strings.HasPrefix(spec, "/"):
			g = path.Join(proj, spec)
		default:
			unres(model.UnresolvedDynamic, "", "glob không tương đối")
			return
		}
		if g == ".." || strings.HasPrefix(g, "../") {
			unres(model.UnresolvedDynamic, "", "glob ra ngoài repo")
			return
		}
		a.Edges = append(a.Edges, model.Edge{From: from, To: g, Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		return
	case imp.Dynamic:
		if imp.Spec == "" {
			unres(model.UnresolvedDynamic, "", "đối số không phải hằng")
			return
		}
		res := r.ResolvePattern(from, imp.Spec, imp.Suffix)
		if res.Class != resolve.ClassInternal {
			unres(model.UnresolvedDynamic, "", "tiền tố tĩnh không phân giải được: "+res.Detail)
			return
		}
		for _, f := range res.Files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: model.EdgeImport, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		}
		// Tệp mới khớp mẫu: cạnh glob tới thư mục của tiền tố.
		if dir := patternDir(from, imp.Spec, res.Files); dir != "" {
			a.Edges = append(a.Edges, model.Edge{From: from, To: escapeGlob(dir) + "/**", Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		} else {
			unres(model.UnresolvedDynamic, "", "không xác định được thư mục của mẫu")
		}
		return
	}
	// new URL('.', import.meta.url) / new URL('./dir/', ...): tham chiếu thư mục (thường để dựng __dirname hoặc đọc tệp
	// trong đó) → phụ thuộc mọi tệp dưới thư mục (tập cha).
	if imp.Kind == model.ImportURL && (imp.Spec == "." || imp.Spec == ".." || strings.HasSuffix(imp.Spec, "/")) {
		d := path.Join(path.Dir(from), imp.Spec)
		if d == ".." || strings.HasPrefix(d, "../") {
			unres(model.UnresolvedUnknown, "", "URL ra ngoài repo")
			return
		}
		g := "**"
		if d != "." {
			g = escapeGlob(d) + "/**"
		}
		a.Edges = append(a.Edges, model.Edge{From: from, To: g, Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		return
	}
	res := r.Resolve(from, imp.Spec)
	// SFC: `src="x.ts"`, `@import "a.css"` là tương đối trước (cú pháp HTML/CSS), dù không có "./".
	if isSFC(from) && !strings.HasPrefix(imp.Spec, ".") && !strings.HasPrefix(imp.Spec, "/") && !strings.HasPrefix(imp.Spec, "#") {
		if rel := r.Resolve(from, "./"+imp.Spec); rel.Class == resolve.ClassInternal {
			if res.Class == resolve.ClassInternal {
				rel.Files = append(rel.Files, res.Files...)
			}
			res = rel
		}
	}
	// Jest: module ngoài (bare) có mock gốc <root>/__mocks__/<tên> thì khi chạy test Jest dùng mock thay module thật
	// (kể cả khi module "thật" là đầu ra build không có trong cây). Cạnh tới mock là phụ thuộc chính xác.
	if files := mocks[imp.Spec]; len(files) > 0 && isBareSpec(imp.Spec) {
		for _, f := range files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: model.EdgeMock, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		}
		if res.Class != resolve.ClassInternal {
			return // external/generated/unknown: lúc chạy test, Jest nạp mock chứ không nạp module này
		}
		res.AlsoGenerated = false
	}
	switch res.Class {
	case resolve.ClassInternal:
		k := edgeKind(imp.Kind)
		for _, f := range res.Files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: k, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
		}
		if res.AlsoGenerated {
			e.addGenerated(a, from, imp, res)
		}
		if res.CaseMismatch {
			a.Warnings = append(a.Warnings, model.Widening{Tier: model.TierNone, Reason: model.ReasonCaseMismatch, Path: from,
				Detail: fmt.Sprintf("%s:%d %q chỉ khớp %s khi bỏ qua hoa thường (chạy được trên Windows/macOS, hỏng trên Linux)",
					from, imp.Line, imp.Spec, strings.Join(res.Files, ", "))})
		}
		// Probe không thành cạnh: cây hiện tại luôn được phân giải lại đầy đủ nên tệp mới che tệp cũ đã hiện thành cạnh
		// import tới tệp mới (tệp đổi). res.Probes dành cho chỉ mục tăng dần sau này.
	case resolve.ClassExternal, resolve.ClassBuiltin:
	case resolve.ClassGenerated:
		e.addGenerated(a, from, imp, res)
	default: // unknown
		if strings.HasPrefix(res.Detail, "undeclared package") && isBareSpec(imp.Spec) {
			// Bare specifier không phân giải được chỉ có thể trỏ vào repo qua một cơ chế thư mục gốc chưa mô hình
			// (NODE_PATH, moduleDirectories, babel module-resolver...): tệp đích khi đó có đường dẫn kết thúc bằng spec,
			// spec.<đuôi> hoặc nằm dưới spec/. Cạnh glob tới các đường dẫn đó giữ fail-closed mà không nới cả dự án.
			// Nút ảo theo spec để nhiều importer dùng chung một bộ pattern.
			v := bareNode(imp.Spec)
			a.Edges = append(a.Edges, model.Edge{From: from, To: v, Kind: model.EdgeImport, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
			esc := escapeGlob(imp.Spec)
			for _, g := range []string{"**/" + esc, "**/" + esc + ".*", "**/" + esc + "/**"} {
				a.Edges = append(a.Edges, model.Edge{From: v, To: g, Kind: model.EdgePattern, Spec: imp.Spec, Provenance: model.ProvScan})
			}
			// Chỉ để báo cáo (bgraph unresolved): ScopeTier none → bộ chọn không gieo hạt.
			a.Unresolved = append(a.Unresolved, model.Unresolved{File: from, Spec: imp.Spec, Class: model.UnresolvedUnknown, Line: imp.Line,
				ScopeTier: model.TierNone, ScopeDir: "", Detail: res.Detail + " (theo dõi qua glob **/" + imp.Spec + ")"})
			return
		}
		scope := proj
		if strings.HasPrefix(imp.Spec, "./") || strings.HasPrefix(imp.Spec, "../") {
			d := path.Join(path.Dir(from), path.Dir(imp.Spec))
			if d == ".." || strings.HasPrefix(d, "../") {
				d = ""
			}
			scope = d
		}
		unres(model.UnresolvedUnknown, scope, res.Detail)
	}
}

// patternDir: thư mục chứa phần tĩnh của import động ("./locales/en-" từ src/a.ts → "src/locales").
func patternDir(from, spec string, files []string) string {
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		d := path.Join(path.Dir(from), spec)
		if !strings.HasSuffix(spec, "/") {
			d = path.Dir(d)
		}
		if d == ".." || strings.HasPrefix(d, "../") {
			return ""
		}
		return d
	}
	// alias/#imports: dùng thư mục chung của các tệp đã khớp.
	if len(files) == 0 {
		return ""
	}
	d := path.Dir(files[0])
	for _, f := range files[1:] {
		for !eco.Within(f, d) {
			d = path.Dir(d)
		}
	}
	return d
}

func escapeGlob(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`*?[]{}\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// detectRunner đoán runner của dự án dir từ dependency của chính nó rồi của gốc.
func detectRunner(pkgs map[string]*pkgInfo, dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if pi, ok := pkgs[d]; ok {
			for _, name := range []string{"vitest", "jest", "@playwright/test", "mocha", "ava"} {
				if pi.DevDependencies[name] != "" || pi.Dependencies[name] != "" {
					return strings.TrimPrefix(strings.TrimSuffix(name, "/test"), "@")
				}
			}
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return "node-test"
}

// isBareSpec: tên package (có thể kèm subpath), không tương đối/tuyệt đối/#/URL.
func isBareSpec(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "#") || strings.Contains(s, ":") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// bareNode là nút ảo của một bare specifier chưa khai báo (không trùng đường dẫn tệp nào của repo).
func bareNode(spec string) string { return "<bare:" + spec + ">" }

// addGenerated ghi phụ thuộc vào đầu ra build của package res.PackageDir: theo [[generated]] (cạnh glob tới nguồn) nếu
// có luật, ngược lại Unresolved phạm vi package đó (mọi thay đổi trong package làm importer bị ảnh hưởng).
func (e *Eco) addGenerated(a *eco.Analysis, from string, imp model.ImportRef, res resolve.Result) {
	if e.Rules != nil {
		var srcs []string
		for _, g := range e.Rules.Generated {
			hit := path.Clean(g.Owner) == res.PackageDir
			for _, o := range g.Outputs {
				if pre := o.StaticPrefix(); pre != "" && (eco.Within(pre, res.PackageDir) || eco.Within(res.PackageDir, pre)) {
					hit = true
				}
			}
			if hit {
				srcs = append(srcs, g.RawSources...)
			}
		}
		if len(srcs) > 0 {
			for _, s := range srcs {
				a.Edges = append(a.Edges, model.Edge{From: from, To: s, Kind: model.EdgePattern, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvRule})
			}
			return
		}
	}
	detail := fmt.Sprintf("build output of package %q is not in the tree", res.Package)
	if res.Detail != "" {
		detail = res.Detail
	}
	a.Unresolved = append(a.Unresolved, model.Unresolved{File: from, Spec: imp.Spec, Class: model.UnresolvedUnknown, Line: imp.Line,
		ScopeTier: model.TierModule, ScopeDir: res.PackageDir, Detail: model.ReasonGeneratedOutput + ": " + detail})
}

// wholeUnitID là định danh của đơn vị "cả bộ test của dự án dir".
func wholeUnitID(dir string) string { return "<all-tests:" + dir + ">" }
