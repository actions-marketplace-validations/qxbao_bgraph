package python

import (
	"path"
	"regexp"
	"slices"
	"sort"

	"github.com/qxbao/bgraph/internal/model"
)

// Collection hooks: a conftest.py (or a plugin module it loads with pytest_plugins, a -p plugin of the pytest configuration, a
// pytest11 entry point) that implements one of these hooks can turn files that are not test_*.py modules into test items
// (pygments collects its example and snippet files with pytest_collect_file). bgraph cannot know which files become items, so
// the hook's scope directory becomes a "directory unit": ID and Files are the directory, the runner passes the directory to
// pytest (which collects every item below it), and a change to any file below the directory, to the hook file or to anything
// they import selects it.
var collectHookRe = regexp.MustCompile(`\bpytest_(collect_file|collect_directory|pycollect_makemodule|ignore_collect)\b`)

// hasCollectHook reports whether file p (a repo Python file) names a collection hook (defined or re-exported).
func (b *builder) hasCollectHook(p string) bool {
	if v, ok := b.hookCache[p]; ok {
		return v
	}
	v := false
	if b.fileSet[p] {
		if src, err := b.t.Read(p); err == nil {
			v = collectHookRe.Match(src)
		}
	}
	b.hookCache[p] = v
	return v
}

// collectHookDirs returns the scope directories of collection hooks: the directory of a conftest.py that implements one (itself
// or through its pytest_plugins), and the rootdir of every pytest configuration whose -p plugins or the repo's pytest11 entry
// points implement one. Sorted, deduplicated.
func (b *builder) collectHookDirs(conftests map[string]model.ScanResult, epFiles []string, cfgPlugins map[string][]string, rootdirs []string) []string {
	var dirs []string
	for c, scan := range conftests {
		hit := b.hasCollectHook(c)
		for _, im := range scan.Imports {
			if hit {
				break
			}
			if im.Kind != model.ImportPlugin || im.Dynamic {
				continue
			}
			for _, f := range b.heuristicFiles(im.Spec, nil) {
				if b.hasCollectHook(f) {
					hit = true
					break
				}
			}
		}
		if hit {
			dirs = append(dirs, path.Dir(c))
		}
	}
	for _, f := range epFiles {
		if b.hasCollectHook(f) {
			dirs = append(dirs, rootdirs...)
			break
		}
	}
	for cfgFile, fs := range cfgPlugins {
		for _, f := range fs {
			if b.hasCollectHook(f) {
				dirs = append(dirs, path.Dir(cfgFile))
				break
			}
		}
	}
	sort.Strings(dirs)
	return slices.Compact(dirs)
}

// addDirUnit adds the directory unit of a collection-hook scope dir (see collectHookRe) with its edges.
func (b *builder) addDirUnit(d string, epFiles []string, cfgPlugins map[string][]string) {
	proj := b.project(d)
	cfg := b.pytestCfgFor(d)
	if cfg == nil {
		if driver, _ := b.djangoDriver(proj); driver != "" {
			return // Django's runner does not use conftest hooks
		}
	}
	u := model.TestUnit{Eco: Name, ID: d, Project: proj, Files: []string{d}, Runner: RunnerPytest}
	if cfg != nil {
		u.Config = cfg.file
	}
	b.a.Tests = append(b.a.Tests, u)
	g := escapeGlob(d) + "/**"
	if d == "." {
		g = "**"
	}
	b.edge(d, g, model.EdgePattern, "pytest collection hook: any file below may be a test item", 0)
	for x := d; ; x = path.Dir(x) {
		if cf := path.Join(x, "conftest.py"); b.fileSet[cf] {
			b.edge(d, cf, model.EdgeConftest, "conftest", 0)
		}
		if x == "." {
			break
		}
	}
	if cfg != nil {
		for _, f := range cfgPlugins[cfg.file] {
			b.edge(d, f, model.EdgeImport, "pytest -p plugin", 0)
		}
	}
	for _, f := range epFiles {
		b.edge(d, f, model.EdgeImport, "pytest11 entry point", 0)
	}
}
