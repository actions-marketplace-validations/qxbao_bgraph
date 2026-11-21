package resolve

import (
	"fmt"
	"path"
	"sort"
)

// PackageJSON là phần package.json mà bộ phân giải và lớp eco cần.
type PackageJSON struct {
	Path string // đường dẫn tệp trong cây
	Dir  string // thư mục chứa
	Name string
	Type string

	Main, Module, Types, Typings string
	// Source is the build entry by microbundle/parcel convention ("source": "src/index.js"): outputs in main/module
	// are compiled from it.
	Source string
	BrowserString                string     // browser khi là chuỗi
	Browser                      *JSONValue // browser thô (chuỗi hoặc object)
	Exports, Imports             *JSONValue // thô, có thứ tự khoá; nil nếu vắng hoặc null
	Dependencies                 map[string]string
	DevDependencies              map[string]string
	PeerDependencies             map[string]string
	OptionalDependencies         map[string]string
	Workspaces                   []string
}

// HasDep: tên có trong một trong bốn nhóm phụ thuộc.
func (p *PackageJSON) HasDep(name string) bool {
	for _, m := range []map[string]string{p.Dependencies, p.DevDependencies, p.PeerDependencies, p.OptionalDependencies} {
		if _, ok := m[name]; ok {
			return true
		}
	}
	return false
}

// ConfigError là một tệp cấu hình (package.json, tsconfig) không đọc/parse được.
type ConfigError struct {
	Path string
	Err  error
}

func (e ConfigError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

type pkgEntry struct {
	pj  *PackageJSON
	err error
}

func (r *Resolver) recordErr(p string, err error) {
	if _, ok := r.errs[p]; !ok {
		r.errs[p] = err
	}
}

// ConfigErrors trả các cấu hình lỗi đã gặp, sắp theo đường dẫn.
func (r *Resolver) ConfigErrors() []ConfigError {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ConfigError, 0, len(r.errs))
	for p, e := range r.errs {
		out = append(out, ConfigError{Path: p, Err: e})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// PackageJSON đọc package.json của thư mục dir (có cache). (nil, nil) nếu không có tệp.
func (r *Resolver) PackageJSON(dir string) (*PackageJSON, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pkgJSON(dir)
}

func (r *Resolver) pkgJSON(dir string) (*PackageJSON, error) {
	p := "package.json"
	if dir != "." && dir != "" {
		p = dir + "/package.json"
	} else {
		dir = "."
	}
	if e, ok := r.pkgs[p]; ok {
		return e.pj, e.err
	}
	var e pkgEntry
	if r.t.Has(p) {
		e.pj, e.err = r.parsePkg(p, dir)
		if e.err != nil {
			e.pj = nil
			r.recordErr(p, e.err)
		}
	}
	r.pkgs[p] = &e
	return e.pj, e.err
}

func (r *Resolver) parsePkg(p, dir string) (*PackageJSON, error) {
	data, err := r.t.Read(p)
	if err != nil {
		return nil, err
	}
	v, err := ParseJSONC(data)
	if err != nil {
		return nil, err
	}
	if v.Kind != KObject {
		return nil, fmt.Errorf("package.json không phải object")
	}
	pj := &PackageJSON{Path: p, Dir: dir}
	str := func(k string) string { s, _ := v.Get(k).AsString(); return s }
	pj.Name, pj.Type = str("name"), str("type")
	pj.Main, pj.Module, pj.Types, pj.Typings = str("main"), str("module"), str("types"), str("typings")
	pj.Source = str("source")
	if b := v.Get("browser"); b != nil {
		pj.Browser = b
		pj.BrowserString, _ = b.AsString()
	}
	if e := v.Get("exports"); e != nil && e.Kind != KNull {
		pj.Exports = e
	}
	if e := v.Get("imports"); e != nil && e.Kind != KNull {
		pj.Imports = e
	}
	deps := func(k string) map[string]string {
		o := v.Get(k)
		if o == nil || o.Kind != KObject {
			return nil
		}
		m := make(map[string]string, len(o.Keys))
		for i, key := range o.Keys {
			m[key], _ = o.Vals[i].AsString()
		}
		return m
	}
	pj.Dependencies, pj.DevDependencies = deps("dependencies"), deps("devDependencies")
	pj.PeerDependencies, pj.OptionalDependencies = deps("peerDependencies"), deps("optionalDependencies")
	if w := v.Get("workspaces"); w != nil {
		if w.Kind == KObject {
			w = w.Get("packages")
		}
		if w != nil && w.Kind == KArray {
			for _, x := range w.Arr {
				if s, ok := x.AsString(); ok {
					pj.Workspaces = append(pj.Workspaces, s)
				}
			}
		}
	}
	return pj, nil
}

// dirOf trả thư mục của một tệp theo quy ước model ("." cho gốc).
func dirOf(file string) string { return path.Dir(file) }
