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

// isTestFile: mẫu tệp test mặc định theo runner. Tên *.test.* / *.spec.* / __tests__ là test với mọi runner. Luật
// "mọi .js dưới test/ hoặc tests/" chỉ đúng với node:test, Mocha, AVA: với Jest/Vitest/Playwright, tệp trong test/
// không theo mẫu tên là helper hoặc DỮ LIỆU (fixture mà spec đọc qua fs) — nhận nhầm là test thì runner chạy dữ liệu
// như test, còn spec thật không được chọn khi dữ liệu đổi.
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
	dirRule := runner == "node-test" || runner == "mocha" || runner == "ava"
	if dirRule && (strings.Contains("/"+path.Dir(p)+"/", "/test/") || strings.Contains("/"+path.Dir(p)+"/", "/tests/")) {
		// Không loại trừ theo tên (utils.js, index.js trong test/ có thể là test thật: axios, babel). Chạy thừa một helper
		// là vô hại; bỏ một test thật là sót im lặng.
		return true
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
