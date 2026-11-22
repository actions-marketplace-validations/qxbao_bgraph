package python

import (
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// harmlessProject: [project] keys whose change cannot alter a test run.
var harmlessProject = map[string]bool{"description": true, "authors": true, "maintainers": true, "urls": true, "classifiers": true,
	"readme": true, "keywords": true, "license": true, "license-files": true}

// harmlessTools: [tool.X] tables of linters, formatters and release tooling that tests do not read.
var harmlessTools = map[string]bool{"black": true, "ruff": true, "isort": true, "mypy": true, "coverage": true, "flake8": true,
	"pylint": true, "pyright": true, "bandit": true, "codespell": true, "towncrier": true, "commitizen": true, "semantic_release": true,
	"cibuildwheel": true, "pydocstyle": true, "interrogate": true, "vulture": true, "check-manifest": true, "docformatter": true,
	"autopep8": true, "yapf": true, "pycln": true, "blacken-docs": true, "pyupgrade": true, "pytype": true, "pycodestyle": true,
	"bumpversion": true, "bump2version": true, "bumpver": true, "slotscheck": true, "sphinx": true, "numpydoc_validation": true}

// harmlessCfgSections: setup.cfg sections of tools that tests do not read.
func harmlessCfgSection(s string) bool {
	switch {
	case s == "flake8", s == "pycodestyle", s == "pep8", s == "pylint", s == "isort", s == "tool:isort", s == "mypy", s == "bdist_wheel",
		s == "sdist", s == "bumpversion", s == "tool:black", s == "pydocstyle", s == "yapf":
		return true
	}
	return strings.HasPrefix(s, "mypy-") || strings.HasPrefix(s, "coverage:") || strings.HasPrefix(s, "bumpversion:") || strings.HasPrefix(s, "pylint.")
}

var harmlessMetadata = map[string]bool{"description": true, "long_description": true, "long_description_content_type": true, "author": true,
	"author_email": true, "maintainer": true, "maintainer_email": true, "url": true, "project_urls": true, "classifiers": true,
	"keywords": true, "license": true, "license_files": true, "license_file": true, "platforms": true, "download_url": true}

// harmlessFiles are tool configurations (linters, formatters, CI helpers) that no test reads.
func harmlessFile(base string) bool {
	switch base {
	case ".flake8", ".pre-commit-config.yaml", ".pre-commit-config.yml", ".pylintrc", ".isort.cfg", ".editorconfig", "MANIFEST.in",
		".readthedocs.yaml", ".readthedocs.yml", "codecov.yml", ".codecov.yml", ".pyup.yml", ".coveragerc", "mypy.ini", "ruff.toml",
		".ruff.toml", ".bandit", ".style.yapf", ".yapfignore", "pyrightconfig.json":
		return true
	}
	return false
}

var nativeExts = map[string]bool{".pyx": true, ".pxd": true, ".pxi": true, ".c": true, ".h": true, ".cc": true, ".cpp": true,
	".cxx": true, ".hpp": true, ".hh": true, ".hxx": true, ".f": true, ".f90": true, ".rs": true, ".cu": true, ".pyd": true, ".so": true}

func hasPython(a *eco.Analysis) bool {
	if a == nil {
		return false
	}
	for _, p := range a.Projects {
		if p.Eco == Name {
			return true
		}
	}
	return false
}

// projectDir returns the deepest Python project containing p ("." when none).
func projectDir(a *eco.Analysis, p string) string {
	best, bestDepth := ".", -1
	for _, pr := range a.Projects {
		if pr.Eco == Name && eco.Within(p, pr.Dir) {
			d := strings.Count(pr.Dir, "/")
			if pr.Dir == "." {
				d = -1
			}
			if d > bestDepth {
				best, bestDepth = pr.Dir, d
			}
		}
	}
	return best
}

func w(reason, p, scope, detail string) model.Widening {
	return model.Widening{Tier: model.TierModule, Reason: reason, Path: p, Scope: scope, Eco: Name, Detail: detail}
}

// ClassifyChange classifies pytest and packaging configuration (docs/ecosystems/python.md; lockfile level L0).
func (e *Eco) ClassifyChange(p string, base, head tree.Tree, a *eco.Analysis) ([]model.Widening, bool) {
	if !hasPython(a) || skipPath(p) {
		return nil, false
	}
	name := path.Base(p)
	dir := path.Dir(p)
	switch {
	case name == "pyproject.toml":
		return classifyPyproject(p, base, head), true
	case name == "setup.cfg":
		return classifySetupCfg(p, base, head), true
	case name == "setup.py":
		return []model.Widening{w(model.ReasonManifestChanged, p, dir, "setup.py is code that runs at build time")}, true
	case name == "Pipfile":
		return []model.Widening{w(model.ReasonManifestChanged, p, dir, "Pipfile (dependencies)")}, true
	case name == "pytest.ini" || name == ".pytest.ini" || name == "pytest.toml" || name == ".pytest.toml":
		return []model.Widening{w(model.ReasonTestConfig, p, dir, "pytest configuration changed")}, true
	case name == "tox.ini" || name == "noxfile.py":
		return []model.Widening{w(model.ReasonTestConfig, p, dir, name+" decides how the tests are launched (and may hold pytest settings)")}, true
	case isLockfile(name) || isRequirementsFile(p):
		return []model.Widening{w(model.ReasonLockChanged, p, projectDir(a, p), "dependency file at level L0 (resolved versions not compared)")}, true
	case harmlessFile(name) || isHarmlessScript(p):
		return nil, true
	case nativeExts[strings.ToLower(path.Ext(name))] || name == "meson.build" || name == "CMakeLists.txt":
		proj := projectDir(a, p)
		if hasNativeBuild(proj, base, head) {
			wd := w(model.ReasonUnknownFile, p, proj, "native extension source of Python project "+proj+": the compiled module is imported by tests")
			wd.Suggest = suggestRule(p)
			return []model.Widening{wd}, true
		}
	}
	return nil, false
}

func suggestRule(p string) string {
	return "[[rule]]\npaths = [\"" + p + "\"]\naffects = [\"<project or glob test>\"]  # or \"none\" if certainly harmless"
}

// hasNativeBuild: the project compiles C/C++/Cython/Rust into an importable module (build backend, ext_modules, .pyx files).
func hasNativeBuild(proj string, ts ...tree.Tree) bool {
	for _, t := range ts {
		if t == nil {
			continue
		}
		if b, err := t.Read(path.Join(proj, "pyproject.toml")); err == nil {
			var doc map[string]any
			if toml.Unmarshal(b, &doc) == nil {
				bs := tbl(doc["build-system"])
				text := strings.ToLower(strings.Join(strList(bs["requires"]), " ") + " " + strings.Join(strList(bs["build-backend"]), " "))
				for _, k := range []string{"maturin", "setuptools-rust", "scikit-build", "cython", "meson", "pybind11", "cffi", "numpy", "mypyc", "nanobind"} {
					if strings.Contains(text, k) {
						return true
					}
				}
				if tbl(doc["tool"])["maturin"] != nil {
					return true
				}
			}
		}
		if b, err := t.Read(path.Join(proj, "setup.py")); err == nil {
			s := string(b)
			if strings.Contains(s, "ext_modules") || strings.Contains(s, "Extension(") || strings.Contains(s, "cythonize") {
				return true
			}
		}
		for _, f := range t.Files() {
			if eco.Within(f, proj) && (strings.HasSuffix(f, ".pyx") || strings.HasSuffix(f, ".pxd")) {
				return true
			}
		}
	}
	return false
}

func readTOML(t tree.Tree, p string) (map[string]any, bool) {
	if t == nil || !t.Has(p) {
		return nil, false
	}
	b, err := t.Read(p)
	if err != nil {
		return nil, false
	}
	var doc map[string]any
	if toml.Unmarshal(b, &doc) != nil {
		return nil, false
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, true
}

// changedPaths lists the dotted key paths that differ between two TOML values, descending maps up to depth levels.
func changedPaths(o, n any, prefix string, depth int, out *[]string) {
	om, ok1 := o.(map[string]any)
	nm, ok2 := n.(map[string]any)
	if ok1 && ok2 && depth > 0 {
		keys := map[string]bool{}
		for k := range om {
			keys[k] = true
		}
		for k := range nm {
			keys[k] = true
		}
		for k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			changedPaths(om[k], nm[k], p, depth-1, out)
		}
		return
	}
	if !reflect.DeepEqual(o, n) {
		*out = append(*out, prefix)
	}
}

func classifyPyproject(p string, base, head tree.Tree) []model.Widening {
	dir := path.Dir(p)
	ob, ok1 := readTOML(base, p)
	nb, ok2 := readTOML(head, p)
	if !ok1 || !ok2 {
		return []model.Widening{w(model.ReasonManifestChanged, p, dir, "pyproject.toml added, deleted or unreadable on one side")}
	}
	var keys []string
	changedPaths(ob, nb, "", 3, &keys)
	sort.Strings(keys)
	var cfgKeys, manKeys []string
	for _, k := range keys {
		parts := strings.Split(k, ".")
		switch {
		case k == "tool.pytest" || strings.HasPrefix(k, "tool.pytest."), k == "tool.tox" || strings.HasPrefix(k, "tool.tox."):
			cfgKeys = append(cfgKeys, k)
		case parts[0] == "project" && len(parts) > 1 && harmlessProject[parts[1]]:
		case parts[0] == "tool" && len(parts) > 1 && harmlessTools[parts[1]]:
		case k == "tool" && onlyHarmlessTools(ob, nb):
		default:
			manKeys = append(manKeys, k)
		}
	}
	var out []model.Widening
	if len(cfgKeys) > 0 {
		out = append(out, w(model.ReasonTestConfig, p, dir, "pytest/tox settings changed: "+strings.Join(cfgKeys, ", ")))
	}
	if len(manKeys) > 0 {
		out = append(out, w(model.ReasonManifestChanged, p, dir, "keys changed: "+strings.Join(manKeys, ", ")))
	}
	return out
}

// onlyHarmlessTools: the whole [tool] table differs only in harmless tool tables (reached when a tool table was added or removed).
func onlyHarmlessTools(ob, nb map[string]any) bool {
	ot, nt := tbl(ob["tool"]), tbl(nb["tool"])
	for k := range ot {
		if !harmlessTools[k] && !reflect.DeepEqual(ot[k], nt[k]) {
			return false
		}
	}
	for k := range nt {
		if !harmlessTools[k] && !reflect.DeepEqual(ot[k], nt[k]) {
			return false
		}
	}
	return true
}

func readINI(t tree.Tree, p string) (*ini, bool) {
	if t == nil || !t.Has(p) {
		return nil, false
	}
	b, err := t.Read(p)
	if err != nil {
		return nil, false
	}
	return parseINI(string(b)), true
}

func classifySetupCfg(p string, base, head tree.Tree) []model.Widening {
	dir := path.Dir(p)
	oi, ok1 := readINI(base, p)
	ni, ok2 := readINI(head, p)
	if !ok1 || !ok2 {
		return []model.Widening{w(model.ReasonManifestChanged, p, dir, "setup.cfg added, deleted or unreadable on one side")}
	}
	names := map[string]bool{}
	for s := range oi.sections {
		names[s] = true
	}
	for s := range ni.sections {
		names[s] = true
	}
	var secs []string
	for s := range names {
		secs = append(secs, s)
	}
	sort.Strings(secs)
	var cfgKeys, manKeys []string
	for _, s := range secs {
		ov, nv := oi.sections[s], ni.sections[s]
		if reflect.DeepEqual(ov, nv) {
			continue
		}
		switch {
		case s == "tool:pytest" || s == "pytest":
			cfgKeys = append(cfgKeys, "["+s+"]")
		case harmlessCfgSection(s):
		case s == "metadata":
			keys := map[string]bool{}
			for k := range ov {
				keys[k] = true
			}
			for k := range nv {
				keys[k] = true
			}
			var ks []string
			for k := range keys {
				if ov[k] != nv[k] && !harmlessMetadata[k] {
					ks = append(ks, k)
				}
			}
			slices.Sort(ks)
			for _, k := range ks {
				manKeys = append(manKeys, "[metadata] "+k)
			}
		default:
			manKeys = append(manKeys, "["+s+"]")
		}
	}
	var out []model.Widening
	if len(cfgKeys) > 0 {
		out = append(out, w(model.ReasonTestConfig, p, dir, "pytest settings changed: "+strings.Join(cfgKeys, ", ")))
	}
	if len(manKeys) > 0 {
		out = append(out, w(model.ReasonManifestChanged, p, dir, "sections changed: "+strings.Join(manKeys, ", ")))
	}
	return out
}
