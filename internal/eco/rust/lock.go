package rust

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/lockfile"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// l1Cargo: Cargo.lock passed the differential check against cargo (docs/lockfiles.md); false keeps it at level L0.
var l1Cargo = true

func readLock(t tree.Tree, p string) (*lockfile.Graph, error) {
	if t == nil || !t.Has(p) {
		return nil, fmt.Errorf("lockfile absent")
	}
	b, err := t.Read(p)
	if err != nil {
		return nil, err
	}
	return lockfile.Parse(p, path.Dir(p), b)
}

// nearestLock returns the Cargo.lock that governs the package in dir: the nearest one in dir or an ancestor.
func nearestLock(t tree.Tree, dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if p := path.Join(d, "Cargo.lock"); t.Has(p) {
			return p
		}
		if d == "." || d == "/" || d == "" {
			return ""
		}
	}
}

// classifyLock compares, for every package governed by the Cargo.lock p, its resolved dependency closure (level L1).
// Cargo.lock records the union of every dependency kind (normal, dev, build), target and feature, and a workspace
// member's closure contains the members it depends on: a member whose closure changed → all its units, and its reverse
// dependents through the closure. Build dependencies and proc-macros need no rule of their own: they are in the closure
// of every member that uses them. A package that cannot be matched to one lockfile entry → every package (L0).
//
// Level L2 is not offered for Cargo: units are whole Cargo targets, and which targets use a crate is not known without
// name resolution (`use`, paths, macros): L2 behaves as L1.
func (e *Eco) classifyLock(p string, base, head tree.Tree, a *eco.Analysis) []model.Widening {
	dir := path.Dir(p)
	l0 := func(detail string) []model.Widening {
		return []model.Widening{{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: dir, Eco: Name,
			Detail: "lockfile at level L0 (" + detail + ")"}}
	}
	switch {
	case e.rules.LockLevel() == rules.LockL0:
		return l0("bgraph.toml [lockfile] level")
	case !l1Cargo:
		return l0("closure not compared for this format")
	}
	bg, err := readLock(base, p)
	if err != nil {
		return l0("base: " + err.Error())
	}
	hg, err := readLock(head, p)
	if err != nil {
		return l0("head: " + err.Error())
	}
	if len(lockfile.ExtraChanged(bg, hg)) > 0 || bg.Version != hg.Version {
		return l0("lockfile version or settings outside the package graph changed")
	}
	var out []model.Widening
	if a == nil {
		return l0("no analysis")
	}
	seen := map[string]bool{}
	for _, pr := range a.Projects {
		if pr.Eco != Name || !eco.Within(pr.Dir, dir) || seen[pr.Dir] || nearestLock(head, pr.Dir) != p {
			continue
		}
		seen[pr.Dir] = true
		bk, ok1 := bg.CargoProject(pr.Name)
		hk, ok2 := hg.CargoProject(pr.Name)
		var detail string
		switch {
		case ok1 && ok2:
			if !lockfile.ClosureChanged(bg, hg, bg.Projects[bk], hg.Projects[hk]) {
				continue
			}
			detail = "dependency closure of " + pr.Name + " changed (L1)"
		case ok1 != ok2:
			detail = "package " + pr.Name + " added to or removed from the lockfile (L1)"
		default:
			if countLocal(bg, pr.Name)+countLocal(hg, pr.Name) > 0 {
				return l0("package " + pr.Name + " matches several lockfile entries")
			}
			detail = "package " + pr.Name + " is not in the lockfile (L1)"
		}
		out = append(out, model.Widening{Tier: model.TierModule, Reason: model.ReasonLockChanged, Path: p, Scope: pr.Dir, Eco: Name, Detail: detail})
	}
	slices.SortFunc(out, func(x, y model.Widening) int { return strings.Compare(x.Scope, y.Scope) })
	return out
}

func countLocal(g *lockfile.Graph, name string) int {
	n := 0
	for k := range g.Projects {
		if c, _, _ := strings.Cut(k, " "); c == name {
			n++
		}
	}
	return n
}
