package js

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

type ref = model.ImportRef

const (
	kStatic  = model.ImportStatic
	kType    = model.ImportTypeOnly
	kDyn     = model.ImportDynamic
	kReq     = model.ImportRequire
	kResolve = model.ImportResolve
	kMock    = model.ImportMock
	kGlob    = model.ImportGlob
	kURL     = model.ImportURL
	kRe      = model.ImportReExport
	kTriple  = model.ImportTripleSlash
)

func st(spec string, line int) ref { return ref{Spec: spec, Kind: kStatic, Line: line} }
func ty(spec string, line int) ref { return ref{Spec: spec, Kind: kType, Line: line} }
func re(spec string, line int) ref { return ref{Spec: spec, Kind: kRe, Line: line} }
func dy(spec string, line int) ref { return ref{Spec: spec, Kind: kDyn, Line: line} }
func rq(spec string, line int) ref { return ref{Spec: spec, Kind: kReq, Line: line} }
func mk(spec string, line int) ref { return ref{Spec: spec, Kind: kMock, Line: line} }
func gl(spec string, line int) ref { return ref{Spec: spec, Kind: kGlob, Line: line} }
func dyn(r ref, suffix string) ref { r.Dynamic = true; r.Suffix = suffix; return r }

type tc struct {
	name string
	path string
	src  string
	want []ref
	// unc != "" : expect Uncertain with a reason code starting with unc.
	unc string
}

func run(t *testing.T, cases []tc) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := c.path
			if p == "" {
				p = "a.ts"
			}
			got := Scan(p, []byte(c.src))
			if c.unc == "" {
				if got.Uncertain {
					t.Fatalf("unexpected uncertain: %s\nimports=%v", got.UncertainReason, got.Imports)
				}
			} else {
				if !got.Uncertain || !strings.HasPrefix(got.UncertainReason, c.unc) {
					t.Fatalf("want uncertain %q, got uncertain=%v reason=%q imports=%v", c.unc, got.Uncertain, got.UncertainReason, got.Imports)
				}
			}
			if c.want != nil || c.unc == "" {
				w := c.want
				if w == nil {
					w = []ref{}
				}
				if !reflect.DeepEqual(got.Imports, w) {
					t.Fatalf("imports mismatch\n got: %+v\nwant: %+v", got.Imports, w)
				}
			}
		})
	}
}

func TestStaticImports(t *testing.T) {
	run(t, []tc{
		{name: "default", src: `import x from 'a'`, want: []ref{st("a", 1)}},
		{name: "named", src: `import {a, b as c} from "a"`, want: []ref{st("a", 1)}},
		{name: "ns", src: `import * as ns from 'a';`, want: []ref{st("a", 1)}},
		{name: "bare", src: `import 'a'`, want: []ref{st("a", 1)}},
		{name: "default+named", src: `import x, {y} from 'a'`, want: []ref{st("a", 1)}},
		{name: "defer", src: `import defer * as x from 'a'`, want: []ref{st("a", 1)}},
		{name: "source", src: `import source x from 'a'`, want: []ref{st("a", 1)}},
		{name: "attributes with", src: "import j from './a.json' with { type: 'json' };\nimport k from './b'", want: []ref{st("./a.json", 1), st("./b", 2)}},
		{name: "attributes assert", src: "import j from './a.json' assert { type: 'json' }", want: []ref{st("./a.json", 1)}},
		{name: "multi-line list", src: "import {\n  a,\n  b as c,\n  d,\n} from\n  './multi'\nimport z from 'z'", want: []ref{st("./multi", 6), st("z", 7)}},
		{name: "string import names", src: `import {"a-b" as ab, from} from 'a'`, want: []ref{st("a", 1)}},
		{name: "default named from", src: `import from from 'a'`, want: []ref{st("a", 1)}},
		{name: "mixed type member", src: `import {type X, y} from 'a'`, want: []ref{st("a", 1)}},
		{name: "all type members", src: `import {type X} from 'a'`, want: []ref{st("a", 1)}},
		{name: "import type default named type", src: `import type from 'a'`, want: []ref{st("a", 1)}},
		{name: "import type, named", src: `import type, {x} from 'a'`, want: []ref{st("a", 1)}},
		{name: "import after export clause", src: "export {a};\nimport b from 'b'", want: []ref{st("b", 2)}},
		{name: "declare module inner", src: "declare module 'x' {\n  import y from 'z'\n}", want: []ref{st("z", 2)}},
		{name: "key named import", src: "const o = { import: 1, require: 2, export: 3 }\nimport a from 'a'", want: []ref{st("a", 2)}},
		{name: "member import", src: "obj.import('x'); x?.import('y'); a.require('z'); #require; b?.require('q')", want: []ref{}},
	})
}

