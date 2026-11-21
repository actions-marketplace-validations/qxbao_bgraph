package evalkit

import (
	"path"
	"strings"
)

// FixtureReq records a fixture-style requirement (data read by the test runner, not a test unit) for oracle auditing.
type FixtureReq struct {
	File        string `json:"file"`
	Root        string `json:"test_root"`
	SatisfiedBy string `json:"satisfied_by,omitempty"` // satisfying test unit; empty = not satisfied
}

// SelUnit is one selected test unit (decoupled from model so the oracle stays independent).
type SelUnit struct {
	Unit  string
	Files []string
	// WholeProject: the unit "run the project's whole test suite" (the runner takes no file list) → satisfies every requirement inside
	// the project directory. "." is the whole repo.
	WholeProject string
	Whole        bool
}

var (
	fixtureDirs  = map[string]bool{"fixtures": true, "__fixtures__": true, "__testfixtures__": true, "fixture": true}
	testRootDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true}
)

// fixtureRoot: if f lies under a fixture directory, returns (outermost fixtures directory, test root, true).
// Test root = nearest ancestor of the fixtures directory named test/tests/__tests__/spec; if none, the parent of the fixtures directory.
func fixtureRoot(f string) (fixDir, root string, ok bool) {
	segs := strings.Split(path.Clean(strings.ReplaceAll(f, "\\", "/")), "/")
	for i := 0; i < len(segs)-1; i++ { // the last segment is the file name, not counted
		if !fixtureDirs[segs[i]] {
			continue
		}
		fixDir = strings.Join(segs[:i+1], "/")
		for j := i - 1; j >= 0; j-- {
			if testRootDirs[segs[j]] {
				return fixDir, strings.Join(segs[:j+1], "/"), true
			}
		}
		return fixDir, strings.Join(segs[:i], "/"), true
	}
	return "", "", false
}

func under(p, dir string) bool {
	if dir == "" {
		return true
	}
	return strings.HasPrefix(p, dir+"/")
}

// CheckRequired returns the required files still missing and the list of fixture requirements.
// Normal requirement: must be in Files (or be the ID) of a selected unit.
// Fixture requirement: satisfied when some selected unit has a file in the test root's tree (any depth, except the fixtures directory itself).
func CheckRequired(req []string, sel []SelUnit) (missing []string, fixtures []FixtureReq) {
	have := map[string]bool{}
	for _, u := range sel {
		have[u.Unit] = true
		for _, f := range u.Files {
			have[f] = true
		}
	}
	coveredWhole := func(f string) string {
		for _, u := range sel {
			if u.Whole && (u.WholeProject == "." || u.WholeProject == "" || under(f, u.WholeProject)) {
				return u.Unit
			}
		}
		return ""
	}
	for _, f := range req {
		if w := coveredWhole(strings.ReplaceAll(f, "\\", "/")); w != "" {
			if _, root, ok := fixtureRoot(f); ok {
				fixtures = append(fixtures, FixtureReq{File: f, Root: root, SatisfiedBy: w})
			}
			continue
		}
		fixDir, root, ok := fixtureRoot(f)
		if !ok {
			if !have[f] {
				missing = append(missing, f)
			}
			continue
		}
		fr := FixtureReq{File: f, Root: root}
	search:
		for _, u := range sel {
			for _, uf := range u.Files {
				uf = strings.ReplaceAll(uf, "\\", "/")
				if under(uf, root) && !under(uf, fixDir) {
					fr.SatisfiedBy = u.Unit
					break search
				}
			}
		}
		fixtures = append(fixtures, fr)
		if fr.SatisfiedBy == "" {
			missing = append(missing, f)
		}
	}
	return
}
