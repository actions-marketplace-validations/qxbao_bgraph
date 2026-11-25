package swift

import (
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qxbao/bgraph/internal/model"
)

func (b *builder) references(p *project, file, node string) {
	result, err := b.sc.Scan(b.t, file)
	if err != nil || result.Uncertain {
		detail := result.UncertainReason
		if err != nil {
			detail = err.Error()
		}
		b.unresolved(node, file, ".", model.UnresolvedUncertainScan, detail)
	}
	for _, ref := range result.Imports {
		if ref.Kind == model.ImportStatic {
			if p.m == nil {
				continue
			}
			for _, t := range p.m.targets {
				if ref.Spec == t.name || ref.Spec == moduleName(t.name) {
					b.edge(node, targetNode(p.dir, t), model.EdgeImport, ref.Spec)
				}
			}
			continue
		}
		if ref.Suffix == "process" && ref.Dynamic && ref.Spec == "" {
			b.programDependencies(node)
			continue
		}
		if ref.Spec == "" {
			b.unresolved(node, file, ".", model.UnresolvedDynamic, "computed Swift runtime file path")
			continue
		}

		spec := strings.TrimPrefix(ref.Spec, "file://")
		explicit := strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(ref.Spec, "file://") || ref.Suffix == "file"
		found := false
		for _, root := range []string{path.Dir(file), p.dir, "."} {
			target, ok := resolvePath(root, spec)
			if !ok || (target == "." && ref.Suffix != "file") || target == file {
				continue
			}
			found = b.referencePath(node, target, "Swift runtime path "+ref.Spec) || found
			if ref.Suffix == "process" {
				if dep := b.packages[target]; dep != nil {
					b.edge(node, libNode(dep.dir), model.EdgeProjectRef, "Swift subprocess package "+ref.Spec)
				}
			}
		}
		if !found && explicit && path.IsAbs(spec) {
			b.unresolved(node, ref.Spec, ".", model.UnresolvedUnknown, "absolute Swift runtime path cannot be located in the Git tree")
		}
		if !found && explicit {
			suffix := strings.TrimPrefix(spec, "/")
			for strings.HasPrefix(suffix, "../") || strings.HasPrefix(suffix, "./") {
				suffix = strings.TrimPrefix(strings.TrimPrefix(suffix, "../"), "./")
			}
			if suffix != "" && suffix != "." && suffix != ".." {
				b.edge(node, "**/"+escapeGlob(suffix), model.EdgePattern, "unresolved Swift runtime path "+ref.Spec)
				b.edge(node, "**/"+escapeGlob(suffix)+"/**", model.EdgePattern, "unresolved Swift runtime directory "+ref.Spec)
			}
		}
		if ref.Dynamic && (explicit || found) {
			b.unresolved(node, ref.Spec, ".", model.UnresolvedDynamic, "computed Swift runtime path")
		}
	}
}

func (b *builder) programDependencies(node string) {
	for _, dir := range slices.Sorted(maps.Keys(b.packages)) {
		p := b.packages[dir]
		if p.opaque != "" {
			b.edge(node, libNode(p.dir), model.EdgeProjectRef, "possible Swift subprocess package")
			continue
		}
		if p.runtimeTools {
			b.edge(node, libNode(p.dir), model.EdgeProjectRef, "possible Swift snippet or implicit executable")
		}
		programs := slices.Sorted(maps.Values(p.m.programs))
		for _, target := range slices.Compact(programs) {
			b.edge(node, targetNode(p.dir, p.targets[target]), model.EdgeProjectRef, "possible Swift subprocess "+target)
		}
	}
}

func (b *builder) referencePath(node, file, detail string) bool {
	found := false
	if b.t.Has(file) {
		b.edge(node, file, model.EdgeFixtureData, detail)
		found = true
	}
	if b.t.IsDir(file) {
		g := "**"
		if file != "." {
			g = escapeGlob(file) + "/**"
		}
		b.edge(node, g, model.EdgePattern, detail)
		found = true
	}
	for _, d := range b.alternateDirs(file) {
		b.edge(node, escapeGlob(d)+"/**", model.EdgePattern, detail)
		found = true
	}
	for _, match := range b.t.FoldLookup(file) {
		if match != file {
			b.edge(node, match, model.EdgeFixtureData, detail)
			found = true
		}
	}
	return found
}

func escapeGlob(s string) string {
	var sb strings.Builder
	for _, c := range s {
		if strings.ContainsRune("*?[]{}\\", c) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

func moduleName(s string) string {
	var b strings.Builder
	first, _ := utf8.DecodeRuneInString(s)
	if unicode.IsDigit(first) {
		b.WriteByte('_')
	}
	for _, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
