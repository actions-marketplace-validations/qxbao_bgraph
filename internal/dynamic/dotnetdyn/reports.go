package dotnetdyn

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Coverage is what one Cobertura report of Microsoft.CodeCoverage says about the source files of the instrumented modules.
type Coverage struct {
	// Hit: source file (as the report names it) → it has at least one line with hits > 0.
	Hit map[string]bool
	// Lines: source file → every line the report lists for it (instrumented lines, hit or not), sorted and unique.
	Lines map[string][]int
	// Classes: class name (as the report names it, nested classes joined with '.' or '+') → it has a hit line.
	Classes map[string]bool
	// ClassFiles: class name -> the files in which it has hit lines.
	ClassFiles map[string][]string
}

type cobLine struct {
	Number int   `xml:"number,attr"`
	Hits   int64 `xml:"hits,attr"`
}

type cobClass struct {
	Name     string    `xml:"name,attr"`
	Filename string    `xml:"filename,attr"`
	Lines    []cobLine `xml:"lines>line"`
	Methods  []struct {
		Lines []cobLine `xml:"lines>line"`
	} `xml:"methods>method"`
}

type cobReport struct {
	Packages []struct {
		Classes []cobClass `xml:"classes>class"`
	} `xml:"packages>package"`
}

// ParseCobertura reads a Cobertura report.
func ParseCobertura(r io.Reader) (Coverage, error) {
	var rep cobReport
	if err := xml.NewDecoder(r).Decode(&rep); err != nil {
		return Coverage{}, fmt.Errorf("cobertura: %w", err)
	}
	c := Coverage{Hit: map[string]bool{}, Lines: map[string][]int{}, Classes: map[string]bool{}, ClassFiles: map[string][]string{}}
	for _, p := range rep.Packages {
		for _, cl := range p.Classes {
			f := cl.Filename
			if f == "" {
				continue
			}
			hit := false
			add := func(ls []cobLine) {
				for _, l := range ls {
					if l.Number > 0 {
						c.Lines[f] = append(c.Lines[f], l.Number)
					}
					if l.Hits > 0 {
						hit = true
					}
				}
			}
			add(cl.Lines)
			for _, m := range cl.Methods {
				add(m.Lines)
			}
			if _, ok := c.Lines[f]; !ok {
				c.Lines[f] = nil // listed without lines: still part of the instrumented universe
			}
			if hit {
				c.Hit[f] = true
				c.Classes[cl.Name] = true
				if !slices.Contains(c.ClassFiles[cl.Name], f) {
					c.ClassFiles[cl.Name] = append(c.ClassFiles[cl.Name], f)
				}
			} else if _, ok := c.Classes[cl.Name]; !ok {
				c.Classes[cl.Name] = false
			}
		}
	}
	for f, ls := range c.Lines {
		slices.Sort(ls)
		c.Lines[f] = slices.Compact(ls)
	}
	return c, nil
}

// ParseCoberturaFile reads a Cobertura report file.
func ParseCoberturaFile(file string) (Coverage, error) {
	f, err := os.Open(file)
	if err != nil {
		return Coverage{}, err
	}
	defer f.Close()
	return ParseCobertura(f)
}

// TestOutcome is the verdict of one test class in a TRX report.
type TestOutcome struct {
	Tests  int // results reported for the class
	Failed int // results whose outcome is neither Passed nor NotExecuted/Inconclusive-skips
}

type trxReport struct {
	Definitions []struct {
		ID     string `xml:"id,attr"`
		Method struct {
			ClassName string `xml:"className,attr"`
		} `xml:"TestMethod"`
	} `xml:"TestDefinitions>UnitTest"`
	Results []trxResult `xml:"Results>UnitTestResult"`
	Summary struct {
		Outcome  string `xml:"outcome,attr"`
		Counters struct {
			Error               int `xml:"error,attr"`
			Timeout             int `xml:"timeout,attr"`
			Aborted             int `xml:"aborted,attr"`
			Disconnected        int `xml:"disconnected,attr"`
			PassedButRunAborted int `xml:"passedButRunAborted,attr"`
		} `xml:"Counters"`
		RunInfos []struct {
			Text string `xml:"Text"`
		} `xml:"RunInfos>RunInfo"`
	} `xml:"ResultSummary"`
}

