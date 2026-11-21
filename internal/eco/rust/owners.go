package rust

import (
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
)

// DirectOwners returns the IDs of the test units whose tests can be defined in file: the units with an ownership edge to it
// (the target's module tree, include!, automod::dir!). A file owned only by a target that cargo test does not run (test=false) has no owner.
// A file no target owns is package-level: its owners are every unit linked to the node that carries it (package, tests/, benches/,
// examples/ or all-files group), reached through build scripts when the file is only read by one. The result is sorted; nil when
// nothing runs tests defined in file.
func DirectOwners(a *eco.Analysis, file string) []string {
	units := map[string]string{} // virtual node → unit ID
	for _, t := range a.Tests {
		units[Node(t.ID)] = t.ID
	}
	in := map[string][]string{} // to → froms
	for _, e := range a.Edges {
		in[e.To] = append(in[e.To], e.From)
	}
	isTarget := func(n string) bool {
		if !strings.HasPrefix(n, "<rust:") || !strings.HasSuffix(n, ">") {
			return false
		}
		_, _, _, ok := ParseUnitID(strings.TrimSuffix(strings.TrimPrefix(n, "<rust:"), ">"))
		return ok
	}
	set := map[string]bool{}
	directTarget := false
	for _, f := range in[file] {
		if isTarget(f) {
			directTarget = true
			if id, ok := units[f]; ok {
				set[id] = true
			}
		}
	}
	if !directTarget {
		// Package-level: walk up through group nodes and build scripts until the unit nodes.
		seen := map[string]bool{file: true}
		queue := []string{file}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			for _, f := range in[n] {
				if seen[f] {
					continue
				}
				seen[f] = true
				if isTarget(f) {
					if id, ok := units[f]; ok {
						set[id] = true
					}
					continue
				}
				queue = append(queue, f)
			}
		}
	}
	var out []string
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
