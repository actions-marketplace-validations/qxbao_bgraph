package php

import (
	"encoding/xml"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	phpscan "github.com/qxbao/bgraph/internal/scan/php"
)

type scanTask struct {
	project *project
	file    string
}

func (b *analysis) queueInclude(p *project, f string) {
	if phpscan.Source(f) && !excludedPath(f) && !b.vendored(f) {
		b.scanTasks = append(b.scanTasks, scanTask{p, f})
	}
}

func (b *analysis) includes(p *project, f string, sc eco.Scanner) {
	r, err := sc.Scan(b.t, f)
	if err != nil || r.Uncertain {
		detail := r.UncertainReason
		if err != nil {
			detail = "cannot scan PHP source"
		}
		b.unresolved(p, f, "", detail, model.UnresolvedUncertainScan, 0, nil)
	}
	for _, imp := range r.Imports {
		if imp.Dynamic {
			detail := "computed PHP include path"
			if imp.Kind == model.ImportRead {
				detail = "computed PHP data read path"
			}
			b.unresolved(p, f, imp.Spec, detail, model.UnresolvedDynamic, imp.Line, nil)
			continue
		}
		if imp.Kind == model.ImportRead {
			b.literalRead(p, f, imp)
			continue
		}
		b.include(p, f, imp)
	}
}

func (b *analysis) literalPaths(p *project, f string, imp model.ImportRef) []string {
	raw := strings.ReplaceAll(imp.Spec, "\\", "/")
	dirs := []string{p.dir, path.Dir(f), "."}
	if imp.Suffix == "__DIR__" {
		raw = strings.TrimPrefix(raw, "/")
		if raw == "" {
			raw = "."
		}
		dirs = []string{path.Dir(f)}
	}
	if real := b.aliases[f]; real != "" && path.Dir(real) != path.Dir(f) {
		dirs = append(dirs, path.Dir(real))
	}
	var candidates []string
	for _, d := range dirs {
		if dst, ok := localPath(d, raw); ok {
			candidates = append(candidates, dst)
		}
	}
	return candidates
}

// externalRead reports literal read operands that never name a repository
// file: absolute paths, network URLs and php:// streams other than
// php://filter, whose resource= target can be a relative file.
func externalRead(imp model.ImportRef) bool {
	if imp.Suffix != "" {
		return false
	}
	spec := strings.ReplaceAll(imp.Spec, "\\", "/")
	if path.IsAbs(spec) || len(spec) >= 3 && spec[1] == ':' && spec[2] == '/' && unicode.IsLetter(rune(spec[0])) {
		return true
	}
	lower := strings.ToLower(spec)
	if strings.HasPrefix(lower, "data:") {
		return true
	}
	scheme, _, ok := strings.Cut(lower, "://")
	if !ok {
		return false
	}
	switch scheme {
	case "http", "https", "ftp", "ftps":
		return true
	case "php":
		return !strings.HasPrefix(lower, "php://filter")
	}
	return false
}

func (b *analysis) literalRead(p *project, f string, imp model.ImportRef) {
	if externalRead(imp) {
		return
	}
	candidates := b.literalPaths(p, f, imp)
	if len(candidates) == 0 {
		b.unresolved(p, f, imp.Spec, "unsupported PHP data path", model.UnresolvedUnknown, imp.Line, nil)
		return
	}
	for _, candidate := range candidates {
		for _, dst := range b.candidates(candidate) {
			if b.t.Has(dst) {
				b.edge(f, dst, model.EdgeImport, "PHP literal read: "+imp.Spec, imp.Line)
			} else if b.t.IsDir(dst) {
				b.edge(f, dirGlob(dst), model.EdgePattern, "PHP literal directory read: "+imp.Spec, imp.Line)
			}
		}
	}
	// Some file functions search the runtime include_path. A bare relative
	// literal may resolve under any of its roots, including roots outside p.
	if imp.Suffix == "" {
		raw := strings.ReplaceAll(imp.Spec, "\\", "/")
		clean := path.Clean(raw)
		if clean != "." && repoPath(clean) && !strings.HasPrefix(raw, "./") && !strings.HasPrefix(raw, "../") {
			pattern := "**/" + escapeGlob(clean)
			b.edge(f, pattern, model.EdgePattern, "PHP data lookup path: "+imp.Spec, imp.Line)
		}
	}
}

