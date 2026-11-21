package node

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/node/resolve"
	"github.com/qxbao/bgraph/internal/rules"
	"github.com/qxbao/bgraph/internal/tree"
)

// applyDeclarations applies the [[config]] declarations of bgraph.toml to the runner configs read from the tree
// (configs is updated in place). For each declaration whose config file exists and belongs to a project:
//   - the config_is_code entries of the replaced keys of that file are dropped;
//   - declared test globs are added to the project's test patterns; the project stops being "tests opaque" once no
//     test-pattern entry is left;
//   - ignore_paths are removed from the project's implicit test dependencies;
//   - the declared aliases are returned (a union over the repo, like the aliases read from configs);
//   - the declared source files are returned: their unresolved imports are described by the declaration.
//
// A declaration naming an absent file changes nothing, so the widening stays (fail-closed).
func (e *Eco) applyDeclarations(t tree.Tree, configs map[string]testConfig) (aliases []resolve.Alias, sources map[string]bool) {
	if e.Rules == nil {
		return nil, nil
	}
	sources = map[string]bool{}
	for _, d := range e.Rules.Configs {
		if !t.Has(d.File) {
			continue
		}
		for dir, tc := range configs {
			if !slices.Contains(tc.files, d.File) {
				continue
			}
			kept := tc.opaque[:0:0]
			for _, op := range tc.opaque {
				if !replaced(op, d) {
					kept = append(kept, op)
				}
			}
			tc.opaque = kept
			tc.testGlobs = append(tc.testGlobs, d.Tests...)
			if tc.testsOpaque {
				tc.testsOpaque = slices.ContainsFunc(tc.opaque, isTestPatternEntry)
			}
			tc.implicit = slices.DeleteFunc(tc.implicit, func(p string) bool { return slices.Contains(d.IgnorePaths, p) })
			tc.implicitGlobs = slices.DeleteFunc(tc.implicitGlobs, func(g string) bool {
				return slices.ContainsFunc(d.IgnorePaths, func(p string) bool { return g == escapeGlob(p)+"/**" })
			})
			configs[dir] = tc
		}
		for _, k := range sortedKeys(d.Alias) {
			aliases = append(aliases, resolve.Alias{Prefix: k, Targets: []string{d.Alias[k]}})
		}
		for _, k := range sortedKeys(d.AliasRegex) {
			// Validated by rules.Parse. Anchored like jest: the pattern matches the whole specifier.
			aliases = append(aliases, resolve.Alias{Regex: regexp.MustCompile("^(?:" + k + ")$"), Targets: []string{d.AliasRegex[k]}})
		}
		for _, s := range d.Sources {
			sources[s] = true
		}
	}
	return aliases, sources
}

// replaced: the config_is_code entry op ("<file>: <key>" or "<file>: <key> <pattern>") is described by d.
func replaced(op string, d rules.Config) bool {
	for _, k := range d.Replaces {
		p := d.File + ": " + k
		if op == p || strings.HasPrefix(op, p+" ") {
			return true
		}
	}
	return false
}

// isTestPatternEntry: a config_is_code entry produced by an unreadable test pattern (testMatch/include/testRegex).
func isTestPatternEntry(op string) bool {
	_, rest, ok := strings.Cut(op, ": ")
	if !ok {
		return false
	}
	key, _, _ := strings.Cut(rest, " ")
	return slices.Contains(rules.TestPatternKeys, key)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// declaredSource: p is listed in the sources of some [[config]].
func (e *Eco) declaredSource(p string) bool {
	if e.Rules == nil {
		return false
	}
	for _, d := range e.Rules.Configs {
		if slices.Contains(d.Sources, p) {
			return true
		}
	}
	return false
}
