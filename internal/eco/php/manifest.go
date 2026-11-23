package php

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

type manifest struct {
	Name         string                     `json:"name"`
	Require      map[string]json.RawMessage `json:"require"`
	RequireDev   map[string]json.RawMessage `json:"require-dev"`
	Autoload     autoload                   `json:"autoload"`
	AutoloadDev  autoload                   `json:"autoload-dev"`
	IncludePath  pathList                   `json:"include-path"`
	Repositories json.RawMessage            `json:"repositories"`
	Scripts      map[string]pathList        `json:"scripts"`
	Config       struct {
		VendorDir string `json:"vendor-dir"`
		BinDir    string `json:"bin-dir"`
	} `json:"config"`
}

func (m manifest) has(pkg string) bool { return m.Require[pkg] != nil || m.RequireDev[pkg] != nil }

func (m manifest) scriptSuite(config string, isOperand func(string) bool) (runner string, custom bool) {
	for _, script := range m.Scripts {
		for _, line := range script {
			// Arguments belong to one command; stop at && || ; | and subshells so
			// "phpunit && phpstan analyse src" does not read "src" as a test operand.
			for _, cmd := range strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune("&|;()\n", r) }) {
				tokens := strings.FieldsFunc(cmd, unicode.IsSpace)
				r, c := commandSuite(tokens, config, isOperand)
				if r != "" && runner != RunnerPest {
					runner = r
				}
				custom = custom || c
			}
		}
	}
	if config == "" && len(m.Scripts["test"]) > 0 && (m.has("phpunit/phpunit") || m.has("pestphp/pest")) {
		custom = true
	}
	return runner, custom
}

func commandSuite(tokens []string, config string, isOperand func(string) bool) (runner string, custom bool) {
	for i, word := range tokens {
		word = strings.Trim(word, "\"'")
		word = path.Base(strings.ToLower(strings.ReplaceAll(word, "\\", "/")))
		switch ext := path.Ext(word); ext {
		case ".phar", ".bat", ".cmd":
			word = strings.TrimSuffix(word, ext)
		}
		switch word {
		case RunnerPest, RunnerPHPUnit:
			if runner != RunnerPest {
				runner = word
			}
			custom = custom || config == "" || alternateSuite(tokens[i+1:], path.Base(config), isOperand)
		}
	}
	return runner, custom
}

func alternateSuite(args []string, standardConfig string, isOperand func(string) bool) bool {
	for i := 0; i < len(args); i++ {
		arg := strings.Trim(args[i], "\"'")
		option, value, inline := strings.Cut(arg, "=")
		switch option {
		case "--configuration", "-c":
			if !inline && i+1 < len(args) {
				i++
				value = strings.Trim(args[i], "\"'")
			}
			if path.Clean(strings.ReplaceAll(value, "\\", "/")) != standardConfig {
				return true
			}
		case "--no-configuration", "--bootstrap", "--test-directory":
			return true
		default:
			if strings.HasPrefix(arg, "-c") && len(arg) > 2 || !strings.HasPrefix(arg, "-") && isOperand(arg) {
				return true
			}
		}
	}
	return false
}

func (m manifest) vendorDirOrDefault() string {
	if m.Config.VendorDir != "" {
		return m.Config.VendorDir
	}
	return "vendor"
}

type autoload struct {
	PSR4     map[string]pathList `json:"psr-4"`
	PSR0     map[string]pathList `json:"psr-0"`
	Classmap pathList            `json:"classmap"`
	Files    pathList            `json:"files"`
}

type pathList []string

func (p *pathList) UnmarshalJSON(data []byte) error {
	var s string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*p = []string{s}
		return nil
	}
	return json.Unmarshal(data, (*[]string)(p))
}

