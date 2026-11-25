package evalkit

import (
	"slices"
	"strings"
	"testing"
)

func dartCandTexts(src string) []string {
	var out []string
	for _, c := range dartScan([]byte(src)) {
		out = append(out, src[c.Start:c.End]+"->"+c.Repl)
	}
	return out
}

func TestDartScanOperators(t *testing.T) {
	src := `import 'package:a/b.dart';
@Deprecated('x == y')
int f(int a, int b, List<int> xs, Map<String, List<int>> m) {
  // a == b in a comment
  final s = 'a < b and ${a + b}';
  if (a == b && a != 0) return a - b;
  if (a < b || b > a) return xs.length + 1;
  if (a <= b) return -1;
  final t = 'x' + s;
  final u = a >= b ? true : false;
  return a+b;
}
`
	got := dartCandTexts(src)
	want := []string{"+->-", "==->!=", "&&->||", "!=->==", "0->1", "-->+", "<->>=", "||->&&", ">-><=", "+->-", "1->0", "<=->>", "1->0", ">=-><", "true->false", "false->true"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("candidates\n got %v\nwant %v", got, want)
	}
	for _, c := range dartScan([]byte(src)) {
		line := strings.Count(src[:c.Start], "\n")
		if line <= 1 {
			t.Fatalf("directive or annotation mutated: %+v", c)
		}
	}
}

func TestDartScanUnterminated(t *testing.T) {
	if got := dartScan([]byte("var a = 'x;\nvar b = a == 1;\n")); len(got) != 0 {
		t.Fatalf("a file that does not lex must give no candidates, got %v", got)
	}
}

func TestDartCandidateFileAndPackage(t *testing.T) {
	pkgs := []string{"pkgs/args", "pkgs/path"}
	for f, want := range map[string]bool{
		"pkgs/args/lib/src/parser.dart": true, "pkgs/args/bin/x.dart": true, "pkgs/args/test/a_test.dart": false,
		"pkgs/async/lib/a.dart": false, "pkgs/path/lib/src/a.g.dart": false, "pkgs/path/lib/README.md": false,
	} {
		if got := dartCandidateFile(f, pkgs); got != want {
			t.Errorf("dartCandidateFile(%q) = %v", f, got)
		}
	}
	if !dartCandidateFile("lib/a.dart", []string{"."}) || dartCandidateFile("example/lib/a.dart", []string{"."}) {
		t.Error("root package candidates")
	}
	if got := dartPackageOf("packages/melos/test/a_test.dart", []string{".", "packages/melos"}); got != "packages/melos" {
		t.Errorf("dartPackageOf = %q", got)
	}
	if got := dartPackageOf("test/a_test.dart", []string{".", "packages/melos"}); got != "." {
		t.Errorf("dartPackageOf root = %q", got)
	}
}

func TestGenDartMutantsSplitsPackages(t *testing.T) {
	big := strings.Repeat("int f%d(int a, int b) => a == b ? 1 : 0;\n", 1)
	var files []RustMutantSource
	for i := range 6 {
		files = append(files, RustMutantSource{Path: "pkgs/big/lib/f" + string(rune('a'+i)) + ".dart", Data: []byte(strings.ReplaceAll(big, "%d", "x"))})
	}
	files = append(files, RustMutantSource{Path: "pkgs/small/lib/s.dart", Data: []byte("bool g(int a) => a != 0;\n")})
	ms := GenDartMutants(files, []string{"pkgs/small", "pkgs/big"}, "v1", "dart-x", 6)
	if len(ms) != 6 {
		t.Fatalf("got %d mutants", len(ms))
	}
	small := 0
	for _, m := range ms {
		if strings.HasPrefix(m.File, "pkgs/small/") {
			small++
		}
		if !strings.HasPrefix(m.ID, "dart-x-") {
			t.Fatalf("id %q", m.ID)
		}
	}
	// pkgs/small has one line with two candidates (one per line is kept): one mutant, the rest goes to pkgs/big.
	if small != 1 {
		t.Fatalf("small package got %d mutants: %+v", small, ms)
	}
	again := GenDartMutants(files, []string{"pkgs/small", "pkgs/big"}, "v1", "dart-x", 6)
	if !slices.Equal(ms, again) {
		t.Fatal("generation is not deterministic")
	}
}

