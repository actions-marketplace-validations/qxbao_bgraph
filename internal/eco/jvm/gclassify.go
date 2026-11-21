package jvm

import (
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// gradleSharedDir reports whether p lies in a directory that defines how every project of a build is built, and returns the
// directory that holds it: buildSrc/, build-logic/ and the Gradle wrapper (gradle/wrapper/, gradlew, gradlew.bat).
func gradleSharedInput(p string) (dir, what string, ok bool) {
	segs := strings.Split(p, "/")
	for i, s := range segs[:len(segs)-1] {
		parent := "."
		if i > 0 {
			parent = strings.Join(segs[:i], "/")
		}
		switch s {
		case "buildSrc", "build-logic":
			return parent, s + "/", true
		case "gradle":
			if i+1 < len(segs)-1 && segs[i+1] == "wrapper" {
				return parent, "Gradle wrapper", true
			}
		}
	}
	base := path.Base(p)
	switch {
	case base == "gradlew" || base == "gradlew.bat":
		return path.Dir(p), "Gradle wrapper", true
	case base == "gradle.properties":
		return path.Dir(p), "gradle.properties", true
	case strings.HasSuffix(base, ".versions.toml"):
		d := path.Dir(p)
		if path.Base(d) == "gradle" {
			d = path.Dir(d)
		}
		return d, "version catalog", true
	case path.Base(path.Dir(p)) == "gradle" && strings.HasSuffix(base, ".toml"):
		return path.Dir(path.Dir(p)), "version catalog", true
	}
	return "", "", false
}

func (e *Eco) classifyGradle(p string, base, head tree.Tree) ([]model.Widening, bool) {
	b := path.Base(p)
	dir, what, shared := gradleSharedInput(p)
	scriptLike := isGradleScript(b) || isSettingsScript(b) || strings.HasSuffix(b, ".gradle") || strings.HasSuffix(b, ".gradle.kts")
	if !shared && !scriptLike {
		return nil, false
	}
	var models []*gradleModel
	for _, t := range []tree.Tree{head, base} {
		if t != nil {
			models = append(models, e.gradleModel(t))
		}
	}
	rootFor := func(dir string) (string, bool) {
		for _, m := range models {
			if gb := m.rootAbove(dir); gb != nil {
				return gb.scope(), true
			}
		}
		return "", false
	}
	if shared {
		if root, found := rootFor(dir); found {
			reason := model.ReasonGlobalInput
			return []model.Widening{scopeWiden(reason, p, root, what+" changed: it configures every project of the build")}, true
		}
		return nil, false
	}
	for _, m := range models {
		if gb, ok := m.isRootScript(p); ok {
			what := "root build script"
			if !isGradleScript(b) {
				what = "settings script"
			}
			return []model.Widening{scopeWiden(model.ReasonManifestChanged, p, gb.scope(), what+" changed: the structure and shared configuration of the build may differ")}, true
		}
	}
	if isGradleScript(b) {
		for _, m := range models {
			why, ok := m.cross[p]
			if !ok {
				continue
			}
			if gb := m.rootAbove(path.Dir(p)); gb != nil {
				return []model.Widening{scopeWiden(model.ReasonManifestChanged, p, gb.scope(), "build script configures other projects ("+why+")")}, true
			}
		}
		for _, m := range models {
			if gb := m.rootAbove(path.Dir(p)); gb != nil && m.isProjectScript(p) {
				return nil, true // a project's own script: owned by the project; edges are covered by the union of both graphs
			}
		}
	}
	return nil, false
}

// isProjectScript: p is the build script of one of the projects.
func (m *gradleModel) isProjectScript(p string) bool {
	for _, gb := range m.builds {
		for _, pr := range gb.order {
			if pr.script == p {
				return true
			}
		}
	}
	return false
}
