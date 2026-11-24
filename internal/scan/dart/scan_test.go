package dart

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

type ref = model.ImportRef

func imp(s string, line int) ref { return ref{Kind: model.ImportDartImport, Spec: s, Line: line} }
func exp(s string, line int) ref { return ref{Kind: model.ImportDartExport, Spec: s, Line: line} }
func part(s string, line int) ref {
	return ref{Kind: model.ImportDartPart, Spec: s, Line: line}
}
func str(s string, line int) ref { return ref{Kind: model.ImportDartString, Spec: s, Line: line} }
func dstr(head, tail string, line int) ref {
	return ref{Kind: model.ImportDartString, Spec: head, Suffix: tail, Dynamic: true, Line: line}
}

func run(t *testing.T, cases []struct {
	name string
	src  string
	want []ref
	unc  string
}) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan([]byte(c.src))
			want := c.want
			if want == nil {
				want = []ref{}
			}
			if !reflect.DeepEqual(got.Imports, want) {
				t.Errorf("imports\n got %+v\nwant %+v", got.Imports, want)
			}
			if got.UncertainReason != c.unc || got.Uncertain != (c.unc != "") {
				t.Errorf("uncertain = %v %q, want %q", got.Uncertain, got.UncertainReason, c.unc)
			}
		})
	}
}

func TestDirectives(t *testing.T) {
	run(t, []struct {
		name string
		src  string
		want []ref
		unc  string
	}{
		{name: "basic", src: "library x;\nimport 'dart:io';\nimport \"package:a/b.dart\" as b show c hide d;\nexport 'src/e.dart';\npart 'f.g.dart';\n",
			want: []ref{imp("dart:io", 2), imp("package:a/b.dart", 3), exp("src/e.dart", 4), part("f.g.dart", 5)}},
		{name: "deferred", src: "import 'a.dart' deferred as a;", want: []ref{imp("a.dart", 1)}},
		{name: "conditional", src: "import 'stub.dart'\n  if (dart.library.io) 'io.dart'\n  if (dart.library.js_interop) 'web.dart' as impl;",
			want: []ref{imp("stub.dart", 1), imp("io.dart", 1), imp("web.dart", 1)}},
		{name: "conditional export with value", src: "export 'a.dart' if (dart.library.io == 'true') 'b.dart';",
			want: []ref{exp("a.dart", 1), exp("b.dart", 1)}},
		{name: "part of", src: "part of 'lib.dart';\npart of a.b;", want: nil},
		{name: "augment", src: "import augment 'x.dart';\naugment library 'y.dart';",
			want: []ref{{Kind: model.ImportDartPart, Spec: "x.dart", Suffix: "augment", Line: 1}, str("y.dart", 2)}},
		{name: "triple and raw uri", src: "import '''a.dart''';\nimport r'b.dart';\nimport \"\"\"c.dart\"\"\";",
			want: []ref{imp("a.dart", 1), imp("b.dart", 2), imp("c.dart", 3)}},
		{name: "annotation before", src: "@TestOn('vm')\nlibrary;\n\nimport 'a.dart';", want: []ref{imp("a.dart", 4)}},
		{name: "adjacent uri", src: "import 'a' '.dart';", want: []ref{imp("a.dart", 1)}},
		{name: "interpolated uri", src: "import 'a$b.dart';", want: nil, unc: "interpolated_uri"},
	})
}

func TestCommentsAndStrings(t *testing.T) {
	run(t, []struct {
		name string
		src  string
		want []ref
		unc  string
	}{
		{name: "line comments", src: "// import 'no.dart';\n/// import 'no2.dart';\nimport 'a.dart';", want: []ref{imp("a.dart", 3)}},
		{name: "nested block", src: "/* a /* import 'no.dart'; */ still 'comment' */ import 'a.dart';", want: []ref{imp("a.dart", 1)}},
		{name: "import in string", src: "var s = \"import 'x.dart';\";", want: []ref{str("import 'x.dart';", 1)}},
		{name: "comment chars in string", src: "var s = '/* not a comment'; import 'a.dart';",
			want: []ref{imp("a.dart", 1), str("/* not a comment", 1)}},
		{name: "interpolation nesting", src: "var s = 'a ${b('}')} ${'${c}'} ok'; import 'z.dart';",
			want: []ref{imp("z.dart", 1)}},
		{name: "interpolated literal", src: "matchesGoldenFile('goldens/$name.png');",
			want: []ref{dstr("goldens/", ".png", 1)}},
		{name: "braced interpolation", src: "File('test/fixtures/${x.y}/data.json');",
			want: []ref{dstr("test/fixtures/", "/data.json", 1)}},
		{name: "head without slash", src: "File('$dir.json');", want: nil},
		{name: "tail with directory", src: "File('$root/fixtures/a.json');", want: []ref{dstr("", "/fixtures/a.json", 1)}},
		{name: "raw string keeps dollar", src: "File(r'a/$b.json');", want: []ref{str("a/$b.json", 1)}},
		{name: "escaped dollar", src: "File('a/\\$b.json');", want: []ref{str("a/$b.json", 1)}},
		{name: "adjacent", src: "File('test/' 'fixtures/' \"x.json\");", want: []ref{str("test/fixtures/x.json", 1)}},
		{name: "adjacent with interpolation", src: "File('test/' '$x' '/y.json');", want: []ref{dstr("test/", "/y.json", 1)}},
		{name: "triple multi-line", src: "var s = '''\nline\n''';\nimport 'a.dart';", want: []ref{imp("a.dart", 4)}},
		{name: "bare words dropped", src: "group('adds two numbers', () {}); tag('vm'); print('dart:io');", want: nil},
		{name: "url dropped", src: "get('https://example.com/a.json');", want: nil},
		{name: "package uri literal", src: "Uri.parse('package:foo/src/a.dart');", want: []ref{str("package:foo/src/a.dart", 1)}},
		{name: "dollar identifier", src: "var $x = 1; final a$b = 'x/y';", want: []ref{str("x/y", 1)}},
		{name: "symbol and numbers", src: "#foo; 1.5e-3; 0x1F; 1_000; .5; 'a.b';", want: []ref{str("a.b", 1)}},
		{name: "bom and script tag", src: "\xef\xbb\xbf#!/usr/bin/env dart\nimport 'a.dart';", want: []ref{imp("a.dart", 2)}},
	})
}