func TestTypeOnlyAndReexport(t *testing.T) {
	run(t, []tc{
		{name: "import type named", src: `import type {X} from 'a'`, want: []ref{ty("a", 1)}},
		{name: "import type default", src: `import type X from 'a'`, want: []ref{ty("a", 1)}},
		{name: "import type ns", src: `import type * as X from 'a'`, want: []ref{ty("a", 1)}},
		{name: "export type named", src: `export type {X} from 'a'`, want: []ref{ty("a", 1)}},
		{name: "export type star", src: `export type * from 'a'`, want: []ref{ty("a", 1)}},
		{name: "export type star as", src: `export type * as ns from 'a'`, want: []ref{ty("a", 1)}},
		{name: "export star", src: `export * from 'a'`, want: []ref{re("a", 1)}},
		{name: "export star as", src: `export * as ns from 'a'`, want: []ref{re("a", 1)}},
		{name: "export named from", src: `export {a, b as c} from 'a'`, want: []ref{re("a", 1)}},
		{name: "export default from", src: `export {default} from 'a'`, want: []ref{re("a", 1)}},
		{name: "export as string", src: `export {a as "b c"} from 'a'`, want: []ref{re("a", 1)}},
		{name: "export without from", src: "const a = 1\nexport {a}\nexport {a as b}\nexport default a\nexport const z = 'from'", want: []ref{}},
		{name: "export type alias", src: "export type A = string\nexport interface B { from: string }\nexport type {C}", want: []ref{}},
		{name: "export default object", src: "export default { a: 1 }\nimport b from 'b'", want: []ref{st("b", 2)}},
	})
}

func TestRequireForms(t *testing.T) {
	run(t, []tc{
		{name: "import equals", src: `import x = require('a')`, want: []ref{rq("a", 1)}},
		{name: "export import equals", src: `export import x = require('a')`, want: []ref{rq("a", 1)}},
		{name: "import type equals", src: `import type x = require('a')`, want: []ref{rq("a", 1)}},
		{name: "import alias", src: "import x = A.B.C\nimport y from 'y'", want: []ref{st("y", 2)}},
		{name: "require", src: `const a = require('a'); const b = require("b")`, want: []ref{rq("a", 1), rq("b", 1)}},
		{name: "require resolve", src: `require.resolve('a')`, want: []ref{{Spec: "a", Kind: kResolve, Line: 1}}},
		{name: "require nested", src: `foo(require('a'), require('b'))`, want: []ref{rq("a", 1), rq("b", 1)}},
		{name: "require dynamic ident", src: `require(x)`, want: []ref{dyn(rq("", 1), "")}},
		{name: "require empty", src: `require()`, want: []ref{dyn(rq("", 1), "")}},
		{name: "require template", src: "require(`./a`)", want: []ref{rq("./a", 1)}},
		{name: "require concat", src: `require('./a/' + x + '.js')`, want: []ref{dyn(rq("./a/", 1), ".js")}},
		{name: "require concat no suffix", src: `require('./a/' + x)`, want: []ref{dyn(rq("./a/", 1), "")}},
		{name: "require concat literals", src: `require('./a' + '/b')`, want: []ref{rq("./a/b", 1)}},
		{name: "require concat call", src: `require('./a/' + f('x') + '.js')`, want: []ref{dyn(rq("./a/", 1), ".js")}},
		{name: "require operator kills head", src: `require('./a' || x)`, want: []ref{dyn(rq("", 1), "")}},
		{name: "require ident concat", src: `require(x + '.js')`, want: []ref{dyn(rq("", 1), "")}},
		{name: "template subst", src: "require(`./locales/${x}.json`)", want: []ref{dyn(rq("./locales/", 1), ".json")}},
		{name: "template two subst", src: "require(`./l/${a}/${b}.json`)", want: []ref{dyn(rq("./l/", 1), ".json")}},
		{name: "template only subst", src: "require(`${a}`)", want: []ref{dyn(rq("", 1), "")}},
		{name: "template nested import", src: "const a = `x ${import('a')} y`", want: []ref{dy("a", 1)}},
		{name: "template nested deep", src: "`a ${ `b ${ require('c') }` } d`", want: []ref{rq("c", 1)}},
		{name: "require as typeof UMD", src: "(function(f){ if (typeof require === 'function' && typeof exports === 'object') {} })()", want: []ref{}},
		{name: "require.main", src: "if (require.main === module) { require('./cli') }", want: []ref{rq("./cli", 1)}},
		{name: "require.cache", src: "delete require.cache[require.resolve('a')]", want: []ref{{Spec: "a", Kind: kResolve, Line: 1}}},
		{name: "const require createRequire", src: "const require = createRequire(import.meta.url)\nrequire('a')", want: []ref{rq("a", 2)}},
		{name: "function require decl", src: "function require(n) { return 1 }\nrequire('a')", want: []ref{rq("a", 2)}},
		{name: "params require", src: "define(function (require, exports, module) { var a = require('a') })", want: []ref{rq("a", 1)}},
		{name: "arrow params require", src: "const f = (require) => require('a')", want: []ref{rq("a", 1)}},
		{name: "class method named require", src: "class A { require(x) { return 1 } import() {} static require() {} }", want: []ref{}},
		{name: "object method named require", src: "const o = { require(x) { return 1 }, import(y) { return 2 } }", want: []ref{}},
		{name: "object key", src: "const o = { require: 1, import: 2, 'x': require('a') }", want: []ref{rq("a", 1)}},
		{name: "interface member", src: "interface I { require(n: string): any; import(n: string): any; require: string }", want: []ref{}},
		{name: "real call after block", src: "if (x) {\n}\nrequire('a')", want: []ref{rq("a", 3)}},
		{name: "real call in object value", src: "const o = { a: require('a'), b: import('b') }", want: []ref{rq("a", 1), dy("b", 1)}},
	})
}

