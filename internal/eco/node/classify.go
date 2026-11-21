package node

import (
	"bytes"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// harmlessPkgKeys là khoá cấp cao của package.json mà đổi chúng chứng minh được không ảnh hưởng test.
var harmlessPkgKeys = map[string]bool{"version": true, "description": true, "keywords": true, "author": true, "contributors": true,
	"homepage": true, "repository": true, "bugs": true, "license": true, "funding": true, "maintainers": true, "publishConfig": true}

// ClassifyChange phân loại manifest/cấu hình/lockfile của Node (mục 6.1 "Nới", 6.8 mức L0).
func (e *Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	if inNodeModules(p) {
		return nil, false
	}
	var projects []eco.Project
	if a != nil {
		projects = a.Projects
	}
	scopeOf := func(p string) string {
		if pr, ok := eco.NearestProject(projects, p); ok {
			return pr.Dir
		}
		return path.Dir(p)
	}
	base1 := path.Base(p)
	switch {
	case base1 == "package.json":
		dir := path.Dir(p)
		if keys, ok := changedTopKeys(base, head, p); ok {
			if len(keys) == 0 {
				return nil, true
			}
			only := true
			for _, k := range keys {
				if !harmlessPkgKeys[k] {
					only = false
				}
			}
			if only {
				return nil, true
			}
			return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: dir,
				Detail: "khoá đổi: " + strings.Join(keys, ", ")}}, true
		}
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonManifestChanged, Path: p, Scope: dir}}, true
	case isLockfile(p):
		// L0: chưa so closure → mọi dự án dưới thư mục của lockfile (thường là gốc workspace).
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: path.Dir(p),
			Detail: "lockfile ở mức L0 (chưa so closure)"}}, true
	case isTSConfig(p):
		dirs := []string{path.Dir(p)}
		dirs = append(dirs, tsconfigExtenders(base, p)...)
		dirs = append(dirs, tsconfigExtenders(head, p)...)
		slices.Sort(dirs)
		dirs = slices.Compact(dirs)
		var ws []model.Widening
		for _, d := range dirs {
			ws = append(ws, model.Widening{Tier: model.TierModule, Reason: model.ReasonTestConfig, Path: p, Scope: d, Detail: "tsconfig/jsconfig đổi"})
		}
		return ws, true
	case configKind(p) != "":
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonTestConfig, Path: p, Scope: scopeOf(p),
			Detail: configKind(p) + " config đổi"}}, true
	}
	return nil, false
}

// changedTopKeys trả các khoá cấp cao khác nhau giữa hai phiên bản JSON; ok=false nếu một phía không đọc được
// (tệp thêm/xoá cũng trả ok=false → nới).
func changedTopKeys(base, head tree.Tree, p string) ([]string, bool) {
	if base == nil || head == nil {
		return nil, false
	}
	ob, err1 := base.Read(p)
	nb, err2 := head.Read(p)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	var o, n map[string]json.RawMessage
	if json.Unmarshal(ob, &o) != nil || json.Unmarshal(nb, &n) != nil {
		return nil, false
	}
	var keys []string
	for k, v := range o {
		if w, ok := n[k]; !ok || !jsonEqual(v, w) {
			keys = append(keys, k)
		}
	}
	for k := range n {
		if _, ok := o[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, true
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// tsconfigExtenders trả thư mục của mọi tsconfig/jsconfig trong t mà `extends` (bắc cầu) trỏ tới target.
func tsconfigExtenders(t tree.Tree, target string) []string {
	if t == nil {
		return nil
	}
	var cfgs []string
	for _, f := range t.Files() {
		if isTSConfig(f) && !inNodeModules(f) {
			cfgs = append(cfgs, f)
		}
	}
	parents := map[string][]string{} // cfg → các tệp nó extends (chỉ đường dẫn tương đối trong repo)
	for _, c := range cfgs {
		b, err := t.Read(c)
		if err != nil {
			continue
		}
		for _, ext := range extendsOf(b) {
			if !strings.HasPrefix(ext, "./") && !strings.HasPrefix(ext, "../") {
				// extends tới package: nếu là workspace thì khó biết; tập cha: coi như có thể là target khi tên trùng đuôi
				if strings.HasSuffix(target, strings.TrimPrefix(ext, "@")) {
					parents[c] = append(parents[c], target)
				}
				continue
			}
			pth := path.Join(path.Dir(c), ext)
			if !strings.HasSuffix(pth, ".json") && !t.Has(pth) {
				pth += ".json"
			}
			parents[c] = append(parents[c], pth)
		}
	}
	affected := map[string]bool{target: true}
	for changed := true; changed; {
		changed = false
		for c, ps := range parents {
			if affected[c] {
				continue
			}
			for _, p := range ps {
				if affected[p] {
					affected[c] = true
					changed = true
					break
				}
			}
		}
	}
	var dirs []string
	for c := range affected {
		if c != target {
			dirs = append(dirs, path.Dir(c))
		}
	}
	slices.Sort(dirs)
	return dirs
}

// extendsOf đọc trường extends (chuỗi hoặc mảng) của tsconfig dạng JSONC; lỗi → nil.
func extendsOf(b []byte) []string {
	s := stripJSComments(string(b))
	idx := keyIndexes(s, "extends")
	var out []string
	for _, i := range idx {
		if v, ok := literalValue(s[i:]); ok {
			out = append(out, v)
		} else if arr, ok := stringArray(s[i:]); ok {
			out = append(out, arr...)
		}
	}
	return out
}
