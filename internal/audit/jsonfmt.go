package audit

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

func topKeys(b []byte) (map[string]bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out, nil
}

// looseString decodes a JSON value that is a string in the reporter's contract but not always in practice: Jest copies
// the raw title of a test.each case, so a title can be an array or a number. Non-strings keep their JSON text.
type looseString string

func (s *looseString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = looseString(str)
		return nil
	}
	*s = looseString(b)
	return nil
}

// Jest and Vitest's json reporters share one shape.
type jestFile struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Assertions []struct {
		FullName looseString   `json:"fullName"`
		Title    looseString   `json:"title"`
		Status   string        `json:"status"`
		Messages []looseString `json:"failureMessages"`
	} `json:"assertionResults"`
}

func parseJest(data []byte) (Result, error) {
	var doc struct {
		TestResults []jestFile `json:"testResults"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Result{}, fmt.Errorf("jest-json: %w", err)
	}
	var r Result
	// Vitest with several projects (or typecheck on) lists the same file once per project, so the same failure can
	// appear several times: report it once.
	seen := map[[2]string]bool{}
	add := func(file, test, msg string) {
		k := [2]string{file, test}
		if seen[k] {
			return
		}
		seen[k] = true
		r.Failures = append(r.Failures, Failure{Test: test, Message: clip(msg), Candidates: nonEmpty(file)})
	}
	for _, f := range doc.TestResults {
		failedHere := 0
		for _, a := range f.Assertions {
			r.Total++
			if a.Status != "failed" {
				continue
			}
			failedHere++
			name := string(a.FullName)
			if name == "" {
				name = string(a.Title)
			}
			msg := ""
			if len(a.Messages) > 0 {
				msg = string(a.Messages[0])
			}
			add(f.Name, name, msg)
		}
		// A suite that failed without any failed assertion (syntax error, import error, setup crash) is a failure too.
		if f.Status == "failed" && failedHere == 0 {
			add(f.Name, "(test suite failed to run)", f.Message)
		}
	}
	return r, nil
}

type pwSuite struct {
	Title  string    `json:"title"`
	File   string    `json:"file"`
	Suites []pwSuite `json:"suites"`
	Specs  []struct {
		Title string `json:"title"`
		File  string `json:"file"`
		OK    bool   `json:"ok"`
		Tests []struct {
			Status  string `json:"status"`
			Results []struct {
				Status string `json:"status"`
				Error  struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"results"`
		} `json:"tests"`
	} `json:"specs"`
}

func parsePlaywright(data []byte) (Result, error) {
	var doc struct {
		Config struct {
			RootDir string `json:"rootDir"`
		} `json:"config"`
		Suites []pwSuite `json:"suites"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Result{}, fmt.Errorf("playwright-json: %w", err)
	}
	var r Result
	root := strings.ReplaceAll(doc.Config.RootDir, "\\", "/")
	var walk func(s pwSuite, file, titles string)
	walk = func(s pwSuite, file, titles string) {
		if s.File != "" {
			file = s.File
		}
		if s.Title != "" && s.Title != file {
			titles += s.Title + " > "
		}
		for _, sp := range s.Specs {
			r.Total++
			failed := !sp.OK
			msg := ""
			for _, t := range sp.Tests {
				if t.Status == "unexpected" {
					failed = true
				}
				for _, res := range t.Results {
					if (res.Status == "failed" || res.Status == "timedOut") && msg == "" {
						msg = res.Error.Message
					}
				}
			}
			if !failed {
				continue
			}
			f := sp.File
			if f == "" {
				f = file
			}
			var cands []string
			if f != "" && root != "" && !isAbs(strings.ReplaceAll(f, "\\", "/")) {
				// Playwright reports spec files relative to config.rootDir.
				cands = append(cands, path.Join(root, strings.ReplaceAll(f, "\\", "/")))
			}
			cands = append(cands, nonEmpty(f)...)
			r.Failures = append(r.Failures, Failure{Test: titles + sp.Title, Message: clip(msg), Candidates: cands})
		}
		for _, sub := range s.Suites {
			walk(sub, file, titles)
		}
	}
	for _, s := range doc.Suites {
		walk(s, "", "")
	}
	return r, nil
}
