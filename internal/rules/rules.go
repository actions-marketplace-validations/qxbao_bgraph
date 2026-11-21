// Package rules reads bgraph.toml (docs/plan.md §6.7): user-declared rules and declarations of generated-code outputs.
//
// Strict decoding: unknown keys are an error. A bgraph.toml error is NEVER ignored; callers widen to full (user_rule).
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// Options holds the optional knobs.
type Options struct {
	MaxChanged int
}

// File is the validated content of bgraph.toml.
type File struct {
	Rules     []Rule
	Generated []Generated
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

	if m := raw.Options.MaxChanged; m != nil {
		if *m < 1 {
			return nil, fmt.Errorf("%s: options.max_changed: must be ≥ 1 (got %d)", name, *m)
		}
		f.Options.MaxChanged = int(*m)
	}
	return f, nil
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
