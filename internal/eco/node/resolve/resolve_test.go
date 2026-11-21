package resolve

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

type tc struct {
	name   string
	from   string
	spec   string
	class  Class
	files  []string
	pkg    string
	mism   bool
	detail string // chuỗi con mong đợi trong Detail
}

func run(t *testing.T, files map[string]string, opt Options, cases []tc) *Resolver {
	t.Helper()
	tr := tree.NewMapTree(files)
	r := New(tr, opt)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := r.Resolve(c.from, c.spec)
			if res.Class != c.class {
				t.Fatalf("class = %q (%s), want %q; files=%v", res.Class, res.Detail, c.class, res.Files)
			}
			if c.files != nil || c.class == ClassInternal {
				want := slices.Clone(c.files)
				slices.Sort(want)
				if !slices.Equal(res.Files, want) {
					t.Fatalf("files = %v, want %v", res.Files, want)
				}
			}
			if c.pkg != "" && res.Package != c.pkg {
				t.Fatalf("package = %q, want %q", res.Package, c.pkg)
			}
			if res.CaseMismatch != c.mism {
				t.Fatalf("casemismatch = %v", res.CaseMismatch)
			}
			if c.detail != "" && !strings.Contains(res.Detail, c.detail) {
				t.Fatalf("detail = %q, want contains %q", res.Detail, c.detail)
			}
		})
	}
	return r
}

func ss(s ...string) []string { return s }

func TestRelative(t *testing.T) {
	files := map[string]string{
		"src/a.ts":                "",
		"src/foo.ts":              "",
		"src/foo/index.ts":        "",
		"src/bar/index.tsx":       "",
		"src/esm.mts":             "",
		"src/cjs.cts":             "",
		"src/jsx.tsx":             "",
		"src/types.d.ts":          "",
		"src/a.svg":               "",
		"src/data.json":           "",
		"src/style.css":           "",
		"src/dirpkg/package.json": `{"main":"./lib/main.js"}`,
		"src/dirpkg/lib/main.ts":  "",
		"src/modpkg/package.json": `{"module":"esm.js","types":"t.d.ts","browser":"br.js"}`,
		"src/modpkg/esm.js":       "",
		"src/modpkg/t.d.ts":       "",
		"src/modpkg/br.js":        "",
		"src/Mixed.ts":            "",
		"src/main.ts":             "",
		"lib/x.ts":                "",
		"package.json":            `{"name":"root"}`,
		"top.ts":                  "",
	}
	run(t, files, Options{}, []tc{
		{name: "exact", from: "src/main.ts", spec: "./a.ts", class: ClassInternal, files: ss("src/a.ts")},
		{name: "ext probe ts", from: "src/main.ts", spec: "./a", class: ClassInternal, files: ss("src/a.ts")},
		{name: "union file and index", from: "src/main.ts", spec: "./foo", class: ClassInternal, files: ss("src/foo.ts", "src/foo/index.ts")},
		{name: "index tsx", from: "src/main.ts", spec: "./bar", class: ClassInternal, files: ss("src/bar/index.tsx")},
		{name: "js to ts", from: "src/main.ts", spec: "./a.js", class: ClassInternal, files: ss("src/a.ts")},
		{name: "js to tsx", from: "src/main.ts", spec: "./jsx.js", class: ClassInternal, files: ss("src/jsx.tsx")},
		{name: "jsx to tsx", from: "src/main.ts", spec: "./jsx.jsx", class: ClassInternal, files: ss("src/jsx.tsx")},
		{name: "mjs to mts", from: "src/main.ts", spec: "./esm.mjs", class: ClassInternal, files: ss("src/esm.mts")},
		{name: "cjs to cts", from: "src/main.ts", spec: "./cjs.cjs", class: ClassInternal, files: ss("src/cjs.cts")},
		{name: "js to d.ts", from: "src/main.ts", spec: "./types.js", class: ClassInternal, files: ss("src/types.d.ts")},
		{name: "asset svg query", from: "src/main.ts", spec: "./a.svg?raw", class: ClassInternal, files: ss("src/a.svg")},
		{name: "asset css", from: "src/main.ts", spec: "./style.css", class: ClassInternal, files: ss("src/style.css")},
		{name: "json", from: "src/main.ts", spec: "./data.json", class: ClassInternal, files: ss("src/data.json")},
		{name: "json no ext", from: "src/main.ts", spec: "./data", class: ClassInternal, files: ss("src/data.json")},
		{name: "dir main", from: "src/main.ts", spec: "./dirpkg", class: ClassInternal, files: ss("src/dirpkg/lib/main.ts")},
		{name: "dir module/types/browser", from: "src/main.ts", spec: "./modpkg", class: ClassInternal, files: ss("src/modpkg/br.js", "src/modpkg/esm.js", "src/modpkg/t.d.ts")},
		{name: "parent", from: "src/main.ts", spec: "../lib/x", class: ClassInternal, files: ss("lib/x.ts")},
		{name: "dot dir import from root", from: "top.ts", spec: "./src/foo", class: ClassInternal, files: ss("src/foo.ts", "src/foo/index.ts")},
		{name: "missing", from: "src/main.ts", spec: "./nope", class: ClassUnknown, detail: "missing"},
		{name: "escape root", from: "src/main.ts", spec: "../../outside", class: ClassUnknown, detail: "escapes"},
		{name: "case mismatch", from: "src/main.ts", spec: "./mixed", class: ClassInternal, files: ss("src/Mixed.ts"), mism: true},
		{name: "empty", from: "src/main.ts", spec: "", class: ClassUnknown},
	})
}

