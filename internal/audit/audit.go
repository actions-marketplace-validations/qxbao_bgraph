// Package audit parses test-result files (JUnit XML, Jest/Vitest JSON, Playwright JSON, `go test -json`) into a flat
// list of failed test cases, and maps each failure to a repo-relative test file (docs/cli.md, audit).
//
// It only extracts facts from the results; deciding whether bgraph's selection covers a failure is done by the
// root package (Index.Audit).
package audit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Format names.
const (
	FormatAuto       = "auto"
	FormatJUnit      = "junit"
	FormatJestJSON   = "jest-json"
	FormatPlaywright = "playwright-json"
	FormatGoJSON     = "go-json"
)

// Formats lists every format name accepted by Parse, auto first.
var Formats = []string{FormatAuto, FormatJUnit, FormatJestJSON, FormatPlaywright, FormatGoJSON}

// Failure is one failed (or errored; never skipped) test case.
type Failure struct {
	Test    string // test case name (full name when the format provides one)
	Message string // first line of the failure message, possibly empty
	// Candidates are the paths that may identify the test file, best first, exactly as written in the results
	// (not normalized). The mapper tries them in order.
	Candidates []string
	// Package is the Go import path of the failed test's package (go-json only). Go test units are packages, so the
	// failure maps to the unit by import path instead of by file.
	Package string
}

// Result is the parsed content of one results file.
type Result struct {
	Source   string // path of the results file ("" when parsed from memory)
	Format   string // the concrete format (never "auto")
	Total    int    // test cases seen
	Failures []Failure
}

// Parse reads and parses a results file. format is one of Formats.
func Parse(path, format string) (Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	r, err := ParseBytes(b, format)
	r.Source = path
	if err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// ParseBytes parses results held in memory.
func ParseBytes(data []byte, format string) (Result, error) {
	if format == "" {
		format = FormatAuto
	}
	if format == FormatAuto {
		f, err := Detect(data)
		if err != nil {
			return Result{}, err
		}
		format = f
	}
	var (
		r   Result
		err error
	)
	switch format {
	case FormatJUnit:
		r, err = parseJUnit(data)
	case FormatJestJSON:
		r, err = parseJest(data)
	case FormatPlaywright:
		r, err = parsePlaywright(data)
	case FormatGoJSON:
		r, err = parseGoJSON(data)
	default:
		return Result{}, fmt.Errorf("unknown format %q (want %s)", format, strings.Join(Formats, "|"))
	}
	r.Format = format
	return r, err
}

// Detect guesses the format from the content: XML → junit; JSON lines whose first object has an Action key → go-json;
// JSON with testResults → jest-json; JSON with suites and config → playwright-json.
func Detect(data []byte) (string, error) {
	t := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	t = bytes.TrimSpace(t)
	if len(t) == 0 {
		return "", errors.New("empty results file")
	}
	switch t[0] {
	case '<':
		return FormatJUnit, nil
	case '{':
		if isGoJSON(t) {
			return FormatGoJSON, nil
		}
		keys, err := topKeys(t)
		if err != nil {
			return "", err
		}
		if keys["testResults"] {
			return FormatJestJSON, nil
		}
		if keys["suites"] && keys["config"] {
			return FormatPlaywright, nil
		}
		return "", errors.New("unrecognized JSON results (no testResults, or suites+config)")
	}
	return "", errors.New("unrecognized results format (neither XML nor JSON)")
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