func TestDynamicImport(t *testing.T) {
	run(t, []tc{
		{name: "import()", src: `import('a')`, want: []ref{dy("a", 1)}},
		{name: "await import", src: "async function f() {\n  const m = await import('./m')\n}", want: []ref{dy("./m", 2)}},
		{name: "import with attrs", src: `import('./a.json', { with: { type: 'json' } })`, want: []ref{dy("./a.json", 1)}},
		{name: "import ident", src: `import(x)`, want: []ref{dyn(dy("", 1), "")}},
		{name: "import template", src: "import(`./pages/${name}.tsx`)", want: []ref{dyn(dy("./pages/", 1), ".tsx")}},
		{name: "import concat", src: `import('./a/' + x + '.js')`, want: []ref{dyn(dy("./a/", 1), ".js")}},
		{name: "import in vi.mock", src: `vi.mock(import('./a'))`, want: []ref{dy("./a", 1)}},
		{name: "import types", src: `let x: typeof import('./a')`, want: []ref{dy("./a", 1)}},
		{name: "import escape", src: `import('a\'b\\c\n\t\x41B\u{43}')`, want: []ref{dy("a'b\\c\n\tABC", 1)}},
		{name: "import line", src: "foo()\n\nimport(\n  'x'\n)", want: []ref{dy("x", 4)}},
		{name: "import bad escape", src: `import('a\qb')`, unc: "bad_escape"},
		{name: "import octal escape", src: `import('a\1b')`, unc: "bad_escape"},
		{name: "import in comment/string", src: "// import('a')\n/* import('b') */\nconst s = \"import('c')\"", want: []ref{}},
		{name: "chain after", src: `import('a').then(m => m.x)`, want: []ref{dy("a", 1)}},
	})
}