func TestProbes(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{"a/x.ts": "", "a/main.ts": ""})
	r := New(tr, Options{})
	res := r.Resolve("a/main.ts", "./x")
	if !slices.Contains(res.Probes, "a/x.tsx") || !slices.Contains(res.Probes, "a/x/index.ts") || slices.Contains(res.Probes, "a/x.ts") {
		t.Fatalf("probes = %v", res.Probes)
	}
	if !slices.IsSorted(res.Probes) {
		t.Fatal("probes not sorted")
	}
}

func TestAbsolute(t *testing.T) {
	files := map[string]string{
		"package.json":       `{"name":"root"}`,
		"public/logo.svg":    "",
		"app/package.json":   `{"name":"app"}`,
		"app/public/img.png": "",
		"app/src/main.ts":    "",
		"src/main.ts":        "",
	}
	run(t, files, Options{}, []tc{
		{name: "root relative", from: "src/main.ts", spec: "/public/logo.svg", class: ClassInternal, files: ss("public/logo.svg")},
		{name: "pkg relative", from: "app/src/main.ts", spec: "/public/img.png", class: ClassInternal, files: ss("app/public/img.png")},
		{name: "pkg and root", from: "app/src/main.ts", spec: "/public/logo.svg", class: ClassInternal, files: ss("public/logo.svg")},
		{name: "abs missing", from: "src/main.ts", spec: "/x/y", class: ClassUnknown},
	})
}

func TestBuiltins(t *testing.T) {
	files := map[string]string{
		"package.json": `{"name":"root"}`,
		"a.ts":         "",
		"mocks/fs.ts":  "",
		"fs.ts":        "",
	}
	opt := Options{Aliases: []Alias{{Regex: regexp.MustCompile(`^fs$`), Targets: []string{"mocks/fs"}}}}
	r := run(t, files, Options{}, []tc{
		{name: "node prefix", from: "a.ts", spec: "node:fs", class: ClassBuiltin},
		{name: "node test", from: "a.ts", spec: "node:test", class: ClassBuiltin},
		{name: "bare fs", from: "a.ts", spec: "fs", class: ClassBuiltin},
		{name: "fs promises", from: "a.ts", spec: "fs/promises", class: ClassBuiltin},
		{name: "path posix", from: "a.ts", spec: "path/posix", class: ClassBuiltin},
		{name: "stream web", from: "a.ts", spec: "stream/web", class: ClassBuiltin},
		{name: "assert strict", from: "a.ts", spec: "assert/strict", class: ClassBuiltin},
		{name: "not builtin subpath", from: "a.ts", spec: "fs/nope", class: ClassUnknown},
	})
	_ = r
	run(t, files, opt, []tc{
		{name: "alias remaps builtin", from: "a.ts", spec: "fs", class: ClassInternal, files: ss("mocks/fs.ts")},
		{name: "node prefix still builtin", from: "a.ts", spec: "node:fs", class: ClassBuiltin},
	})
	// workspace trùng tên builtin không che được
	run(t, map[string]string{"pk/package.json": `{"name":"fs","main":"i.js"}`, "pk/i.js": "", "a.ts": ""},
		Options{Workspaces: map[string]string{"fs": "pk"}}, []tc{
			{name: "workspace cannot shadow", from: "a.ts", spec: "fs", class: ClassBuiltin},
		})
}

const nodeDocsExports = `{
  "name": "pkg",
  "exports": {
    ".": { "import": "./esm/index.js", "require": "./cjs/index.cjs" },
    "./feature": [{"node": "./feat-node.js"}, "./feat-default.js"],
    "./features/*.js": "./src/features/*.js",
    "./features/private/*": null,
    "./features/*": "./src/features/*",
    "./features/private-internal/*": null,
    "./nested": { "node": { "import": "./n-imp.js", "require": "./n-req.js" }, "default": "./n-def.js" },
    "./deep/*": "./deep/*",
    "./pkg.json": "./package.json",
    "./only-null": null,
    "./data/*.json": "./data/*.json"
  }
}`

func exportsTree() map[string]string {
	return map[string]string{
		"package.json":                       `{"name":"root","dependencies":{"ext":"1"}}`,
		"app/main.ts":                        "",
		"libs/pkg/package.json":              nodeDocsExports,
		"libs/pkg/esm/index.js":              "",
		"libs/pkg/cjs/index.cjs":             "",
		"libs/pkg/feat-node.js":              "",
		"libs/pkg/feat-default.js":           "",
		"libs/pkg/src/features/x.js":         "",
		"libs/pkg/src/features/y.json":       "",
		"libs/pkg/src/features/private/z.js": "",
		"libs/pkg/n-imp.js":                  "",
		"libs/pkg/n-req.js":                  "",
		"libs/pkg/n-def.js":                  "",
		"libs/pkg/deep/a/b/c.js":             "",
		"libs/pkg/data/d.json":               "",
		"libs/pkg/internal.js":               "",
		"libs/pkg/src/self.ts":               "",
	}
}

