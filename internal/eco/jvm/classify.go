package jvm

import (
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// versionOnly lists the element paths whose change is a pure version bump: the project's own <version> and its <parent>'s.
var versionOnly = map[string]bool{"project/version": true, "project/parent/version": true}

// hasMvnDir: p lies below a .mvn directory (returns the directory that holds .mvn).
func underDotMvn(p string) (string, bool) {
	segs := strings.Split(p, "/")
	for i, s := range segs[:len(segs)-1] {
		if s == ".mvn" {
			if i == 0 {
				return ".", true
			}
			return strings.Join(segs[:i], "/"), true
		}
	}
	return "", false
}

func (e *Eco) classifyMaven(p string, base, head tree.Tree) ([]model.Widening, bool) {
	b := path.Base(p)
	if dir, ok := underDotMvn(p); ok {
		return []model.Widening{scopeWiden(model.ReasonGlobalInput, p, mavenRootAbove(e, dir, base, head), "Maven .mvn/ configuration changed")}, true
	}
	if b == "mvnw" || b == "mvnw.cmd" {
		return []model.Widening{scopeWiden(model.ReasonGlobalInput, p, mavenRootAbove(e, path.Dir(p), base, head), "Maven wrapper changed")}, true
	}
	if b != "pom.xml" {
		return nil, false
	}
	var mb, mh *mvnModel
	var pb, ph *pomInfo
	if base != nil {
		mb = e.mavenModel(base)
		pb = mb.poms[p]
	}
	if head != nil {
		mh = e.mavenModel(head)
		ph = mh.poms[p]
	}
	if pb == nil && ph == nil {
		return nil, false // a POM that is no module (test resource, archetype template): owned by the enclosing module
	}
	// The scope is the whole component when the POM is its top (nothing lists it, no local parent); for a POM in the middle it is the
	// POM's own directory: the modules that inherit from it elsewhere are reached through their parent edge.
	root := func() string {
		m, pi := mh, ph
		if pi == nil {
			m, pi = mb, pb
		}
		if len(pi.declaredBy) == 0 && pi.localParent == "" {
			return m.compRoot[m.comp[p]]
		}
		return pi.dir
	}
	widen := func(detail string) []model.Widening {
		return []model.Widening{scopeWiden(model.ReasonManifestChanged, p, root(), detail)}
	}
	shared := (pb != nil && mb.isShared(pb)) || (ph != nil && mh.isShared(ph))
	if (pb != nil && pb.profileModules) || (ph != nil && ph.profileModules) {
		return widen("a profile of this POM declares <modules>: the module list depends on the active profiles"), true
	}
	// A remote parent whose coordinates changed brings other inherited configuration.
	if pb != nil && ph != nil && pb.parent != nil && ph.parent != nil && pb.localParent == "" && ph.localParent == "" &&
		(pb.parent.g != ph.parent.g || pb.parent.a != ph.parent.a || pb.parent.v != ph.parent.v) {
		return widen("remote parent changed: " + pb.parent.g + ":" + pb.parent.a + ":" + pb.parent.v + " -> " + ph.parent.g + ":" + ph.parent.a + ":" + ph.parent.v), true
	}
	if !shared {
		return nil, true // a leaf module POM: owned by its module; dependency changes are covered by the union of both graphs
	}
	if pb == nil || ph == nil {
		return widen("a root/shared POM was added or removed"), true
	}
	if pb.err != nil || ph.err != nil || pb.root == nil || ph.root == nil {
		return widen("a root/shared POM is unreadable on one side"), true
	}
	if pb.root.canon(versionOnly) == ph.root.canon(versionOnly) {
		return nil, true // only <version> changed (or only formatting): the modules inheriting it are reached through the parent edge
	}
	return widen("root/shared POM changed beyond <version>: inherited dependencies, plugins and properties may differ"), true
}

// mavenRootAbove returns the directory to widen for a change below dir (the repo-level Maven files .mvn/ and mvnw): the common
// directory of the component whose POM sits in dir, or dir itself.
func mavenRootAbove(e *Eco, dir string, base, head tree.Tree) string {
	for _, t := range []tree.Tree{head, base} {
		if t == nil {
			continue
		}
		m := e.mavenModel(t)
		if pi, ok := m.poms[path.Join(dir, "pom.xml")]; ok {
			return m.compRoot[m.comp[pi.path]]
		}
	}
	return dir
}
