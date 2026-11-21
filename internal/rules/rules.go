// Package rules reads bgraph.toml (docs/plan.md §6.7): user-declared rules and declarations of generated-code outputs.
//
// Strict decoding: unknown keys are an error. A bgraph.toml error is NEVER ignored; callers widen to full (user_rule).
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/qxbao/bgraph/internal/glob"

	"github.com/pelletier/go-toml/v2"
)

// FileName is the name of the rules file at the repo root.
const FileName = "bgraph.toml"

// DefaultMaxChanged is the default threshold on the number of changed files; beyond it a full run is used.
const DefaultMaxChanged = 5000

// Rule is a [[rule]]. AffectsAll/AffectsNone/Affects are mutually exclusive.
type Rule struct {
	Index       int // order in the file, from 0
	Paths       []*glob.Pattern
	RawPaths    []string
	AffectsAll  bool
	AffectsNone bool
	Affects     []string // project directory or test glob
	Reason      string
}

// Generated is a [[generated]]: an ignored output that code imports.
type Generated struct {
	Outputs, Sources       []*glob.Pattern
	RawOutputs, RawSources []string
	Owner                  string
}

// Config is a [[config]]: the user's description of runner-config keys whose value is computed in code, which bgraph
// cannot read and therefore widens on (config_is_code). A declaration replaces that widening for the named keys of one
// config file. It is a user assertion: a wrong declaration can make bgraph skip a test that depends on the change (the
// same trust as affects = "none"), so every field is explicit and a reason is required.
type Config struct {
	Index    int
	File     string   // repo-relative runner config file (vitest.config.ts, jest.config.js...)
	Replaces []string // computed keys this declaration describes (see ConfigKeys)
	// Alias (vite semantics: spec == key or starts with key + "/" → target + rest) and AliasRegex (jest moduleNameMapper
	// semantics: full-match regex → target with $1..$9). Both empty with an alias key replaced: the computed alias only
	// redirects to what normal package resolution (workspace package.json exports/main) finds.
	Alias      map[string]string
	AliasRegex map[string]string
	// Tests: test-file globs (repo-relative) for replaced test-pattern keys; empty → bgraph's default test-file detection
	// finds every test.
	Tests       []*glob.Pattern
	RawTests    []string
	Sources     []string // helper files that compute the keys: their unresolved imports are described by this declaration
	IgnorePaths []string // path literals in the config that belong to the computation, not inputs of every test
	Reason      string
}

// Config keys a [[config]] may replace, by kind.
var (
	AliasKeys       = []string{"alias", "moduleNameMapper"}
	TestPatternKeys = []string{"include", "testMatch", "testRegex"}
	ProjectKeys     = []string{"projects", "workspace"}
)

// ConfigKeys are all the keys a [[config]] may replace.
var ConfigKeys = slices.Concat(AliasKeys, TestPatternKeys, ProjectKeys)

// Options holds the optional knobs.
type Options struct {
	MaxChanged int
}

// File is the validated content of bgraph.toml.
type File struct {
	Rules     []Rule
	Generated []Generated
	Configs   []Config
	Options   Options
	Path      string // path of the file read; empty if there is no file
}

type rawFile struct {
	Rule []struct {
		Paths   []string `toml:"paths"`
		Affects any      `toml:"affects"`
		Reason  string   `toml:"reason"`
	} `toml:"rule"`
	Generated []struct {
		Outputs []string `toml:"outputs"`
		Sources []string `toml:"sources"`
		Owner   string   `toml:"owner"`
	} `toml:"generated"`
	Config []struct {
		File        string            `toml:"file"`
		Replaces    []string          `toml:"replaces"`
		Alias       map[string]string `toml:"alias"`
		AliasRegex  map[string]string `toml:"alias_regex"`
		Tests       []string          `toml:"tests"`
		Sources     []string          `toml:"sources"`
		IgnorePaths []string          `toml:"ignore_paths"`
		Reason      string            `toml:"reason"`
	} `toml:"config"`
	Options struct {
		MaxChanged *int64 `toml:"max_changed"`
	} `toml:"options"`
}

