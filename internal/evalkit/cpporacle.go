package evalkit

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// CPPTestLoc records source files and literal native registrations that can
// account for one benchmark result. Registrations are scoped to their manifests.
type CPPTestLoc struct {
	Name          string            `json:"name"`
	Files         []string          `json:"files,omitempty"`
	Registrations []CPPRegistration `json:"registrations,omitempty"`
	InPatch       bool              `json:"in_patch"`
}

type CPPRegistration struct {
	System string `json:"system"`
	File   string `json:"file"`
	Name   string `json:"name"`
}

var (
	cppGTest        = regexp.MustCompile(`\b(?:TYPED_TEST(?:_P)?|TEST(?:_F|_P)?)\s*\(\s*([A-Za-z_]\w*)\s*,\s*([A-Za-z_]\w*)\s*\)`)
	cppCatch        = regexp.MustCompile(`\b(?:TEST_CASE|SCENARIO|TEMPLATE_TEST_CASE(?:_SIG|_METHOD)?)\s*\(\s*("(?:\\.|[^"\\])*")`)
	cppCTest        = regexp.MustCompile(`(?im)^\s*add_test\s*\(\s*(?:NAME\s+)?(?:"([^"\r\n]+)"|([^\s()]+))`)
	cppMeson        = regexp.MustCompile(`(?m)^\s*test\s*\(\s*'([^'\r\n]+)'\s*,`)
	cppJSONStandard = regexp.MustCompile(`_(?:cpp[0-9]+|default|all|valgrind|cxx[0-9]+)(?:_.*)?$`)
	cppSourceToken  = regexp.MustCompile(`(?s)//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'`)
)

var cppZstdAliases = map[string][]string{
	"fuzztest":      {"fuzzer", "test-fuzzer"},
	"zstream_tests": {"zstreamtest", "test-zstream-1", "test-zstream-2", "test-zstream-3"},
	"playtest":      {"playtests", "test-zstd"},
}

func cppOracleManifest(file string) string {
	switch path.Base(file) {
	case "CMakeLists.txt":
		return "cmake"
	case "meson.build":
		return "meson"
	}
	return ""
}

func cppOracleSource(file string) bool {
	switch strings.ToLower(path.Ext(file)) {
	case ".c", ".cc", ".cpp", ".cxx", ".c++", ".sh":
		return true
	}
	return false
}

func cppTestDirectory(file string) bool {
	for _, component := range strings.Split(path.Dir(file), "/") {
		switch strings.ToLower(component) {
		case "test", "tests", "selftest", "testing":
			return true
		}
	}
	return false
}

// cppSourceMasks removes comments and optionally literals while retaining
// offsets, so looking up labels never matches source text inside a comment.
func cppSourceMasks(src string) (withoutComments, withoutLiterals string) {
	comments := []byte(src)
	tokens := []byte(src)
	for _, span := range cppSourceToken.FindAllStringIndex(src, -1) {
		for i := span[0]; i < span[1]; i++ {
			if src[i] != '\n' {
				tokens[i] = ' '
				if src[span[0]] == '/' {
					comments[i] = ' '
				}
			}
		}
	}
	return string(comments), string(tokens)
}

func cppSourceLabels(repo, file, src string) []string {
	var labels []string
	if cppTestDirectory(file) {
		labels = append(labels, file, path.Base(file), strings.TrimSuffix(path.Base(file), path.Ext(file)))
		if strings.EqualFold(repo, "nlohmann/json") && strings.HasPrefix(path.Base(file), "unit-") {
			labels = append(labels, "test-"+strings.TrimPrefix(strings.TrimSuffix(path.Base(file), path.Ext(file)), "unit-"))
		}
	}
	if path.Ext(file) == ".sh" {
		return labels
	}
	noComments, code := cppSourceMasks(src)
	for _, match := range cppGTest.FindAllStringSubmatch(code, -1) {
		labels = append(labels, match[1]+"."+match[2])
	}
	for _, match := range cppCatch.FindAllStringSubmatchIndex(noComments, -1) {
		if code[match[0]] == ' ' {
			continue
		}
		if label, err := strconv.Unquote(noComments[match[2]:match[3]]); err == nil {
			labels = append(labels, label)
		}
	}
	return labels
}