func (b *analysis) include(p *project, f string, imp model.ImportRef) {
	raw := strings.ReplaceAll(imp.Spec, "\\", "/")
	candidates := b.literalPaths(p, f, imp)
	if len(candidates) == 0 {
		b.unresolved(p, f, imp.Spec, "unsupported PHP include path", model.UnresolvedUnknown, imp.Line, nil)
		return
	}
	bare := imp.Suffix == "" && !strings.HasPrefix(raw, "./") && !strings.HasPrefix(raw, "../")
	if bare {
		for _, inc := range p.manifest.IncludePath {
			if d, ok := localPath(p.dir, inc); ok {
				if dst, ok := localPath(d, raw); ok {
					candidates = append(candidates, dst)
				}
			}
		}
	}
	slices.Sort(candidates)
	var triggers []string
	found := false
	for _, candidate := range slices.Compact(candidates) {
		triggers = append(triggers, escapeGlob(candidate))
		// Installed Composer autoloaders are absent from normal Git trees.
		if b.composerAutoload(p, candidate, f, imp.Line) {
			found = true
			continue
		}
		for _, dst := range b.candidates(candidate) {
			if b.t.Has(dst) {
				found = true
				b.edge(f, dst, model.EdgeImport, imp.Spec, imp.Line)
				b.queueInclude(p, dst)
			}
		}
	}
	if bare && repoPath(path.Clean(raw)) && path.Clean(raw) != "." {
		// PHP can consult include_path. A suffix match covers custom roots
		// established at runtime without claiming ownership of unrelated files.
		pattern := "**/" + escapeGlob(path.Clean(raw))
		triggers = append(triggers, pattern)
		b.edge(f, pattern, model.EdgePattern, imp.Spec, imp.Line)
		suffix := strings.ToLower(path.Clean(raw))
		for _, dst := range b.filesNamed(path.Base(suffix)) {
			lower := strings.ToLower(dst)
			if strings.HasSuffix(lower, "/"+suffix) || lower == suffix {
				found = true
				b.edge(f, dst, model.EdgeImport, imp.Spec, imp.Line)
				b.queueInclude(p, dst)
			}
		}
	}
	if !found {
		b.unresolved(p, f, imp.Spec, "PHP include target is missing", model.UnresolvedUnknown, imp.Line, triggers)
	}
}

func (b *analysis) configEdges(p *project) {
	if p.config == "" {
		return
	}
	data, err := b.t.Read(p.config)
	if err != nil {
		b.unresolved(p, virtual(p.dir), p.config, "cannot read PHPUnit XML", model.UnresolvedUncertainScan, 0, nil)
		return
	}
	for _, ref := range xmlReferences(string(data)) {
		if ref.problem != "" {
			b.unresolved(p, virtual(p.dir), p.config, ref.problem, model.UnresolvedUncertainScan, 0, nil)
			continue
		}
		dirs := []string{path.Dir(p.config)}
		if real := b.aliases[p.config]; real != "" && path.Dir(real) != dirs[0] {
			dirs = append(dirs, path.Dir(real))
		}
		resolved := false
		for _, dir := range dirs {
			f, ok := localPath(dir, ref.path)
			if ok {
				resolved = true
				if !b.composerAutoload(p, f, virtual(p.dir), 0) {
					spec := "PHPUnit XML " + ref.element
					if ref.dir && strings.ContainsAny(ref.path, "*?[]{}") {
						b.referenceGlob(p, f, ref.path, spec, true)
					} else {
						b.reference(p, f, spec, ref.dir, 0)
					}
				}
			}
		}
		if !resolved {
			b.unresolved(p, virtual(p.dir), ref.path, "unsupported PHPUnit configuration path", model.UnresolvedUncertainScan, 0, nil)
		}
	}
}

