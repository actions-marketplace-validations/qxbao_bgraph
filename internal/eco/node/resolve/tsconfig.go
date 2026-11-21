package resolve

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

type tsEntry struct {
	val string // giá trị (có thể chứa '*')
	abs bool   // đã là đường dẫn tương đối gốc repo (do ${configDir}), không nối thêm base
}

// tsEff là compilerOptions hiệu lực (sau extends) của một tsconfig/jsconfig.
type tsEff struct {
	HasBase   bool
	BaseURL   string // tương đối gốc repo
	HasPaths  bool
	Paths     map[string][]tsEntry
	PathsBase string // thư mục của config ĐỊNH NGHĨA paths
	HasOut    bool
	OutDir    string
	HasRoot   bool
	RootDir   string
}

type effEntry struct {
	eff *tsEff
	err error
}

func (e *tsEff) merge(p *tsEff) {
	if p.HasBase {
		e.HasBase, e.BaseURL = true, p.BaseURL
	}
	if p.HasPaths { // paths thay thế nguyên khối qua extends
		e.HasPaths, e.Paths, e.PathsBase = true, p.Paths, p.PathsBase
	}
	if p.HasOut {
		e.HasOut, e.OutDir = true, p.OutDir
	}
	if p.HasRoot {
		e.HasRoot, e.RootDir = true, p.RootDir
	}
}

// configsIn liệt kê tsconfig*.json / jsconfig*.json trong đúng thư mục dir.
func (r *Resolver) configsIn(dir string) []string {
	if r.cfgIdx == nil {
		r.cfgIdx = map[string][]string{}
		for _, f := range r.t.Files() {
			b := path.Base(f)
			if (strings.HasPrefix(b, "tsconfig") || strings.HasPrefix(b, "jsconfig")) && strings.HasSuffix(b, ".json") {
				d := path.Dir(f)
				r.cfgIdx[d] = append(r.cfgIdx[d], f)
			}
		}
	}
	return r.cfgIdx[dir]
}

func (r *Resolver) effective(cfg string) (*tsEff, error) {
	if e, ok := r.effs[cfg]; ok {
		return e.eff, e.err
	}
	eff, err := r.loadEff(cfg, path.Dir(cfg), []string{cfg})
	r.effs[cfg] = &effEntry{eff, err}
	return eff, err
}

// resolveVal chuyển một giá trị đường dẫn trong config cfg thành đường dẫn tương đối gốc repo.
func resolveVal(cfgDir, rootDir, s string) (string, bool, bool) {
	if strings.Contains(s, "${configDir}") {
		p, ok := join(".", strings.ReplaceAll(s, "${configDir}", rootDir))
		return p, true, ok
	}
	if strings.HasPrefix(s, "/") || strings.Contains(s, ":") {
		return "", false, false
	}
	p, ok := join(cfgDir, s)
	return p, false, ok
}

func (r *Resolver) loadEff(cfg, rootDir string, stack []string) (*tsEff, error) {
	data, err := r.t.Read(cfg)
	if err != nil {
		r.recordErr(cfg, err)
		return nil, err
	}
	v, err := ParseJSONC(data)
	if err == nil && v.Kind != KObject {
		err = errors.New("config không phải object")
	}
	if err != nil {
		r.recordErr(cfg, err)
		return nil, err
	}
	cfgDir := path.Dir(cfg)
	eff := &tsEff{}
	var exts []string
	if x := v.Get("extends"); x != nil {
		if s, ok := x.AsString(); ok {
			exts = append(exts, s)
		} else if x.Kind == KArray {
			for _, e := range x.Arr {
				if s, ok := e.AsString(); ok {
					exts = append(exts, s)
				}
			}
		}
	}
	for _, e := range exts {
		pp, ok := r.resolveExtends(cfg, e)
		if !ok {
			continue
		}
		if slices.Contains(stack, pp) {
			err := fmt.Errorf("extends vòng: %s", pp)
			r.recordErr(cfg, err)
			return nil, err
		}
		pe, err := r.loadEff(pp, rootDir, append(slices.Clone(stack), pp))
		if err != nil {
			return nil, err
		}
		eff.merge(pe)
	}
	co := v.Get("compilerOptions")
	if co == nil || co.Kind != KObject {
		return eff, nil
	}
	if s, ok := co.Get("baseUrl").AsString(); ok {
		if p, _, ok := resolveVal(cfgDir, rootDir, s); ok {
			eff.HasBase, eff.BaseURL = true, p
		}
	}
	if s, ok := co.Get("outDir").AsString(); ok {
		if p, _, ok := resolveVal(cfgDir, rootDir, s); ok {
			eff.HasOut, eff.OutDir = true, p
		}
	}
	if s, ok := co.Get("rootDir").AsString(); ok {
		if p, _, ok := resolveVal(cfgDir, rootDir, s); ok {
			eff.HasRoot, eff.RootDir = true, p
		}
	}
	if pv := co.Get("paths"); pv != nil && pv.Kind == KObject {
		m := map[string][]tsEntry{}
		for i, k := range pv.Keys {
			arr := pv.Vals[i]
			var list []*JSONValue
			if arr.Kind == KArray {
				list = arr.Arr
			} else {
				list = []*JSONValue{arr}
			}
			for _, e := range list {
				s, ok := e.AsString()
				if !ok {
					continue
				}
				if strings.Contains(s, "${configDir}") {
					p, _, ok := resolveVal(cfgDir, rootDir, s)
					if ok {
						m[k] = append(m[k], tsEntry{p, true})
					}
				} else {
					m[k] = append(m[k], tsEntry{s, false})
				}
			}
			if _, ok := m[k]; !ok {
				m[k] = nil
			}
		}
		eff.HasPaths, eff.Paths, eff.PathsBase = true, m, cfgDir
	}
	return eff, nil
}