func TestExportsWorkspace(t *testing.T) {
	opt := Options{Workspaces: map[string]string{"pkg": "libs/pkg"}}
	run(t, exportsTree(), opt, []tc{
		{name: "dot conditions union", from: "app/main.ts", spec: "pkg", class: ClassInternal, files: ss("libs/pkg/esm/index.js", "libs/pkg/cjs/index.cjs")},
		{name: "array fallbacks union", from: "app/main.ts", spec: "pkg/feature", class: ClassInternal, files: ss("libs/pkg/feat-node.js", "libs/pkg/feat-default.js")},
		{name: "nested conditions", from: "app/main.ts", spec: "pkg/nested", class: ClassInternal, files: ss("libs/pkg/n-imp.js", "libs/pkg/n-req.js", "libs/pkg/n-def.js")},
		{name: "pattern longer prefix wins", from: "app/main.ts", spec: "pkg/features/x.js", class: ClassInternal, files: ss("libs/pkg/src/features/x.js")},
		{name: "pattern fallback", from: "app/main.ts", spec: "pkg/features/y.json", class: ClassInternal, files: ss("libs/pkg/src/features/y.json")},
		{name: "null pattern more specific", from: "app/main.ts", spec: "pkg/features/private/z.js", class: ClassUnknown, detail: "null"},
		{name: "pattern spans slash", from: "app/main.ts", spec: "pkg/deep/a/b/c.js", class: ClassInternal, files: ss("libs/pkg/deep/a/b/c.js")},
		{name: "exact key", from: "app/main.ts", spec: "pkg/pkg.json", class: ClassInternal, files: ss("libs/pkg/package.json")},
		{name: "only null", from: "app/main.ts", spec: "pkg/only-null", class: ClassUnknown, detail: "null"},
		{name: "not exported", from: "app/main.ts", spec: "pkg/internal.js", class: ClassUnknown, detail: "not exported"},
		{name: "pattern suffix", from: "app/main.ts", spec: "pkg/data/d.json", class: ClassInternal, files: ss("libs/pkg/data/d.json")},
	})
}

func TestExportsSugarAndMisc(t *testing.T) {
	files := map[string]string{
		"a/package.json": `{"name":"sugar","exports":"./i.js"}`,
		"a/i.js":         "",
		"b/package.json": `{"name":"arr","exports":["./x.js","./y.js"]}`,
		"b/x.js":         "",
		"b/y.js":         "",
		"c/package.json": `{"name":"condsugar","exports":{"import":"./m.mjs","default":"./d.js"}}`,
		"c/m.mjs":        "",
		"c/d.js":         "",
		"d/package.json": `{"name":"mixed","exports":{".":"./i.js","import":"./m.js"}}`,
		"d/i.js":         "",
		"e/package.json": `{"name":"nomain"}`,
		"e/index.js":     "",
		"e/sub/file.js":  "",
		"f/package.json": `{"name":"hash","exports":{".":"./i.js","./a/*":"./a/*.js","./a/b/*":"./ab/*.js"}}`,
		"f/ab/q.js":      "",
		"f/a/z.js":       "",
		"f/i.js":         "",
		"main.ts":        "",
		"package.json":   `{"name":"root"}`,
	}
	opt := Options{Workspaces: map[string]string{"sugar": "a", "arr": "b", "condsugar": "c", "mixed": "d", "nomain": "e", "hash": "f"}}
	run(t, files, opt, []tc{
		{name: "string sugar", from: "main.ts", spec: "sugar", class: ClassInternal, files: ss("a/i.js")},
		{name: "string sugar subpath not exported", from: "main.ts", spec: "sugar/x", class: ClassUnknown, detail: "not exported"},
		{name: "array sugar", from: "main.ts", spec: "arr", class: ClassInternal, files: ss("b/x.js", "b/y.js")},
		{name: "conditions sugar", from: "main.ts", spec: "condsugar", class: ClassInternal, files: ss("c/m.mjs", "c/d.js")},
		{name: "mixed keys invalid", from: "main.ts", spec: "mixed", class: ClassUnknown, detail: "invalid exports"},
		{name: "no exports index", from: "main.ts", spec: "nomain", class: ClassInternal, files: ss("e/index.js")},
		{name: "no exports subpath", from: "main.ts", spec: "nomain/sub/file", class: ClassInternal, files: ss("e/sub/file.js")},
		{name: "pattern priority", from: "main.ts", spec: "hash/a/b/q", class: ClassInternal, files: ss("f/ab/q.js")},
		{name: "pattern shorter", from: "main.ts", spec: "hash/a/z", class: ClassInternal, files: ss("f/a/z.js")},
	})
}

func TestMatchKeyCompare(t *testing.T) {
	m := kv{
		[]string{"./a/*", "./a/b/*", "./a/b/c*", "./a/b/c*.js"},
		[]*JSONValue{{Kind: KString, Str: "1"}, {Kind: KString, Str: "2"}, {Kind: KString, Str: "3"}, {Kind: KString, Str: "4"}},
	}
	for sub, want := range map[string]string{"./a/x": "1", "./a/b/x": "2", "./a/b/cx": "3", "./a/b/cx.js": "4"} {
		v, _, pat, ok := matchKey(m, sub)
		if !ok || !pat || v.Str != want {
			t.Errorf("%s -> %v ok=%v", sub, v, ok)
		}
	}
	if _, _, _, ok := matchKey(m, "./b"); ok {
		t.Error("unexpected match")
	}
}

func TestSelfReference(t *testing.T) {
	files := map[string]string{
		"package.json":       `{"name":"@me/self","exports":{".":"./src/index.ts","./util":"./src/util.ts","./x/*":"./src/x/*.ts"}}`,
		"src/index.ts":       "",
		"src/util.ts":        "",
		"src/x/y.ts":         "",
		"src/a/b/c.ts":       "",
		"src/a/package.json": `{"type":"module"}`,
		"other/package.json": `{"name":"other"}`,
		"other/z.ts":         "",
	}
	run(t, files, Options{}, []tc{
		{name: "self root", from: "src/a/b/c.ts", spec: "@me/self", class: ClassInternal, files: ss("src/index.ts")},
		{name: "self subpath", from: "src/a/b/c.ts", spec: "@me/self/util", class: ClassInternal, files: ss("src/util.ts")},
		{name: "self pattern", from: "other/z.ts", spec: "@me/self/x/y", class: ClassInternal, files: ss("src/x/y.ts")},
		{name: "self not exported", from: "src/a/b/c.ts", spec: "@me/self/nope", class: ClassUnknown, detail: "not exported"},
	})
}