func TestLoadsJoinsProcesses(t *testing.T) {
	load := func(spec, suffix string, dyn bool, line int) ref {
		return ref{Kind: model.ImportDartLoad, Spec: spec, Suffix: suffix, Dynamic: dyn, Line: line}
	}
	proc := ref{Kind: model.ImportDartProcess, Line: 1}
	run(t, []struct {
		name string
		src  string
		want []ref
		unc  string
	}{
		{name: "spawnUri literal", src: "Isolate.spawnUri(Uri.parse('worker.dart'), [], null);",
			want: []ref{load("worker.dart", "", false, 1), str("worker.dart", 1)}},
		{name: "spawnUri file", src: "Isolate.spawnUri(Uri.file('bin/w.dart'), [], null);",
			want: []ref{load("bin/w.dart", "file", false, 1), str("bin/w.dart", 1)}},
		{name: "spawnUri string", src: "Isolate.spawnUri('w.dart', [], null);",
			want: []ref{load("w.dart", "", false, 1), str("w.dart", 1)}},
		{name: "spawnUri computed", src: "Isolate.spawnUri(uri, [], null);", want: []ref{load("", "", true, 1)}},
		{name: "spawnUri interpolated", src: "Isolate.spawnUri(Uri.parse('bin/$x.dart'), [], null);",
			want: []ref{load("bin/", "", true, 1), dstr("bin/", ".dart", 1)}},
		{name: "tear-off", src: "final f = Isolate.spawnUri;", want: []ref{load("", "", true, 1)}},
		{name: "loadUri", src: "currentMirrorSystem().isolate.loadUri(u);", want: []ref{load("", "", true, 1)}},
		{name: "process", src: "Process.run('dart', ['bin/x.dart']);", want: []ref{proc, str("bin/x.dart", 1)}},
		{name: "test process", src: "TestProcess.start(Platform.executable, []);", want: []ref{proc}},
		{name: "not process", src: "server.start(); x.run();", want: nil},
		{name: "join literal", src: "p.join('test', 'fixtures', 'a.json');",
			want: []ref{str("test/fixtures/a.json", 1), str("a.json", 1)}},
		{name: "join head", src: "path.join('test', 'goldens', name);", want: []ref{dstr("test/goldens/", "", 1)}},
		{name: "join tail", src: "p.join(dir, 'fixtures', 'a.json');",
			want: []ref{dstr("", "/fixtures/a.json", 1), str("a.json", 1)}},
		{name: "join single tail word", src: "p.join(dir, 'fixtures');", want: nil},
	})
}

func TestUncertain(t *testing.T) {
	for _, c := range []struct{ src, why string }{
		{"var s = 'abc", "unterminated_string"},
		{"var s = 'abc\n';", "unterminated_string"},
		{"var s = '''abc", "unterminated_string"},
		{"/* /* */", "unterminated_comment"},
		{"var s = '${a", "unterminated_interpolation"},
		{strings.Repeat("'${", 300), "nesting_too_deep"},
		{strings.Repeat("/*", 300), "nesting_too_deep"},
	} {
		got := Scan([]byte(c.src))
		if !got.Uncertain || got.UncertainReason != c.why {
			t.Errorf("%q: got %v %q, want %q", c.src, got.Uncertain, got.UncertainReason, c.why)
		}
	}
	got := Scan(make([]byte, MaxFileSize+1))
	if !got.Uncertain || got.UncertainReason != "file_too_large" {
		t.Fatalf("got %+v", got)
	}
	// Found references are kept.
	got = Scan([]byte("import 'a.dart';\nvar s = 'x"))
	if !reflect.DeepEqual(got.Imports, []ref{imp("a.dart", 1)}) {
		t.Fatalf("got %+v", got.Imports)
	}
}
