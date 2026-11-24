package evalkit

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qxbao/bgraph/internal/tree"
)

const swiftCommandResult = "__swift_test_command__"

// SwiftTestLoc links a Longtail-SWE name to source candidates in P.
type SwiftTestLoc struct {
	Name    string   `json:"name"`
	Form    string   `json:"form"`
	Files   []string `json:"files,omitempty"`
	InPatch bool     `json:"in_patch,omitempty"`
}

type swiftDeclaration struct {
	name, display string
	suites        []string
}

type swiftToken struct {
	value  string
	quoted bool
}

// swiftTokens retains string tokens for @Test display names, but discards comments and
// keeps the contents of quoted strings out of the declaration stream.
func swiftTokens(input string) []swiftToken {
	var tokens []swiftToken
	for len(input) > 0 {
		if strings.HasPrefix(input, "//") {
			_, input, _ = strings.Cut(input, "\n")
			continue
		}
		if strings.HasPrefix(input, "/*") {
			input = swiftSkipComment(input[2:])
			continue
		}
		if input[0] == 96 {
			if i := strings.IndexByte(input[1:], 96); i >= 0 {
				tokens = append(tokens, swiftToken{value: input[1 : i+1]})
				input = input[i+2:]
				continue
			}
		}
		pounds := len(input) - len(strings.TrimLeft(input, "#"))
		if pounds < len(input) && input[pounds] == '"' {
			literal, rest := swiftString(input, pounds)
			tokens = append(tokens, swiftToken{literal, true})
			input = rest
			continue
		}
		r, size := utf8.DecodeRuneInString(input)
		if unicode.IsSpace(r) {
			input = input[size:]
			continue
		}
		if r == '_' || unicode.IsLetter(r) {
			end := size
			for end < len(input) {
				r, n := utf8.DecodeRuneInString(input[end:])
				if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) {
					break
				}
				end += n
			}
			size = end
		}
		tokens = append(tokens, swiftToken{value: input[:size]})
		input = input[size:]
	}
	return tokens
}

func swiftSkipComment(input string) string {
	depth := 1
	for i := 0; i+1 < len(input); {
		switch input[i : i+2] {
		case "/*":
			depth++
			i += 2
		case "*/":
			depth--
			i += 2
			if depth == 0 {
				return input[i:]
			}
		default:
			i++
		}
	}
	return ""
}

func swiftString(input string, pounds int) (value, rest string) {
	quote := "\""
	if strings.HasPrefix(input[pounds:], "\"\"\"") {
		quote = "\"\"\""
	}
	close := quote + strings.Repeat("#", pounds)
	escape := "\\" + strings.Repeat("#", pounds)
	start := pounds + len(quote)
	for end := start; end < len(input); end++ {
		switch {
		case strings.HasPrefix(input[end:], close):
			value = input[start:end]
			if pounds == 0 && len(quote) == 1 {
				if decoded, err := strconv.Unquote(input[:end+len(close)]); err == nil {
					value = decoded
				}
			}
			return value, input[end+len(close):]
		case strings.HasPrefix(input[end:], escape):
			end += len(escape)
		}
	}
	return "", ""
}

// swiftAttribute reads an attribute name and its balanced argument list.
func swiftAttribute(tokens []swiftToken, start int) (name, display string, next int) {
	next = start + 2
	name = tokens[start+1].value
	for next+1 < len(tokens) && tokens[next].value == "." {
		name = tokens[next+1].value
		next += 2
	}
	if next >= len(tokens) || tokens[next].value != "(" {
		return name, "", next
	}
	if next+1 < len(tokens) && tokens[next+1].quoted {
		display = tokens[next+1].value
	}
	depth := 0
	for ; next < len(tokens); next++ {
		if tokens[next].quoted {
			continue
		}
		switch tokens[next].value {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return name, display, next + 1
			}
		}
	}
	return name, display, next
}