func TestMocks(t *testing.T) {
	run(t, []tc{
		{name: "jest.mock", src: `jest.mock('./a', () => ({ x: 1 }))`, want: []ref{mk("./a", 1)}},
		{name: "jest others", src: "jest.unmock('a'); jest.doMock('b'); jest.dontMock('c'); jest.requireActual('d'); jest.requireMock('e'); jest.createMockFromModule('f'); jest.setMock('g', {})",
			want: []ref{mk("a", 1), mk("b", 1), mk("c", 1), mk("d", 1), mk("e", 1), mk("f", 1), mk("g", 1)}},
		{name: "vi others", src: "vi.mock('a'); vi.doMock('b'); vi.unmock('c'); vi.doUnmock('d'); await vi.importActual('e'); vi.importMock('f')",
			want: []ref{mk("a", 1), mk("b", 1), mk("c", 1), mk("d", 1), mk("e", 1), mk("f", 1)}},
		{name: "jest.mock dynamic arg", src: `jest.mock(name)`, want: []ref{dyn(mk("", 1), "")}},
		{name: "foo.jest.mock", src: `foo.jest.mock('a')`, want: []ref{mk("a", 1)}},
		{name: "not jest", src: `foo.mock('a'); mock('b'); jest.other('c'); vi.requireActual('d')`, want: []ref{}},
		{name: "nested actual", src: "vi.mock('./a', async () => {\n  const actual = await vi.importActual('./b')\n  return { ...actual }\n})", want: []ref{mk("./a", 1), mk("./b", 2)}},
		{name: "importActual generic", src: "vi.importActual<typeof import('./a')>('./b')", want: []ref{dy("./a", 1), mk("./b", 1)}},
		{name: "jest.mock template", src: "jest.mock(`./a`)", want: []ref{mk("./a", 1)}},
	})
}

func TestGlobCtxURL(t *testing.T) {
	run(t, []tc{
		{name: "glob str", src: `import.meta.glob('./a/*.ts')`, want: []ref{gl("./a/*.ts", 1)}},
		{name: "glob opts", src: `import.meta.glob('./a/*.ts', { eager: true, import: 'default' })`, want: []ref{gl("./a/*.ts", 1)}},
		{name: "glob eager", src: `import.meta.globEager('./a/*.ts')`, want: []ref{gl("./a/*.ts", 1)}},
		{name: "glob array", src: "import.meta.glob(['./a/*.ts', '!./b/*.ts',\n `./c/*.ts`])", want: []ref{gl("./a/*.ts", 1), gl("!./b/*.ts", 1), gl("./c/*.ts", 2)}},
		{name: "glob array dynamic", src: "import.meta.glob(['./a/*.ts', x])", want: []ref{gl("./a/*.ts", 1), dyn(gl("", 1), "")}},
		{name: "glob ident", src: "import.meta.glob(p)", want: []ref{dyn(gl("", 1), "")}},
		{name: "glob generic", src: "import.meta.glob<{default: string}>('./a/*.ts')", want: []ref{gl("./a/*.ts", 1)}},
		{name: "import.meta other", src: "const e = import.meta.env.X; const u = import.meta.url", want: []ref{}},
		{name: "ctx recursive", src: `require.context('./dir', true, /\.js$/)`, want: []ref{gl("./dir/**", 1)}},
		{name: "ctx nonrec", src: `require.context('./dir', false)`, want: []ref{gl("./dir/*", 1)}},
		{name: "ctx absent", src: `require.context('./dir')`, want: []ref{gl("./dir/*", 1)}},
		{name: "ctx slash", src: `require.context('./dir/', true)`, want: []ref{gl("./dir/**", 1)}},
		{name: "ctx dot", src: `require.context('.', true)`, want: []ref{gl("./**", 1)}},
		{name: "new URL", src: `new URL('./x.wasm', import.meta.url)`, want: []ref{{Spec: "./x.wasm", Kind: kURL, Line: 1}}},
		{name: "new URL dyn", src: "new URL(`./a/${n}.png`, import.meta.url)", want: []ref{{Spec: "./a/", Suffix: ".png", Kind: kURL, Line: 1, Dynamic: true}}},
		{name: "new URL other", src: `new URL('https://example.com'); new URL(x, base); new URL('./a', 'http://b')`, want: []ref{}},
		{name: "URL not new", src: `URL('./x', import.meta.url)`, want: []ref{}},
	})
}

