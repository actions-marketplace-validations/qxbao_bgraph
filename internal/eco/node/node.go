// Package node là hệ sinh thái Node (JS/TS, CJS/ESM) của bgraph (mục 6.1).
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"bgraph/internal/eco"
	"bgraph/internal/eco/node/resolve"
	"bgraph/internal/model"
	"bgraph/internal/rules"
	"bgraph/internal/tree"
)

// Name là tên hệ sinh thái.
const Name = "node"

// AnalyzerVersion: tăng mỗi khi đổi luật phân tích/phân giải để vô hiệu snapshot phân tích đã lưu.
const AnalyzerVersion = "node-2"

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
	configs := map[string]testConfig{}
	var aliases []resolve.Alias
	for dir, pi := range pkgs {
		tc := readTestConfig(t, dir, pi.Jest)
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
		if isTestFile(p) {
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
			e.addImport(a, r, t, p, proj, imp)
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
		for _, tf := range projTests {
			for _, tg := range targets {
				if tg == tf {
					continue
				}
				k := model.EdgeImplicit
				if strings.HasSuffix(tg, "/**") {
					k = model.EdgePattern
				}
				a.Edges = append(a.Edges, model.Edge{From: tf, To: tg, Kind: k, Provenance: model.ProvScan, Spec: "test config"})
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
func (e *Eco) addImport(a *eco.Analysis, r *resolve.Resolver, t tree.Tree, from, proj string, imp model.ImportRef) {
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
	switch res.Class {
	case resolve.ClassInternal:
		k := edgeKind(imp.Kind)
		for _, f := range res.Files {
			a.Edges = append(a.Edges, model.Edge{From: from, To: f, Kind: k, Line: imp.Line, Spec: imp.Spec, Provenance: model.ProvScan})
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
		if e.Rules != nil {
			// Đích bị thiếu nằm đâu đó trong package: khớp [[generated]] theo owner, hoặc theo tiền tố tĩnh của glob output
			// nằm trong thư mục package (outputs = ["packages/a/build/**"]).
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
		unres(model.UnresolvedUnknown, res.PackageDir, model.ReasonGeneratedOutput+": "+res.Detail)
	default: // unknown
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