func TestSelfReferenceNoExports(t *testing.T) {
	files := map[string]string{
		"package.json": `{"name":"selfp","main":"lib/m.js"}`,
		"lib/m.js":     "",
		"deep/a.ts":    "",
		"deep/sub.ts":  "",
	}
	run(t, files, Options{}, []tc{
		{name: "main", from: "deep/a.ts", spec: "selfp", class: ClassInternal, files: ss("lib/m.js")},
		{name: "literal subpath", from: "deep/a.ts", spec: "selfp/deep/sub", class: ClassInternal, files: ss("deep/sub.ts")},
	})
}

func importsTree() map[string]string {
	return map[string]string{
		"package.json": `{"name":"root","dependencies":{"lodash":"4"},"imports":{
			"#utils": "./src/utils.ts",
			"#lib/*": "./src/lib/*.ts",
			"#lib/special/*": "./src/special/*.ts",
			"#/*": "./src/root/*",
			"#dep": "lodash",
			"#dep2": {"node":"lodash","default":"./src/utils.ts"},
			"#cond": {"import":"./src/esm.ts","require":"./src/cjs.ts"},
			"#arr": ["./src/a1.ts","./src/a2.ts"],
			"#nul": null,
			"#nul2": {"node": null},
			"#dist": "./dist/gen.js",
			"#fs": "node:fs",
			"#undeclared": "nopkg"
		}}`,
		"src/utils.ts": "", "src/lib/a.ts": "", "src/special/s.ts": "", "src/root/r.ts": "",
		"src/esm.ts": "", "src/cjs.ts": "", "src/a1.ts": "", "src/a2.ts": "",
		"src/main.ts":             "",
		"pkgs/inner/package.json": `{"name":"inner","type":"module"}`,
		"pkgs/inner/x.ts":         "",
		"pkgs/own/package.json":   `{"name":"own","imports":{"#o":"./o.ts"}}`,
		"pkgs/own/o.ts":           "",
		"pkgs/own/deep/d.ts":      "",
	}
}

func TestImports(t *testing.T) {
	run(t, importsTree(), Options{}, []tc{
		{name: "exact", from: "src/main.ts", spec: "#utils", class: ClassInternal, files: ss("src/utils.ts")},
		{name: "pattern", from: "src/main.ts", spec: "#lib/a", class: ClassInternal, files: ss("src/lib/a.ts")},
		{name: "pattern priority", from: "src/main.ts", spec: "#lib/special/s", class: ClassInternal, files: ss("src/special/s.ts")},
		{name: "hash slash", from: "src/main.ts", spec: "#/r", class: ClassInternal, files: ss("src/root/r.ts")},
		{name: "to external package", from: "src/main.ts", spec: "#dep", class: ClassExternal, pkg: "lodash"},
		{name: "conditions union to ext and file", from: "src/main.ts", spec: "#dep2", class: ClassInternal, files: ss("src/utils.ts")},
		{name: "conditions union", from: "src/main.ts", spec: "#cond", class: ClassInternal, files: ss("src/esm.ts", "src/cjs.ts")},
		{name: "array", from: "src/main.ts", spec: "#arr", class: ClassInternal, files: ss("src/a1.ts", "src/a2.ts")},
		{name: "null", from: "src/main.ts", spec: "#nul", class: ClassUnknown, detail: "null"},
		{name: "nested null", from: "src/main.ts", spec: "#nul2", class: ClassUnknown, detail: "null"},
		{name: "no key", from: "src/main.ts", spec: "#missing", class: ClassUnknown, detail: "no imports key"},
		{name: "builtin target", from: "src/main.ts", spec: "#fs", class: ClassBuiltin},
		{name: "undeclared target", from: "src/main.ts", spec: "#undeclared", class: ClassUnknown, detail: "undeclared"},
		{name: "dist generated", from: "src/main.ts", spec: "#dist", class: ClassGenerated, pkg: "root"},
		{name: "nested package.json shadows", from: "pkgs/inner/x.ts", spec: "#utils", class: ClassUnknown, detail: "no imports"},
		{name: "own scope", from: "pkgs/own/deep/d.ts", spec: "#o", class: ClassInternal, files: ss("pkgs/own/o.ts")},
		{name: "own scope does not see outer", from: "pkgs/own/deep/d.ts", spec: "#utils", class: ClassUnknown},
	})
	run(t, map[string]string{"a.ts": ""}, Options{}, []tc{
		{name: "no scope", from: "a.ts", spec: "#x", class: ClassUnknown, detail: "no package.json"},
	})
}

func TestImportsJsToTs(t *testing.T) {
	files := map[string]string{
		"package.json": `{"name":"r","imports":{"#src/*":"./src/*.js"}}`,
		"src/a.ts":     "",
		"x.ts":         "",
	}
	run(t, files, Options{}, []tc{
		{name: "js→ts in imports", from: "x.ts", spec: "#src/a", class: ClassInternal, files: ss("src/a.ts")},
	})
}