const dartJSONSample = `{"protocolVersion":"0.1.1","runnerVersion":"1.33.0","pid":61,"type":"start","time":0}
{"suite":{"id":0,"platform":"vm","path":"test/a_test.dart"},"type":"suite","time":0}
{"test":{"id":1,"name":"loading test/a_test.dart","suiteID":0,"groupIDs":[]},"type":"testStart","time":1}
{"suite":{"id":2,"platform":"vm","path":"test/b_test.dart"},"type":"suite","time":7}
{"test":{"id":3,"name":"loading test/b_test.dart","suiteID":2,"groupIDs":[]},"type":"testStart","time":7}
{"suite":{"id":4,"platform":"vm","path":"test/c_test.dart"},"type":"suite","time":7}
{"test":{"id":5,"name":"loading test/c_test.dart","suiteID":4,"groupIDs":[]},"type":"testStart","time":7}
{"testID":1,"result":"success","skipped":false,"hidden":true,"type":"testDone","time":10}
{"testID":3,"result":"success","skipped":false,"hidden":true,"type":"testDone","time":10}
{"testID":5,"error":"Failed to load \"test/c_test.dart\":\nlib/src/x.dart:52:19: Error: A value of type 'String' can't be assigned to a variable of type 'int'.","stackTrace":"","isFailure":false,"type":"error","time":11}
{"testID":5,"result":"error","skipped":false,"hidden":false,"type":"testDone","time":11}
{"test":{"id":6,"name":"group works","suiteID":0,"groupIDs":[]},"type":"testStart","time":12}
{"testID":6,"result":"success","skipped":false,"hidden":false,"type":"testDone","time":13}
{"test":{"id":7,"name":"group works","suiteID":0,"groupIDs":[]},"type":"testStart","time":12}
{"testID":7,"result":"success","skipped":false,"hidden":false,"type":"testDone","time":13}
{"testID":7,"error":"This test failed after it had already completed.","stackTrace":"","isFailure":false,"type":"error","time":14}
not json output of a child process
{"test":{"id":8,"name":"fails","suiteID":2,"groupIDs":[]},"type":"testStart","time":12}
{"testID":8,"result":"failure","skipped":false,"hidden":false,"type":"testDone","time":13}
{"test":{"id":9,"name":"skipped","suiteID":2,"groupIDs":[]},"type":"testStart","time":12}
{"testID":9,"result":"success","skipped":true,"hidden":false,"type":"testDone","time":13}
{"success":false,"type":"done","time":15}
`

func TestParseDartTestJSON(t *testing.T) {
	run, err := parseDartTestJSON("pkgs/x", []byte(dartJSONSample))
	if err != nil {
		t.Fatal(err)
	}
	if !run.Done || !slices.Equal(run.Suites, []string{"pkgs/x/test/a_test.dart", "pkgs/x/test/b_test.dart", "pkgs/x/test/c_test.dart"}) {
		t.Fatalf("run = %+v", run)
	}
	if len(run.Tests) != 7 || len(run.Compile) != 1 {
		t.Fatalf("tests %d compile %v", len(run.Tests), run.Compile)
	}
	if _, dup := run.Tests["pkgs/x/test/a_test.dart\x00group works\x00#2"]; !dup {
		t.Fatalf("repeated test name not kept apart: %v", run.Tests)
	}
	red, onlyExcluded := run.redUnits(nil)
	if !slices.Equal(red, []string{"pkgs/x/test/a_test.dart", "pkgs/x/test/b_test.dart", "pkgs/x/test/c_test.dart"}) || len(onlyExcluded) != 0 {
		t.Fatalf("red = %v %v", red, onlyExcluded)
	}
	red, onlyExcluded = run.redUnits(map[string]bool{"pkgs/x/test/b_test.dart\x00fails": true, "pkgs/x/test/a_test.dart\x00group works\x00#2": true})
	if !slices.Equal(red, []string{"pkgs/x/test/c_test.dart"}) || !slices.Equal(onlyExcluded, []string{"pkgs/x/test/a_test.dart", "pkgs/x/test/b_test.dart"}) {
		t.Fatalf("red with exclusions = %v %v", red, onlyExcluded)
	}
	root, err := parseDartTestJSON(".", []byte(dartJSONSample))
	if err != nil || root.Suites[0] != "test/a_test.dart" {
		t.Fatalf("root package suites %v %v", root.Suites, err)
	}
}

func TestParseDartTestJSONIncomplete(t *testing.T) {
	cut := dartJSONSample[:strings.Index(dartJSONSample, `{"success"`)]
	run, err := parseDartTestJSON(".", []byte(cut))
	if err != nil || run.Done {
		t.Fatalf("a run without its done event must not be complete: %v %v", run.Done, err)
	}
	if _, err := parseDartTestJSON(".", []byte(`{"testID":3,"result":"success","type":"testDone"}`)); err == nil {
		t.Fatal("an event of an unknown test must be an error")
	}
}

func TestDartMissingSuites(t *testing.T) {
	if got := dartMissingSuites([]string{"a", "b", "c"}, []string{"a", "c", "d"}); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("missing = %v", got)
	}
}

func TestDartTestOn(t *testing.T) {
	src := "// comment\n@TestOn('browser')\nlibrary;\n"
	if m := dartTestOn.FindStringSubmatch(src); m == nil || m[1] != "'browser'" {
		t.Fatalf("match = %v", m)
	}
}