func swiftDeclarations(src []byte) []swiftDeclaration {
	tokens := swiftTokens(string(src))
	var decls []swiftDeclaration
	var suites []string
	var suiteDepths []int
	depth := 0
	suite, display := "", ""
	atTest := false
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].quoted {
			continue
		}
		switch tokens[i].value {
		case "@":
			name, arg, next := swiftAttribute(tokens, i)
			if name == "Test" {
				atTest, display = true, arg
			}
			i = next - 1
		case "extension", "class", "struct", "enum", "actor":
			suite = tokens[i+1].value
			atTest = false
		case "{":
			depth++
			if suite != "" {
				suites = append(suites, suite)
				suiteDepths = append(suiteDepths, depth)
				suite = ""
			}
			atTest = false
		case "}":
			if n := len(suiteDepths); n > 0 && suiteDepths[n-1] == depth {
				suites = suites[:n-1]
				suiteDepths = suiteDepths[:n-1]
			}
			depth--
			atTest = false
		case "func":
			name := tokens[i+1].value
			if atTest || strings.HasPrefix(name, "test") {
				d := swiftDeclaration{name: name, suites: append([]string(nil), suites...)}
				if atTest {
					d.display = display
					if d.display == "" && strings.ContainsAny(name, " \t") {
						d.display = name
					}
				}
				decls = append(decls, d)
			}
			atTest = false
			suite = ""
		case "let", "var", ";":
			atTest = false
		}
	}
	return decls
}

// Longtail-SWE incorporates native xUnit identifiers, source function paths, and
// display labels emitted by Swift Testing (sometimes followed by a parameter count).
var swiftName = regexp.MustCompile(`^(?:(.*)[ /.:])?([\pL_][\pL\pN_]*)((?:\([^()]*\))?)$`)
var swiftCases = regexp.MustCompile(` with [0-9]+ test cases?$`)

func swiftLabel(name string) string {
	label := strings.TrimSpace(swiftCases.ReplaceAllString(name, ""))
	if decoded, err := strconv.Unquote(label); err == nil {
		return decoded
	}
	return label
}

// LocateSwiftTests finds the test methods and Swift Testing annotations that can
// account for each result. A recognized suite qualifier disambiguates repeated methods.
func LocateSwiftTests(t tree.Tree, names []string, patch map[string]bool) ([]SwiftTestLoc, error) {
	functions := map[string][]string{}
	displays := map[string][]string{}
	suites := map[[2]string][]string{}
	for _, file := range t.Files() {
		if !swiftSource(file) {
			continue
		}
		src, err := t.Read(file)
		if err != nil {
			return nil, err
		}
		for _, d := range swiftDeclarations(src) {
			functions[d.name] = append(functions[d.name], file)
			if d.display != "" {
				displays[d.display] = append(displays[d.display], file)
			}
			for _, suite := range d.suites {
				key := [2]string{suite, d.name}
				suites[key] = append(suites[key], file)
			}
		}
	}
	out := make([]SwiftTestLoc, 0, len(names))
	for _, name := range names {
		l := SwiftTestLoc{Name: name, Form: "display"}
		label := swiftLabel(name)
		switch {
		case label == swiftCommandResult:
			l.Form = "command"
		default:
			l.Files = append(l.Files, displays[label]...)
			if match := swiftName.FindStringSubmatch(label); match != nil {
				l.Form = "function"
				files := functions[match[2]]
				qualifiers := strings.FieldsFunc(match[1], func(r rune) bool {
					return r == '.' || r == '/' || r == ':' || unicode.IsSpace(r)
				})
				if n := len(qualifiers); n > 0 {
					if qualified := suites[[2]string{qualifiers[n-1], match[2]}]; len(qualified) != 0 {
						files = qualified
					}
				}
				l.Files = append(l.Files, files...)
			}
		}
		l.Files = uniqSorted(l.Files)
		for _, file := range l.Files {
			l.InPatch = l.InPatch || patch[file]
		}
		out = append(out, l)
	}
	return out, nil
}

func swiftSource(file string) bool { return strings.HasSuffix(file, ".swift") }

// SwiftLocateTask creates the test overlay without running the candidate repository.
func SwiftLocateTask(ctx context.Context, repoDir, idxFile string, t Task) ([]SwiftTestLoc, error) {
	names, err := ParseF2P(t.F2P)
	if err != nil {
		return nil, err
	}
	ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		return nil, err
	}
	return swiftLocate(ctx, repoDir, p, names, patchFiles(t.TestPatch))
}

func swiftLocate(ctx context.Context, dir, commit string, names []string, touched map[string]bool) ([]SwiftTestLoc, error) {
	gt, err := newGitTreeWant(ctx, dir, commit, swiftSource)
	if err != nil {
		return nil, err
	}
	defer gt.Close()
	locs, err := LocateSwiftTests(gt, names, touched)
	if err != nil {
		return nil, fmt.Errorf("scan Swift tests: %w", err)
	}
	return locs, nil
}
