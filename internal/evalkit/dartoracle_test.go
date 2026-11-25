package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestParseDartName(t *testing.T) {
	for _, c := range []struct {
		in               string
		form, path, test string
	}{
		{"/repo/pkg/test/a_test.dart: g t", "path-name", "/repo/pkg/test/a_test.dart", "g t"},
		{"test/config_test.dart: unsupported configuration", "path-name", "test/config_test.dart", "unsupported configuration"},
		{"/widgetbook/packages/widgetbook/test/x_test.dart", "path", "/widgetbook/packages/widgetbook/test/x_test.dart", ""},
		{"loading test/src/core/action_test.dart", "loading", "test/src/core/action_test.dart", ""},
		{"(setUpAll)", "hook", "", "(setUpAll)"},
		{"Group (tearDownAll)", "hook", "", "Group (tearDownAll)"},
		{"Request has correct url", "name", "", "Request has correct url"},
		{"code_generator:  Function Binding", "name", "", "code_generator:  Function Binding"},
		{"handles a.dart: file", "name", "", "handles a.dart: file"},
	} {
		got := parseDartName(c.in)
		if got.form != c.form || got.path != c.path || got.test != c.test {
			t.Errorf("parseDartName(%q) = %+v", c.in, got)
		}
	}
}

func TestReadDartLit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want dartLit
		n    int
	}{
		{`'a b' rest`, dartLit{"a b"}, 5},
		{`"it's"`, dartLit{"it's"}, 6},
		{`'don\'t \$x'`, dartLit{"don't $x"}, 12},
		{`r'raw $x \n'`, dartLit{`raw $x \n`}, 12},
		{`'v $name and ${a.b('}')} end'`, dartLit{"v ", " and ", " end"}, 29},
		{`'''multi 'q' line'''`, dartLit{"multi 'q' line"}, 20},
		{`'unterminated`, nil, 0},
		{`'\u{1F600}\x41'`, dartLit{"\U0001F600A"}, 15},
	} {
		got, n := readDartLit(c.in)
		if !reflect.DeepEqual(got, c.want) || n != c.n {
			t.Errorf("readDartLit(%q) = %q, %d; want %q, %d", c.in, got, n, c.want, c.n)
		}
	}
}

func TestScanDartTest(t *testing.T) {
	src := `
import 'package:test/test.dart';
import 'helpers.dart' if (dart.library.io) 'helpers_io.dart';
// test('commented out', () {});
/* group('block /* nested */ comment', () {}); */
@reflectiveTest
class FooTest { void test_bar() {} }
void main() {
  group(Serializers, () {
    test('can be ' 'added', () {});
    blocTest<CounterCubit, int>('emits [1]', build: () => CounterCubit());
    testWidgets('renders $kind tile', (tester) async {});
    group('$FleatherToolbar', () {});
    expect(Uri.parse('http://x'), isNotNull);
    check(name: 'named case');
    for (final v in values) { test(v.name, () {}); }
    if ('a' == x) {}
  });
}
`
	fs := scanDartTest([]byte(src))
	if want := []string{"package:test/test.dart", "helpers.dart", "helpers_io.dart"}; !reflect.DeepEqual(fs.imports, want) {
		t.Errorf("imports = %q", fs.imports)
	}
	if want := []string{"Serializers", "can be added", "emits [1]", "FleatherToolbar"}; !reflect.DeepEqual(fs.strong, want) {
		t.Errorf("strong = %q", fs.strong)
	}
	if want := []string{"http://x", "named case"}; !reflect.DeepEqual(fs.weak, want) {
		t.Errorf("weak = %q", fs.weak)
	}
	if want := []dartLit{{"renders ", " tile"}}; !reflect.DeepEqual(fs.strongTpls, want) {
		t.Errorf("strongTpls = %q", fs.strongTpls)
	}
	if !fs.dynamic || !reflect.DeepEqual(fs.classes, []string{"FooTest"}) || !reflect.DeepEqual(fs.methods, []string{"test_bar"}) {
		t.Errorf("dynamic %v classes %q methods %q", fs.dynamic, fs.classes, fs.methods)
	}
}

func TestDartIsTestFile(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{
		"pubspec.yaml": "", "test/a_test.dart": "", "lib/b_test.dart": "", "test/helper.dart": "",
		"packages/p/pubspec.yaml": "", "packages/p/test/x/c_test.dart": "", "packages/p/integration_test/d_test.dart": "",
		"packages/p/.dart_tool/test/e_test.dart": "", "nopkg/test/f_test.dart": "",
	})
	for f, want := range map[string]bool{
		"test/a_test.dart": true, "lib/b_test.dart": false, "test/helper.dart": false, "packages/p/test/x/c_test.dart": true,
		"packages/p/integration_test/d_test.dart": true, "packages/p/.dart_tool/test/e_test.dart": false,
		"nopkg/test/f_test.dart": false, // nearest pubspec is the root one: nopkg/test/... is not under <root>/test/
	} {
		if got := DartIsTestFile(mt, f); got != want {
			t.Errorf("DartIsTestFile(%s) = %v", f, got)
		}
	}
}

