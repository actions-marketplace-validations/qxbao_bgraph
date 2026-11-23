package php

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
	"github.com/qxbao/bgraph/internal/tree"
)

func analyze(t *testing.T, files map[string]string) *eco.Analysis {
	t.Helper()
	a, err := New(nil).Analyze(context.Background(), tree.NewMapTree(files), scancache.New())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func unit(t *testing.T, a *eco.Analysis, id string) model.TestUnit {
	t.Helper()
	for _, u := range a.Tests {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("unit %s missing from %+v", id, a.Tests)
	return model.TestUnit{}
}

func TestTestSuffixIsCaseSensitive(t *testing.T) {
	a := analyze(t, map[string]string{
		"composer.json":       `{"require-dev":{"phpunit/phpunit":"^10"}}`,
		"src/Latest.php":      "<?php",
		"src/Contest.php":     "<?php",
		"tests/FooTest.php":   "<?php",
		"tests/basic.phpt":    "--TEST--",
		"tests/Support/x.php": "<?php",
	})
	for f, want := range map[string]model.Role{"src/Latest.php": model.RoleSource, "src/Contest.php": model.RoleSource, "tests/FooTest.php": model.RoleTest, "tests/basic.phpt": model.RoleTest} {
		if got := a.Roles[f]; got != want {
			t.Errorf("role of %s = %v, want %v", f, got, want)
		}
	}
	if got := unit(t, a, ".::php").RunnerPaths; !slices.Equal(got, []string{"tests"}) {
		t.Errorf("RunnerPaths = %v, want [tests]", got)
	}
}

func TestConfigOnlyProjectInheritsBinDir(t *testing.T) {
	a := analyze(t, map[string]string{
		"composer.json":                      `{"require-dev":{"phpunit/phpunit":"^10"},"config":{"bin-dir":"tools/bin"}}`,
		"phpunit.xml.dist":                   `<phpunit/>`,
		"dev/tests/unit/phpunit.xml.dist":    `<phpunit/>`,
		"dev/tests/unit/Magento/FooTest.php": "<?php",
		"standalone/phpunit.xml":             `<phpunit/>`,
	})
	if got := unit(t, a, "dev/tests/unit::php").BinDir; got != "tools/bin" {
		t.Errorf("config-only BinDir = %q, want tools/bin", got)
	}
	if got := unit(t, a, ".::php").BinDir; got != "tools/bin" {
		t.Errorf("root BinDir = %q, want tools/bin", got)
	}
}

func TestPHPUnitConfigLookupOrder(t *testing.T) {
	a := analyze(t, map[string]string{
		"composer.json":    `{"require-dev":{"phpunit/phpunit":"^10"}}`,
		"phpunit.xml.dist": `<phpunit/>`,
		"phpunit.dist.xml": `<phpunit/>`,
	})
	if got := unit(t, a, ".::php").Config; got != "phpunit.dist.xml" {
		t.Errorf("Config = %q, want phpunit.dist.xml", got)
	}
}

func TestScriptSuiteStopsAtCommandBoundary(t *testing.T) {
	dirs := map[string]bool{"src": true}
	isOperand := func(s string) bool { return dirs[s] }
	for _, tc := range []struct {
		script string
		custom bool
	}{
		{"phpunit && phpstan analyse src", false},
		{"phpunit; phpstan analyse src", false},
		{"phpunit || echo src", false},
		{"phpstan analyse src && phpunit", false},
		{"phpunit src && phpstan", true},
		{"vendor/bin/phpunit --configuration other.xml", true},
	} {
		m := manifest{Scripts: map[string]pathList{"test": {tc.script}}}
		runner, custom := m.scriptSuite("phpunit.xml", isOperand)
		if runner != RunnerPHPUnit || custom != tc.custom {
			t.Errorf("%q: runner %q custom %v; want phpunit %v", tc.script, runner, custom, tc.custom)
		}
	}
}

func TestExternalReadsDoNotWiden(t *testing.T) {
	a := analyze(t, map[string]string{
		"composer.json":     `{"require-dev":{"phpunit/phpunit":"^10"}}`,
		"phpunit.xml":       `<phpunit/>`,
		"tests/FooTest.php": "<?php",
		"src/Api.php": `<?php
json_decode(file_get_contents('php://input'));
$h = fopen('php://memory', 'r+');
$r = fopen('https://example.com/x', 'r');
is_dir('/tmp');
file_exists('C:\\Windows');
file_get_contents('php://filter/read=string.rot13/resource=data.txt');`,
	})
	var specs []string
	for _, u := range a.Unresolved {
		if u.File == "src/Api.php" {
			specs = append(specs, u.Spec)
		}
	}
	if len(specs) != 1 || !strings.HasPrefix(specs[0], "php://filter") {
		t.Errorf("unresolved = %v, want only the php://filter read", specs)
	}
}
