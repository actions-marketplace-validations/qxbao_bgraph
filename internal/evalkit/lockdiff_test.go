package evalkit

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestParseRequirements(t *testing.T) {
	in := `-e ./packages/a
./packages/b
anyio==4.14.2 ; python_full_version >= '3.11'
    # via httpx
Typing_Extensions==4.16.0 \
pydantic[email]==2.14.3
strict-no-cover @ git+https://example.invalid/x.git@abc ; python_full_version >= '3.11'
`
	got := strings.Join(slices.Sorted(maps.Keys(parseRequirements(in))), " ")
	if want := "anyio@4.14.2 pydantic@2.14.3 strict-no-cover@* typing-extensions@4.16.0"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
