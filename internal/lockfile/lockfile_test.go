package lockfile

import (
	"errors"
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

// Recent pnpm writes an "env lockfile" (the package manager's pinned version) as a first YAML document; the project
// lockfile is the second document. Reading only the first document would see no project dependencies at all.
const pnpmEnvDoc = `---
lockfileVersion: '9.0'

importers:

  .:
    configDependencies: {}
    packageManagerDependencies:
      pnpm:
        specifier: 10.0.0
        version: 10.0.0

packages:

  pnpm@10.0.0:
    resolution: {integrity: sha512-pnpm}

snapshots:

  pnpm@10.0.0: {}

---
`

func TestPnpmEnvDocument(t *testing.T) {
	g := mustParse(t, "pnpm-lock.yaml", ".", pnpmEnvDoc+pnpm9)
	if got := len(g.Projects); got != 3 {
		t.Fatalf("projects = %d", got)
	}
	var got []string
	for _, id := range g.Packages(g.Projects["packages/a"]) {
		if n := g.Nodes[id]; n.Project == "" {
			got = append(got, n.Package+"@"+n.Version)
		}
	}
	if !slices.Equal(got, []string{"lodash@4.17.21", "ms@2.1.3"}) {
		t.Fatalf("closure of a = %v", got)
	}
	// A package manager version bump is outside the graph: not comparable.
	plain := mustParse(t, "pnpm-lock.yaml", ".", pnpm9)
	if _, err := Compare(plain, g); err == nil {
		t.Fatal("env lockfile change must not compare")
	}
	// An unknown first document is an error.
	if _, err := Parse("pnpm-lock.yaml", ".", []byte("---\nfoo: 1\n---\n"+pnpm9)); err == nil {
		t.Fatal("unknown multi-document layout must not parse")
	}
}

// A tarball dependency is keyed by its URL; its version is the recorded manifest version.
func TestPnpmTarballVersion(t *testing.T) {
	const url = "https://codeload.github.com/u/r/tar.gz/abc"
	data := strings.Replace(pnpm9, "        version: 4.17.21", "        version: "+url, 1)
	data = strings.Replace(data, "  lodash@4.17.21:\n    resolution: {integrity: sha512-lodash}", "  lodash@"+url+":\n    resolution: {tarball: "+url+"}\n    version: 4.17.21", 1)
	data = strings.Replace(data, "  lodash@4.17.21: {}", "  lodash@"+url+": {}", 1)
	g := mustParse(t, "pnpm-lock.yaml", ".", data)
	n := g.Nodes["pnpm:lodash@"+url]
	if n == nil || n.Package != "lodash" || n.Version != "4.17.21" || !n.Local {
		t.Fatalf("node = %+v", n)
	}
}

// A catalog bump is visible in the importers that use it: it is not a settings change.
func TestPnpmCatalogsNotExtra(t *testing.T) {
	cat := func(v string) string {
		return strings.Replace(pnpm9, "importers:", "catalogs:\n  default:\n    ms:\n      specifier: ^2\n      version: "+v+"\n\nimporters:", 1)
	}
	base := mustParse(t, "pnpm-lock.yaml", ".", cat("2.1.3"))
	head := mustParse(t, "pnpm-lock.yaml", ".", strings.Replace(cat("2.1.2"), "        version: 2.1.3", "        version: 2.1.2", 1))
	d, err := Compare(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ChangedProjects(); !slices.Equal(got, []string{"packages/a", "packages/b"}) {
		t.Fatalf("changed = %v", got)
	}
}

// Overrides only steer resolution: what they change is in the resolved graph, so they are not a settings change.
func TestPnpmOverridesCompared(t *testing.T) {
	ov := func(data, v string) string {
		return strings.Replace(data, "importers:", "overrides:\n  ms: "+v+"\n\nimporters:", 1)
	}
	base := mustParse(t, "pnpm-lock.yaml", ".", ov(pnpm9, "2.1.3"))
	// The override moves b's ms: b and a (which links b) change.
	head := mustParse(t, "pnpm-lock.yaml", ".", ov(strings.Replace(pnpm9, "        version: 2.1.3", "        version: 2.1.2", 1), "2.1.2"))
	d, err := Compare(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ChangedProjects(); !slices.Equal(got, []string{"packages/a", "packages/b"}) {
		t.Fatalf("changed = %v", got)
	}
	// An override that changes nothing that is installed selects nothing.
	same := mustParse(t, "pnpm-lock.yaml", ".", strings.Replace(ov(pnpm9, "2.1.3"), "  ms: 2.1.3\n", "  ms: 2.1.3\n  left-pad: 1.3.0\n", 1))
	if d, err := Compare(base, same); err != nil || len(d.Changed) != 0 {
		t.Fatalf("unused override: changed=%v err=%v", d, err)
	}
}

// pnpmPatched patches lodash@4.17.21 with hash h; patches is the patchedDependencies section.
func pnpmPatched(h, patches string) string {
	data := strings.Replace(pnpm9, "importers:", "patchedDependencies:\n"+patches+"\nimporters:", 1)
	data = strings.Replace(data, "        version: 4.17.21", "        version: 4.17.21(patch_hash="+h+")", 1)
	return strings.Replace(data, "  lodash@4.17.21: {}", "  lodash@4.17.21(patch_hash="+h+"): {}", 1)
}

func TestPnpmPatches(t *testing.T) {
	base := mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h1", "  lodash@4.17.21: h1\n"))
	changed := func(t *testing.T, head string) []string {
		t.Helper()
		d, err := Compare(base, mustParse(t, "pnpm-lock.yaml", ".", head))
		if err != nil {
			t.Fatal(err)
		}
		if !d.Names["lodash"] {
			t.Errorf("names = %v", d.ChangedNames())
		}
		return d.ChangedProjects()
	}
	// A new patch hash: only the project that installs lodash.
	if got := changed(t, pnpmPatched("h2", "  lodash@4.17.21: h2\n")); !slices.Equal(got, []string{"packages/a"}) {
		t.Fatalf("hash changed: %v", got)
	}
	// The patch entry changes while the package keys do not (older {hash, path} form, path moved): still marked.
	old := mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h1", "  lodash@4.17.21:\n    hash: h1\n    path: patches/lodash.patch\n"))
	d, err := Compare(old, mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h1", "  lodash@4.17.21:\n    hash: h1\n    path: patches/x/lodash.patch\n")))
	if err != nil || !slices.Equal(d.ChangedProjects(), []string{"packages/a"}) {
		t.Fatalf("path moved: %v %v", d, err)
	}
	// Patch removed.
	if got := changed(t, pnpm9); !slices.Equal(got, []string{"packages/a"}) {
		t.Fatalf("patch removed: %v", got)
	}
	// A bare package name selects every version.
	bare := mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h1", "  lodash: h1\n"))
	if d, err := Compare(bare, mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h2", "  lodash: h2\n"))); err != nil || !slices.Equal(d.ChangedProjects(), []string{"packages/a"}) {
		t.Fatalf("bare selector: %v %v", d, err)
	}
	// A range, or a selector that matches no package: not comparable.
	for _, sel := range []string{"lodash@^4.17.0", "lodash@4.17.20", "left-pad"} {
		b := mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h1", "  lodash@4.17.21: h1\n  "+sel+": x1\n"))
		h := mustParse(t, "pnpm-lock.yaml", ".", pnpmPatched("h1", "  lodash@4.17.21: h1\n  "+sel+": x2\n"))
		var ie *IncomparableError
		if _, err := Compare(b, h); !errors.As(err, &ie) || !slices.Equal(ie.Reasons, []string{"patch selector"}) {
			t.Fatalf("%s: err = %v", sel, err)
		}
	}
	// An unchanged patch next to another change does not widen by itself.
	head := strings.Replace(pnpmPatched("h1", "  lodash@4.17.21: h1\n"), "        version: 2.1.3", "        version: 2.1.2", 1)
	if d, err := Compare(base, mustParse(t, "pnpm-lock.yaml", ".", head)); err != nil ||
		!slices.Equal(d.ChangedProjects(), []string{"packages/a", "packages/b"}) || d.Names["lodash"] {
		t.Fatalf("unchanged patch: %v %v", d, err)
	}
}

// Resolution settings, the env lockfile and other top-level keys stay incomparable, with the reason named.
func TestPnpmIncomparableReasons(t *testing.T) {
	base := mustParse(t, "pnpm-lock.yaml", ".", pnpm9)
	for _, c := range []struct {
		head string
		want []string
	}{
		{strings.Replace(pnpm9, "autoInstallPeers: true", "autoInstallPeers: false", 1), []string{"settings"}},
		{strings.Replace(pnpm9, "importers:", "pnpmfileChecksum: abc\n\nimporters:", 1), []string{"pnpmfileChecksum"}},
		{pnpmEnvDoc + pnpm9, []string{"<env lockfile>"}},
	} {
		var ie *IncomparableError
		if _, err := Compare(base, mustParse(t, "pnpm-lock.yaml", ".", c.head)); !errors.As(err, &ie) || !slices.Equal(ie.Reasons, c.want) {
			t.Errorf("err = %v, want reasons %v", err, c.want)
		}
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
	// A workspace crate with the same name and version as a registry crate: the reference without a source is the
	// path package, the registry one carries its source.
	dup := cargo4 + `
[[package]]
name = "core"
version = "0.1.0"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "ddd"
`
	dup = strings.Replace(dup, `dependencies = ["core", "serde"]`, `dependencies = ["core 0.1.0", "core 0.1.0 (registry+https://github.com/rust-lang/crates.io-index)", "serde"]`, 1)
	g := mustParse(t, "Cargo.lock", ".", dup)
	app := g.Nodes[g.Projects["app 0.1.0"]]
	if app.Deps["core 0.1.0"] != "cargo:core 0.1.0 " || app.Deps["core 0.1.0 (registry+https://github.com/rust-lang/crates.io-index)"] == "cargo:core 0.1.0 " {
		t.Fatalf("deps = %v", app.Deps)
	}
	if _, err := Parse("Cargo.lock", ".", []byte(strings.Replace(cargo4, "version = 4", "version = 2", 1))); err == nil {
		t.Fatal("version 2 must not parse")
	}
}
