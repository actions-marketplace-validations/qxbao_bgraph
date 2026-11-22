package jvm

import (
	"path"
	"strings"
)

// DynamicSource reports whether p is a Java or Kotlin file in a compiled source root whose classes the JVM collector maps back
// to their file (docs/dynamic-layer.md §9.2 rule 1): src/{main,test}/{java,kotlin}/** and the Kotlin Multiplatform roots
// src/<target>{Main,Test}/kotlin/**. Not module-info.java, templates (src/main/java-templates), generated or build-output
// directories, nor the build logic in buildSrc/ and build-logic/.
func DynamicSource(p string) bool {
	ext := path.Ext(p)
	if ext != ".java" && ext != ".kt" || path.Base(p) == "module-info.java" {
		return false
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "buildSrc", "build-logic", "target", "build", ".gradle", "generated", "generated-sources", "generated-test-sources":
			return false
		}
	}
	for i := 0; i+3 < len(segs); i++ {
		if segs[i] != "src" {
			continue
		}
		set, lang := segs[i+1], segs[i+2]
		switch {
		case (set == "main" || set == "test") && (lang == "java" || lang == "kotlin"):
			return true
		case lang == "kotlin" && len(set) > 4 && (strings.HasSuffix(set, "Main") || strings.HasSuffix(set, "Test")) && lowerStart(set):
			return true
		}
		return false // only the first src/ segment decides
	}
	return false
}

func lowerStart(s string) bool { return s != "" && s[0] >= 'a' && s[0] <= 'z' }

// TestRoot reports whether rel (relative to the unit directory) lies under a test source set of the unit: src/test*/**,
// src/*Test*/**, src/it/**, src/testFixtures/** (any file, code or resource). The class list of a unit is trusted only while
// these files are unchanged (docs/dynamic-layer.md §9.1).
func TestRoot(rel string) bool {
	r, ok := strings.CutPrefix(rel, "src/")
	if !ok {
		return false
	}
	set, _, ok := strings.Cut(r, "/")
	if !ok {
		return false
	}
	ls := strings.ToLower(set)
	return ls == "it" || strings.Contains(ls, "test")
}
