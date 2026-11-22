package nodedyn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestPackageJSONPart(t *testing.T) {
	a := PackageJSONPart([]byte(`{"name":"x","version":"1.0.0","type":"module","dependencies":{"a":"1"},"scripts":{"t":"x"},"exports":"./i.js"}`))
	b := PackageJSONPart([]byte(`{"exports":"./i.js","version":"2.0.0","name":"x","dependencies":{"a":"2","b":"1"},"type":"module"}`))
	if a != b {
		t.Errorf("installation keys matter: %s vs %s", a, b)
	}
	c := PackageJSONPart([]byte(`{"name":"x","type":"commonjs","exports":"./i.js"}`))
	if a == c {
		t.Error("type ignored")
	}
	if PackageJSONPart([]byte("{oops")) != "{oops" {
		t.Error("unparsable")
	}
}

func TestEnvFiles(t *testing.T) {
	base := map[string]string{
		"package.json": `{"name":"x","version":"1","jest":{"roots":["test"]}}`, "vitest.config.ts": "a", "tsconfig.json": "{}",
		"babel.config.js": "b", ".mocharc.yml": "m", "src/a.ts": "s", "node_modules/p/package.json": "{}", "pnpm-lock.yaml": "l",
	}
	f := EnvFiles(tree.NewMapTree(base))
	for _, p := range []string{"package.json", "vitest.config.ts", "tsconfig.json", "babel.config.js", ".mocharc.yml"} {
		if f[p] == "" {
			t.Errorf("%s not fingerprinted", p)
		}
	}
	for _, p := range []string{"src/a.ts", "node_modules/p/package.json", "pnpm-lock.yaml"} {
		if f[p] != "" {
			t.Errorf("%s fingerprinted", p)
		}
	}
	probe := []string{"node v22.17.1"}
	fp := func(m map[string]string, inst ...string) string {
		return Fingerprint("c", probe, inst, tree.NewMapTree(m))
	}
	ref := fp(base)
	mod := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	if fp(mod("package.json", `{"name":"x","version":"2","jest":{"roots":["test"]},"devDependencies":{"q":"1"}}`)) != ref {
		t.Error("a version/dependency bump changes the fingerprint")
	}
	if fp(mod("pnpm-lock.yaml", "l2")) != ref || fp(mod("src/a.ts", "s2")) != ref {
		t.Error("lockfile or source in the fingerprint")
	}
	for k, v := range map[string]string{"package.json": `{"name":"x","version":"1","jest":{"roots":["src"]}}`, "vitest.config.ts": "a2", "tsconfig.json": `{"x":1}`} {
		if fp(mod(k, v)) == ref {
			t.Errorf("%s change not in the fingerprint", k)
		}
	}
	if fp(base, "installed . .package-lock.json 1") == fp(base, "installed . .package-lock.json 2") {
		t.Error("installed state not in the fingerprint")
	}
	if Fingerprint("c", []string{"node v22.17.1"}, nil, tree.NewMapTree(base)) == Fingerprint("c", []string{"node v24.0.0"}, nil, tree.NewMapTree(base)) {
		t.Error("node version not in the fingerprint")
	}
}

func TestInstalled(t *testing.T) {
	root := t.TempDir()
	tr := tree.NewMapTree(map[string]string{"package.json": "{}", "packages/a/package.json": "{}", "packages/b/package.json": "{}"})
	os.MkdirAll(filepath.Join(root, "node_modules", ".pnpm"), 0o755)
	os.WriteFile(filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(root, "node_modules", ".modules.yaml"), []byte("prunedAt: now"), 0o644)
	writePkg(t, filepath.Join(root, "packages", "a", "node_modules", "dep"), "dep", "1.0.0")
	writePkg(t, filepath.Join(root, "packages", "a", "node_modules", "@s", "q"), "@s/q", "2.0.0")
	lines, files, err := Installed(root, tr)
	if err != nil || len(lines) != 2 || len(files) != 2 {
		t.Fatalf("%v %v %v", lines, files, err)
	}
	if !strings.HasPrefix(lines[0], "installed . .pnpm/lock.yaml ") || !strings.HasPrefix(lines[1], "installed packages/a packages ") {
		t.Errorf("%v", lines)
	}
	os.WriteFile(filepath.Join(root, "node_modules", ".modules.yaml"), []byte("prunedAt: later"), 0o644)
	if l2, _, _ := Installed(root, tr); l2[0] != lines[0] {
		t.Error(".modules.yaml timestamps change the state")
	}
	writePkg(t, filepath.Join(root, "packages", "a", "node_modules", "dep"), "dep", "1.0.1")
	if l2, _, _ := Installed(root, tr); l2[1] == lines[1] {
		t.Error("a package version change is not seen")
	}
	os.WriteFile(filepath.Join(root, ".pnp.cjs"), []byte(""), 0o644)
	if _, _, err := Installed(root, tr); err == nil {
		t.Error("Plug'n'Play accepted")
	}
}
