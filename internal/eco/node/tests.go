package node

import (
	"path"
	"strings"
)

// jsExts are the JS/TS source file extensions the scanner understands.
var jsExts = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// sfcExts are embedded-script formats: the first version treats them as uncertain_scan.
var sfcExts = []string{".vue", ".svelte", ".astro", ".mdx"}

func hasExt(p string, exts []string) bool {
	for _, e := range exts {
		if strings.HasSuffix(p, e) {
			return true
		}
	}
	return false
}

func isJS(p string) bool  { return hasExt(p, jsExts) }
func isSFC(p string) bool { return hasExt(p, sfcExts) }

// inNodeModules: the path passes through node_modules (never repo code).
func inNodeModules(p string) bool {
	return p == "node_modules" || strings.HasPrefix(p, "node_modules/") || strings.Contains(p, "/node_modules/")
}

// isTestFile: default test-file patterns per runner. Names *.test.* / *.spec.* / __tests__ are tests for every runner. The rule
// "every .js under test/ or tests/" is only right for node:test, Mocha, AVA: with Jest/Vitest/Playwright, files in test/
// not matching the name pattern are helpers or DATA (fixtures a spec reads via fs) — mistaking them for tests makes the runner run data
// as tests, while the real spec is not selected when the data changes.
func isTestFile(p, runner string) bool {
	if !isJS(p) || strings.HasSuffix(p, ".d.ts") || strings.HasSuffix(p, ".d.mts") || strings.HasSuffix(p, ".d.cts") {
		return false
	}
	base := path.Base(p)
	stem := base[:len(base)-len(path.Ext(base))]
	// Jest: **/__tests__/**/*.[jt]s?(x)
	if strings.Contains("/"+p, "/__tests__/") {
		return true
	}
	// Jest/Vitest/Playwright: *.test.*, *.spec.*, and the name being exactly test/spec.
	for _, suf := range []string{".test", ".spec", "-test", "_test", ".e2e", ".e2e-spec"} {
		if strings.HasSuffix(stem, suf) {
			return true
		}
	}
	if stem == "test" || stem == "spec" || strings.HasPrefix(stem, "test-") {
		return true // node:test: test.js, test-*.js
	}
	// node:test / Mocha: every .js/.cjs/.mjs file under test/ (Mocha default ./test/*.spec.js, node --test test/**).
	dirRule := runner == "node-test" || runner == "mocha" || runner == "ava"
	if dirRule && (strings.Contains("/"+path.Dir(p)+"/", "/test/") || strings.Contains("/"+path.Dir(p)+"/", "/tests/")) {
		// No exclusion by name (utils.js, index.js in test/ may be real tests: axios, babel). Running an extra helper
		// is harmless; dropping a real test is a silent miss.
		return true
	}
	return false
}

// snapshotOwner: "__snapshots__/a.test.ts.snap" → "a.test.ts" (same parent directory).
func snapshotOwner(p string) (string, bool) {
	dir, base := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if path.Base(dir) != "__snapshots__" || !strings.HasSuffix(base, ".snap") {
		return "", false
	}
	return path.Join(path.Dir(dir), strings.TrimSuffix(base, ".snap")), true
}

// configKind classifies config files related to tests/compilation of a Node project.
func configKind(p string) string {
	base := path.Base(p)
	name := base
	if i := strings.Index(base, ".config."); i > 0 {
		name = base[:i]
	}
	switch name {
	case "jest", "vitest", "vite", "playwright", "babel", "webpack", "rollup", "esbuild", "tsup", "rspack", "karma", "ava", "cypress", "next", "nuxt", "svelte", "astro", "vitest.workspace", "vitest.setup", "jest.setup":
		if strings.Contains(base, ".config.") {
			return name
		}
	}
	switch base {
	case ".babelrc", ".babelrc.json", ".babelrc.js", ".babelrc.cjs", ".swcrc", ".mocharc", ".mocharc.json", ".mocharc.yml", ".mocharc.yaml", ".mocharc.js", ".mocharc.cjs",
		"vitest.workspace.ts", "vitest.workspace.js", "vitest.workspace.json", ".c8rc", ".c8rc.json", ".nycrc", ".nycrc.json", "jest.preset.js", "jest-preset.js",
		"node.config.json", ".browserslistrc", "browserslist":
		return base
	}
	return ""
}

func isTSConfig(p string) bool {
	base := path.Base(p)
	return strings.HasSuffix(base, ".json") && (strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, "jsconfig"))
}

func isLockfile(p string) bool {
	switch path.Base(p) {
	case "pnpm-lock.yaml", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "bun.lock", "bun.lockb":
		return true
	}
	return false
}
