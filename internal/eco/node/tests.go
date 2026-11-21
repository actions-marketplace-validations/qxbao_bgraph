package node

import (
	"path"
	"strings"
)

// jsExts là đuôi tệp mã JS/TS mà bộ quét hiểu.
var jsExts = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// sfcExts là định dạng nhúng script: bản đầu coi là uncertain_scan (mục 5).
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

// inNodeModules: đường dẫn đi qua node_modules (không bao giờ là mã của repo).
func inNodeModules(p string) bool {
	return p == "node_modules" || strings.HasPrefix(p, "node_modules/") || strings.Contains(p, "/node_modules/")
}

// isTestFile: hợp các mẫu mặc định của Jest, Vitest, Playwright, node:test, Mocha (tập cha: chọn thừa một helper là
// chạy thừa, không sót). Runner cụ thể lọc lại ở adapter.
func isTestFile(p string) bool {
	if !isJS(p) || strings.HasSuffix(p, ".d.ts") || strings.HasSuffix(p, ".d.mts") || strings.HasSuffix(p, ".d.cts") {
		return false
	}
	base := path.Base(p)
	stem := base[:len(base)-len(path.Ext(base))]
	// Jest: **/__tests__/**/*.[jt]s?(x)
	if strings.Contains("/"+p, "/__tests__/") {
		return true
	}
	// Jest/Vitest/Playwright: *.test.*, *.spec.*, và tên chính là test/spec.
	for _, suf := range []string{".test", ".spec", "-test", "_test", ".e2e", ".e2e-spec"} {
		if strings.HasSuffix(stem, suf) {
			return true
		}
	}
	if stem == "test" || stem == "spec" || strings.HasPrefix(stem, "test-") {
		return true // node:test: test.js, test-*.js
	}
	// node:test / Mocha: mọi tệp .js/.cjs/.mjs dưới thư mục test/ (Mocha mặc định ./test/*.spec.js, node --test test/**).
	if strings.Contains("/"+path.Dir(p)+"/", "/test/") || strings.Contains("/"+path.Dir(p)+"/", "/tests/") {
		return !isHelperName(stem)
	}
	return false
}

// isHelperName: tệp trong thư mục test/ có tên rõ là helper/fixture thì không phải tệp test (vẫn là nút đồ thị).
func isHelperName(stem string) bool {
	for _, h := range []string{"helper", "helpers", "util", "utils", "setup", "fixture", "fixtures", "mock", "mocks", "index", "common", "config",
		"global", "globalsetup", "globalteardown", "teardown", "support", "env", "environment"} {
		if stem == h || strings.HasPrefix(stem, h+"-") || strings.HasPrefix(stem, h+"_") || strings.HasPrefix(stem, h+".") {
			return true
		}
	}
	return false
}

// snapshotOwner: "__snapshots__/a.test.ts.snap" → "a.test.ts" (cùng thư mục cha).
func snapshotOwner(p string) (string, bool) {
	dir, base := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if path.Base(dir) != "__snapshots__" || !strings.HasSuffix(base, ".snap") {
		return "", false
	}
	return path.Join(path.Dir(dir), strings.TrimSuffix(base, ".snap")), true
}

// configKind phân loại tệp cấu hình liên quan test/biên dịch của dự án Node.
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