// Load reads <root>/bgraph.toml. No file → an empty File with defaults, no error.
func Load(root string) (*File, error) {
	p := filepath.Join(root, FileName)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{Options: Options{MaxChanged: DefaultMaxChanged}}, nil
		}
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return Parse(p, data)
}

// Parse decodes and validates the content; name is used in error messages and assigned to File.Path.
func Parse(name string, data []byte) (*File, error) {
	var raw rawFile
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var se *toml.StrictMissingError
		if errors.As(err, &se) {
			return nil, fmt.Errorf("%s: invalid key:\n%s", name, se.String())
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			return nil, fmt.Errorf("%s: %s", name, de.String())
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f := &File{Path: name, Options: Options{MaxChanged: DefaultMaxChanged}}

	for i, r := range raw.Rule {
		out := Rule{Index: i, RawPaths: r.Paths, Reason: r.Reason}
		if len(r.Paths) == 0 {
			return nil, fmt.Errorf("%s: rule[%d]: 'paths' is required and must not be empty", name, i)
		}
		for _, s := range r.Paths {
			p, err := glob.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%s: rule[%d].paths: %w", name, i, err)
			}
			out.Paths = append(out.Paths, p)
		}
		switch v := r.Affects.(type) {
		case nil:
			return nil, fmt.Errorf("%s: rule[%d]: 'affects' is required", name, i)
		case string:
			switch v {
			case "all":
				out.AffectsAll = true
			case "none":
				out.AffectsNone = true
			default:
				return nil, fmt.Errorf("%s: rule[%d].affects: a string must be \"all\" or \"none\" (got %q); use an array for directories/globs", name, i, v)
			}
		case []any:
			if len(v) == 0 {
				return nil, fmt.Errorf("%s: rule[%d].affects: empty array", name, i)
			}
			for _, e := range v {
				s, ok := e.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("%s: rule[%d].affects: elements must be non-empty strings", name, i)
				}
				if _, err := glob.Compile(s); err != nil {
					return nil, fmt.Errorf("%s: rule[%d].affects: %w", name, i, err)
				}
				out.Affects = append(out.Affects, s)
			}
		default:
			return nil, fmt.Errorf("%s: rule[%d].affects: must be \"all\", \"none\" or an array of strings", name, i)
		}
		f.Rules = append(f.Rules, out)
	}

	for i, g := range raw.Generated {
		out := Generated{RawOutputs: g.Outputs, RawSources: g.Sources, Owner: g.Owner}
		if len(g.Outputs) == 0 {
			return nil, fmt.Errorf("%s: generated[%d]: 'outputs' is required and must not be empty", name, i)
		}
		if g.Owner == "" {
			return nil, fmt.Errorf("%s: generated[%d]: 'owner' is required", name, i)
		}
		for _, s := range g.Outputs {
			p, err := glob.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%s: generated[%d].outputs: %w", name, i, err)
			}
			out.Outputs = append(out.Outputs, p)
		}
		for _, s := range g.Sources {
			p, err := glob.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%s: generated[%d].sources: %w", name, i, err)
			}
			out.Sources = append(out.Sources, p)
		}
		f.Generated = append(f.Generated, out)
	}

	for i, c := range raw.Config {
		at := fmt.Sprintf("%s: config[%d]", name, i)
		file, ok := repoPath(c.File)
		if !ok {
			return nil, fmt.Errorf("%s: 'file' is required: the repo-relative path of the runner config whose keys are computed in code", at)
		}
		out := Config{Index: i, File: file, Replaces: c.Replaces, Alias: map[string]string{}, AliasRegex: map[string]string{},
			RawTests: c.Tests, Reason: c.Reason}
		if len(c.Replaces) == 0 {
			return nil, fmt.Errorf("%s: 'replaces' is required: the computed keys this declaration describes (%s)", at, strings.Join(ConfigKeys, ", "))
		}
		for _, k := range c.Replaces {
			if !slices.Contains(ConfigKeys, k) {
				return nil, fmt.Errorf("%s.replaces: %q is not supported (allowed: %s)", at, k, strings.Join(ConfigKeys, ", "))
			}
		}
		if strings.TrimSpace(c.Reason) == "" {
			return nil, fmt.Errorf("%s: 'reason' is required (a declaration narrows the selection: say why it is right)", at)
		}
		hasAny := func(keys []string) bool {
			return slices.ContainsFunc(c.Replaces, func(k string) bool { return slices.Contains(keys, k) })
		}
		if (len(c.Alias) > 0 || len(c.AliasRegex) > 0) && !hasAny(AliasKeys) {
			return nil, fmt.Errorf("%s: 'alias'/'alias_regex' need an alias key in 'replaces' (%s)", at, strings.Join(AliasKeys, ", "))
		}
		if len(c.Tests) > 0 && !hasAny(TestPatternKeys) {
			return nil, fmt.Errorf("%s: 'tests' needs a test-pattern key in 'replaces' (%s)", at, strings.Join(TestPatternKeys, ", "))
		}
		for k, v := range c.Alias {
			t, ok := repoPath(v)
			if k == "" || !ok {
				return nil, fmt.Errorf("%s.alias: %q = %q: the key must be non-empty and the target a repo-relative path inside the repo", at, k, v)
			}
			out.Alias[k] = t
		}
		for k, v := range c.AliasRegex {
			if _, err := regexp.Compile(k); err != nil {
				return nil, fmt.Errorf("%s.alias_regex: %q: %w", at, k, err)
			}
			if _, ok := repoPath(v); !ok {
				return nil, fmt.Errorf("%s.alias_regex: %q = %q: the target must be a repo-relative path inside the repo", at, k, v)
			}
			out.AliasRegex[k] = strings.ReplaceAll(v, "\\", "/")
		}
		for _, g := range c.Tests {
			p, err := glob.Compile(g)
			if err != nil {
				return nil, fmt.Errorf("%s.tests: %w", at, err)
			}
			out.Tests = append(out.Tests, p)
		}
		for _, list := range []struct {
			key string
			in  []string
			out *[]string
		}{{"sources", c.Sources, &out.Sources}, {"ignore_paths", c.IgnorePaths, &out.IgnorePaths}} {
			for _, v := range list.in {
				t, ok := repoPath(v)
				if !ok {
					return nil, fmt.Errorf("%s.%s: %q: must be a repo-relative path inside the repo", at, list.key, v)
				}
				*list.out = append(*list.out, t)
			}
		}
		f.Configs = append(f.Configs, out)
	}

	if m := raw.Options.MaxChanged; m != nil {
		if *m < 1 {
			return nil, fmt.Errorf("%s: options.max_changed: must be ≥ 1 (got %d)", name, *m)
		}
		f.Options.MaxChanged = int(*m)
	}
	return f, nil
}

// repoPath cleans a user-written repo-relative path (slash form) and reports whether it stays inside the repo.
func repoPath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	c := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") || (len(c) > 1 && c[1] == ':') {
		return "", false
	}
	return c, true
}

// Match returns the rules whose paths match path, in file order.
func (f *File) Match(path string) []Rule {
	var out []Rule
	for _, r := range f.Rules {
		for _, p := range r.Paths {
			if p.Match(path) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// GeneratedFor returns the first [[generated]] whose outputs match path, or nil.
func (f *File) GeneratedFor(path string) *Generated {
	for i := range f.Generated {
		for _, p := range f.Generated[i].Outputs {
			if p.Match(path) {
				return &f.Generated[i]
			}
		}
	}
	return nil
}
