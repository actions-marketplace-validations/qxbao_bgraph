package node

import (
	"path"
	"strings"
)

// DynamicSource reports whether p is a source file kind the dynamic layer's collector observes when a test loads it
// (docs/dynamic-layer.md §8.4): JS/TS modules, not declaration files, embedded-script formats or data.
func DynamicSource(p string) bool {
	return isJS(p) && !strings.HasSuffix(p, ".d.ts") && !strings.HasSuffix(p, ".d.mts") && !strings.HasSuffix(p, ".d.cts")
}

// EnvConfigFile reports whether p is a runner/compiler configuration file whose content enters the dynamic layer's environment
// fingerprint (docs/dynamic-layer.md §8.4): runner configs, tsconfig/jsconfig, babel/swc/mocha/ava/c8/nyc/browserslist configs.
// package.json is fingerprinted separately (without its installation keys).
func EnvConfigFile(p string) bool {
	if inNodeModules(p) {
		return false
	}
	return isRunnerConfig(p) || isTSConfig(p) || configKind(p) != ""
}

// ViteConfig reports whether p is a vite/vitest configuration file (bundled natively by vite, docs/dynamic-layer.md §8.3 rule 4).
func ViteConfig(p string) bool {
	base := path.Base(p)
	return (isRunnerConfig(p) || configKind(p) != "") && (strings.HasPrefix(base, "vite.") || strings.HasPrefix(base, "vitest."))
}

// JestConfigFile reports whether p is a jest configuration file.
func JestConfigFile(p string) bool {
	return strings.HasPrefix(path.Base(p), "jest.") && (isRunnerConfig(p) || configKind(p) != "")
}

// InNodeModules reports whether p passes through a node_modules directory.
func InNodeModules(p string) bool { return inNodeModules(p) }