func TestBareExternalAndUndeclared(t *testing.T) {
	files := map[string]string{
		"package.json":    `{"name":"root","devDependencies":{"vitest":"1","@scope/pkg":"1","left-pad":"1","@types/node-fetch":"1"}}`,
		"pk/package.json": `{"name":"pk","peerDependencies":{"react":"18"},"optionalDependencies":{"fsevents":"2"}}`,
		"pk/src/a.ts":     "",
		"a.ts":            "",
	}
	run(t, files, Options{}, []tc{
		{name: "root dev dep", from: "a.ts", spec: "vitest", class: ClassExternal, pkg: "vitest"},
		{name: "deep import", from: "a.ts", spec: "vitest/config", class: ClassExternal, pkg: "vitest"},
		{name: "scoped", from: "a.ts", spec: "@scope/pkg/sub", class: ClassExternal, pkg: "@scope/pkg"},
		{name: "ancestor package", from: "pk/src/a.ts", spec: "react", class: ClassExternal, pkg: "react"},
		{name: "root dep from nested", from: "pk/src/a.ts", spec: "left-pad", class: ClassExternal, pkg: "left-pad"},
		{name: "optional", from: "pk/src/a.ts", spec: "fsevents", class: ClassExternal, pkg: "fsevents"},
		{name: "types only", from: "a.ts", spec: "node-fetch", class: ClassExternal, pkg: "node-fetch"},
		{name: "undeclared", from: "a.ts", spec: "react", class: ClassUnknown, detail: "undeclared"},
		{name: "invalid scope", from: "a.ts", spec: "@scope", class: ClassUnknown},
		{name: "not visible from sibling", from: "a.ts", spec: "fsevents", class: ClassUnknown},
	})
}

func TestWorkspaceAndGenerated(t *testing.T) {
	files := map[string]string{
		"package.json":                        `{"name":"root"}`,
		"app/package.json":                    `{"name":"app","dependencies":{"@w/lib":"workspace:*","@w/gen":"workspace:*","@w/nomap":"*","@w/main":"*","@w/srcmap":"*"}}`,
		"app/a.ts":                            "",
		"packages/lib/package.json":           `{"name":"@w/lib","main":"./dist/index.js","types":"./dist/index.d.ts","exports":{".":{"types":"./dist/index.d.ts","default":"./dist/index.js"},"./sub":"./dist/sub.js"}}`,
		"packages/lib/tsconfig.json":          `{"compilerOptions":{"outDir":"./dist","rootDir":"./source"}}`,
		"packages/lib/source/index.ts":        "",
		"packages/lib/source/sub.ts":          "",
		"packages/gen/package.json":           `{"name":"@w/gen","exports":"./dist/index.js"}`,
		"packages/nomap/package.json":         `{"name":"@w/nomap","main":"build/x.js"}`,
		"packages/main/package.json":          `{"name":"@w/main","main":"./lib/m.js"}`,
		"packages/main/src/m.ts":              "",
		"packages/srcmap/package.json":        `{"name":"@w/srcmap","main":"./dist/index.js"}`,
		"packages/srcmap/tsconfig.build.json": `{"compilerOptions":{"outDir":"dist"}}`,
		"packages/srcmap/src/index.tsx":       "",
		"packages/srcmap/index.ts":            "",
	}
	opt := Options{Workspaces: map[string]string{
		"@w/lib": "packages/lib", "@w/gen": "packages/gen", "@w/nomap": "packages/nomap", "@w/main": "packages/main", "@w/srcmap": "packages/srcmap",
	}}
	r := run(t, files, opt, []tc{
		{name: "outDir/rootDir mapping", from: "app/a.ts", spec: "@w/lib", class: ClassInternal, files: ss("packages/lib/source/index.ts")},
		{name: "outDir mapping subpath", from: "app/a.ts", spec: "@w/lib/sub", class: ClassInternal, files: ss("packages/lib/source/sub.ts")},
		{name: "generated", from: "app/a.ts", spec: "@w/gen", class: ClassGenerated, pkg: "@w/gen"},
		{name: "generated no tsconfig", from: "app/a.ts", spec: "@w/nomap", class: ClassGenerated, pkg: "@w/nomap"},
		{name: "main lib → src heuristic", from: "app/a.ts", spec: "@w/main", class: ClassInternal, files: ss("packages/main/src/m.ts")},
		{name: "outDir without rootDir → src and pkg root", from: "app/a.ts", spec: "@w/srcmap", class: ClassInternal, files: ss("packages/srcmap/index.ts", "packages/srcmap/src/index.tsx")},
	})
	res := r.Resolve("app/a.ts", "@w/gen")
	if res.PackageDir != "packages/gen" {
		t.Fatalf("PackageDir = %q", res.PackageDir)
	}
}

func TestTsconfigPaths(t *testing.T) {
	files := map[string]string{
		"package.json": `{"name":"r","dependencies":{"react":"1"}}`,
		"tsconfig.json": `{
			// comment
			"compilerOptions": {
				"baseUrl": ".",
				"paths": {
					"@app/*": ["src/app/*", "src/fallback/*"],
					"@app/special/*": ["src/special/*"],
					"@exact": ["src/exact.ts"],
					"~/*": ["./src/*"],
				},
			},
		}`,
		"src/app/a.ts": "", "src/fallback/a.ts": "", "src/fallback/b.ts": "", "src/special/s.ts": "",
		"src/exact.ts": "", "src/x.ts": "", "src/main.ts": "",
		"lib/util.ts":    "",
		"react/index.ts": "", // repo dir called react: baseUrl lookup (over-approx)
	}
	run(t, files, Options{}, []tc{
		{name: "array both candidates", from: "src/main.ts", spec: "@app/a", class: ClassInternal, files: ss("src/app/a.ts", "src/fallback/a.ts")},
		{name: "array second only", from: "src/main.ts", spec: "@app/b", class: ClassInternal, files: ss("src/fallback/b.ts")},
		{name: "longest prefix wins", from: "src/main.ts", spec: "@app/special/s", class: ClassInternal, files: ss("src/special/s.ts")},
		{name: "exact key", from: "src/main.ts", spec: "@exact", class: ClassInternal, files: ss("src/exact.ts")},
		{name: "tilde", from: "src/main.ts", spec: "~/x", class: ClassInternal, files: ss("src/x.ts")},
		{name: "baseUrl", from: "src/main.ts", spec: "lib/util", class: ClassInternal, files: ss("lib/util.ts")},
		{name: "baseUrl shadows external (union)", from: "src/main.ts", spec: "react", class: ClassInternal, files: ss("react/index.ts")},
		{name: "paths nothing exists", from: "src/main.ts", spec: "@app/zzz", class: ClassUnknown},
	})
}