func TestTripleSlash(t *testing.T) {
	run(t, []tc{
		{name: "path", src: "/// <reference path=\"x.d.ts\" />\n/// <reference types=\"node\" />\n/// <reference lib=\"es2020\" />\nimport a from 'a'",
			want: []ref{{Spec: "x.d.ts", Kind: kTriple, Line: 1}, st("a", 4)}},
		{name: "single quote", src: "///<reference path='y.d.ts'/>", want: []ref{{Spec: "y.d.ts", Kind: kTriple, Line: 1}}},
		{name: "not reference", src: "/// just a comment\n//// <reference path=\"no\" />", want: []ref{}},
		{name: "in block comment", src: "/*\n/// <reference path=\"x\" />\n*/", want: []ref{}},
	})
}

func TestLexerHardCases(t *testing.T) {
	imp := "\nimport z from 'zzz'\n"
	run(t, []tc{
		{name: "regex quotes", src: "const r = /['\"]\\/[/]/;" + imp, want: []ref{st("zzz", 2)}},
		{name: "regex class slash", src: "x = /[/]'/g.test(s)" + imp, want: []ref{st("zzz", 2)}},
		{name: "division chain", src: "a / b / c; x = y / 2 / z; q = 'x' + 1 / 2 / 'y'" + imp, want: []ref{st("zzz", 2)}},
		{name: "if paren regex", src: "if (a) /re'/.test(s)" + imp, want: []ref{st("zzz", 2)}},
		{name: "while paren regex", src: "while (a) /re'/.test(s)" + imp, want: []ref{st("zzz", 2)}},
		{name: "for paren regex", src: "for (;;) /re'/.test(s)" + imp, want: []ref{st("zzz", 2)}},
		{name: "call paren division", src: "x = f(a) / 2 / 3; y = (a + b) / 2 / 3" + imp, want: []ref{st("zzz", 2)}},
		{name: "block then regex", src: "function f() {}\n/re'/.test(s)" + imp, want: []ref{st("zzz", 3)}},
		{name: "if block then regex", src: "if (a) { b() } else { c() }\n/re'/.test(s)" + imp, want: []ref{st("zzz", 3)}},
		{name: "arrow block then regex", src: "const f = () => { }\n/re'/.test(s)" + imp, want: []ref{st("zzz", 3)}},
		{name: "object then division", src: "x = {a: 1} / 2 / 3; y = {} / 4" + imp, want: []ref{st("zzz", 2)}},
		{name: "return regex", src: "function f() { return /'/.test(s) }" + imp, want: []ref{st("zzz", 2)}},
		{name: "keyword regex", src: "x = typeof /'/; y = a in /'/; z = void /'/; throw /'/" + imp, want: []ref{st("zzz", 2)}},
		{name: "property keyword division", src: "a.return / 2 / 3; a.default / 2 / 3; a?.if / 2 / 3; o = { if: 1 }; p = o.in / 2 / 3" + imp, want: []ref{st("zzz", 2)}},
		{name: "postfix division", src: "i++ / 2 / 3; j-- / 2 / 3" + imp, want: []ref{st("zzz", 2)}},
		{name: "regex after assign and comma", src: "x = /'/; f(/'/, /\"/)" + imp, want: []ref{st("zzz", 2)}},
		{name: "regex after colon and bracket", src: "o = {a: /'/}; l = [/'/, /\"/]" + imp, want: []ref{st("zzz", 2)}},
		{name: "regex after ! and &&", src: "if (!/'/.test(s) && /\"/.test(s)) {}" + imp, want: []ref{st("zzz", 2)}},
		{name: "template nested", src: "`a ${ `b ${c} 'x` } d`;\n" + imp, want: []ref{st("zzz", 3)}},
		{name: "template with import inside", src: "x = `a ${import('a')} b ${ require(\"r\") }`", want: []ref{dy("a", 1), rq("r", 1)}},
		{name: "template braces in subst", src: "x = `a ${ {a: 1}.a } ${ (() => { return `}` })() }`" + imp, want: []ref{st("zzz", 2)}},
		{name: "template fake", src: "x = `import y from 'fake'; require('fake')`" + imp, want: []ref{st("zzz", 2)}},
		{name: "comment fake", src: "// import x from 'fake'\n/* require('fake')\nimport y from 'fake' */\n/** jest.mock('fake') */" + imp, want: []ref{st("zzz", 5)}},
		{name: "string fake", src: "a = \"import x from 'fake'\"; b = 'require(\"fake\")';" + imp, want: []ref{st("zzz", 2)}},
		{name: "numeric forms", src: "x = 1_000 + 0xFF + 1n + 1.5e+10 + .5 + 1..toString() + 0b1_0;" + imp, want: []ref{st("zzz", 2)}},
		{name: "ts generics", src: "const m: Array<Map<string, number>> = new Map<string, number>(); const y = a < b; if (x < y && y > z) {}" + imp, want: []ref{st("zzz", 2)}},
		{name: "ts misc", src: "const x = y as const; const z = w satisfies T; a?.b; a ?? b; class A { #x = 1; get y() { return this.#x } }\n@foo() class B {}" + imp, want: []ref{st("zzz", 3)}},
		{name: "ts return type object", src: "function f(): { a: number } { return { a: 1 } }\nfunction g(): void { require('a') }", want: []ref{rq("a", 2)}},
		{name: "ts arrow generic", src: "const f = <T>(x: T) => x; const g = <T extends object>(x: T) => x" + imp, want: []ref{st("zzz", 2)}},
		{name: "ts type assertion", src: "const x = <any>y; const z = <Foo<Bar>>w;" + imp, want: []ref{st("zzz", 2)}},
		{name: "ts abstract/enum/namespace", src: "abstract class A { abstract m(): void }\nenum E { A = 1, B }\nnamespace N { export const x = 1 }\ndeclare global { interface W { x: 1 } }" + imp, want: []ref{st("zzz", 5)}},
		{name: "keyword property string", src: "x = { 'import': 1, \"require\": 2 }; y = a['import'];" + imp, want: []ref{st("zzz", 2)}},
		{name: "getter setter", src: "const o = { get a() { return 1 }, set a(v) {} }; class C { static async *g() {} }" + imp, want: []ref{st("zzz", 2)}},
		{name: "label and case", src: "switch (x) { case 1: { break } default: { } }\nl: for (;;) { break l }" + imp, want: []ref{st("zzz", 3)}},
		{name: "unicode ident and nbsp", src: "const ünï = 1; const b = ünï / 2 / 3;" + imp, want: []ref{st("zzz", 2)}},
		{name: "hashbang", src: "#!/usr/bin/env node\nimport a from 'a'", want: []ref{st("a", 2)}},
		{name: "bom", src: "\xef\xbb\xbfimport a from 'a'", want: []ref{st("a", 1)}},
		{name: "bom hashbang", src: "\xef\xbb\xbf#!/usr/bin/env node\nrequire('a')", want: []ref{rq("a", 2)}},
		{name: "crlf lines", src: "import a from 'a'\r\n\r\nconst s = `x\r\ny`;\r\n/* c\r\n */\r\nimport('b')\r\n", want: []ref{st("a", 1), dy("b", 7)}},
		{name: "line continuation", src: "x = 'a\\\nb';\nrequire('c')", want: []ref{rq("c", 3)}},
		{name: "line continuation crlf", src: "x = 'a\\\r\nb';\r\nrequire('c')", want: []ref{rq("c", 3)}},
		{name: "html comment-ish", src: "a = b <!--c\n; require('x')", want: []ref{rq("x", 2)}},
		{name: "regex flags then division", src: "a = /x/g / 2; b = c / /y/.source.length" + imp, want: []ref{st("zzz", 2)}},
		{name: "regex with escaped slash and class", src: "a = /\\/[\\]/]+/u" + imp, want: []ref{st("zzz", 2)}},
		{name: "async arrow in call", src: "foo(async () => { await import('a') }, () => {});\n/'/.test(s)" + imp, want: []ref{dy("a", 1), st("zzz", 3)}},
		{name: "of regex", src: "for (const x of /a/g.exec(s)) {}" + imp, want: []ref{st("zzz", 2)}},
		{name: "yield await regex", src: "function* g() { yield /'/; } async function h() { await /'/ }" + imp, want: []ref{st("zzz", 2)}},
		{name: "default export regex", src: "export default /'/;" + imp, want: []ref{st("zzz", 2)}},
		{name: "shift left not jsx", path: "a.js", src: "x = 128<<r%32; y = a<<b; z = (a)<<c" + imp, want: []ref{st("zzz", 2)}},
		{name: "ts non-null division", path: "a.tsx", src: "const a = s! / h; const b = (x!) / 2 / 3; if (a != b && !/'/.test(c)) {}" + imp, want: []ref{st("zzz", 2)}},
		{name: "class member import type", path: "a.d.ts", src: "declare class A { x(): import('events').EventEmitter; import(n: string): void; static require(): any }\ninterface I { y: import('./i'); require(n: string): any }", want: []ref{dy("events", 1), dy("./i", 2)}},
	})
}