// Aborted returns why the run did not complete ("" when it did): a crashed or aborted test host still leaves a TRX report
// with the results so far, so a missing class would look absent rather than unrun.
func (rep *trxReport) aborted() string {
	sum := rep.Summary
	switch sum.Outcome {
	case "", "Completed", "Passed", "Failed", "Warning":
	default:
		return "run outcome " + sum.Outcome
	}
	c := sum.Counters
	if c.Error+c.Timeout+c.Aborted+c.Disconnected+c.PassedButRunAborted > 0 {
		return fmt.Sprintf("run counters: %d errors, %d timeouts, %d aborted, %d disconnected", c.Error, c.Timeout, c.Aborted+c.PassedButRunAborted, c.Disconnected)
	}
	for _, ri := range sum.RunInfos {
		t := strings.ToLower(ri.Text)
		if strings.Contains(t, "aborted") || strings.Contains(t, "crashed") {
			return "run aborted: " + firstLine(ri.Text)
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

type trxResult struct {
	TestID  string      `xml:"testId,attr"`
	Outcome string      `xml:"outcome,attr"`
	Inner   []trxResult `xml:"InnerResults>UnitTestResult"`
}

// ParseTRX reads a VSTest TRX report: top-level class (nested classes, "Outer+Inner", count as their outer class) → outcome.
// A run that did not complete (crashed or aborted host) is an error.
func ParseTRX(r io.Reader) (map[string]TestOutcome, error) {
	var rep trxReport
	if err := xml.NewDecoder(r).Decode(&rep); err != nil {
		return nil, fmt.Errorf("trx: %w", err)
	}
	if why := rep.aborted(); why != "" {
		return nil, fmt.Errorf("trx: %s", why)
	}
	class := map[string]string{}
	out := map[string]TestOutcome{}
	for _, d := range rep.Definitions {
		c := TopLevelClass(d.Method.ClassName)
		class[d.ID] = c
		if c != "" {
			if _, ok := out[c]; !ok {
				out[c] = TestOutcome{}
			}
		}
	}
	for _, res := range rep.Results {
		c := class[res.TestID]
		if c == "" {
			continue
		}
		o := out[c]
		o.Tests++
		if failedOutcome(res) {
			o.Failed++
		}
		out[c] = o
	}
	return out, nil
}

func failedOutcome(r trxResult) bool {
	switch r.Outcome {
	case "Passed", "NotExecuted", "Inconclusive", "":
	default:
		return true
	}
	for _, in := range r.Inner {
		if failedOutcome(in) {
			return true
		}
	}
	return false
}

// ParseTRXFile reads a TRX report file.
func ParseTRXFile(file string) (map[string]TestOutcome, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseTRX(f)
}

// TopLevelClass cuts a VSTest class name at the first nested-class separator ('+', or '/' as some adapters write it) and drops
// a generic arity suffix of the outer class.
func TopLevelClass(c string) string {
	if i := strings.IndexAny(c, "+/"); i >= 0 {
		c = c[:i]
	}
	if i := strings.IndexByte(c, '`'); i >= 0 {
		if _, err := strconv.Atoi(c[i+1:]); err == nil {
			c = c[:i]
		}
	}
	return c
}

// Ranges encodes sorted unique line numbers compactly ("1-5,7,9-12").
func Ranges(ls []int) string {
	var b strings.Builder
	for i := 0; i < len(ls); {
		j := i
		for j+1 < len(ls) && ls[j+1] == ls[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(ls[i]))
		if j > i {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(ls[j]))
		}
		i = j + 1
	}
	return b.String()
}

// ParseRanges decodes Ranges; ok=false on malformed input.
func ParseRanges(s string) ([]int, bool) {
	if s == "" {
		return nil, true
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(part, "-")
		x, err := strconv.Atoi(a)
		if err != nil || x <= 0 {
			return nil, false
		}
		y := x
		if isRange {
			if y, err = strconv.Atoi(b); err != nil || y < x {
				return nil, false
			}
		}
		for n := x; n <= y; n++ {
			out = append(out, n)
		}
	}
	return out, true
}
