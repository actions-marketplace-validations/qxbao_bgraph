package audit

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
)

type xmlProblem struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

type xmlCase struct {
	Name      string       `xml:"name,attr"`
	Classname string       `xml:"classname,attr"`
	File      string       `xml:"file,attr"`
	Failures  []xmlProblem `xml:"failure"`
	Errors    []xmlProblem `xml:"error"`
}

// xmlSuite decodes both <testsuites> and <testsuite> roots (a <testsuites> has only nested suites).
type xmlSuite struct {
	Name   string     `xml:"name,attr"`
	File   string     `xml:"file,attr"`
	Suites []xmlSuite `xml:"testsuite"`
	Cases  []xmlCase  `xml:"testcase"`
}

func parseJUnit(data []byte) (Result, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var root xmlSuite
	if err := dec.Decode(&root); err != nil && err != io.EOF {
		return Result{}, fmt.Errorf("junit: %w", err)
	}
	var r Result
	walkSuite(root, &r)
	return r, nil
}

func walkSuite(s xmlSuite, r *Result) {
	for _, c := range s.Cases {
		r.Total++
		probs := append(append([]xmlProblem(nil), c.Failures...), c.Errors...)
		if len(probs) == 0 {
			continue // passed or skipped
		}
		msg := probs[0].Message
		if msg == "" {
			msg = probs[0].Text
		}
		name := c.Name
		if c.Classname != "" && c.Name != "" {
			name = c.Classname + " > " + c.Name
		}
		r.Failures = append(r.Failures, Failure{Test: name, Message: clip(msg),
			Candidates: nonEmpty(c.File, s.File, c.Classname, s.Name)})
	}
	for _, sub := range s.Suites {
		// A nested suite without its own file attribute inherits the parent's.
		if sub.File == "" {
			sub.File = s.File
		}
		walkSuite(sub, r)
	}
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