func TestTsconfigExtends(t *testing.T) {
	files := map[string]string{
		"package.json":       `{"name":"r"}`,
		"configs/base.json":  `{"compilerOptions":{"paths":{"@base/*":["../shared/*"],"@old/*":["../old/*"]}}}`,
		"configs/extra.json": `{"compilerOptions":{"paths":{"@extra/*":["../extra/*"]}}}`,
		"shared/s.ts":        "", "old/o.ts": "", "extra/e.ts": "",
		"pkg1/tsconfig.json": `{"extends":"../configs/base.json"}`,
		"pkg1/a.ts":          "",
		"pkg2/tsconfig.json": `{"extends":["../configs/base","../configs/extra.json"],"compilerOptions":{"paths":{"@own/*":["./own/*"]}}}`,
		"pkg2/own/o.ts":      "",
		"pkg2/a.ts":          "",
		"pkg3/tsconfig.json": `{"extends":"../configs/extra.json"}`,
		"pkg3/a.ts":          "",
		"pkg4/tsconfig.json": `{"extends":"../configs/base.json","compilerOptions":{"baseUrl":"./src"}}`,
		"pkg4/src/p4.ts":     "",
		"pkg4/a.ts":          "",
		"pkg5/tsconfig.json": `{"extends":"@tsconfig/node20/tsconfig.json","compilerOptions":{"paths":{"@c/*":["${configDir}/c/*"]}}}`,
		"pkg5/c/c.ts":        "",
		"pkg5/a.ts":          "",
		"pkg6/tsconfig.json": `{"extends":"./missing.json"}`,
		"pkg6/a.ts":          "",
		"pkg7/tsconfig.json": `{"extends":["../configs/base.json","../configs/extra.json"]}`,
		"pkg7/a.ts":          "",
	}
	r := run(t, files, Options{}, []tc{
		{name: "extends chain paths", from: "pkg1/a.ts", spec: "@base/s", class: ClassInternal, files: ss("shared/s.ts")},
		{name: "own paths replace extended", from: "pkg2/a.ts", spec: "@extra/e", class: ClassUnknown},
		{name: "array extends: later replaces earlier", from: "pkg7/a.ts", spec: "@extra/e", class: ClassInternal, files: ss("extra/e.ts")},
		{name: "array extends: earlier replaced", from: "pkg7/a.ts", spec: "@base/s", class: ClassUnknown},
		{name: "array extends: own paths replace", from: "pkg2/a.ts", spec: "@own/o", class: ClassInternal, files: ss("pkg2/own/o.ts")},

		{name: "paths relative to defining config dir", from: "pkg3/a.ts", spec: "@extra/e", class: ClassInternal, files: ss("extra/e.ts")},
		{name: "baseUrl override changes paths base", from: "pkg4/a.ts", spec: "@base/p4", class: ClassUnknown},
		{name: "baseUrl override", from: "pkg4/a.ts", spec: "p4", class: ClassInternal, files: ss("pkg4/src/p4.ts")},
		{name: "package extends ignored, configDir", from: "pkg5/a.ts", spec: "@c/c", class: ClassInternal, files: ss("pkg5/c/c.ts")},
		{name: "extends missing relative: not affecting resolution", from: "pkg6/a.ts", spec: "./nope", class: ClassUnknown},
	})
	errs := r.ConfigErrors()
	if len(errs) != 0 {
		// pkg6 chỉ bị ghi khi tsconfig của nó được đọc (bare specifier)
		t.Logf("errs: %v", errs)
	}
	res := r.Resolve("pkg6/a.ts", "whatever")
	if res.Class != ClassUnknown {
		t.Fatalf("got %v", res)
	}
	found := false
	for _, e := range r.ConfigErrors() {
		if e.Path == "pkg6/tsconfig.json" && strings.Contains(e.Err.Error(), "missing.json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing extends not recorded: %v", r.ConfigErrors())
	}
}

func TestTsconfigAncestorUnionAndJsconfig(t *testing.T) {
	files := map[string]string{
		"package.json":          `{"name":"r"}`,
		"tsconfig.json":         `{"compilerOptions":{"paths":{"@r/*":["root/*"]}}}`,
		"root/r.ts":             "",
		"web/jsconfig.json":     `{"compilerOptions":{"paths":{"@w/*":["./w/*"]}}}`,
		"web/w/w.js":            "",
		"web/tsconfig.app.json": `{"compilerOptions":{"paths":{"@app/*":["./app/*"]}}}`,
		"web/app/x.ts":          "",
		"web/src/main.js":       "",
		"other/main.js":         "",
	}
	run(t, files, Options{}, []tc{
		{name: "jsconfig", from: "web/src/main.js", spec: "@w/w", class: ClassInternal, files: ss("web/w/w.js")},
		{name: "tsconfig.app.json", from: "web/src/main.js", spec: "@app/x", class: ClassInternal, files: ss("web/app/x.ts")},
		{name: "ancestor root", from: "web/src/main.js", spec: "@r/r", class: ClassInternal, files: ss("root/r.ts")},
		{name: "sibling not applied", from: "other/main.js", spec: "@w/w", class: ClassUnknown},
	})
}