func TestJSX(t *testing.T) {
	imp := "\nimport z from 'zzz'\n"
	run(t, []tc{
		{name: "text apostrophe", path: "a.jsx", src: "const a = <p>don't // do this</p>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "attr apostrophe", path: "a.tsx", src: "const a = <a title=\"it's\">x</a>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "attr dquote in single", path: "a.js", src: "const a = <a title='say \"hi\"'>x</a>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "self closing", path: "a.jsx", src: "const a = <Foo bar={1} baz=\"x\" />;" + imp, want: []ref{st("zzz", 2)}},
		{name: "fragment", path: "a.jsx", src: "const a = <>it's <b>x</b> a // b</>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "expr children", path: "a.jsx", src: "const a = <ul>{items.map(i => <li key={i}>{i} 'x</li>)}</ul>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "import in jsx expr", path: "a.jsx", src: "const a = <div>{import('a')} text's {require('b')}</div>", want: []ref{dy("a", 1), rq("b", 1)}},
		{name: "spread attr", path: "a.jsx", src: "const a = <Foo {...props} x={`}`} y={{a: '}'}} />;" + imp, want: []ref{st("zzz", 2)}},
		{name: "return jsx", path: "a.js", src: "function f() { return <div className=\"a\">\n  hello's\n</div> }\nimport q from 'q'", want: []ref{st("q", 4)}},
		{name: "ternary jsx", path: "a.jsx", src: "x = c ? <a>'</a> : <b>\"</b>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "and jsx", path: "a.jsx", src: "x = c && <a>'</a>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "member tag", path: "a.jsx", src: "x = <Foo.Bar a:b=\"1\" data-x='2'>'</Foo.Bar>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "tag comment", path: "a.jsx", src: "x = <a /* c ' */ b={1}>'</a>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "tsx generic arrow comma", path: "a.tsx", src: "const f = <T,>(x: T) => x; const g = <T extends U>(x: T) => x;" + imp, want: []ref{st("zzz", 2)}},
		{name: "tsx generic with default", path: "a.tsx", src: "const f = <T = any>(x: T) => x;" + imp, want: []ref{st("zzz", 2)}},
		{name: "tsx type args on element", path: "a.tsx", src: "const a = <Foo<string> x=\"1\">it's</Foo>;" + imp, want: []ref{st("zzz", 2)}},
		{name: "less than not jsx", path: "a.jsx", src: "if (a < b) {} for (i = 0; i < n; i++) {}; x = a<b>c;" + imp, want: []ref{st("zzz", 2)}},
		{name: "ts no jsx", path: "a.ts", src: "const x = <any>y; const q = cond ? <T>a : <U>b;" + imp, want: []ref{st("zzz", 2)}},
		{name: "mismatched closing", path: "a.jsx", src: "x = <a><b></a>;\nimport q from 'q'", unc: "jsx"},
		{name: "eof in jsx", path: "a.jsx", src: "x = <a>text", unc: "jsx"},
		{name: "eof in tag", path: "a.jsx", src: "x = <a b=", unc: "jsx"},
		{name: "mts not jsx", path: "a.mts", src: "const x = <T>(y: T) => y" + imp, want: []ref{st("zzz", 2)}},
	})
}

func TestUncertain(t *testing.T) {
	run(t, []tc{
		{name: "unterminated string", src: "const a = 'abc\nimport x from 'a'", unc: "unterminated_string"},
		{name: "unterminated string eof", src: "const a = 'abc", unc: "unterminated_string"},
		{name: "unterminated template", src: "const a = `abc ${x}", unc: "unterminated_template"},
		{name: "unterminated template 2", src: "const a = `abc", unc: "unterminated_template"},
		{name: "unterminated regex", src: "const a = /abc\n", unc: "unterminated_regex"},
		{name: "unterminated comment", src: "import a from 'a'\n/* abc", want: []ref{st("a", 1)}, unc: "unterminated_comment"},
		{name: "unbalanced open", src: "import a from 'a'\nfunction f() {", want: []ref{st("a", 1)}, unc: "unbalanced_brackets"},
		{name: "unbalanced close", src: "import a from 'a'\n)", want: []ref{st("a", 1)}, unc: "unbalanced_brackets"},
		{name: "mismatch", src: "f(]", unc: "unbalanced_brackets"},
		{name: "mismatch brace", src: "{ ( }", unc: "unbalanced_brackets"},
		{name: "stray close brace", src: "}", unc: "unbalanced_brackets"},
		{name: "unbalanced bracket in template subst", src: "`${ ( }`", unc: "unbalanced_brackets"},
		{name: "NUL", src: "import a from 'a'\x00", unc: "binary_content"},
		{name: "invalid utf8", src: "import a from 'a' // \xff\xfe", unc: "invalid_utf8"},
		{name: "require alias", src: "const r = require; r('x')", unc: "require_value"},
		{name: "require arg", src: "foo(require)", unc: "require_value"},
		{name: "require arg 2", src: "foo(a, require)", unc: "require_value"},
		{name: "module.exports = require", src: "module.exports = require", unc: "require_value"},
		{name: "require in array", src: "x = [require]", unc: "require_value"},
		{name: "require shorthand", src: "x = { require }", unc: "require_value"},
		{name: "require ternary", src: "x = a ? require : b", unc: "require_value"},
		{name: "require at eof", src: "x = require", unc: "require_value"},
		{name: "require.bind", src: "x = require.bind(null)", unc: "require_value"},
		{name: "require in cond paren", src: "if (require) {}", unc: "require_value"},
		{name: "ident escape", src: "\\u0069mport('a')", unc: "ident_escape"},
		{name: "keeps found imports", src: "import a from 'a'\nrequire('b')\nx = require", want: []ref{st("a", 1), rq("b", 2)}, unc: "require_value"},
	})
	t.Run("too large", func(t *testing.T) {
		got := Scan("a.js", make([]byte, MaxFileSize+1))
		if !got.Uncertain || !strings.HasPrefix(got.UncertainReason, "file_too_large") {
			t.Fatalf("got %+v", got)
		}
		src := []byte(strings.Repeat(" ", MaxFileSize))
		if got := Scan("a.js", src); got.Uncertain {
			t.Fatalf("exact max size should be certain: %+v", got)
		}
	})
}

func TestMinified(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, `function f%d(a,b){return a/b/2+(/x'\/[/]/.test(a)?1:0)}var o%d={a:1,b:[1,2,3],c:"it's"};if(a)/'/.test(b);`, i, i)
	}
	b.WriteString(`import("m1");var q=require("m2");export*from"m3";import x from"m4";`)
	got := Scan("a.js", []byte(b.String()))
	if got.Uncertain {
		t.Fatalf("uncertain: %s", got.UncertainReason)
	}
	want := []ref{dy("m1", 1), rq("m2", 1), re("m3", 1), st("m4", 1)}
	if !reflect.DeepEqual(got.Imports, want) {
		t.Fatalf("got %+v", got.Imports)
	}
}

func TestDeterministicOrder(t *testing.T) {
	src := "require(foo(require('a'), import('b')))\nimport.meta.glob(['x', import('y')])\nvi.mock(import('c'))"
	got := Scan("a.ts", []byte(src))
	want := []ref{dyn(rq("", 1), ""), rq("a", 1), dy("b", 1), gl("x", 2), dyn(gl("", 2), ""), dy("y", 2), dy("c", 3)}
	if !reflect.DeepEqual(got.Imports, want) || got.Uncertain {
		t.Fatalf("got %+v uncertain=%v", got.Imports, got.Uncertain)
	}
}

func TestDeepNesting(t *testing.T) {
	got := Scan("a.jsx", []byte(strings.Repeat("<a>{", 1000)))
	if !got.Uncertain {
		t.Fatal("want uncertain")
	}
	got = Scan("a.ts", []byte(strings.Repeat("`${", 1000)))
	if !got.Uncertain {
		t.Fatal("want uncertain")
	}
	got = Scan("a.ts", []byte(strings.Repeat("(", 5000)))
	if !got.Uncertain {
		t.Fatal("want uncertain")
	}
}
