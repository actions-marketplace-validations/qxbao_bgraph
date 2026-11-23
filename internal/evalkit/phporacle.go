package evalkit

import (
	"context"
	"encoding/xml"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"

	phpscan "github.com/qxbao/bgraph/internal/scan/php"
	"github.com/qxbao/bgraph/internal/tree"
)

// PHPTestLoc locates a PHPUnit class, including all candidates if the name is ambiguous.
type PHPTestLoc struct {
	Name    string   `json:"name"`
	Files   []string `json:"files,omitempty"`
	InPatch bool     `json:"in_patch,omitempty"`
}

const phpIdentifier = "[a-zA-Z_\\x{80}-\\x{10FFFF}][a-zA-Z0-9_\\x{80}-\\x{10FFFF}]*"

var phpDeclaration = regexp.MustCompile(`(?im)^[\t ]*(?:<\?php[\t ]*)?(?:namespace[\t ]*(` + phpIdentifier + `(?:\\` + phpIdentifier + `)*)?[\t ]*[;{]|(?:(?:abstract|final|readonly)[\t ]+)*class[\t ]+(` + phpIdentifier + `))`)
var phpClassCache sync.Map // Git blob OID -> []string

func phpClassName(test string) string {
	class, _, ok := strings.Cut(test, "::")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(class), `\`))
}

func phpClassBase(class string) string { return class[strings.LastIndexByte(class, '\\')+1:] }

func phpClasses(contents []byte) []string {
	var classes []string
	var ns string
	for _, match := range phpDeclaration.FindAllSubmatch(contents, -1) {
		if len(match[2]) == 0 {
			ns = string(match[1])
			continue
		}
		class := string(match[2])
		if ns != "" {
			class = ns + `\` + class
		}
		classes = append(classes, strings.ToLower(class))
	}
	return classes
}

func phpScanCandidate(file string, basenames map[string]bool) bool {
	lower := strings.ToLower(file)
	if !strings.HasSuffix(lower, ".php") {
		return false
	}
	base := path.Base(lower)
	if basenames[strings.TrimSuffix(base, ".php")] || strings.HasSuffix(base, "test.php") {
		return true
	}
	for _, segment := range strings.Split(path.Dir(lower), "/") {
		if segment == "test" || segment == "tests" || segment == "spec" || segment == "specs" || segment == "phpunit" {
			return true
		}
	}
	return false
}

// LocatePHPTests maps PHPUnit Class::method (including data sets and inherited
// test methods) to the class file in the patched tree.
func LocatePHPTests(t tree.Tree, names []string, touched map[string]bool) ([]PHPTestLoc, error) {
	basenames := map[string]bool{}
	for _, name := range names {
		if class := phpClassName(name); class != "" {
			basenames[phpClassBase(class)] = true
		}
	}
	full, short := map[string][]string{}, map[string][]string{}
	for _, file := range t.Files() {
		if !phpScanCandidate(file, basenames) {
			continue
		}
		oid := t.Key(file)
		cached, found := phpClassCache.Load(oid)
		if !found || oid == "" {
			body, err := t.Read(file)
			if err != nil {
				return nil, fmt.Errorf("read PHP class %s: %w", file, err)
			}
			cached = phpClasses(body)
			if oid != "" {
				phpClassCache.Store(oid, cached)
			}
		}
		for _, class := range cached.([]string) {
			base := phpClassBase(class)
			if basenames[base] {
				full[class] = append(full[class], file)
				short[base] = append(short[base], file)
			}
		}
	}
	var out []PHPTestLoc
	for _, test := range names {
		class := phpClassName(test)
		files := full[class]
		if class != "" && !strings.Contains(class, `\`) {
			files = short[class]
		}
		loc := PHPTestLoc{Name: test, Files: uniqSorted(files)}
		for _, file := range files {
			loc.InPatch = loc.InPatch || touched[file]
		}
		out = append(out, loc)
	}
	return out, nil
}

func phpConfigFile(f string) bool {
	switch path.Base(f) {
	case "phpunit.xml", "phpunit.xml.dist", "phpunit.dist.xml":
		return true
	default:
		return false
	}
}

func phpOracleTree(ctx context.Context, dir, p string, names []string, evaluate bool) (*gitTree, error) {
	gt, err := newGitTreeWant(ctx, dir, p, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	basenames := map[string]bool{}
	for _, name := range names {
		basenames[phpClassBase(phpClassName(name))] = true
	}
	err = prefetchBlobs(ctx, dir, gt, func(f string) bool {
		if evaluate && (phpConfigFile(f) || phpscan.Source(f) || goPrefetchWant(f) ||
			path.Base(f) == "composer.json" || path.Base(f) == "composer.lock") {
			return true
		}
		if !phpScanCandidate(f, basenames) {
			return false
		}
		_, cached := phpClassCache.Load(gt.oid[f])
		return !cached
	})
	if err != nil {
		gt.Close()
		return nil, err
	}
	return gt, nil
}

// PHPLocateTask builds P and locates each FAIL_TO_PASS name without a checkout.
func PHPLocateTask(ctx context.Context, repoDir, idxFile string, t Task) ([]PHPTestLoc, error) {
	names, err := ParseF2P(t.F2P)
	if err != nil {
		return nil, err
	}
	p := t.BaseCommit
	if strings.TrimSpace(t.TestPatch) != "" {
		ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
		ib.allowPromisedBlobs = true
		defer ib.Close()
		p, err = ib.Apply(ctx, t.TestPatch, "eval: test patch")
		if err != nil {
			return nil, fmt.Errorf("test_patch: %w", err)
		}
	}
	gt, err := phpOracleTree(ctx, repoDir, p, names, false)
	if err != nil {
		return nil, err
	}
	defer gt.Close()
	touched := map[string]bool{}
	if p != t.BaseCommit {
		paths, err := git(ctx, repoDir, "diff", "--name-only", "--no-renames", "-z", t.BaseCommit, p)
		if err != nil {
			return nil, err
		}
		for _, f := range strings.Split(paths, "\x00") {
			if f != "" {
				touched[f] = true
			}
		}
	}
	return LocatePHPTests(gt, names, touched)
}

type phpSuite struct {
	Directories []struct {
		Path   string `xml:",chardata"`
		Suffix string `xml:"suffix,attr"`
	} `xml:"directory"`
	Files   []string `xml:"file"`
	Exclude []string `xml:"exclude"`
}

type phpProjectConfig struct {
	dir    string
	suites []phpSuite
}

func phpNearestProject(dir string, projects map[string]bool) (string, bool) {
	for d := dir; ; d = path.Dir(d) {
		if projects[d] {
			return d, true
		}
		if d == "." {
			return "", false
		}
	}
}

func phpContains(dir, file string) bool {
	if !strings.ContainsAny(dir, "*?[") {
		return dir == "." || file == dir || strings.HasPrefix(file, dir+"/")
	}
	for candidate := file; ; candidate = path.Dir(candidate) {
		if ok, _ := path.Match(dir, candidate); ok {
			return true
		}
		if candidate == "." {
			return false
		}
	}
}

func phpSuiteContains(cfg phpProjectConfig, suite phpSuite, file string) bool {
	for _, exclude := range suite.Exclude {
		if phpContains(path.Join(cfg.dir, strings.TrimSpace(exclude)), file) {
			return false
		}
	}
	for _, entry := range suite.Files {
		if found, _ := path.Match(path.Join(cfg.dir, strings.TrimSpace(entry)), file); found {
			return true
		}
	}
	for _, entry := range suite.Directories {
		dir := path.Join(cfg.dir, strings.TrimSpace(entry.Path))
		suffix := entry.Suffix
		if suffix == "" {
			suffix = "Test.php"
		}
		if phpContains(dir, file) && strings.HasSuffix(file, suffix) {
			return true
		}
	}
	return false
}

// phpTestOwners derives projects using Composer roots and PHPUnit suite entries
// from the tree. It avoids the selector's graph so an omitted unit stays visible.
func phpTestOwners(t tree.Tree, files []string) (map[string][]string, error) {
	projects := map[string]bool{}
	for _, f := range t.Files() {
		if path.Base(f) == "composer.json" {
			projects[path.Dir(f)] = true
		}
	}
	var configs []phpProjectConfig
	seen := map[string]bool{}
	for _, f := range t.Files() {
		if !phpConfigFile(f) {
			continue
		}
		dir := path.Dir(f)
		if seen[dir] ||
			(path.Base(f) != "phpunit.xml" && t.Has(path.Join(dir, "phpunit.xml"))) ||
			(path.Base(f) == "phpunit.xml.dist" && t.Has(path.Join(dir, "phpunit.dist.xml"))) {
			continue
		}
		seen[dir] = true
		projects[dir] = true
		body, err := t.Read(f)
		if err != nil {
			return nil, err
		}
		var spec struct {
			Suites []phpSuite `xml:"testsuites>testsuite"`
			Legacy []phpSuite `xml:"testsuite"`
		}
		if err := xml.Unmarshal(body, &spec); err != nil {
			continue
		}
		configs = append(configs, phpProjectConfig{dir: dir, suites: append(spec.Suites, spec.Legacy...)})
	}
	result := make(map[string][]string, len(files))
	for _, file := range files {
		var units []string
		for _, cfg := range configs {
			for _, suite := range cfg.suites {
				if phpSuiteContains(cfg, suite, file) {
					units = append(units, cfg.dir+"::php")
					break
				}
			}
		}
		if d, ok := phpNearestProject(path.Dir(file), projects); ok && len(units) == 0 {
			units = append(units, d+"::php")
		}
		result[file] = uniqSorted(units)
	}
	return result, nil
}
