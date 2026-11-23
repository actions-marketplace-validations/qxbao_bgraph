package lockfile

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{"Typing_Extensions": "typing-extensions", "zope.interface": "zope-interface",
		"a-_.b": "a-b", "PyYAML": "pyyaml"} {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

const uvWorkspace = `version = 1
revision = 3
requires-python = ">=3.10"

[options]
exclude-newer = "2026-01-01T00:00:00Z"

[manifest]
members = ["a", "b", "root"]

[[package]]
name = "a"
version = "0.1.0"
source = { editable = "packages/a" }
dependencies = [
    { name = "requests" },
]

[package.optional-dependencies]
socks = [
    { name = "pysocks" },
]

[[package]]
name = "b"
version = "0.1.0"
source = { editable = "packages/b" }
dependencies = [
    { name = "a" },
    { name = "numpy", version = "2.0.0", source = { registry = "https://pypi.org/simple" }, marker = "python_full_version >= '3.12'" },
]

[[package]]
name = "numpy"
version = "1.26.0"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://example.invalid/numpy-1.26.0.tar.gz", hash = "sha256:n126", size = 10, upload-time = "2024-01-01T00:00:00Z" }

[[package]]
name = "numpy"
version = "2.0.0"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://example.invalid/numpy-2.0.0.tar.gz", hash = "sha256:n200", size = 10, upload-time = "2024-06-01T00:00:00Z" }

[[package]]
name = "pysocks"
version = "1.7.1"
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "pytest"
version = "8.0.0"
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "requests"
version = "2.31.0"
source = { registry = "https://pypi.org/simple" }
dependencies = [
    { name = "urllib3", marker = "python_full_version >= '3.10'" },
]

[[package]]
name = "root"
version = "0.1.0"
source = { virtual = "." }

[package.dev-dependencies]
dev = [
    { name = "pytest" },
]

[package.metadata]
requires-dist = []

[[package]]
name = "urllib3"
version = "2.0.0"
source = { registry = "https://pypi.org/simple" }
`

func mustUv(t *testing.T, s string) *Graph {
	t.Helper()
	g, err := Parse("uv.lock", ".", []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func names(g *Graph, dir string) []string {
	var out []string
	for _, id := range g.Packages(g.Projects[dir]) {
		if n, ok := g.Nodes[id]; ok && n.Project == "" {
			out = append(out, n.Name+"@"+n.Version)
		}
	}
	slices.Sort(out)
	return out
}

func TestUvWorkspaceClosure(t *testing.T) {
	g := mustUv(t, uvWorkspace)
	if g.Format != FormatUv || g.Version != "1.3" {
		t.Fatalf("format %s version %s", g.Format, g.Version)
	}
	if got := strings.Join(slices.Sorted(func(yield func(string) bool) {
		for k := range g.Projects {
			if !yield(k) {
				return
			}
		}
	}), ","); got != ".,packages/a,packages/b" {
		t.Fatalf("projects %s", got)
	}
	// Extras are part of the closure; the versioned reference picks one of the two numpy entries; b reaches a's packages.
	if got := strings.Join(names(g, "packages/a"), " "); got != "pysocks@1.7.1 requests@2.31.0 urllib3@2.0.0" {
		t.Errorf("a: %s", got)
	}
	if got := strings.Join(names(g, "packages/b"), " "); got != "numpy@2.0.0 pysocks@1.7.1 requests@2.31.0 urllib3@2.0.0" {
		t.Errorf("b: %s", got)
	}
	if got := strings.Join(names(g, "."), " "); got != "pytest@8.0.0" {
		t.Errorf("root: %s", got)
	}
}

func TestUvCompare(t *testing.T) {
	base := mustUv(t, uvWorkspace)
	for _, c := range []struct {
		name, from, to string
		changed        []string
		incomparable   string
	}{
		{"transitive bump", `name = "urllib3"
version = "2.0.0"`, `name = "urllib3"
version = "2.0.1"`, []string{"packages/a", "packages/b"}, ""},
		{"upload-time and size only", `size = 10, upload-time = "2024-06-01T00:00:00Z"`, `size = 11, upload-time = "2025-06-01T00:00:00Z"`, nil, ""},
		{"edge marker", `marker = "python_full_version >= '3.10'"`, `marker = "python_full_version >= '3.11'"`, []string{"packages/a", "packages/b"}, ""},
		{"artifact hash", `hash = "sha256:n200"`, `hash = "sha256:n201"`, []string{"packages/b"}, ""},
		{"root group", `name = "pytest"
version = "8.0.0"`, `name = "pytest"
version = "8.1.0"`, []string{"."}, ""},
		{"member specifiers only", `requires-dist = []`, `requires-dist = [{ name = "x" }]`, nil, ""},
		{"options", `exclude-newer = "2026-01-01T00:00:00Z"`, `exclude-newer = "2026-02-01T00:00:00Z"`, nil, ""},
		{"requires-python", `requires-python = ">=3.10"`, `requires-python = ">=3.11"`, nil, ""},
		{"conflicts", `requires-python = ">=3.10"`, "requires-python = \">=3.10\"\nconflicts = [[{ package = \"a\", extra = \"socks\" }]]", nil, "conflicts"},
		{"revision", `revision = 3`, `revision = 2`, nil, "version"},
	} {
		head := mustUv(t, strings.Replace(uvWorkspace, c.from, c.to, 1))
		d, err := Compare(base, head)
		if c.incomparable != "" {
			var ie *IncomparableError
			if !errors.As(err, &ie) || !slices.Contains(ie.Reasons, c.incomparable) {
				t.Errorf("%s: err %v, want reason %s", c.name, err, c.incomparable)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := d.ChangedProjects(); !slices.Equal(got, c.changed) {
			t.Errorf("%s: changed %v, want %v", c.name, got, c.changed)
		}
	}
}

func TestUvErrors(t *testing.T) {
	for name, s := range map[string]string{
		"ambiguous reference": strings.Replace(uvWorkspace, `{ name = "numpy", version = "2.0.0", source = { registry = "https://pypi.org/simple" }, marker`, `{ name = "numpy", marker`, 1),
		"unknown revision":    strings.Replace(uvWorkspace, "revision = 3", "revision = 99", 1),
		"unknown version":     strings.Replace(uvWorkspace, "version = 1\n", "version = 2\n", 1),
		"not toml":            "version = 1\n[[package]\n",
	} {
		if _, err := Parse("uv.lock", ".", []byte(s)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestUvVirtualRoot(t *testing.T) {
	s := strings.Replace(uvWorkspace, `[[package]]
name = "root"
version = "0.1.0"
source = { virtual = "." }

[package.dev-dependencies]
dev = [
    { name = "pytest" },
]

[package.metadata]
requires-dist = []
`, "", 1)
	s = strings.Replace(s, `members = ["a", "b", "root"]`, `members = ["a", "b"]

[manifest.dependency-groups]
dev = [{ name = "pytest", specifier = ">=8" }, { name = "Missing_Tool" }]`, 1)
	g := mustUv(t, s)
	if got := strings.Join(names(g, "."), " "); got != "pytest@8.0.0" {
		t.Errorf("virtual root: %s", got)
	}
	if n := g.Nodes[g.Projects["."]]; n.Package != "" || !strings.HasPrefix(n.ID, virtualRootPrefix) {
		t.Errorf("virtual root node %+v", n)
	}
	head := mustUv(t, strings.Replace(s, `version = "8.0.0"`, `version = "8.1.0"`, 1))
	d, err := Compare(g, head)
	if err != nil || !slices.Equal(d.ChangedProjects(), []string{"."}) {
		t.Errorf("virtual root group bump: %v %v", d, err)
	}
}

const poetryLock = `# This file is automatically @generated by Poetry 2.1.3 and should not be changed by hand.

[[package]]
name = "numpy"
version = "1.26.0"
description = "old"
optional = false
python-versions = ">=3.9"
groups = ["main"]
markers = "python_version < \"3.12\""
files = [
    {file = "numpy-1.26.0.tar.gz", hash = "sha256:n126"},
]

[[package]]
name = "numpy"
version = "2.0.0"
description = "new"
optional = false
python-versions = ">=3.12"
groups = ["main"]
markers = "python_version >= \"3.12\""
files = [
    {file = "numpy-2.0.0.tar.gz", hash = "sha256:n200"},
]

[[package]]
name = "Scipy"
version = "1.11.0"
description = "sci"
optional = false
python-versions = ">=3.9"
groups = ["main"]
files = []

[package.dependencies]
numpy = ">=1.25"
missing-dep = {version = "*", markers = "python_version < \"3.0\""}

[metadata]
lock-version = "2.1"
python-versions = "^3.10"
content-hash = "aa"
`

func TestPoetry(t *testing.T) {
	g, err := Parse("poetry.lock", "app", []byte(poetryLock))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Projects) != 1 || g.Projects["app"] == "" {
		t.Fatalf("projects %v", g.Projects)
	}
	if got := strings.Join(names(g, "app"), " "); got != "numpy@1.26.0 numpy@2.0.0 scipy@1.11.0" {
		t.Errorf("closure %s", got)
	}
	sc := g.Nodes["poetry:scipy 1.11.0 null"]
	if sc == nil || len(sc.Deps) != 3 || !slices.Contains(mapValues(sc.Deps), MissingID("missing-dep")) {
		t.Errorf("scipy edges %+v", sc)
	}
	for _, c := range []struct {
		name, from, to string
		changed        bool
		incomparable   bool
	}{
		{"content-hash only", `content-hash = "aa"`, `content-hash = "bb"`, false, false},
		{"description only", `description = "sci"`, `description = "Sci"`, false, false},
		{"project python range", `python-versions = "^3.10"`, `python-versions = "^3.11"`, false, false},
		{"version bump", `version = "1.11.0"`, `version = "1.11.1"`, true, false},
		{"optional flag", "optional = false\npython-versions = \">=3.9\"\ngroups = [\"main\"]\nfiles = []", "optional = true\npython-versions = \">=3.9\"\ngroups = [\"main\"]\nfiles = []", true, false},
		{"requirement", `numpy = ">=1.25"`, `numpy = ">=1.26"`, true, false},
		{"lock-version", `lock-version = "2.1"`, `lock-version = "2.0"`, false, true},
		{"top-level key", `[metadata]`, "[extras]\nx = []\n\n[metadata]", false, true},
	} {
		h, err := Parse("poetry.lock", "app", []byte(strings.Replace(poetryLock, c.from, c.to, 1)))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		d, err := Compare(g, h)
		if c.incomparable {
			if err == nil {
				t.Errorf("%s: comparable", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if d.Changed["app"] != c.changed {
			t.Errorf("%s: changed %v", c.name, d.Changed)
		}
	}
	if _, err := Parse("poetry.lock", ".", []byte(strings.Replace(poetryLock, `lock-version = "2.1"`, `lock-version = "3.0"`, 1))); err == nil {
		t.Error("unknown lock-version parsed")
	}
}

func TestPoetryOldFiles(t *testing.T) {
	old := `[[package]]
name = "six"
version = "1.16.0"
description = "x"
category = "main"
optional = false
python-versions = "*"

[metadata]
lock-version = "1.1"
python-versions = "^3.8"
content-hash = "aa"

[metadata.files]
six = [
    {file = "six-1.16.0.tar.gz", hash = "sha256:a"},
]
`
	b, err := Parse("poetry.lock", ".", []byte(old))
	if err != nil {
		t.Fatal(err)
	}
	h, err := Parse("poetry.lock", ".", []byte(strings.Replace(old, "sha256:a", "sha256:b", 1)))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Compare(b, h)
	if err != nil || !d.Changed["."] {
		t.Errorf("[metadata.files] hash change: %v %v", d, err)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