func (b *analysis) composerAutoload(p *project, file, from string, line int) bool {
	if path.Base(file) != "autoload.php" {
		return false
	}
	owners := b.vendorRoots[path.Dir(file)]
	for _, target := range owners {
		if target != p {
			b.edge(from, virtual(target.dir), model.EdgeImport, "Composer autoloader", line)
		}
	}
	if len(owners) > 0 && b.t.Has(file) {
		b.edge(from, file, model.EdgeImport, "Composer autoloader", line)
	}
	return len(owners) > 0
}

type xmlPath struct {
	path, element, problem string
	dir                    bool
}

func xmlReferences(data string) []xmlPath {
	d := xml.NewDecoder(strings.NewReader(data))
	var refs []xmlPath
	var element string
	var text strings.Builder
	var level, active, suite int
	var isDir bool
	root := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			if !root {
				refs = append(refs, xmlPath{problem: "missing PHPUnit XML root"})
			}
			return refs
		}
		if err != nil {
			return append(refs, xmlPath{problem: "malformed or unsupported PHPUnit XML"})
		}
		switch v := tok.(type) {
		case xml.Directive:
			refs = append(refs, xmlPath{problem: "unsupported PHPUnit XML directive"})
		case xml.StartElement:
			level++
			if level == 1 {
				root = v.Name.Local == "phpunit"
			}
			switch v.Name.Local {
			case "directory", "file", "includePath":
				element, active = v.Name.Local, level
				isDir = v.Name.Local != "file"
				if suite > 0 {
					element = "testsuite " + element
				}
				text.Reset()
			case "testsuite":
				suite = level
			case "include":
				if v.Name.Space == "http://www.w3.org/2001/XInclude" {
					refs = append(refs, xmlPath{problem: "unsupported PHPUnit XML include"})
				}
			}
			for _, attr := range v.Attr {
				switch attr.Name.Local {
				case "bootstrap", "file", "baseline":
					refs = append(refs, xmlPath{path: attr.Value, element: attr.Name.Local})
				case "extensionsDirectory":
					refs = append(refs, xmlPath{path: attr.Value, element: attr.Name.Local, dir: true})
				}
			}
		case xml.CharData:
			if active != 0 && active == level {
				text.Write(v)
			}
		case xml.EndElement:
			if active == level {
				if s := strings.TrimSpace(text.String()); s != "" {
					refs = append(refs, xmlPath{path: s, element: element, dir: isDir})
				}
				active = 0
			}
			if suite == level {
				suite = 0
			}
			level--
		}
	}
}

func (b *analysis) edge(from, to string, kind model.EdgeKind, spec string, line int) {
	if from == to {
		return
	}
	b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Line: line, Provenance: model.ProvScan})
}

func (b *analysis) unresolved(p *project, file, spec, detail string, class model.UnresolvedClass, line int, globs []string) {
	// Unknown PHP code can load resources throughout the repository. Seeding
	// the importing file confines the resulting graph walk to its consumers.
	b.a.Unresolved = append(b.a.Unresolved, model.Unresolved{File: file, Spec: spec, Class: class, Line: line,
		ScopeDir: ".", ScopeTier: model.TierModule, Detail: p.dir + ": " + detail, TriggerGlobs: globs})
}

func (b *analysis) filesNamed(base string) []string {
	if b.basenames == nil {
		b.basenames = map[string][]string{}
		for _, f := range b.paths {
			n := strings.ToLower(path.Base(f))
			b.basenames[n] = append(b.basenames[n], f)
		}
	}
	return b.basenames[base]
}

func (b *analysis) candidates(f string) []string {
	out := []string{f}
	for _, folded := range b.t.FoldLookup(f) {
		if folded != f {
			out = append(out, folded)
		}
	}
	return out
}

func filesWithin(files []string, dir string) []string {
	if dir == "." {
		return files
	}
	i := sort.SearchStrings(files, dir+"/")
	j := i
	for j < len(files) && eco.Within(files[j], dir) {
		j++
	}
	return files[i:j]
}

func escapeGlob(s string) string {
	var out strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[]{}\`, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func dirGlob(s string) string {
	if s == "." {
		return "**"
	}
	return escapeGlob(s) + "/**"
}
