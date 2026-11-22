package tree

import (
	"maps"
	"slices"
	"testing"
)

func TestFollowLinks(t *testing.T) {
	files := map[string]string{"lib/a.js": "A", "lib/sub/b.js": "B", "x/f.txt": "F", "y/g.txt": "G"}
	links := map[string]string{
		"chain.js":  "via/a.js",      // through a directory link component
		"via":       "lib",           // directory link
		"nest/l":    "../via",        // directory link to a directory link
		"lib/inner": "../x/f.txt",    // file link inside a linked directory
		"x/toy":     "../y",          // mutual cycle with y/tox
		"y/tox":     "../x",          //
		"bad/abs":   "C:\\data",      // absolute (Windows drive)
		"bad/out":   "../../up",      // outside
		"bad/empty": "",              // dangling
		"bad/mid":   "chain.js/oops", // a file used as a directory
		"self":      "self",          // a link to itself
	}
	tr := NewMapTreeLinks(files, links)
	al := tr.Aliases()
	want := map[string]string{
		"chain.js":            "lib/a.js",
		"via/a.js":            "lib/a.js",
		"via/sub/b.js":        "lib/sub/b.js",
		"via/inner":           "x/f.txt",
		"lib/inner":           "x/f.txt",
		"nest/l/a.js":         "lib/a.js",
		"nest/l/sub/b.js":     "lib/sub/b.js",
		"nest/l/inner":        "x/f.txt",
		"x/toy/g.txt":         "y/g.txt",
		"x/toy/tox/f.txt":     "x/f.txt", // one level into the cycle, not into its re-entry
		"y/tox/f.txt":         "x/f.txt",
		"y/tox/toy/g.txt":     "y/g.txt",
		"x/toy/tox/toy/g.txt": "",
	}
	for p, w := range want {
		if al[p] != w {
			t.Errorf("alias %s = %q, want %q", p, al[p], w)
		}
	}
	problems := map[string]string{}
	for _, l := range tr.Links() {
		problems[l.Path] = l.Problem
	}
	wantP := map[string]string{"chain.js": "", "via": "", "nest/l": "", "lib/inner": "", "x/toy": LinkCycle, "y/tox": LinkCycle,
		"bad/abs": LinkAbsolute, "bad/out": LinkOutside, "bad/empty": LinkDangling, "bad/mid": LinkDangling, "self": LinkCycle}
	if !maps.Equal(problems, wantP) {
		t.Errorf("problems = %v\nwant       %v", problems, wantP)
	}
	if !tr.Has("nest/l/sub/b.js") || !tr.IsDir("nest/l/sub") || tr.Has("via") || tr.Has("bad/out") {
		t.Error("Has/IsDir")
	}
	if b, _ := tr.Read("chain.js"); string(b) != "A" || tr.Key("chain.js") != tr.Key("lib/a.js") {
		t.Error("Read/Key through links")
	}
	if !slices.IsSorted(tr.Files()) || slices.Contains(tr.Files(), "via") || !slices.Contains(tr.Files(), "via/a.js") {
		t.Error("Files")
	}
	// Without links the tree is unchanged.
	plain := NewMapTree(files)
	if len(plain.Aliases()) != 0 || len(plain.Links()) != 0 || len(plain.Files()) != len(files) {
		t.Error("plain tree")
	}
}
