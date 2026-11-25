package graph

import (
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
)

func TestWalkTainted(t *testing.T) {
	g := Build(&eco.Analysis{Edges: []model.Edge{
		{From: "lib", To: "a.go", Kind: model.EdgeImplicit, Spec: "package file"},
		{From: "u1", To: "lib", Kind: model.EdgeImport},
		{From: "u2", To: "lib", Kind: model.EdgeFixtureData},
		{From: "u3", To: "u2", Kind: model.EdgeImport},
		{From: "u4", To: "lib", Kind: model.EdgeImport},
		{From: "u4", To: "u2", Kind: model.EdgeImport},
		{From: "u5", To: "dir/**", Kind: model.EdgePattern},
		{From: "u6", To: "b.go", Kind: model.EdgeImport},
	}})
	plain := func(e model.Edge) bool { return e.Kind == model.EdgeImport || e.Kind == model.EdgeImplicit }
	got := g.WalkTainted([]string{"a.go", "dir/c.txt"}, plain)
	want := map[string]bool{"u2": true, "u3": true, "u4": true, "u5": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %s in %v", k, got)
		}
	}
}
