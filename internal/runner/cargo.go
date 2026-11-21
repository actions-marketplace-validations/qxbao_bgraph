package runner

import (
	"sort"

	"github.com/qxbao/bgraph/internal/eco/rust"
	"github.com/qxbao/bgraph/internal/model"
)

// cargoNote is attached to every plan that contains cargo commands.
const cargoNote = "cargo: running a subset with -p changes feature unification compared to a full workspace run"

// cargoCommands turns selected Cargo-target units into `cargo test` commands, one per package (plus a separate `--doc` command,
// because cargo cannot combine --doc with other target selectors). Units are addressed by package name (-p) when the unit carries
// it, otherwise by --manifest-path.
func cargoCommands(tests []model.SelectedTest) ([]Command, []string) {
	type pkgKey struct{ dir, name string }
	type group struct {
		flags []string // "--lib", "--test", "a", ...
		doc   bool
	}
	groups := map[pkgKey]*group{}
	var keys []pkgKey
	for _, t := range tests {
		dir, kind, name, ok := rust.ParseUnitID(t.Unit)
		if !ok {
			// An ID we cannot interpret: run the whole package rather than drop it.
			dir, kind = cleanProject(t.Project), ""
		}
		k := pkgKey{cleanProject(dir), t.Package}
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
			keys = append(keys, k)
		}
		switch kind {
		case rust.KindLib:
			g.flags = append(g.flags, "--lib")
		case rust.KindDoc:
			g.doc = true
		case rust.KindBin:
			g.flags = append(g.flags, "--bin", name)
		case rust.KindTest:
			g.flags = append(g.flags, "--test", name)
		case rust.KindBench:
			g.flags = append(g.flags, "--bench", name)
		case rust.KindExample:
			g.flags = append(g.flags, "--example", name)
		default:
			g.flags = append(g.flags, "--all-targets")
			g.doc = true
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dir != keys[j].dir {
			return keys[i].dir < keys[j].dir
		}
		return keys[i].name < keys[j].name
	})
	var cmds []Command
	for _, k := range keys {
		g := groups[k]
		sel := []string{"-p", k.name}
		if k.name == "" {
			sel = []string{"--manifest-path", manifestPath(k.dir)}
		}
		if len(g.flags) > 0 {
			argv := append([]string{"cargo", "test"}, sel...)
			cmds = append(cmds, Command{Runner: "cargo", Project: k.dir, Argv: append(argv, dedupeFlags(g.flags)...)})
		}
		if g.doc {
			argv := append([]string{"cargo", "test"}, sel...)
			cmds = append(cmds, Command{Runner: "cargo", Project: k.dir, Argv: append(argv, "--doc"),
				Note: "cargo cannot combine --doc with other target selectors"})
		}
	}
	return cmds, []string{cargoNote}
}

func manifestPath(dir string) string {
	if dir == "." {
		return "Cargo.toml"
	}
	return dir + "/Cargo.toml"
}

// dedupeFlags sorts the target selectors (lib first, then bin/test/bench/example groups by name) and drops duplicates.
func dedupeFlags(flags []string) []string {
	type sel struct{ flag, name string }
	var sels []sel
	for i := 0; i < len(flags); i++ {
		if flags[i] == "--lib" || flags[i] == "--all-targets" {
			sels = append(sels, sel{flags[i], ""})
			continue
		}
		sels = append(sels, sel{flags[i], flags[i+1]})
		i++
	}
	order := map[string]int{"--all-targets": 0, "--lib": 1, "--bin": 2, "--test": 3, "--bench": 4, "--example": 5}
	sort.Slice(sels, func(i, j int) bool {
		if order[sels[i].flag] != order[sels[j].flag] {
			return order[sels[i].flag] < order[sels[j].flag]
		}
		return sels[i].name < sels[j].name
	})
	var out []string
	for i, s := range sels {
		if i > 0 && s == sels[i-1] {
			continue
		}
		out = append(out, s.flag)
		if s.name != "" {
			out = append(out, s.name)
		}
	}
	return out
}