func readManifest(t tree.Tree, file string) (manifest, error) {
	var m manifest
	b, err := t.Read(file)
	if err != nil {
		return m, err
	}
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return m, errors.New("composer.json must contain an object")
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func (b *analysis) composerEdges(p *project) {
	if p.problem != "" {
		b.unresolved(p, virtual(p.dir), "composer.json", p.problem, model.UnresolvedUncertainScan, 0, nil)
	}
	for _, a := range []autoload{p.manifest.Autoload, p.manifest.AutoloadDev} {
		for _, mappings := range []map[string]pathList{a.PSR4, a.PSR0} {
			for _, paths := range mappings {
				for _, raw := range paths {
					// Composer accepts an empty string to mean the package directory.
					if raw == "" {
						raw = "."
					}
					b.composerPath(p, raw, "Composer PSR autoload root", true, false)
				}
			}
		}
		for _, raw := range a.Classmap {
			b.composerPath(p, raw, "Composer classmap", true, true)
		}
		for _, raw := range a.Files {
			b.composerPath(p, raw, "Composer autoload files", false, false)
		}
	}
	for _, raw := range p.manifest.IncludePath {
		b.composerPath(p, raw, "Composer include-path", true, false)
	}
	b.lockPathEdges(p)
	repos, err := repositories(p.manifest.Repositories)
	if err != nil {
		b.unresolved(p, virtual(p.dir), "repositories", "unsupported Composer repository configuration", model.UnresolvedUncertainScan, 0, nil)
		return
	}
	for _, r := range repos {
		switch typ := strings.ToLower(r.Type); {
		case typ == "path" || typ == "artifact":
			b.composerPath(p, r.URL, "Composer "+typ+" repository", true, typ == "path")
		case (typ == "vcs" || typ == "git" || typ == "svn" || typ == "hg" || typ == "fossil") && r.URL != "" &&
			!strings.Contains(r.URL, "://") && !strings.Contains(r.URL, "@"):
			b.composerPath(p, r.URL, "Composer local "+typ+" repository", true, false)
		case typ != "" && typ != "composer" && typ != "package" && typ != "vcs" && typ != "git" && typ != "svn" && typ != "hg" && typ != "fossil":
			b.unresolved(p, virtual(p.dir), typ, "unsupported Composer repository type", model.UnresolvedUncertainScan, 0, nil)
		}
	}
}

// Composer install can reuse locked local sources even when their repository
// definition came from an earlier manifest or a global Composer config.
func (b *analysis) lockPathEdges(p *project) {
	f := path.Join(p.dir, "composer.lock")
	if !b.t.Has(f) {
		return
	}
	type lockedPackage struct {
		Dist struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"dist"`
	}
	var locked struct {
		Packages    []lockedPackage `json:"packages"`
		PackagesDev []lockedPackage `json:"packages-dev"`
	}
	data, err := b.t.Read(f)
	if err != nil || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) || json.Unmarshal(data, &locked) != nil {
		b.unresolved(p, virtual(p.dir), f, "cannot read Composer locked path dependencies", model.UnresolvedUncertainScan, 0, nil)
		return
	}
	for _, pkg := range slices.Concat(locked.Packages, locked.PackagesDev) {
		if pkg.Dist.Type == "path" {
			b.composerPath(p, pkg.Dist.URL, "Composer locked path dependency", true, false)
		}
	}
}

type repository struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func repositories(data json.RawMessage) ([]repository, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var raw []json.RawMessage
	if data[0] == '{' {
		var byName map[string]json.RawMessage
		if err := json.Unmarshal(data, &byName); err != nil {
			return nil, err
		}
		for _, r := range byName {
			raw = append(raw, r)
		}
	} else if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]repository, 0, len(raw))
	for _, r := range raw {
		// Composer accepts entries such as {"packagist.org": false}.
		if string(r) == "false" {
			continue
		}
		var repo repository
		if err := json.Unmarshal(r, &repo); err != nil {
			return nil, err
		}
		out = append(out, repo)
	}
	return out, nil
}

func (b *analysis) composerPath(p *project, raw, spec string, dir, wildcard bool) {
	f, ok := localPath(p.dir, raw)
	if !ok {
		b.unresolved(p, virtual(p.dir), raw, "unsupported "+spec+" path", model.UnresolvedUncertainScan, 0, nil)
		return
	}
	if wildcard && strings.ContainsAny(raw, "*?[]{}") {
		b.referenceGlob(p, f, raw, spec, dir)
		return
	}
	b.reference(p, f, spec, dir, 0)
}

func (b *analysis) referenceGlob(p *project, f, raw, spec string, dir bool) {
	// A literal path can contain metacharacters on a case-sensitive filesystem.
	found := b.reference(p, f, spec, dir, 0)
	pattern, err := glob.Compile(f)
	if err != nil || strings.ContainsAny(f, "{}") {
		b.unresolved(p, virtual(p.dir), raw, "unsupported wildcard in "+spec, model.UnresolvedUncertainScan, 0, nil)
		return
	}
	visited := map[string]bool{}
	for _, file := range b.paths {
		for d := file; d != "." && !visited[d]; d = path.Dir(d) {
			visited[d] = true
			if pattern.Match(d) {
				found = b.reference(p, d, spec, dir, 0) || found
			}
		}
	}
	if !found {
		b.unresolved(p, virtual(p.dir), raw, spec+" has no matching path", model.UnresolvedUnknown, 0, []string{f, f + "/**"})
	}
}

// Reference records explicit local dependencies, including directories outside
// the owner. Contents of a separate Composer project may be both an input to
// this project and an independently runnable unit.
func (b *analysis) reference(p *project, f, spec string, dir bool, line int) bool {
	from := virtual(p.dir)
	found := false
	for _, candidate := range b.candidates(f) {
		switch {
		case b.t.Has(candidate):
			found = true
			if path.Base(candidate) != "composer.json" {
				b.edge(from, candidate, model.EdgeImport, spec, line)
				b.queueInclude(p, candidate)
			} else if target := b.projects[path.Dir(candidate)]; target != nil && target != p {
				b.edge(from, virtual(target.dir), model.EdgeImport, spec, line)
			}
		case dir && b.t.IsDir(candidate):
			found = true
			if target := b.projects[candidate]; target != nil && target != p {
				b.edge(from, virtual(target.dir), model.EdgeImport, spec, line)
			}
			if strings.HasPrefix(spec, "PHPUnit XML testsuite ") {
				if target := b.owner(candidate); target != nil && target != p && target.binDir != "" {
					b.edge(from, virtual(target.dir), model.EdgeImport, spec, line)
				}
			}
			for _, file := range filesWithin(b.paths, candidate) {
				if excludedPath(file) || b.vendored(file) {
					continue
				}
				if path.Base(file) == "composer.json" {
					if target := b.projects[path.Dir(file)]; target != nil && target != p {
						b.edge(from, virtual(target.dir), model.EdgeImport, spec, line)
					}
					continue
				}
				if b.fileOwner(file) != p {
					b.edge(from, file, model.EdgeImport, spec, line)
					b.queueInclude(p, file)
				}
			}
		}
	}
	return found
}

func decodeObject(t tree.Tree, p string) (map[string]any, error) {
	b, err := t.Read(p)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v map[string]any
	if err := d.Decode(&v); err != nil || v == nil {
		return nil, errors.New("invalid JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("unexpected trailing JSON")
	}
	return v, nil
}
