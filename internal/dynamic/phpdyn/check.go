package phpdyn

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/tree"
)

// spansScript parses each file of ARGV with PHP's own tokenizer (PhpToken::tokenize with TOKEN_PARSE, which fails on syntax
// errors) and prints, per file, "OK <index> <spans>" with the span "<line of function>-<line of the closing brace>" of every
// named function or method with a body, at any depth, or "ERR <index> <message>".
const spansScript = `<?php
$id = '/^[A-Za-z_\x80-\xff][A-Za-z0-9_\x80-\xff]*$/';
foreach (array_slice($argv, 1) as $i => $f) {
    try {
        $t = PhpToken::tokenize(file_get_contents($f), TOKEN_PARSE);
    } catch (Throwable $e) {
        echo "ERR $i ", str_replace("\n", ' ', $e->getMessage()), "\n";
        continue;
    }
    $n = count($t);
    $next = function (int $k) use ($t, $n): int {
        for ($k++; $k < $n && $t[$k]->isIgnorable(); $k++) {}
        return $k;
    };
    $spans = [];
    for ($k = 0; $k < $n; $k++) {
        if ($t[$k]->id !== T_FUNCTION) { continue; }
        $j = $next($k);
        if ($j < $n && $t[$j]->text === '&') { $j = $next($j); }
        if ($j >= $n || !preg_match($id, $t[$j]->text)) { continue; }
        $p = $next($j);
        if ($p >= $n || $t[$p]->text !== '(') { continue; }
        $d = 0;
        for (; $p < $n; $p++) {
            if ($t[$p]->text === '(') { $d++; }
            if ($t[$p]->text === ')') { $d--; if ($d === 0) { break; } }
        }
        $d = 0;
        for ($p++; $p < $n; $p++) {
            $x = $t[$p]->text;
            if ($x === '(') { $d++; } elseif ($x === ')') { $d--; }
            if ($d === 0 && ($x === '{' || $x === ';')) { break; }
        }
        if ($p >= $n || $t[$p]->text === ';') { continue; }
        $d = 0;
        for ($q = $p; $q < $n; $q++) {
            $x = $t[$q];
            if ($x->text === '{' || $x->id === T_CURLY_OPEN || $x->id === T_DOLLAR_OPEN_CURLY_BRACES) { $d++; }
            elseif ($x->text === '}') { $d--; if ($d === 0) { break; } }
        }
        if ($q >= $n) { echo "ERR $i unbalanced braces\n"; continue 2; }
        $spans[] = $t[$k]->line . '-' . $t[$q]->line;
    }
    echo "OK $i ", implode(',', $spans), "\n";
}
`

// CheckFile is one changed file whose changes the body guard accepted.
type CheckFile struct {
	Path       string
	Base, Head []byte
}

// CheckFiles returns the files the tokenizer cross-check must compare: every KindLines file.
func CheckFiles(changed []Changed, base, head tree.Tree) []CheckFile {
	var out []CheckFile
	for _, c := range changed {
		if c.Kind != KindLines {
			continue
		}
		b, err1 := base.Read(c.Path)
		h, err2 := head.Read(c.Path)
		if err1 != nil || err2 != nil {
			continue // Downgrade checks a missing result
		}
		out = append(out, CheckFile{Path: c.Path, Base: b, Head: h})
	}
	return out
}

// sameSpans reports whether the spans PHP printed ("a-b" joined by commas) are exactly the named functions with a body the
// guard's structure found in src, as multisets.
func sameSpans(src []byte, php string) bool {
	mine, ok := Spans(src)
	if !ok {
		return false
	}
	var a []string
	for _, s := range mine {
		a = append(a, strconv.Itoa(s.Start)+"-"+strconv.Itoa(s.End))
	}
	var b []string
	if php != "" {
		b = strings.Split(php, ",")
	}
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// CrossCheck runs the tokenizer cross-check of docs/dynamic-layer.md §11.2 with the PHP executable exe: the head of every file
// passes `php -l`, and the function spans PHP's tokenizer finds at base and head are those of the body guard. It returns, per
// file path, why the file must fall back to file level (files absent from the map passed).
func CrossCheck(ctx context.Context, exe string, files []CheckFile) map[string]string {
	fail := map[string]string{}
	if len(files) == 0 {
		return fail
	}
	all := func(why string) map[string]string {
		for _, f := range files {
			fail[f.Path] = why
		}
		return fail
	}
	dir, err := os.MkdirTemp("", "bgraph-phpcheck-")
	if err != nil {
		return all("cannot run the tokenizer cross-check: " + err.Error())
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Dir = dir
		cmd.Env = probeEnv()
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String() + errb.String(), err
	}
	script := filepath.Join(dir, "spans.php")
	if err := os.WriteFile(script, []byte(spansScript), 0o644); err != nil {
		return all("cannot run the tokenizer cross-check: " + err.Error())
	}
	args := []string{script}
	for i, f := range files {
		b := filepath.Join(dir, fmt.Sprintf("%d-base.php", i))
		h := filepath.Join(dir, fmt.Sprintf("%d-head.php", i))
		if os.WriteFile(b, f.Base, 0o644) != nil || os.WriteFile(h, f.Head, 0o644) != nil {
			return all("cannot write temporary files for the tokenizer cross-check")
		}
		if out, err := run("-l", h); err != nil {
			fail[f.Path] = "php -l fails at the head: " + firstN(strings.TrimSpace(out), 200)
		}
		args = append(args, b, h)
	}
	out, err := run(args...)
	if err != nil {
		return all(fmt.Sprintf("the tokenizer cross-check failed: %v %s", err, firstN(strings.TrimSpace(out), 200)))
	}
	got := map[int]string{}
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		kind, rest, ok := strings.Cut(l, " ")
		if !ok || kind != "OK" && kind != "ERR" {
			continue
		}
		is, val, _ := strings.Cut(rest, " ")
		i, err := strconv.Atoi(is)
		if err != nil || i < 0 || i >= 2*len(files) {
			continue
		}
		if kind == "ERR" {
			val = "\x00" + val
		}
		got[i] = val
	}
	for i, f := range files {
		if _, bad := fail[f.Path]; bad {
			continue
		}
		for side, src := range [][]byte{f.Base, f.Head} {
			name := [2]string{"base", "head"}[side]
			v, ok := got[2*i+side]
			switch {
			case !ok:
				fail[f.Path] = "the tokenizer cross-check gave no result for the " + name
			case strings.HasPrefix(v, "\x00"):
				fail[f.Path] = "PHP cannot parse the " + name + ": " + firstN(v[1:], 160)
			case !sameSpans(src, v):
				fail[f.Path] = "the guard's function spans differ from PHP's tokenizer at the " + name
			}
			if fail[f.Path] != "" {
				break
			}
		}
	}
	return fail
}

// Downgrade returns changed with the KindLines files that failed the cross-check (or were not checked) back at file level.
func Downgrade(changed []Changed, fail map[string]string, checked map[string]bool) []Changed {
	out := slices.Clone(changed)
	for i, c := range out {
		if c.Kind != KindLines {
			continue
		}
		why, bad := fail[c.Path]
		if !checked[c.Path] {
			bad, why = true, "not cross-checked"
		}
		if bad {
			out[i] = Changed{Path: c.Path, Kind: KindFile, Note: why + " (file level)", Test: c.Test}
		}
	}
	return out
}
