package dotnet

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

func TestUnitID(t *testing.T) {
	id := UnitID("src/A/A.csproj")
	if id != "dotnet:src/A/A.csproj" {
		t.Fatal(id)
	}
	if p, ok := ParseUnitID(id); !ok || p != "src/A/A.csproj" {
		t.Fatal(p, ok)
	}
	for _, bad := range []string{"", "dotnet:", "src/A/A.csproj", "dotnet:src/readme.md"} {
		if _, ok := ParseUnitID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestEval(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{"Directory.Build.props": "<Project/>", "src/a/x.csproj": "<Project/>"})
	e := &env{t: mt, project: "src/a/x.csproj", props: map[string][]propVal{
		"root": {{raw: "$(MSBuildThisFileDirectory)", file: "Directory.Build.props"}},
		"two":  {{raw: "a", file: "x"}, {raw: "b", file: "x"}},
	}}
	cases := []struct {
		in   string
		want []string
	}{
		{"$(MSBuildProjectDirectory)", []string{vroot + "/src/a"}},
		{"$(Root)shared", []string{vroot + "/shared"}},
		{"$(Two)/x", []string{"a/x", "b/x"}},
		{"$(Configuration)/x", []string{"*/x"}},
		{"$([MSBuild]::GetPathOfFileAbove('Directory.Build.props', '$(MSBuildThisFileDirectory)../'))", []string{vroot + "/Directory.Build.props"}},
		{"$([MSBuild]::GetPathOfFileAbove('Missing.props', '$(MSBuildThisFileDirectory)'))", []string{""}},
		{"$([MSBuild]::NormalizeDirectory('$(MSBuildProjectDirectory)', '..', 'b'))", []string{vroot + "/src/b/"}},
		{`$([System.IO.Path]::Combine('$(MSBuildThisFileDirectory)', '..\x.cs'))`, []string{vroot + "/src/a/../x.cs"}},
		{"$([MSBuild]::EnsureTrailingSlash('a'))b", []string{"a/b"}},
	}
	for _, c := range cases {
		got, err := e.eval(c.in, "src/a/x.csproj", 0)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"$(Nope)", "$(Foo.Bar())", "@(Compile)", "$([MSBuild]::Unknown('a'))", "$(Root"} {
		if _, err := e.eval(bad, "x", 0); err == nil {
			t.Errorf("%s evaluated", bad)
		}
	}
	if _, err := e.eval("$(OutDir)x", "x", 0); err != errIgnore {
		t.Errorf("OutDir: %v", err)
	}
}

func TestToTarget(t *testing.T) {
	got := toTarget(`..\Shared\*.cs`, []string{"src/App"})
	if len(got) != 1 || got[0].rel != "src/Shared/*.cs" || got[0].outside {
		t.Errorf("%+v", got)
	}
	if got := toTarget("../../../x", []string{"src/App"}); !got[0].outside {
		t.Errorf("%+v", got)
	}
	if got := toTarget(vroot+"/../x", nil); !got[0].outside {
		t.Errorf("%+v", got)
	}
	if got := toTarget(`C:\x\y.cs`, []string{"."}); !got[0].outside {
		t.Errorf("%+v", got)
	}
}

func TestNoProjectsCostsNothing(t *testing.T) {
	a, err := New(nil).Analyze(context.Background(), tree.NewMapTree(map[string]string{"a.go": "", "b/c.sln": ""}), nil)
	if err != nil || len(a.Tests) != 0 || len(a.Edges) != 0 || len(a.Roles) != 0 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestMTPConfig(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{
		"global.json":   `{"sdk":{"version":"10.0.100"},"test":{"runner":"Microsoft.Testing.Platform"}}`,
		"a/a.csproj":    "<Project/>",
		"w/global.json": `{"sdk":{"version":"10.0.100"}}`,
		"w/b/b.csproj":  "<Project/>",
	})
	a, err := New(nil).Analyze(context.Background(), mt, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{}
	for _, u := range a.Tests {
		cfg[u.ID] = u.Config
	}
	if cfg["dotnet:a/a.csproj"] != "global.json" || cfg["dotnet:w/b/b.csproj"] != "" {
		t.Errorf("%v", cfg)
	}
}

func TestEdgesOfNestedProjects(t *testing.T) {
	mt := tree.NewMapTree(map[string]string{"o/o.csproj": "<Project/>", "o/i/i.csproj": "<Project/>", "o/i/x.cs": "", "o/README.md": ""})
	a, err := New(nil).Analyze(context.Background(), mt, nil)
	if err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, e := range a.Edges {
		if e.To == "o/i/x.cs" {
			from = append(from, e.From)
		}
		if e.To == "o/README.md" || (e.To == "o/i/i.csproj" && e.From != "<dotnet:o/i/i.csproj>") {
			t.Errorf("unexpected edge %+v", e)
		}
	}
	slices.Sort(from)
	if !reflect.DeepEqual(from, []string{"<dotnet:o/i/i.csproj>", "<dotnet:o/o.csproj>"}) {
		t.Errorf("owners of x.cs: %v", from)
	}
	if a.Roles["o/o.csproj"] != model.RoleManifest {
		t.Errorf("role %v", a.Roles["o/o.csproj"])
	}
}
