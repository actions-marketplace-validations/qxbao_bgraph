package swift

import (
	"reflect"
	"testing"
)

func TestLexStrings(t *testing.T) {
	got, err := Lex([]byte(`/* outer /* nested */ */ #"a\b"# "foo\u{2F}bar" "a\(f("b"))" """` + "\n" + `a` + "\n" + `""" tail`))
	if err != nil || len(got) != 5 || got[0].Text != `a\b` || got[1].Text != "foo/bar" || got[2].Kind != ComputedString || got[3].Kind != ComputedString || got[4].Text != "tail" {
		t.Fatalf("Lex: %+v, %v", got, err)
	}
}

func TestLexCommentInsertion(t *testing.T) {
	parts := []string{"import", "PackageDescription", "let", "package", "=", "Package", "(", "name", ":", `"Demo"`, ")"}
	for i := 1; i < len(parts); i++ {
		var input, decorated string
		for j, token := range parts {
			input += " " + token
			if j == i {
				decorated += " /* outer /* inner */ */ "
			} else {
				decorated += " "
			}
			decorated += token
		}
		a, errA := Lex([]byte(input))
		b, errB := Lex([]byte(decorated))
		if errA != nil || errB != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("insertion at %d: %v, %v; %v, %v", i, a, errA, b, errB)
		}
	}
}

func FuzzLex(f *testing.F) {
	for _, input := range []string{`"\("#"a"#)"`, `/* /* */`, `##"\#\##(fn())"##`, `#/["]/#`, "ÿ", "`foo`"} {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		tokens, err := Lex(src)
		if err == nil && len(tokens) > len(src) {
			t.Fatalf("more tokens than bytes: %v", tokens)
		}
	})
}