func TestParseDartCmd(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{
		"packages/bloc/pubspec.yaml": "", "packages/bloc/test/a_test.dart": "", "packages/bloc_test/test/b_test.dart": "",
		"pkgs/swift2objc/test/unit/c_test.dart": "", "json_serializable/test/config_test.dart": "",
	})
	for _, c := range []struct {
		cmd  string
		want DartCmdScope
	}{
		{"cd packages/bloc && dart pub get && dart test --reporter json && cd ../bloc_test && dart test --reporter json",
			DartCmdScope{Dirs: []string{"packages/bloc", "packages/bloc_test"}}},
		{"cd /native/pkgs/swift2objc && dart test test/unit/c_test.dart --no-color", DartCmdScope{Files: []string{"pkgs/swift2objc/test/unit/c_test.dart"}}},
		{"cd /flutter-code-editor && export PATH=\"/tmp/flutter/bin:$PATH\" && flutter test --no-color", DartCmdScope{Dirs: []string{""}}},
		{"cd json_serializable && dart test --reporter expanded test/config_test.dart", DartCmdScope{Files: []string{"json_serializable/test/config_test.dart"}}},
		{"dart pub run test --reporter json", DartCmdScope{Dirs: []string{""}}},
		{"TZ=UTC /opt/flutter/bin/flutter test -j 1", DartCmdScope{Dirs: []string{""}}},
		{"dart format .", DartCmdScope{}},
	} {
		if got := ParseDartCmd(mt, c.cmd); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseDartCmd(%q) = %+v, want %+v", c.cmd, got, c.want)
		}
	}
}

func TestLocateDart(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{
		"pubspec.yaml": "",
		"test/request_test.dart": `void main() {
  group('Request', () {
    test('has correct url', () {});
    group('body', () { test('is empty', () {}); });
  });
  group('', () { test('Ingest download counts', () {}); });
  for (final n in [1, 2]) { test('variant $n works', () {}); }
}`,
		"test/other_test.dart": `void main() { test('shared name', () {}); test('has correct url', () {}); }`,
		"test/third_test.dart": `void main() { test('shared name', () {}); }`,
		"test/shared.dart":     `void sharedTests() { group('Shared', () { test('runs', () {}); }); }`,
		"test/uses_shared_test.dart": `import 'shared.dart';
void main() => sharedTests();`,
		"pkg/pubspec.yaml":           "",
		"pkg/test/config_test.dart":  `void main() { test('unsupported configuration', () {}); }`,
		"pkg2/pubspec.yaml":          "",
		"pkg2/test/config_test.dart": `void main() { test('other', () {}); }`,
		"test/widget_test.dart":      `void main() { testWidgets('paints', (t) async {}, variant: TargetPlatformVariant.all()); }`,
	})
	names := []string{
		"Request has correct url",              // groups + test, unique
		"Request body is empty",                // nested groups
		" Ingest download counts",              // empty group
		"shared name",                          // ambiguous
		"variant 2 works",                      // template
		"Shared runs",                          // helper -> includer
		"Request > body > is empty",            // " > " harness separator
		"paints (variant: TargetPlatform.iOS)", // testWidgets variant suffix
		"test/config_test.dart: unsupported configuration",
		"/container/pkg2/test/config_test.dart: other",
		"loading test/request_test.dart",
		"(setUpAll)",
		"nowhere to be found",
	}
	patch := map[string]bool{"test/third_test.dart": true}
	locs := LocateDartTests(mt, names, patch, "")
	want := []struct {
		files []string
		via   string
		amb   bool
	}{
		{[]string{"test/request_test.dart"}, "desc", false},
		{[]string{"test/request_test.dart"}, "desc", false},
		{[]string{"test/request_test.dart"}, "desc", false},
		{[]string{"test/other_test.dart", "test/third_test.dart"}, "desc", true},
		{[]string{"test/request_test.dart"}, "template", false},
		{[]string{"test/uses_shared_test.dart"}, "desc", false},
		{[]string{"test/request_test.dart"}, "desc+gt", false},
		{[]string{"test/widget_test.dart"}, "desc+variant", false},
		{[]string{"pkg/test/config_test.dart"}, "path-suffix+name", false},
		{[]string{"pkg2/test/config_test.dart"}, "path-container", false},
		{[]string{"test/request_test.dart"}, "path", false},
		{nil, "", false},
		{nil, "", false},
	}
	for i, l := range locs {
		if !reflect.DeepEqual(l.Files, want[i].files) || l.Via != want[i].via || l.Ambiguous != want[i].amb {
			t.Errorf("%q: files %v via %q ambiguous %v; want %v %q %v", l.Name, l.Files, l.Via, l.Ambiguous, want[i].files, want[i].via, want[i].amb)
		}
	}
	if !locs[3].InPatch || !reflect.DeepEqual(locs[3].PatchFiles, []string{"test/third_test.dart"}) {
		t.Errorf("patch files: %+v", locs[3])
	}
	if !reflect.DeepEqual(locs[5].Helpers, []string{"test/shared.dart"}) {
		t.Errorf("helpers: %+v", locs[5])
	}
	if !locs[11].Hook || !locs[12].NotFound {
		t.Errorf("hook/not found: %+v %+v", locs[11], locs[12])
	}
	// The test command narrows an ambiguous name to the files it ran.
	locs = LocateDartTests(mt, []string{"shared name"}, nil, "dart test test/third_test.dart")
	if !reflect.DeepEqual(locs[0].Files, []string{"test/third_test.dart"}) || locs[0].Via != "desc+cmd" {
		t.Errorf("cmd scope: %+v", locs[0])
	}
}

