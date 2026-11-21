package sfc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

type want struct {
	spec string
	line int
}

func specs(r model.ScanResult) []want {
	out := []want{}
	for _, im := range r.Imports {
		out = append(out, want{im.Spec, im.Line})
	}
	return out
}

func TestScan(t *testing.T) {
	cases := []struct {
		name, path, src string
		imps            []want
		unc             string // chuỗi con của lý do; rỗng = chắc chắn
	}{
		// ---- Vue ----
		{name: "vue basic", path: "a.vue", src: "<template><div/></template>\n<script>\nimport a from './a'\nimport('./b')\n</script>\n",
			imps: []want{{"./a", 3}, {"./b", 4}}},
		{name: "vue setup ts", path: "a.vue", src: "<script setup lang=\"ts\">\nimport type { X } from './x'\nconst a = <T,>(v: T) => v\n</script>",
			imps: []want{{"./x", 2}}},
		{name: "vue script+setup", path: "a.vue", src: "<script>\nimport a from 'a'\n</script>\n<script setup>\nimport b from 'b'\n</script>",
			imps: []want{{"a", 2}, {"b", 5}}},
		{name: "vue script src", path: "a.vue", src: "<script src=\"./x.ts\" lang=\"ts\"></script>\n<template/>", imps: []want{{"./x.ts", 1}}},
		{name: "vue script src and inline", path: "a.vue", src: "\n<script src=\"./x.ts\">\nimport z from 'z'\n</script>", imps: []want{{"./x.ts", 2}, {"z", 3}}},
		{name: "vue comment fake script", path: "a.vue", src: "<!-- <script>import evil from 'evil'</script> -->\n<script>\nimport ok from 'ok'\n</script>",
			imps: []want{{"ok", 3}}},
		{name: "vue comment unclosed", path: "a.vue", src: "<!-- <script>\n", unc: "unclosed_comment"},
		{name: "vue nested script in template ignored", path: "a.vue",
			src:  "<template>\n<div><script>import x from 'x'</script><template v-if=\"a\"><b/></template></div>\n</template>\n<script>\nimport y from 'y'\n</script>",
			imps: []want{{"y", 5}}},
		{name: "vue template unclosed", path: "a.vue", src: "<template><template></template>", unc: "unclosed_block"},
		{name: "vue style import", path: "a.vue", src: "<template/>\n<style scoped>\n@import './a.css';\n@import url(\"./b.css\");\n@import url(c.css) screen;\n@import 'https://x/y.css';\n</style>",
			imps: []want{{"./a.css", 3}, {"./b.css", 4}, {"c.css", 5}}},
		{name: "vue style src", path: "a.vue", src: "<style src=\"./a.css\"></style>", imps: []want{{"./a.css", 1}}},
		{name: "vue scss use", path: "a.vue", src: "<style lang=\"scss\">\n// @import 'no'\n@use \"./vars\" as v;\n@use 'sass:math';\n@forward '../x';\n@import 'a', 'b';\n/* @import 'c' */\n</style>",
			imps: []want{{"./vars", 3}, {"../x", 5}, {"a", 6}, {"b", 6}}},
		{name: "vue less options", path: "a.vue", src: "<style lang=\"less\">\n@import (reference) \"./a.less\";\n</style>", imps: []want{{"./a.less", 2}}},
		{name: "vue stylus unsupported", path: "a.vue", src: "<style lang=\"stylus\">\n@import 'a'\n</style>", unc: "style_lang"},
		{name: "vue css dynamic", path: "a.vue", src: "<style lang=\"scss\">\n@import \"a#{$b}\";\n</style>", unc: "css_dynamic_import"},
		{name: "vue template src and custom src", path: "a.vue", src: "<template src=\"./t.html\"></template>\n<i18n src=\"./loc.json\"></i18n>\n<docs>\nimport x from 'x'\n</docs>",
			imps: []want{{"./t.html", 1}, {"./loc.json", 2}}},
		{name: "vue custom self-closing", path: "a.vue", src: "<i18n src=\"./l.json\" />\n<script>import a from 'a'</script>", imps: []want{{"./l.json", 1}, {"a", 2}}},
		{name: "vue unclosed script", path: "a.vue", src: "<script>\nimport a from 'a'\n", unc: "unclosed_block"},
		{name: "vue unclosed tag", path: "a.vue", src: "<script lang=\"ts\"", unc: "unclosed_tag"},
		{name: "vue duplicate setup", path: "a.vue", src: "<script setup>a</script>\n<script setup>b</script>", unc: "duplicate_script_setup"},
		{name: "vue duplicate script", path: "a.vue", src: "<script>a</script>\n<script>b</script>", unc: "duplicate_script"},
		{name: "vue stray close", path: "a.vue", src: "<script>\nconst a = 1\n</script>\n</script>\n", unc: "stray_close_tag"},
		{name: "vue close in string", path: "a.vue", src: "<script>\nconst s = '</script>'\nimport x from 'x'\n';\n</script>", unc: "script"},
		{name: "vue close in string truncating", path: "a.vue", src: "<script>\nconst s = '</script>';\n", unc: "script"},
		{name: "vue bad lang", path: "a.vue", src: "<script lang=\"coffee\">x</script>", unc: "script_lang"},
		{name: "vue js unterminated string propagates", path: "a.vue", src: "<script>\nconst a = 'abc\n</script>", unc: "script:"},
		{name: "vue CRLF lines", path: "a.vue", src: "<template>\r\n<div/>\r\n</template>\r\n<script>\r\nimport a from 'a'\r\n\r\nimport b from 'b'\r\n</script>\r\n",
			imps: []want{{"a", 5}, {"b", 7}}},
		{name: "vue uppercase ext and tags", path: "A.VUE", src: "<SCRIPT>import a from 'a'</SCRIPT>", imps: []want{{"a", 1}}},
		{name: "vue BOM", path: "a.vue", src: "\xEF\xBB\xBF<script>import a from 'a'</script>", imps: []want{{"a", 1}}},
		{name: "vue empty", path: "a.vue", src: "", imps: []want{}},
		{name: "vue remote src skipped", path: "a.vue", src: "<script src=\"https://cdn/x.js\"></script>", imps: []want{}},

		// ---- Svelte ----
		{name: "svelte basic", path: "A.svelte", src: "<script>\n  import B from './B.svelte';\n  export let x;\n</script>\n{#if x}<B/>{/if}\n<style>\n@import './s.css';\n</style>\n",
			imps: []want{{"./B.svelte", 2}, {"./s.css", 7}}},
		{name: "svelte module and ts", path: "A.svelte", src: "<script context=\"module\" lang=\"ts\">\nimport type { A } from './a'\n</script>\n<script lang=\"ts\">\nimport { b } from './b'\nlet x: number = 1\n</script>",
			imps: []want{{"./a", 2}, {"./b", 5}}},
		{name: "svelte module attr", path: "A.svelte", src: "<script module>\nimport m from './m'\n</script>", imps: []want{{"./m", 2}}},
		{name: "svelte comment", path: "A.svelte", src: "<!-- <script>import x from 'x'</script> -->\n<script>import y from 'y'</script>", imps: []want{{"y", 2}}},
		{name: "svelte unclosed", path: "A.svelte", src: "<script>\nimport a from 'a'", unc: "unclosed_block"},
		{name: "svelte attr braces", path: "A.svelte", src: "<div class={a > b ? '>' : 'x'}>t</div>\n<script>import y from 'y'</script>", imps: []want{{"y", 2}}},
		{name: "svelte jsonld skipped", path: "A.svelte", src: "<svelte:head><script type=\"application/ld+json\">{\"a\":1}</script></svelte:head>", imps: []want{}},
		{name: "svelte markup only", path: "A.svelte", src: "<div>{#each xs as x}<p>{x}</p>{/each}</div>", imps: []want{}},

		// ---- Astro ----
		{name: "astro frontmatter", path: "a.astro", src: "---\nimport L from '../L.astro'\nimport type { P } from './p'\nconst { a }: P = Astro.props\n---\n<L/>\n",
			imps: []want{{"../L.astro", 2}, {"./p", 3}}},
		{name: "astro no frontmatter", path: "a.astro", src: "<div>hi</div>\n<script>\nimport x from './x'\n</script>", imps: []want{{"./x", 3}}},
		{name: "astro frontmatter CRLF and body script", path: "a.astro", src: "---\r\nimport a from './a'\r\n---\r\n<script>\r\nimport b from './b'\r\n</script>\r\n<style>@import './c.css';</style>",
			imps: []want{{"./a", 2}, {"./b", 5}, {"./c.css", 7}}},
		{name: "astro unclosed frontmatter", path: "a.astro", src: "---\nimport a from './a'\n<div/>", unc: "unclosed_frontmatter"},
		{name: "astro script src relative and inline", path: "a.astro", src: "<script src=\"./x.js\"></script>\n<script src=\"/pub/y.js\"></script>\n<script is:inline src=\"../z.js\"></script>\n<script is:inline>import q from 'q'</script>",
			imps: []want{{"./x.js", 1}, {"../z.js", 3}}},
		{name: "astro frontmatter then comment script", path: "a.astro", src: "---\nconst a = 1\n---\n<!-- <script>import x from 'x'</script> -->", imps: []want{}},
		{name: "astro empty frontmatter", path: "a.astro", src: "---\n---\n<div/>", imps: []want{}},
		{name: "astro leading blank then frontmatter", path: "a.astro", src: "\n---\nimport a from 'a'\n---\n", imps: []want{{"a", 3}}},
		{name: "astro ts syntax", path: "a.astro", src: "---\nconst x = <HTMLElement>y\nimport z from 'z'\n---\n", imps: []want{{"z", 3}}},

		// ---- MDX ----
		{name: "mdx basic", path: "a.mdx", src: "import A from './A'\nimport { b } from './b'\n\n# Title\n\n<A />\n\nexport const meta = {\n  t: 1\n}\n\ntext\n",
			imps: []want{{"./A", 1}, {"./b", 2}}},
		{name: "mdx fenced code skipped", path: "a.mdx", src: "# T\n\n```js\nimport x from 'x'\n```\n\n~~~\nimport y from 'y'\n~~~\n\nimport z from './z'\n",
			imps: []want{{"./z", 11}}},
		{name: "mdx longer fence", path: "a.mdx", src: "````md\n```\nimport x from 'x'\n```\nimport w from 'w'\n````\nimport k from 'k'\n", imps: []want{{"k", 7}}},
		{name: "mdx indented not esm", path: "a.mdx", src: "  import x from 'x'\n\ntext import y from 'y'\n", imps: []want{}},
		{name: "mdx frontmatter yaml", path: "a.mdx", src: "---\nimport: x\ntitle: a\n---\nimport a from './a'\n", imps: []want{{"./a", 5}}},
		{name: "mdx frontmatter unclosed", path: "a.mdx", src: "---\ntitle: a\n", unc: "unclosed_frontmatter"},
		{name: "mdx jsx esm", path: "a.mdx", src: "export const A = () => <div>{x}</div>\nimport b from './b'\n", imps: []want{{"./b", 2}}},
		{name: "mdx unterminated esm", path: "a.mdx", src: "export const a = {\n\nfoo\n", unc: "mdx esm"},
		{name: "mdx CRLF", path: "a.mdx", src: "# T\r\n\r\nimport a from './a'\r\n\r\ntext\r\n\r\nimport b from './b'\r\n", imps: []want{{"./a", 3}, {"./b", 7}}},
		{name: "mdx no esm", path: "a.mdx", src: "# hello\n\nsome text\n", imps: []want{}},
		{name: "mdx unclosed fence", path: "a.mdx", src: "```\nimport x from 'x'\n", imps: []want{}},

		// ---- khác ----
		{name: "unsupported", path: "a.html", src: "<script>import a from 'a'</script>", unc: "unsupported_sfc"},
		{name: "invalid utf8", path: "a.vue", src: "<script>\xff</script>", unc: "invalid_utf8"},
		{name: "nul", path: "a.vue", src: "<script>\x00</script>", unc: "binary_content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Scan(c.path, []byte(c.src))
			if r.Imports == nil {
				t.Fatalf("Imports nil")
			}
			if c.unc != "" {
				if !r.Uncertain || !strings.Contains(r.UncertainReason, c.unc) {
					t.Fatalf("want uncertain %q, got uncertain=%v reason=%q imps=%v", c.unc, r.Uncertain, r.UncertainReason, specs(r))
				}
				return
			}
			if r.Uncertain {
				t.Fatalf("unexpected uncertain: %s", r.UncertainReason)
			}
			if got := specs(r); !reflect.DeepEqual(got, c.imps) {
				t.Fatalf("imports\n got %v\nwant %v", got, c.imps)
			}
		})
	}
}

