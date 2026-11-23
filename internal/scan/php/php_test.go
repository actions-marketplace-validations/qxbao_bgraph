package php

import (
	"slices"
	"strings"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

func TestScanIncludes(t *testing.T) {
	src := "require 'outside.php';\n<?php\n" +
		"// include 'comment.php';\n/* require 'block.php'; */\n" +
		"require_once(__DIR__ . '/[bootstrap].php');\n" +
		"include 'fixtures/data.inc';\n" +
		"include \"$folder/file.php\";\n" +
		"$text = <<<'TEXT'\nrequire 'nowdoc.php';\n  TEXT;\n" +
		"?>include 'html.php';\n<?= include 'partial.phtml' ?>"
	got := Scan([]byte(src))
	want := []model.ImportRef{
		{Kind: model.ImportRequire, Line: 5, Spec: "/[bootstrap].php", Suffix: "__DIR__"},
		{Kind: model.ImportRequire, Line: 6, Spec: "fixtures/data.inc"},
		{Kind: model.ImportRequire, Line: 7, Dynamic: true},
		{Kind: model.ImportRequire, Line: 12, Spec: "partial.phtml"},
	}
	if got.Uncertain || !slices.Equal(got.Imports, want) {
		t.Fatalf("Scan: %+v; want %+v", got, want)
	}
}

func TestCommentInsertionPreservesImports(t *testing.T) {
	tokens := []string{"<?php ", "require_once", "(", "__DIR__", ".", "'/boot.php'", ")", ";", "include", "$path", ";"}
	want := Scan([]byte(strings.Join(tokens, " ")))
	for i := 1; i < len(tokens); i++ {
		src := strings.Join(tokens[:i], " ") + " /* include 'not-a-dependency.php'; */ " + strings.Join(tokens[i:], " ")
		got := Scan([]byte(src))
		if got.Uncertain || !slices.Equal(got.Imports, want.Imports) {
			t.Fatalf("after token %d: %+v; want %+v", i, got, want)
		}
	}
}

func TestScanSkipsMembersAndDeclarations(t *testing.T) {
	src := "<?php\n" +
		"$include = $opts['x'];\n" +
		"$this->include($tpl);\n" +
		"$x?->require('a.php');\n" +
		"$request->file('avatar');\n" +
		"new File($path);\n" +
		"self::INCLUDE;\n" +
		"function file($p) {}\n" +
		"const REQUIRE = 1;\n" +
		"\\file_get_contents('global.json');\n" +
		"include 'real.php';\n"
	got := Scan([]byte(src))
	want := []model.ImportRef{
		{Kind: model.ImportRead, Line: 10, Spec: "global.json"},
		{Kind: model.ImportRequire, Line: 11, Spec: "real.php"},
	}
	if got.Uncertain || !slices.Equal(got.Imports, want) {
		t.Fatalf("Scan: %+v; want %+v", got, want)
	}
}

func FuzzScan(f *testing.F) {
	for _, seed := range []string{"<?php require __DIR__ . '/a.php';", "<?= include $asset ?>", "<?php <<<END\ntext\nEND;"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, imp := range Scan([]byte(s)).Imports {
			if imp.Line < 1 {
				t.Fatalf("invalid include position: %+v", imp)
			}
		}
	})
}
