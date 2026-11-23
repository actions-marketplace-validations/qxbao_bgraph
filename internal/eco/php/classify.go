package php

import (
	"maps"
	"path"
	"reflect"
	"slices"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	phpscan "github.com/qxbao/bgraph/internal/scan/php"
	"github.com/qxbao/bgraph/internal/tree"
)

func (*Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	if dirs := vendorOwners(p, base, head, a); len(dirs) != 0 {
		return scoped(p, model.ReasonLockChanged, "installed Composer dependency changed", dirs), true
	}
	if excludedPath(p) {
		return nil, false
	}
	n := path.Base(p)
	isConfig := testConfig(n) || n == "Pest.php"
	isSource := phpscan.Source(p)
	if n != "composer.json" && n != "composer.lock" && !isConfig && !isSource {
		return nil, false
	}
	dirs := []string{projectDir(base, p), projectDir(head, p)}
	owned := dirs[0] != "" || dirs[1] != ""
	switch {
	case n == "composer.json" && owned:
		if metadataOnly(p, base, head) {
			return nil, true
		}
		return scoped(p, model.ReasonManifestChanged, "Composer manifest changed", dirs), true
	case n == "composer.lock" && owned:
		return scoped(p, model.ReasonLockChanged, "Composer lockfile at level L0", dirs), true
	case isConfig && owned:
		return scoped(p, model.ReasonTestConfig, "PHP test configuration changed", dirs), true
	case isSource && !owned && a != nil && len(a.Projects) > 0:
		// A referenced path outside a project has a graph edge and can be
		// classified precisely by the selector.
		aliases := []string{p}
		for alias, real := range tree.AliasesOf(head) {
			if real == p {
				aliases = append(aliases, alias)
			}
		}
		for _, e := range a.Edges {
			for _, alias := range aliases {
				if e.To == alias || e.Kind == model.EdgePattern && glob.Match(e.To, alias) {
					return nil, false
				}
			}
		}
		return []model.Widening{{Tier: model.TierFull, Reason: model.ReasonUnknownFile, Path: p, Eco: Name,
			Detail: "PHP source is outside every Composer or PHP test project"}}, true
	}
	return nil, false
}

func scoped(p, reason, detail string, dirs []string) []model.Widening {
	var out []model.Widening
	slices.Sort(dirs)
	for _, d := range slices.Compact(dirs) {
		if d != "" {
			out = append(out, model.Widening{Tier: model.TierModule, Reason: reason, Path: p, Scope: d, Eco: Name,
				Exact: true, Detail: detail, Seeds: []string{virtual(d)}})
		}
	}
	return out
}

func projectDir(t tree.Tree, file string) string {
	if t == nil {
		return ""
	}
	for d := path.Dir(file); ; d = path.Dir(d) {
		if t.Has(path.Join(d, "composer.json")) {
			return d
		}
		for _, n := range []string{"phpunit.xml", "phpunit.dist.xml", "phpunit.xml.dist"} {
			if t.Has(path.Join(d, n)) {
				return d
			}
		}
		if d == "." {
			return ""
		}
	}
}

func vendorOwners(p string, base, head tree.Tree, a *eco.Analysis) []string {
	// Only read custom vendor-dir settings if this path is unclaimed in the
	// current analysis. The fast path covers the default Composer layout.
	var owners []string
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		if path.Base(d) == "vendor" {
			f := path.Join(path.Dir(d), "composer.json")
			if base != nil && base.Has(f) || head != nil && head.Has(f) {
				owners = append(owners, path.Dir(d))
			}
		}
	}
	if a == nil || a.Roles[p] != "" {
		return owners
	}
	for _, pr := range a.Projects {
		for _, t := range []tree.Tree{head, base} {
			if t == nil {
				continue
			}
			f := path.Join(pr.Dir, "composer.json")
			if !t.Has(f) {
				continue
			}
			m, err := readManifest(t, f)
			if err != nil || m.Config.VendorDir == "" {
				continue
			}
			if d, ok := localPath(pr.Dir, m.Config.VendorDir); ok && d != pr.Dir && eco.Within(p, d) {
				owners = append(owners, pr.Dir)
				break
			}
		}
	}
	return owners
}

var harmlessMetadata = []string{"description", "keywords", "homepage", "readme", "authors", "support", "funding", "license"}

func metadataOnly(p string, base, head tree.Tree) bool {
	if base == nil || head == nil {
		return false
	}
	a, errA := decodeObject(base, p)
	b, errB := decodeObject(head, p)
	if errA != nil || errB != nil {
		return false
	}
	harmless := func(k string, _ any) bool { return slices.Contains(harmlessMetadata, k) }
	maps.DeleteFunc(a, harmless)
	maps.DeleteFunc(b, harmless)
	return reflect.DeepEqual(a, b)
}