func TestDeterministic(t *testing.T) {
	src := []byte("<script>\nimport a from 'a'\n</script>\n<style>@import 'x';</style>")
	a, b := Scan("a.vue", src), Scan("a.vue", src)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("non-deterministic")
	}
}

func FuzzScan(f *testing.F) {
	seeds := []string{
		"<script>import a from 'a'</script>", "<script setup lang=\"ts\">\n</script><style>@import 'x'</style>",
		"---\nimport a from 'a'\n---\n<script>x</script>", "import a from 'a'\n\n```\nimport\n```\n",
		"<!-- <script>", "<template><template></template></template>", "<style lang=scss>@use \"a\"", "<script src=\"./x\"/>",
		"<div class={a}", "<style>@import url(", "---\n", "<script>\r\n'</script>",
	}
	exts := []string{".vue", ".svelte", ".astro", ".mdx"}
	for _, s := range seeds {
		for i := range exts {
			f.Add(uint8(i), []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, e uint8, src []byte) {
		path := "f" + exts[int(e)%len(exts)]
		r1 := Scan(path, src)
		r2 := Scan(path, src)
		if !reflect.DeepEqual(r1, r2) {
			t.Fatalf("non-deterministic for %q", src)
		}
		if r1.Imports == nil {
			t.Fatal("nil imports")
		}
		if r1.Uncertain && r1.UncertainReason == "" {
			t.Fatal("uncertain without reason")
		}
		for _, im := range r1.Imports {
			if im.Line < 1 {
				t.Fatalf("bad line %d for %q", im.Line, src)
			}
		}
	})
}
