package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// goEvent is one line of `go test -json` (cmd/test2json). Build events of Go ≥ 1.24 carry ImportPath instead of Package
// and are ignored: a failed build also ends in a package-level fail event.
type goEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// parseGoJSON reads `go test -json` output. A failed test maps to its package (Failure.Package): the Go unit is the
// package, never a file. Only leaf failures are reported (a parent test fails whenever a subtest does). A package that
// failed without any failed test (build failure, panic in TestMain or init, timeout, os.Exit) is one failure named
// "(package failed)". Lines that are not JSON (go build errors when stderr is merged into stdout) are skipped; output that
// holds no event at all is an error.
func parseGoJSON(data []byte) (Result, error) {
	var r Result
	var order []goKey
	failed := map[goKey]bool{}
	msg := map[goKey]string{}
	events := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev goEvent
		if json.Unmarshal(line, &ev) != nil || ev.Action == "" {
			continue
		}
		events++
		if ev.Package == "" {
			continue
		}
		k := goKey{ev.Package, ev.Test}
		switch ev.Action {
		case "output":
			// The message is the test's last output line before its result (t.Fatal's reason, usually).
			if ev.Test != "" && !failed[k] {
				if s := strings.TrimSpace(ev.Output); s != "" && !strings.HasPrefix(s, "=== ") && !strings.HasPrefix(s, "--- ") {
					msg[k] = s
				}
			}
		case "pass", "skip":
			if ev.Test != "" {
				r.Total++
			}
		case "fail":
			if ev.Test != "" {
				r.Total++
			}
			if !failed[k] {
				failed[k] = true
				order = append(order, k)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("go-json: %w", err)
	}
	if events == 0 {
		return Result{}, errors.New("go-json: no test2json event")
	}
	// Leaf failures only; the package-level fail only when none of its tests failed.
	pkgHasTest := map[string]bool{}
	for _, k := range order {
		if k.test != "" {
			pkgHasTest[k.pkg] = true
		}
	}
	for _, k := range order {
		if k.test == "" {
			if !pkgHasTest[k.pkg] {
				r.Failures = append(r.Failures, Failure{Test: "(package failed)", Package: k.pkg})
			}
			continue
		}
		if hasFailedChild(failed, k.pkg, k.test) {
			continue
		}
		r.Failures = append(r.Failures, Failure{Test: k.test, Message: clip(msg[k]), Package: k.pkg})
	}
	return r, nil
}

type goKey struct{ pkg, test string }

func hasFailedChild(failed map[goKey]bool, pkg, test string) bool {
	for k := range failed {
		if k.pkg == pkg && strings.HasPrefix(k.test, test+"/") {
			return true
		}
	}
	return false
}

// isGoJSON reports whether the first JSON value of data looks like a test2json event.
func isGoJSON(data []byte) bool {
	line := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		line = data[:i]
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(line), &m) != nil {
		return false
	}
	_, ok := m["Action"]
	return ok
}