// TestLocateDartHarnessCases covers the name shapes seen in the datasets: test_reflective_loader names (dartdoc), a computed test
// description in a loop (rohd), a group from a test-file call combined with tests defined by an imported helper and a name:
// argument (nyxx), tests defined in a test-helper package under lib/ reached through its public library (dio), and a test
// directory that is a symbolic link into another package (cbl-dart).
func TestLocateDartHarnessCases(t *testing.T) {
	mt := tree.NewMapTreeLinks(map[string]string{
		"pubspec.yaml": "",
		"test/reflective_test.dart": `@reflectiveTest
class ExtensionMethodsTest { void test_reexportWithShow() {} }`,
		"test/other_reflective_test.dart": `class OtherTest { void test_reexportWithShow() {} }`,
		"test/loop_test.dart": `void main() {
  group('functional', () {
    group('flopped expressions', () {
      for (final c in Kind.values) { test(c.name, () {}); }
    });
  });
}`,
		"test/test_manager.dart": `void testManager(String name, List<ParsingTest> tests) {
  group(name, () {
    test('parse', () {});
    for (final p in tests) { test(p.name, () {}); }
  });
}`,
		"test/guild_manager_test.dart": `import 'test_manager.dart';
import 'channel_manager_test.dart';
void main() { testManager<Guild, GuildManager>('GuildManager', [ParsingTest(name: 'parseWelcomeScreen')]); }`,
		"test/channel_manager_test.dart": `import 'test_manager.dart';
void main() { testManager('ChannelManager', []); }`,
		"dio_test/pubspec.yaml":                        "",
		"dio_test/lib/tests.dart":                      `export 'src/test/status_code_tests.dart';`,
		"dio_test/lib/src/test/status_code_tests.dart": `void statusCodeTests() { group('status code', () { test('404 throws', () {}); }); }`,
		"dio/pubspec.yaml":                             "",
		"dio/test/status_code_test.dart": `import 'package:dio_test/tests.dart';
void main() => statusCodeTests();`,
		"e2e/pubspec.yaml":               "",
		"e2e/lib/src/registry_test.dart": `void main() { group('ResultSet', () { test('typed', () {}); }); }`,
		"standalone/pubspec.yaml":        "",
	}, map[string]string{"standalone/test/e2e": "../../e2e/lib/src"})
	names := []string{
		"ExtensionMethodsTest | test_reexportWithShow",
		"functional flopped expressions caseNormal",
		"GuildManager parse",
		"GuildManager parseWelcomeScreen",
		"ChannelManager parse",
		"status code 404 throws",
		"ResultSet typed",
		"loading test/e2e/registry_test.dart",
	}
	want := []struct {
		files []string
		via   string
	}{
		{[]string{"test/reflective_test.dart"}, "reflective"},
		{[]string{"test/loop_test.dart"}, "dynamic"},
		{[]string{"test/guild_manager_test.dart"}, "desc"},
		{[]string{"test/guild_manager_test.dart"}, "weak"},
		{[]string{"test/channel_manager_test.dart"}, "desc"},
		{[]string{"dio/test/status_code_test.dart"}, "desc"},
		{[]string{"standalone/test/e2e/registry_test.dart"}, "desc"},
		{[]string{"standalone/test/e2e/registry_test.dart"}, "path-cmd-file"},
	}
	locs := LocateDartTests(mt, names, nil, "cd /repo/standalone && dart test test/e2e/registry_test.dart")
	for i, l := range locs {
		if !reflect.DeepEqual(l.Files, want[i].files) || l.Via != want[i].via || l.NotUnit {
			t.Errorf("%q: files %v via %q not_unit %v; want %v %q", l.Name, l.Files, l.Via, l.NotUnit, want[i].files, want[i].via)
		}
	}
	// A computed description may not stand for most of the name.
	if l := LocateDartTests(mt, []string{"functional something entirely different and long"}, nil, ""); !l[0].NotFound {
		t.Errorf("dynamic over-match: %+v", l[0])
	}
}
