// Package audit parses test-result files (JUnit XML, Jest/Vitest JSON, Playwright JSON) into a flat list of failed
// test cases, and maps each failure to a repo-relative test file (docs/cli.md, audit).
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
)

// Failure is one failed (or errored; never skipped) test case.
type Failure struct {
	Test    string // test case name (full name when the format provides one)
	Message string // first line of the failure message, possibly empty
	// Candidates are the paths that may identify the test file, best first, exactly as written in the results
	// (not normalized). The mapper tries them in order.
	Candidates []string
}

// Result is the parsed content of one results file.
type Result struct {
	Source   string // path of the results file ("" when parsed from memory)
	Format   string // the concrete format (never "auto")
	Total    int    // test cases seen
	Failures []Failure
}

// Parse reads and parses a results file. format is auto|junit|jest-json|playwright-json.
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
	default:
		return Result{}, fmt.Errorf("unknown format %q (want auto|junit|jest-json|playwright-json)", format)
	}
	r.Format = format
	return r, err
}

// Detect guesses the format from the content: XML → junit; JSON with testResults → jest-json; JSON with suites and
// config → playwright-json.
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
