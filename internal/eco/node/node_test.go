package node

import (
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestComputedRunnerConfigOrder(t *testing.T) {
	projects := []string{"packages/a", "packages/b", "packages/c", "packages/d"}
	files := map[string]string{}
	for _, p := range projects {
		files[p+"/package.json"] = `{"devDependencies":{"vitest":"1.0.0"}}`
		files[p+"/vitest.config.ts"] = `export default { resolve: { alias: getAliases() } }`
		files[p+"/index.test.ts"] = `export {}`
	}
	tr := tree.NewMapTree(files)
	sc := scancache.New()
	for range 16 {
		a, err := New(nil).Analyze(t.Context(), tr, sc)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, u := range a.Unresolved {
			if strings.HasPrefix(u.Detail, model.ReasonConfigIsCode+": ") {
				got = append(got, u.ScopeDir)
			}
		}
		if !slices.Equal(got, projects) {
			t.Fatalf("computed config project order = %v, want %v", got, projects)
		}
	}
}