// resolveExtends tìm tệp được extends. Đường dẫn tương đối thiếu → ConfigError; tên package → thử workspace, không có thì bỏ qua.
func (r *Resolver) resolveExtends(cfg, e string) (string, bool) {
	try := func(c string) (string, bool) {
		for _, x := range []string{c, c + ".json", c + "/tsconfig.json"} {
			if r.t.Has(x) {
				return x, true
			}
		}
		return "", false
	}
	if e == "." || e == ".." || strings.HasPrefix(e, "./") || strings.HasPrefix(e, "../") || strings.HasPrefix(e, "/") {
		c, ok := join(path.Dir(cfg), strings.TrimPrefix(e, "/"))
		if ok {
			if p, ok := try(c); ok {
				return p, true
			}
		}
		r.recordErr(cfg, fmt.Errorf("extends %q không tồn tại", e))
		return "", false
	}
	name, sub, ok := splitPkg(e)
	if !ok {
		return "", false
	}
	if dir, ok := r.opt.Workspaces[name]; ok {
		c, ok := join(dir, strings.TrimPrefix(sub, "/"))
		if ok {
			if p, ok := try(c); ok {
				return p, true
			}
		}
	}
	return "", false // package ngoài cây: bỏ qua
}

// tsMatch chọn mục paths: khoá chính xác trước, sau đó mẫu có '*' với tiền tố dài nhất.
func tsMatch(paths map[string][]tsEntry, spec string) ([]tsEntry, string, bool) {
	if v, ok := paths[spec]; ok && !strings.Contains(spec, "*") {
		return v, "", true
	}
	bestKey := ""
	best := -1
	var subst string
	for k := range paths {
		i := strings.IndexByte(k, '*')
		if i < 0 || strings.LastIndexByte(k, '*') != i {
			continue
		}
		pre, suf := k[:i], k[i+1:]
		if len(spec) < len(pre)+len(suf) || !strings.HasPrefix(spec, pre) || !strings.HasSuffix(spec, suf) {
			continue
		}
		if len(pre) > best || (len(pre) == best && len(k) > len(bestKey)) {
			best, bestKey = len(pre), k
			subst = spec[len(pre) : len(spec)-len(suf)]
		}
	}
	if best < 0 {
		return nil, "", false
	}
	return paths[bestKey], subst, true
}

// tsCandidates sinh các đường dẫn ứng viên (chưa dò) cho spec không tương đối từ mọi tsconfig/jsconfig
// ở dir(from) và tổ tiên. Lỗi config ghi vào a.cfgErr.
func (a *acc) tsCandidates(from, spec string) []string {
	var out []string
	for d := dirOf(from); ; d = path.Dir(d) {
		for _, cfg := range a.r.configsIn(d) {
			eff, err := a.r.effective(cfg)
			if err != nil {
				a.setCfgErr(cfg, err)
				continue
			}
			base := eff.PathsBase
			if eff.HasBase {
				base = eff.BaseURL
			}
			if eff.HasPaths {
				if ents, subst, ok := tsMatch(eff.Paths, spec); ok {
					for _, e := range ents {
						val := strings.ReplaceAll(e.val, "*", subst)
						if e.abs {
							if p, ok := join(".", val); ok {
								out = append(out, p)
							}
						} else if p, ok := join(base, val); ok {
							out = append(out, p)
						}
					}
				}
			}
			if eff.HasBase {
				if p, ok := join(eff.BaseURL, spec); ok {
					out = append(out, p)
				}
			}
		}
		if d == "." {
			break
		}
	}
	return out
}

// mapOutDir ánh xạ đường dẫn p dưới outDir của package pkgDir về ứng viên mã nguồn.
func (a *acc) mapOutDir(pkgDir, p string) []string {
	var out []string
	add := func(root, rel string) {
		m, ok := join(root, rel)
		if !ok {
			return
		}
		out = append(out, m)
		for _, s := range [][2]string{{".d.ts", ".ts"}, {".d.mts", ".mts"}, {".d.cts", ".cts"}} {
			if strings.HasSuffix(m, s[0]) {
				b := strings.TrimSuffix(m, s[0])
				out = append(out, b+s[1])
				if s[1] == ".ts" {
					out = append(out, b+".tsx")
				}
			}
		}
	}
	srcRoots := func(eff *tsEff) []string {
		if eff != nil && eff.HasRoot {
			return []string{eff.RootDir}
		}
		var rs []string
		if s, ok := join(pkgDir, "src"); ok && a.r.t.IsDir(s) {
			rs = append(rs, s)
		}
		return append(rs, pkgDir)
	}
	foundCfgOut := false
	for _, cfg := range a.r.configsIn(pkgDir) {
		eff, err := a.r.effective(cfg)
		if err != nil {
			a.setCfgErr(cfg, err)
			continue
		}
		if !eff.HasOut {
			continue
		}
		foundCfgOut = true
		if strings.HasPrefix(p, eff.OutDir+"/") {
			rel := p[len(eff.OutDir)+1:]
			for _, root := range srcRoots(eff) {
				add(root, rel)
			}
		}
	}
	if !foundCfgOut {
		// không có outDir khai báo: thử thư mục build quen thuộc khi có src/
		if s, ok := join(pkgDir, "src"); ok && a.r.t.IsDir(s) {
			for _, o := range []string{"dist", "build", "lib", "out"} {
				if od, ok := join(pkgDir, o); ok && strings.HasPrefix(p, od+"/") {
					add(s, p[len(od)+1:])
				}
			}
		}
	}
	return out
}
