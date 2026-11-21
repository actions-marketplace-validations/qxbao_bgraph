package evalkit

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseF2P(t *testing.T) {
	for in, want := range map[string][]string{
		`["a b","c"]`:           {"a b", "c"},
		`"[\"a\", \"b\"]"`:      {"a", "b"},
		`"['x > y', \"it's\"]"`: {"x > y", "it's"},
	} {
		got, err := ParseF2P(json.RawMessage(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
}

func TestMapF2P(t *testing.T) {
	files := map[string]string{
		"test/a.test.js": "describe('x', () => { it('does   the thing well', () => {}) })",
		"test/b.test.js": "it('other', () => {})",
		"test/snap.snap": "does the thing well",
	}
	req, m, un := MapF2P([]string{"X > does the thing well", "nothing here at all", "tests/run/test.ts > case-1 (dom)"}, files,
		func(p string) bool { return p == "tests/run/test.ts" })
	if !reflect.DeepEqual(req, []string{"test/a.test.js", "tests/run/test.ts"}) || len(un) != 1 || m[0].Method != "title" || m[2].Method != "path_p" {
		t.Fatalf("%v %v %v", req, m, un)
	}
}
