// Package cpp models CMake/CTest and Meson test targets without configuring or
// building the projects.
package cpp

import "strings"

const (
	Name        = "cpp"
	CMake       = "cmake"
	Meson       = "meson"
	CTestRunner = "ctest"
	MesonRunner = "meson"
)

var encodeID = strings.NewReplacer("%", "%25", ":", "%3A")
var decodeID = strings.NewReplacer("%25", "%", "%3A", ":")

// UnitID identifies a registered test, or the whole project when name is empty.
func UnitID(buildSystem, dir, name string) string {
	if dir == "" {
		dir = "."
	}
	prefix := Name + ":" + buildSystem + ":" + encodeID.Replace(dir)
	if name == "" {
		return prefix + "::all"
	}
	return prefix + "::test:" + encodeID.Replace(name)
}

// ParseUnitID recovers the build system, project directory, and registered
// test name. A successful whole-project ID has an empty name.
func ParseUnitID(id string) (buildSystem, dir, name string, ok bool) {
	body, valid := strings.CutPrefix(id, Name+":")
	if !valid {
		return "", "", "", false
	}
	system, body, valid := strings.Cut(body, ":")
	if !valid || (system != CMake && system != Meson) {
		return "", "", "", false
	}
	d, suffix, valid := strings.Cut(body, "::")
	if !valid || d == "" {
		return "", "", "", false
	}
	switch {
	case suffix == "all":
		return system, decodeID.Replace(d), "", true
	case strings.HasPrefix(suffix, "test:") && len(suffix) > len("test:"):
		return system, decodeID.Replace(d), decodeID.Replace(suffix[len("test:"):]), true
	default:
		return "", "", "", false
	}
}