func TestBrokenConfigs(t *testing.T) {
	files := map[string]string{
		"package.json":        `{"name":"r","dependencies":{"react":"1"}}`,
		"a.ts":                "",
		"bad/tsconfig.json":   `{"compilerOptions": {`,
		"bad/a.ts":            "",
		"badpkg/package.json": `{"name": `,
		"badpkg/a.ts":         "",
		"badpkg/b.ts":         "",
		"badpkg/sub/c.ts":     "",
	}
	r := run(t, files, Options{}, []tc{
		{name: "relative unaffected by tsconfig", from: "bad/a.ts", spec: "../a", class: ClassInternal, files: ss("a.ts")},
		{name: "bare with broken tsconfig", from: "bad/a.ts", spec: "react", class: ClassUnknown, detail: "config error"},
		{name: "relative with broken package.json in dir", from: "a.ts", spec: "./badpkg", class: ClassUnknown, detail: "config error"},
		{name: "imports with broken scope", from: "badpkg/b.ts", spec: "#x", class: ClassUnknown, detail: "config error"},
		{name: "bare with broken package.json ancestor", from: "badpkg/sub/c.ts", spec: "react", class: ClassUnknown, detail: "config error"},
	})
	errs := r.ConfigErrors()
	var paths []string
	for _, e := range errs {
		paths = append(paths, e.Path)
	}
	if !slices.Equal(paths, ss("bad/tsconfig.json", "badpkg/package.json")) {
		t.Fatalf("ConfigErrors = %v", paths)
	}
	if _, err := r.PackageJSON("badpkg"); err == nil {
		t.Fatal("expected error")
	}
}