func cppRegistrationNames(file, src string) []CPPRegistration {
	var result []CPPRegistration
	system := cppOracleManifest(file)
	expression := cppCTest
	if system == "meson" {
		expression = cppMeson
	}
	for _, match := range expression.FindAllStringSubmatch(src, -1) {
		name := match[1]
		if name == "" {
			name = match[2]
		}
		if name != "" && !strings.ContainsAny(name, "$@#") {
			result = append(result, CPPRegistration{System: system, File: file, Name: name})
		}
	}
	return result
}

// LocateCPPTests uses exact macro identities, native registration literals,
// and test-entry file names. All matches survive, including ambiguous matches.
func LocateCPPTests(t tree.Tree, repo string, names []string, touched map[string]bool) ([]CPPTestLoc, error) {
	files := map[string][]string{}
	registrations := map[string][]CPPRegistration{}
	for _, file := range t.Files() {
		manifest := cppOracleManifest(file) != ""
		if !manifest && !cppOracleSource(file) {
			continue
		}
		src, err := t.Read(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		if manifest {
			for _, reg := range cppRegistrationNames(file, string(src)) {
				key := strings.ToLower(reg.Name)
				registrations[key] = append(registrations[key], reg)
			}
			continue
		}
		for _, label := range cppSourceLabels(repo, file, string(src)) {
			key := strings.ToLower(label)
			files[key] = append(files[key], file)
		}
	}
	out := make([]CPPTestLoc, 0, len(names))
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		loc := CPPTestLoc{Name: name, Registrations: append([]CPPRegistration(nil), registrations[key]...)}
		if strings.EqualFold(repo, "facebook/zstd") {
			// These Make targets invoke an executable or script also used
			// by some historical CTest or Meson configurations.
			for _, alias := range cppZstdAliases[key] {
				loc.Registrations = append(loc.Registrations, registrations[alias]...)
			}
		}
		matches := files[key]
		if strings.EqualFold(repo, "nlohmann/json") {
			matches = append(append([]string{}, matches...), files[cppJSONStandard.ReplaceAllString(key, "")]...)
			if entry, _, section := strings.Cut(key, " > "); section {
				// A section label can occur under many test cases. Require the
				// exact section to be present in the same file as its test case.
				for _, file := range files[entry] {
					src, err := t.Read(file)
					if err != nil {
						return nil, err
					}
					if cppContainsSection(string(src), strings.TrimPrefix(key, entry+" > ")) {
						matches = append(matches, file)
					}
				}
			}
		}
		loc.Files = uniqSorted(matches)
		for _, file := range loc.Files {
			loc.InPatch = loc.InPatch || touched[file]
		}
		out = append(out, loc)
	}
	return out, nil
}

var cppSection = regexp.MustCompile(`\b(?:SECTION|GIVEN|WHEN|THEN)\s*\(\s*("(?:\\.|[^"\\])*")`)

func cppContainsSection(src, section string) bool {
	noComments, code := cppSourceMasks(src)
	for _, match := range cppSection.FindAllStringSubmatchIndex(noComments, -1) {
		if code[match[0]] == ' ' {
			continue
		}
		if value, err := strconv.Unquote(noComments[match[2]:match[3]]); err == nil && strings.EqualFold(value, section) {
			return true
		}
	}
	return false
}

func CPPLocateTask(ctx context.Context, dir, idxFile string, task Task) ([]CPPTestLoc, error) {
	names, err := ParseF2P(task.F2P)
	if err != nil {
		return nil, err
	}
	ib := newIndexBuilder(dir, idxFile, task.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, task.TestPatch, "eval: test patch")
	if err != nil {
		return nil, err
	}
	t, err := newGitTreeWant(ctx, dir, p, cppOracleInput)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	return LocateCPPTests(t, task.Repo, names, patchFiles(task.TestPatch))
}

func cppOracleInput(file string) bool {
	return cppOracleSource(file) || cppOracleManifest(file) != ""
}
