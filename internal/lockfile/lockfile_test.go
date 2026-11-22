package lockfile

import (
	"slices"
	"strings"
	"testing"
)

const pnpm9 = `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true

importers:

  .:
    devDependencies:
      vitest:
        specifier: ^1.0.0
        version: 1.0.0

  packages/a:
    dependencies:
      b:
        specifier: workspace:*
        version: link:../b
      lodash:
        specifier: ^4
        version: 4.17.21

  packages/b:
    dependencies:
      ms:
        specifier: ^2
        version: 2.1.3

packages:

  lodash@4.17.21:
    resolution: {integrity: sha512-lodash}

  ms@2.1.3:
    resolution: {integrity: sha512-ms3}

  ms@2.1.2:
    resolution: {integrity: sha512-ms2}

  vitest@1.0.0:
    resolution: {integrity: sha512-vitest}
    hasBin: true

snapshots:

  lodash@4.17.21: {}

  ms@2.1.3: {}

  ms@2.1.2: {}

  vitest@1.0.0:
    dependencies:
      ms: 2.1.2
`

func mustParse(t *testing.T, name, dir, data string) *Graph {
	t.Helper()
	g, err := Parse(name, dir, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestPnpmWorkspaceClosure(t *testing.T) {
	base := mustParse(t, "pnpm-lock.yaml", ".", pnpm9)
	if got := len(base.Projects); got != 3 {
		t.Fatalf("projects = %d", got)
	}
	// ms of packages/b bumped: b changes, and a (which links b) too; the root does not.
	head := mustParse(t, "pnpm-lock.yaml", ".", strings.Replace(pnpm9, "        version: 2.1.3", "        version: 2.1.2", 1))
	d, err := Compare(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ChangedProjects(); !slices.Equal(got, []string{"packages/a", "packages/b"}) {
		t.Fatalf("changed = %v", got)
	}
	if dc := d.DirectChanges("packages/a"); dc.Risky == "" {
		t.Fatalf("a link dependency change must not narrow: %+v", dc)
	}
	// lockfile version change → not comparable.
	other := mustParse(t, "pnpm-lock.yaml", ".", strings.Replace(pnpm9, "autoInstallPeers: true", "autoInstallPeers: false", 1))
	if _, err := Compare(base, other); err == nil {
		t.Fatal("settings change must not compare")
	}
}

const npm3 = `{
  "name": "root", "lockfileVersion": 3, "requires": true,
  "packages": {
    "": {"name": "root", "workspaces": ["packages/*"], "devDependencies": {"jest": "^29"}},
    "node_modules/jest": {"version": "29.0.0", "integrity": "sha-jest", "dependencies": {"ms": "^2"}},
    "node_modules/ms": {"version": "2.1.3", "integrity": "sha-ms3"},
    "node_modules/a": {"resolved": "packages/a", "link": true},
    "packages/a": {"name": "a", "dependencies": {"ms": "^2.0.0", "left": "^1"}},
    "packages/a/node_modules/ms": {"version": "2.0.0", "integrity": "sha-ms0"},
    "node_modules/left": {"version": "1.0.0", "integrity": "sha-left"}
  }
}`

func TestNpmHoisting(t *testing.T) {
	g := mustParse(t, "package-lock.json", "web", npm3)
	a, ok := g.Projects["web/packages/a"]
	if !ok {
		t.Fatalf("projects = %v", g.Projects)
	}
	var got []string
	for _, id := range g.Packages(a) {
		if n := g.Nodes[id]; n.Project == "" {
			got = append(got, n.Package+"@"+n.Version)
		}
	}
	if !slices.Equal(got, []string{"left@1.0.0", "ms@2.0.0"}) {
		t.Fatalf("closure of a = %v", got)
	}
	head := mustParse(t, "package-lock.json", "web", strings.Replace(npm3, `"sha-ms3"`, `"sha-ms3b"`, 1))
	d, err := Compare(g, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ChangedProjects(); !slices.Equal(got, []string{"web"}) {
		t.Fatalf("changed = %v", got)
	}
	if dc := d.DirectChanges("web"); dc.Risky != "" || !slices.Equal(dc.Deps, []string{"jest"}) {
		t.Fatalf("direct = %+v", dc)
	}
}

const cargo4 = `version = 4

[[package]]
name = "app"
version = "0.1.0"
dependencies = ["core", "serde"]

[[package]]
name = "core"
version = "0.1.0"
dependencies = ["log"]

[[package]]
name = "tool"
version = "0.1.0"

[[package]]
name = "log"
version = "0.4.20"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "aaa"

[[package]]
name = "serde"
version = "1.0.0"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "bbb"
`

func TestCargoMembers(t *testing.T) {
	base := mustParse(t, "Cargo.lock", ".", cargo4)
	head := mustParse(t, "Cargo.lock", ".", strings.Replace(strings.Replace(cargo4, `"0.4.20"`, `"0.4.21"`, 1), `"aaa"`, `"ccc"`, 1))
	var changed []string
	for _, name := range []string{"app", "core", "tool"} {
		bk, ok1 := base.CargoProject(name)
		hk, ok2 := head.CargoProject(name)
		if !ok1 || !ok2 {
			t.Fatalf("%s not found", name)
		}
		if ClosureChanged(base, head, base.Projects[bk], head.Projects[hk]) {
			changed = append(changed, name)
		}
	}
	if !slices.Equal(changed, []string{"app", "core"}) {
		t.Fatalf("changed = %v", changed)
	}
	if _, err := Parse("Cargo.lock", ".", []byte(strings.Replace(cargo4, "version = 4", "version = 2", 1))); err == nil {
		t.Fatal("version 2 must not parse")
	}
}
