package js

import (
	"reflect"
	"strings"
	"testing"
)

var fuzzSeeds = []string{
	`import x from 'a'; import {a, b as c} from "a"; import * as ns from 'a'; import 'a'; import x, {y} from 'a'`,
	`import defer * as x from 'a'; import source y from 'b'; import j from './a.json' with { type: 'json' }`,
	`import type {X} from 'a'; export type * from 'a'; export * as ns from 'a'; export {a, b as c} from 'a'; export {a}`,
	`import x = require('a'); export import y = require("b"); const r = require; r('x'); foo(require)`,
	"import('a'); await import(`./x/${y}.js`); vi.mock(import('./a')); require('./a/' + x + '.js'); require.resolve('q')",
	`jest.mock('a', () => ({})); vi.importActual<typeof import('./a')>('./b'); jest.requireActual(p)`,
	`import.meta.glob(['a/*.ts', '!b']); import.meta.globEager('x'); require.context('./d', true, /re/); new URL('./x.wasm', import.meta.url)`,
	"/// <reference path=\"x.d.ts\" />\n/// <reference types=\"node\" />\n",
	"#!/usr/bin/env node\n\xef\xbb\xbf",
	"\xef\xbb\xbfimport a from 'a'\r\nconst s = `x\r\ny`;\r\n",
	`const r = /['"]\/[/]/; a / b / c; x = y / 2 / z; if (a) /re/.test(s); f(a) / 2; function f() {}
/re/.test(s); x = {a:1} / 2`,
	"`a ${ `b ${c} ${import('a')}` } d`",
	"const a = <p>don't // do this</p>; const b = <a title=\"it's\" {...p} x={`}`}>{x}<b/></a>; <>frag</>",
	"const f = <T,>(x: T) => x; const g = <T extends U>(x: T) => x; const y = <any>z; Array<Map<string, number>>",
	`class A { require(x) {} import() {} static async *g() {} get y() { return this.#x } } @foo() class B {}`,
	`const o = { import: 1, require: 2, if: 3 }; a.import('x'); a?.require('y'); a.return / 2 / 3; i++ / 2`,
	`declare module 'x' { import y from 'z' } enum E { A } namespace N { } interface I { require(n: string): any }`,
	`typeof require === 'function'; require.main; require.cache[require.resolve('a')]; delete require.cache`,
	`x = 'a\
b'; y = "\u{1F600}\x41\uD83D\uDE00"; z = '\q'`,
	"/* unterminated", "'unterminated", "`unterminated ${", "/unterminated", "(((", ")))", "{[(}])", "<a><b></a>", "<a>{",
	"a = 1_000 + 0xFF + 1n + .5 + 1..toString()",
	"\x00", "\xff\xfe",
}

func FuzzScan(f *testing.F) {
	for _, s := range fuzzSeeds {
		for _, p := range []string{"a.ts", "a.tsx", "a.js"} {
			f.Add(p, s)
		}
	}
	f.Fuzz(func(t *testing.T, path, src string) {
		a := Scan(path, []byte(src))
		b := Scan(path, []byte(src))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("non-deterministic: %+v vs %+v", a, b)
		}
		if a.Uncertain != (a.UncertainReason != "") {
			t.Fatalf("inconsistent uncertain flag: %+v", a)
		}
		if a.Imports == nil {
			t.Fatal("nil Imports")
		}
		maxLine := strings.Count(src, "\n") + 1
		for _, r := range a.Imports {
			if r.Line < 1 || r.Line > maxLine {
				t.Fatalf("line %d out of range [1,%d]: %+v", r.Line, maxLine, r)
			}
			if r.Kind == "" {
				t.Fatalf("empty kind: %+v", r)
			}
			if !r.Dynamic && r.Suffix != "" {
				t.Fatalf("suffix on static ref: %+v", r)
			}
		}
	})
}

// Every seed must run without panicking in all three file-extension modes (this also runs when -fuzz is not enabled).
func TestFuzzSeeds(t *testing.T) {
	for _, s := range fuzzSeeds {
		for _, p := range []string{"a.ts", "a.tsx", "a.js"} {
			Scan(p, []byte(s))
		}
	}
}
