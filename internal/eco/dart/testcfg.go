package dart

import (
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/tree"
)

// testCfg is what a package's dart_test.yaml says about which files are tests.
type testCfg struct {
	paths    []string // top-level `paths:` (relative to the package); nil: the default (test/)
	filename *glob.Pattern
	// opaque is non-empty when the test file set cannot be read from the file (it does not parse, includes another file,
	// or sets paths/filename below the top level: presets, tags, platforms). The package then gets a whole unit.
	opaque string
}

var defaultFilename = glob.MustCompile("*_test.dart")

func loadTestCfg(t tree.Tree, dir string) testCfg {
	c := testCfg{filename: defaultFilename}
	file := path.Join(dir, "dart_test.yaml")
	src, err := t.Read(file)
	if err != nil {
		if t.Has(file) {
			c.opaque = "dart_test.yaml cannot be read: " + err.Error()
		}
		return c
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		c.opaque = "dart_test.yaml does not parse: " + err.Error()
		return c
	}
	if len(doc.Content) == 0 {
		return c
	}
	top := doc.Content[0]
	if top.Kind == yaml.ScalarNode && top.Tag == "!!null" {
		return c
	}
	if top.Kind != yaml.MappingNode {
		c.opaque = "dart_test.yaml is not a mapping"
		return c
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		k, v := top.Content[i], top.Content[i+1]
		switch k.Value {
		case "paths":
			switch v.Kind {
			case yaml.SequenceNode:
				for _, e := range v.Content {
					if e.Kind != yaml.ScalarNode {
						c.opaque = "dart_test.yaml paths has a non-string entry"
						return c
					}
					c.paths = append(c.paths, e.Value)
				}
			case yaml.ScalarNode:
				c.paths = append(c.paths, v.Value)
			default:
				c.opaque = "dart_test.yaml paths is not a list"
				return c
			}
		case "filename":
			if v.Kind != yaml.ScalarNode {
				c.opaque = "dart_test.yaml filename is not a string"
				return c
			}
			g, err := glob.Compile(v.Value)
			if err != nil || strings.Contains(v.Value, "/") {
				c.opaque = "dart_test.yaml filename " + v.Value + " is not a supported glob"
				return c
			}
			c.filename = g
		case "include":
			c.opaque = "dart_test.yaml includes another configuration file"
			return c
		default:
			if nestedKey(v, 0) {
				c.opaque = "dart_test.yaml sets paths or filename below the top level (presets, tags or platforms)"
				return c
			}
		}
	}
	return c
}

// nestedKey: a mapping at or below n has a paths or filename key.
func nestedKey(n *yaml.Node, depth int) bool {
	if n == nil || depth > 64 {
		return depth > 64
	}
	if n.Kind == yaml.AliasNode {
		return nestedKey(n.Alias, depth+1)
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch n.Content[i].Value {
			case "paths", "filename", "include":
				return true
			}
		}
	}
	for _, c := range n.Content {
		if nestedKey(c, depth+1) {
			return true
		}
	}
	return false
}

// isTest reports whether rel (a path relative to the package directory) is a test file package:test would run: under test/
// with a name matching *_test.dart (what `dart test` and `flutter test` do with no arguments), or under one of the configured
// paths with a name matching the configured filename (the union: flutter test does not read paths).
func (c testCfg) isTest(rel string) bool {
	if !strings.HasSuffix(rel, ".dart") {
		return false
	}
	base := path.Base(rel)
	if within(rel, "test") && defaultFilename.Match(base) {
		return true
	}
	if c.filename != nil && c.filename.Match(base) {
		for _, p := range c.paths {
			p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
			if p == "." || within(rel, p) {
				return true
			}
		}
		if c.paths == nil && within(rel, "test") {
			return true
		}
	}
	return false
}