func TestJSONC(t *testing.T) {
	src := string(rune(0xFEFF)) + "{\n // c1\n \"a\": [1,2,], /* c2 */ \"b\": \"x // not comment, } ]\",\n \"c\": {\"k\":null,},\n}\n"
	v, err := ParseJSONC([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := v.Get("b").AsString(); s != "x // not comment, } ]" {
		t.Fatalf("b = %q", s)
	}
	if len(v.Get("a").Arr) != 2 || v.Get("c").Get("k").Kind != KNull {
		t.Fatal("bad parse")
	}
	if !slices.Equal(v.Keys, ss("a", "b", "c")) {
		t.Fatalf("order %v", v.Keys)
	}
	if _, err := ParseJSONC([]byte(`{"a":1} x`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := ParseJSONC([]byte(`{"a": /* unterminated`)); err == nil {
		t.Fatal("unterminated accepted")
	}
}

func TestOrderedKeys(t *testing.T) {
	v, err := ParseJSONC([]byte(`{"z":1,"a":{"y":"1","b":"2"},"m":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Keys, ss("z", "a", "m")) || !slices.Equal(v.Get("a").Keys, ss("y", "b")) {
		t.Fatal("key order lost")
	}
}

func TestAliases(t *testing.T) {
	files := map[string]string{
		"package.json":      `{"name":"r","dependencies":{"vue":"3"}}`,
		"src/app/x.ts":      "",
		"src/main.ts":       "",
		"stubs/style.ts":    "",
		"src/ui/button.tsx": "",
		"src/styles/a.css":  "",
	}
	opt := Options{Aliases: []Alias{
		{Regex: regexp.MustCompile(`^@/(.*)$`), Targets: []string{"src/$1"}},
		{Regex: regexp.MustCompile(`\.(css|less)$`), Targets: []string{"stubs/style.ts"}},
		{Prefix: "@ui", Targets: []string{"src/ui"}},
		{Prefix: "vue", Targets: []string{"stubs/vue"}},
		{Regex: regexp.MustCompile(`^esc/(.*)$`), Targets: []string{"../$1"}},
	}}
	run(t, files, opt, []tc{
		{name: "regex capture", from: "src/main.ts", spec: "@/app/x", class: ClassInternal, files: ss("src/app/x.ts")},
		{name: "regex css stub + real", from: "src/main.ts", spec: "./styles/a.css", class: ClassInternal, files: ss("src/styles/a.css", "stubs/style.ts")},
		{name: "prefix", from: "src/main.ts", spec: "@ui/button", class: ClassInternal, files: ss("src/ui/button.tsx")},
		{name: "prefix not partial", from: "src/main.ts", spec: "@uix/button", class: ClassUnknown},
		{name: "alias target missing falls to external", from: "src/main.ts", spec: "vue", class: ClassExternal, pkg: "vue"},
		{name: "alias escaping root", from: "src/main.ts", spec: "esc/x", class: ClassUnknown, detail: "escapes"},
	})
}

func TestResolvePattern(t *testing.T) {
	files := map[string]string{
		"package.json":             `{"name":"r","dependencies":{"lodash":"1"},"imports":{"#loc/*":"./src/locales/*"}}`,
		"src/main.ts":              "",
		"src/locales/en.json":      "",
		"src/locales/vi.json":      "",
		"src/locales/en.ts":        "",
		"src/locales/sub/de.json":  "",
		"src/locales-extra/x.json": "",
		"src/pages/a.vue":          "",
		"tsconfig.json":            `{"compilerOptions":{"paths":{"@l/*":["src/locales/*"]}}}`,
		"x.json":                   "",
	}
	tr := tree.NewMapTree(files)
	r := New(tr, Options{})
	chk := func(name, from, spec, suffix string, class Class, want ...string) {
		t.Run(name, func(t *testing.T) {
			res := r.ResolvePattern(from, spec, suffix)
			if res.Class != class {
				t.Fatalf("class %q (%s) files %v", res.Class, res.Detail, res.Files)
			}
			slices.Sort(want)
			if class == ClassInternal && !slices.Equal(res.Files, want) {
				t.Fatalf("files = %v want %v", res.Files, want)
			}
		})
	}
	chk("locales json", "src/main.ts", "./locales/", ".json", ClassInternal, "src/locales/en.json", "src/locales/sub/de.json", "src/locales/vi.json")
	chk("name prefix", "src/main.ts", "./locales/e", ".json", ClassInternal, "src/locales/en.json")
	chk("no suffix", "src/main.ts", "./pages/", "", ClassInternal, "src/pages/a.vue")
	chk("empty dir match", "src/main.ts", "./locales/zz", ".json", ClassInternal)
	chk("missing dir", "src/main.ts", "./nodir/", ".json", ClassUnknown)
	chk("empty prefix", "src/main.ts", "", ".json", ClassUnknown)
	chk("root relative", "x.json", "./", ".json", ClassInternal, "package.json", "src/locales/en.json", "src/locales/sub/de.json", "src/locales/vi.json", "src/locales-extra/x.json", "tsconfig.json", "x.json")
	chk("imports", "src/main.ts", "#loc/", ".json", ClassInternal, "src/locales/en.json", "src/locales/sub/de.json", "src/locales/vi.json")
	chk("tsconfig paths", "src/main.ts", "@l/", ".json", ClassInternal, "src/locales/en.json", "src/locales/sub/de.json", "src/locales/vi.json")
	chk("bare external dir", "src/main.ts", "lodash/", "", ClassExternal)
	chk("bare undeclared", "src/main.ts", "react/", "", ClassUnknown)
	chk("bare name only", "src/main.ts", "lodash", "", ClassUnknown)
	chk("escape", "src/main.ts", "../../../x/", "", ClassUnknown)
	chk("node prefix", "src/main.ts", "node:", "", ClassBuiltin)
}

func TestCaseMismatchDir(t *testing.T) {
	files := map[string]string{"src/Utils/Index.ts": "", "src/a.ts": "", "package.json": `{}`}
	run(t, files, Options{}, []tc{
		{name: "dir case", from: "src/a.ts", spec: "./utils/Index", class: ClassInternal, files: ss("src/Utils/Index.ts"), mism: true},
	})
}

func TestDeterminismAndCache(t *testing.T) {
	tr := tree.NewMapTree(exportsTree())
	r := New(tr, Options{Workspaces: map[string]string{"pkg": "libs/pkg"}})
	a := r.Resolve("app/main.ts", "pkg/nested")
	b := r.Resolve("app/main.ts", "pkg/nested")
	if !slices.Equal(a.Files, b.Files) || !slices.IsSorted(a.Files) {
		t.Fatal("non-deterministic")
	}
	pj, err := r.PackageJSON("libs/pkg")
	if err != nil || pj == nil || pj.Name != "pkg" || pj.Exports == nil {
		t.Fatalf("pj = %+v %v", pj, err)
	}
	pj2, _ := r.PackageJSON("libs/pkg")
	if pj != pj2 {
		t.Fatal("not cached")
	}
	if pj, err := r.PackageJSON("nonexistent"); pj != nil || err != nil {
		t.Fatal("expected nil,nil")
	}
}

func TestPackageJSONFields(t *testing.T) {
	tr := tree.NewMapTree(map[string]string{"package.json": `{"name":"n","type":"module","main":"m","module":"mo","types":"t","typings":"ty","browser":"b.js","workspaces":{"packages":["a/*"]},"dependencies":{"x":"1"},"exports":null}`})
	r := New(tr, Options{})
	pj, err := r.PackageJSON(".")
	if err != nil {
		t.Fatal(err)
	}
	if pj.Type != "module" || pj.Main != "m" || pj.Module != "mo" || pj.Types != "t" || pj.Typings != "ty" || pj.BrowserString != "b.js" ||
		!slices.Equal(pj.Workspaces, ss("a/*")) || !pj.HasDep("x") || pj.Exports != nil {
		t.Fatalf("%+v", pj)
	}
}

func TestEscapingAndWeird(t *testing.T) {
	files := map[string]string{"package.json": `{"name":"r"}`, "a.ts": ""}
	run(t, files, Options{}, []tc{
		{name: "dotdot from root", from: "a.ts", spec: "..", class: ClassUnknown, detail: "escapes"},
		{name: "dot is dir", from: "a.ts", spec: ".", class: ClassUnknown},
		{name: "url like", from: "a.ts", spec: "https://x.y/z", class: ClassUnknown},
	})
}

func TestImportsStopsAtNodeModules(t *testing.T) {
	files := map[string]string{
		"package.json":                  `{"imports":{"#a":"./a.ts"}}`,
		"a.ts":                          "",
		"node_modules/dep/package.json": `{"name":"dep"}`,
		"node_modules/dep/x.js":         "",
	}
	run(t, files, Options{}, []tc{
		{name: "inside nm", from: "node_modules/dep/x.js", spec: "#a", class: ClassUnknown, detail: "node_modules"},
		{name: "outside", from: "a.ts", spec: "#a", class: ClassInternal, files: ss("a.ts")},
	})
}
