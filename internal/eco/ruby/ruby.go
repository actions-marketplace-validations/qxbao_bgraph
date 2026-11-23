// Package ruby is bgraph's Ruby ecosystem (RSpec, Minitest / Test::Unit, Rails test; Bundler), at test-file granularity.
//
// # Projects
//
// A Ruby project is a directory holding a Gemfile, gems.rb, a *.gemspec, a Rakefile (rakefile, Rakefile.rb) or a .rspec file,
// plus the repository root when a Ruby file lies outside every such directory. Markers below a test-data directory (a path
// segment spec, test, tests, fixtures, fixture, tmp, node_modules, .bundle, or vendor/bundle) are data, not projects: an engine's
// test/dummy app or a spec/fixtures gem belongs to the enclosing project. A repository with no marker is not analyzed (a stray
// .rb file is left to the general rules), and so is a repository without any .rb, Gemfile or *.gemspec (cost ~0). Every file
// belongs to its nearest project.
//
// Ruby files: .rb, .rake, .ru, .gemspec, Gemfile, Rakefile, and extensionless files directly under a bin/, exe/ or libexec/
// directory whose shebang names ruby (executables).
//
// # Test units
//
// One model.TestUnit per test file: Eco "ruby", ID = the repo-relative file, Files = [the file], Project = its project. A file is a
// test when it is a .rb file outside the never-unit directories (fixtures, fixture, vendor, tmp, node_modules, .bundle) and
//
//   - it lies below a directory named spec or test and its name matches *_spec.rb, *_test.rb, test_*.rb or tc_*.rb; or
//   - it matches a literal test pattern of its project: .rspec / .rspec-local / .rspec-parallel --pattern (also below
//     --default-path), Rake::TestTask / RSpec::Core::RakeTask `pattern =` / `test_files =` (FileList[...], Dir[...], Dir.glob,
//     %w[...], ENV["X"] || "literal"), and Rake::TestTask's default test/test*.rb when a task sets neither.
//
// Runner "rspec" for a *_spec.rb file whose nearest spec/test ancestor is spec (or below the .rspec --default-path) and for files
// matched by an RSpec pattern; "rails" for files below test/ of a Rails application (bin/rails and config/application.rb);
// "minitest" otherwise. Config is the Gemfile bundler uses (the nearest Gemfile or gems.rb at or above the project, "" for
// none), followed for minitest units by "|" and the comma-separated -I directories (Rake::TestTask libs when literal, else lib
// and the unit's top test directory). A computed test pattern (or ERB in .rspec) adds a whole-project unit
// ID "<rb-all-tests:DIR>" (Whole, Runner "rake" or "rspec"), selected on any change (config_is_code).
//
// # Graph (From depends on To)
//
//   - require / require_dependency / autoload "a/b" (and load "a/b.rb"): suffix union. The importer depends on the virtual node
//     <rbreq:a/b> (load: <rbload:a/b.rb>), which depends on every Ruby file whose path is a/b.rb or ends with /a/b.rb, plus a
//     pattern edge **/a/b.rb so a file added later is caught. This is a superset of every $LOAD_PATH and working-directory
//     interpretation inside the repository, so $LOAD_PATH edits need no evaluation. Leading ./ and ../ (working-directory
//     paths) are dropped before the suffix match. Nothing found: an external gem or the standard library (no widening).
//   - require_relative "x" and evaluated path expressions (File.expand_path("x", __dir__), File.join(__dir__, ...),
//     File.dirname(__FILE__) + "/x", "#{__dir__}/x", Pathname(__dir__).join, Rails.root.join): the exact file (x.rb for require);
//     a missing target keeps the exact edge (adding it is caught) and is reported (unknown, no widening).
//   - a static prefix (require "a/#{x}", File.join(__dir__, "plugins", x)): pattern edges to every .rb file under the prefix.
//   - Dir[...] / Dir.glob / Pathname.glob whose files a block requires or loads (`.each { |f| require f }`,
//     `&method(:require)`): pattern edges to the glob (relative to the file, the project root for Rails.root, or by suffix for a
//     working-directory glob).
//   - Zeitwerk::Loader.for_gem / for_gem_extension: every .rb below the calling file's directory; loader.push_dir(d): every .rb
//     below d. A class inheriting Rails::Application or Rails::Engine depends on every file of its project outside the test
//     directories (pattern edges DIR/<top-level dir>/** for app, lib, config, db, vendor and every other top-level directory
//     except spec, test, tests, tmp, log, coverage, node_modules, doc, docs).
//   - Bundler.require: the features of the gems of the nearest Gemfile (gem name, its require: option, name with - as /, and the
//     gemspecs named by `gemspec`).
//   - every RSpec unit depends on the --require / -r features of its project's .rspec files; every minitest unit on the
//     Rake::TestTask ruby_opts -r features (implicit edges);
//   - test and test-support files (any file below a spec/test/tests directory, and every unit) depend on the existing files and
//     directories their path-like string literals and path expressions name, relative to the file's directory, the project root,
//     the repository root or a fixtures directory of the project (fixture_data edges; heuristic and add-only), on the files of
//     their data Dir globs (pattern edges), on executables named by Gem.bin_path, and on test/fixtures/** and spec/fixtures/**
//     for `fixtures :all`. Executables are sources: a test naming exe/x depends on it, and it depends on what it requires.
//
// # Unresolved
//
// A require, load, autoload, push_dir or Dir loop whose path is computed, `send(:require, x)` and eval(File.read(x)) are
// dynamic; a file the lexer cannot read reliably is uncertain_scan. Both are Unresolved at repository scope with
// TriggerGlobs on Ruby files (*.rb, *.rake, *.ru, *.gemspec, bin/*, exe/*, libexec/*): a Ruby file added, changed or deleted
// anywhere seeds the file with the dynamic site, and every test reaching it is selected.
//
// # Widenings (ClassifyChange, all with Eco "ruby")
//
// Gemfile / gems.rb: manifest_changed at its directory. *.gemspec: nothing when only summary, description, homepage,
// author(s), email, metadata, license(s), post_install_message, rdoc_options or extra_rdoc_files assignments changed (the
// scanner's GemspecFingerprint), manifest_changed at its directory otherwise. Gemfile.lock / gems.locked: lock_changed (level L0)
// at its directory. .rspec, .rspec-local, .rspec-parallel, spec/spec.opts, Rakefile, *.rake below tasks/ or rakelib/, and
// .simplecov: test_config_changed at the project. .rubocop.yml, .rubocop_todo.yml, .reek.yml, .yardopts, .document,
// .standard.yml at a project root, CHANGELOG* and *.rdoc in a project: nothing. Native extension sources (below an ext/
// directory: C/C++/Java sources and extconf.rb): unknown_file at the project. Marker files outside any project (test data) are
// left to the general rules.
//
// AnalyzerVersion must be bumped on every change of these rules.
package ruby

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/rules"
	rbscan "github.com/qxbao/bgraph/internal/scan/ruby"
	"github.com/qxbao/bgraph/internal/tree"
)

// Name is the ecosystem name.
const Name = "ruby"

// Runner names of the units.
const (
	RunnerRSpec    = "rspec"
	RunnerRails    = "rails"
	RunnerMinitest = "minitest"
	RunnerRake     = "rake" // whole-project unit run by `rake test`
)

// AnalyzerVersion: bump whenever the analysis rules change to invalidate stored analysis snapshots.
const AnalyzerVersion = "ruby-1"

// Eco is the Ruby ecosystem.
type Eco struct{}

// New creates the Ruby ecosystem. [[rule]] entries are applied by the selector.
func New(_ *rules.File) *Eco { return &Eco{} }

func (e *Eco) Name() string    { return Name }
func (e *Eco) Version() string { return AnalyzerVersion }

// WholeUnitID is the ID of the "whole test suite of project dir" unit.
func WholeUnitID(dir string) string { return "<rb-all-tests:" + dir + ">" }

// ParseWholeUnitID returns the project directory of a whole-project unit ID.
func ParseWholeUnitID(id string) (string, bool) {
	s, ok := strings.CutPrefix(id, "<rb-all-tests:")
	if !ok || !strings.HasSuffix(s, ">") {
		return "", false
	}
	return strings.TrimSuffix(s, ">"), true
}

// triggers: the files a dynamic require can load.
var triggers = []string{"**/*.rb", "**/*.rake", "**/*.ru", "**/*.gemspec", "**/bin/*", "**/exe/*", "**/libexec/*"}

type edgeKey struct {
	from, to string
	kind     model.EdgeKind
}

type fileInfo struct {
	p    string
	proj string
	scan model.ScanResult
	exe  bool
}

type builder struct {
	t  tree.Tree
	sc eco.Scanner
	a  *eco.Analysis

	markers  map[string]bool
	projOf   map[string]string
	rbFiles  []string        // Ruby files (sorted), executables included
	isRuby   map[string]bool // set of rbFiles
	exes     map[string]bool
	byBase   map[string][]string // base name → Ruby files
	fixtures map[string][]string // project → fixtures directories
	topDirs  map[string]map[string]bool
	gemfiles map[string]string // directory → Gemfile / gems.rb in it
	foreign  map[string]bool   // directories with another ecosystem's manifest
	rbDirs   map[string]bool   // directories with a Ruby file below them
	seen     map[edgeKey]bool
	infos    map[string]*fileInfo
	reqDone  map[string]bool // virtual nodes already linked
	rails    map[string]bool // projects that are Rails applications
}

// skipPath: never repository code.
func skipPath(p string) bool {
	return strings.HasPrefix(p, "node_modules/") || strings.Contains(p, "/node_modules/") || strings.HasPrefix(p, ".bundle/") ||
		strings.Contains(p, "/.bundle/") || strings.HasPrefix(p, "vendor/bundle/") || strings.Contains(p, "/vendor/bundle/") ||
		strings.HasPrefix(p, ".git/")
}

// dataDir: a path segment below which markers are test data and files are never units.
func dataSegment(seg string) bool {
	switch seg {
	case "fixtures", "fixture", "tmp", "node_modules", ".bundle":
		return true
	}
	return false
}

func underDataDir(p string, alsoTests bool) bool {
	segs := strings.Split(path.Dir(p), "/")
	for i, s := range segs {
		if dataSegment(s) || alsoTests && (s == "spec" || s == "test" || s == "tests") ||
			s == "vendor" && (alsoTests || i+1 < len(segs) && segs[i+1] == "bundle") {
			return true
		}
	}
	return false
}

func isMarker(base string) bool {
	switch base {
	case "Gemfile", "gems.rb", "Rakefile", "rakefile", "Rakefile.rb", ".rspec":
		return true
	}
	return strings.HasSuffix(base, ".gemspec")
}

func rubyExt(p string) bool {
	switch path.Ext(p) {
	case ".rb", ".rake", ".ru", ".gemspec":
		return true
	}
	switch path.Base(p) {
	case "Gemfile", "Rakefile", "rakefile":
		return true
	}
	return false
}

// exeCandidate: an extensionless file directly below bin/, exe/ or libexec/.
func exeCandidate(p string) bool {
	if path.Ext(p) != "" {
		return false
	}
	switch path.Base(path.Dir(p)) {
	case "bin", "exe", "libexec":
		return true
	}
	return false
}

var foreignManifests = map[string]bool{"package.json": true, "pyproject.toml": true, "setup.py": true, "go.mod": true, "Cargo.toml": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true}

// Analyze analyzes tree t.
func (e *Eco) Analyze(ctx context.Context, t tree.Tree, sc eco.Scanner) (*eco.Analysis, error) {
	a := &eco.Analysis{Roles: map[string]model.Role{}}
	files := t.Files()
	any := false
	for _, p := range files {
		if strings.HasSuffix(p, ".rb") || strings.HasSuffix(p, ".gemspec") || path.Base(p) == "Gemfile" {
			any = true
			break
		}
	}
	if !any {
		return a, nil
	}
	b := &builder{t: t, sc: sc, a: a, markers: map[string]bool{}, projOf: map[string]string{}, isRuby: map[string]bool{},
		exes: map[string]bool{}, byBase: map[string][]string{}, fixtures: map[string][]string{}, topDirs: map[string]map[string]bool{},
		gemfiles: map[string]string{}, foreign: map[string]bool{}, rbDirs: map[string]bool{}, seen: map[edgeKey]bool{},
		infos: map[string]*fileInfo{}, reqDone: map[string]bool{}, rails: map[string]bool{}}
	var exeCands []string
	fixtureDirs := map[string]bool{}
	for _, p := range files {
		if skipPath(p) {
			continue
		}
		base := path.Base(p)
		if isMarker(base) && !underDataDir(p, true) {
			b.markers[path.Dir(p)] = true
		}
		if base == "Gemfile" || base == "gems.rb" {
			if _, ok := b.gemfiles[path.Dir(p)]; !ok || base == "Gemfile" {
				b.gemfiles[path.Dir(p)] = p
			}
		}
		if foreignManifests[base] || strings.HasSuffix(base, ".csproj") || strings.HasSuffix(base, ".fsproj") {
			b.foreign[path.Dir(p)] = true
		}
		switch {
		case rubyExt(p):
			b.rbFiles = append(b.rbFiles, p)
		case exeCandidate(p):
			exeCands = append(exeCands, p)
		}
		if i := fixtureIndex(p); i > 0 {
			fixtureDirs[p[:i]] = true
		}
	}
	if len(b.markers) == 0 {
		return a, nil // Ruby files but no Ruby project: left to the general rules
	}
	for _, p := range exeCands {
		if isRubyScript(t, p) {
			b.rbFiles = append(b.rbFiles, p)
			b.exes[p] = true
		}
	}
	sort.Strings(b.rbFiles)
	for _, p := range b.rbFiles {
		b.isRuby[p] = true
		b.byBase[path.Base(p)] = append(b.byBase[path.Base(p)], p)
		for d := path.Dir(p); !b.rbDirs[d]; d = path.Dir(d) {
			b.rbDirs[d] = true
			if d == "." {
				break
			}
		}
	}

	// Projects.
	markerDirs := make([]string, 0, len(b.markers))
	for d := range b.markers {
		markerDirs = append(markerDirs, d)
	}
	sort.Strings(markerDirs)
	if !b.markers["."] {
		for _, p := range b.rbFiles {
			if b.project(path.Dir(p)) == "." {
				b.markers["."] = true
				a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: "."})
				break
			}
		}
	}
	for _, d := range markerDirs {
		a.Projects = append(a.Projects, eco.Project{Eco: Name, Dir: d})
	}
	for d := range fixtureDirs {
		pr := b.project(d)
		b.fixtures[pr] = append(b.fixtures[pr], d)
		if t.IsDir(d + "/files") {
			b.fixtures[pr] = append(b.fixtures[pr], d+"/files")
		}
	}
	for _, fs := range b.fixtures {
		sort.Strings(fs)
	}
	for _, p := range files {
		if skipPath(p) {
			continue
		}
		pr := b.project(path.Dir(p))
		rel := p
		if pr != "." {
			rel = strings.TrimPrefix(p, pr+"/")
		}
		if i := strings.IndexByte(rel, '/'); i > 0 {
			if b.topDirs[pr] == nil {
				b.topDirs[pr] = map[string]bool{}
			}
			b.topDirs[pr][rel[:i]] = true
		}
	}
	for _, d := range append([]string{"."}, markerDirs...) {
		if b.markers[d] && t.Has(path.Join(d, "bin/rails")) && t.Has(path.Join(d, "config/application.rb")) {
			b.rails[d] = true
		}
	}

	// Scan.
	cfg := map[string]*projCfg{}
	for _, p := range b.rbFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fi := &fileInfo{p: p, proj: b.project(path.Dir(p)), exe: b.exes[p]}
		if fi.exe {
			src, err := t.Read(p)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", p, err)
			}
			fi.scan = rbscan.Scan(p, src)
		} else {
			res, err := sc.Scan(t, p)
			if err != nil {
				return nil, fmt.Errorf("scan %s: %w", p, err)
			}
			fi.scan = res
		}
		b.infos[p] = fi
		c := cfg[fi.proj]
		if c == nil {
			c = &projCfg{}
			cfg[fi.proj] = c
		}
		c.collect(fi)
	}
	for _, pr := range a.Projects {
		if cfg[pr.Dir] == nil {
			cfg[pr.Dir] = &projCfg{}
		}
		cfg[pr.Dir].readRSpec(t, pr.Dir)
	}

	// Units.
	units := b.units(cfg)
	for _, p := range b.rbFiles {
		fi := b.infos[p]
		role := model.RoleSource
		base := path.Base(p)
		switch {
		case units[p] != nil:
			role = model.RoleTest
		case base == "Gemfile" || base == "gems.rb" || strings.HasSuffix(base, ".gemspec"):
			role = model.RoleManifest
		case base == "Rakefile" || base == "rakefile" || base == "Rakefile.rb" || isTaskFile(p):
			role = model.RoleConfig
		}
		a.Roles[p] = role
		b.fileEdges(fi, units[p] != nil)
	}
	var ids []string
	for p := range units {
		ids = append(ids, p)
	}
	sort.Strings(ids)
	for _, p := range ids {
		u := units[p]
		a.Tests = append(a.Tests, *u)
		c := cfg[u.Project]
		switch u.Runner {
		case RunnerRSpec:
			for _, r := range c.rspecRequires {
				b.feature(p, r.spec, ".rspec --require", false, model.EdgeImplicit)
			}
		default:
			for _, r := range c.rubyOpts {
				b.feature(p, r, "Rake::TestTask ruby_opts -r", false, model.EdgeImplicit)
			}
		}
	}
	// Whole-project units for computed test patterns.
	var projs []string
	for d := range cfg {
		projs = append(projs, d)
	}
	sort.Strings(projs)
	for _, d := range projs {
		c := cfg[d]
		if c.computed == "" {
			continue
		}
		runner := RunnerRake
		if c.computedBy == "rspec" {
			runner = RunnerRSpec
		}
		a.Tests = append(a.Tests, model.TestUnit{Eco: Name, ID: WholeUnitID(d), Project: d, Runner: runner, Whole: true,
			Config: b.gemfileFor(d)})
		a.Unresolved = append(a.Unresolved, model.Unresolved{File: c.computed, Spec: "test pattern", Class: model.UnresolvedUnknown,
			ScopeTier: model.TierNone, Detail: "the test file pattern is computed: the project's whole suite is one unit"})
	}
	b.otherRoles(files)
	slices.SortFunc(a.Edges, func(x, y model.Edge) int {
		if c := strings.Compare(x.From, y.From); c != 0 {
			return c
		}
		if c := strings.Compare(x.To, y.To); c != 0 {
			return c
		}
		return strings.Compare(string(x.Kind), string(y.Kind))
	})
	return a, nil
}

// fixtureIndex returns the length of the prefix of p up to and including its first "fixtures" or "fixture" directory below a
// spec/test/tests directory (0 when there is none).
func fixtureIndex(p string) int {
	off := 0
	inTests := false
	segs := strings.Split(p, "/")
	for i, s := range segs[:len(segs)-1] {
		off += len(s)
		if i > 0 {
			off++
		}
		if s == "spec" || s == "test" || s == "tests" {
			inTests = true
		}
		if inTests && (s == "fixtures" || s == "fixture") {
			return off
		}
	}
	return 0
}

// isRubyScript: the file starts with a shebang naming ruby.
func isRubyScript(t tree.Tree, p string) bool {
	b, err := t.Read(p)
	if err != nil || !bytes.HasPrefix(b, []byte("#!")) {
		return false
	}
	line := b
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		line = b[:i]
	}
	return bytes.Contains(line, []byte("ruby"))
}

func isTaskFile(p string) bool {
	if path.Ext(p) != ".rake" {
		return false
	}
	for _, s := range strings.Split(path.Dir(p), "/") {
		if s == "tasks" || s == "rakelib" {
			return true
		}
	}
	return false
}

// project returns the project directory of the files in dir.
func (b *builder) project(dir string) string {
	if v, ok := b.projOf[dir]; ok {
		return v
	}
	v := "."
	if b.markers[dir] {
		v = dir
	} else if dir != "." && dir != "" {
		v = b.project(path.Dir(dir))
	}
	b.projOf[dir] = v
	return v
}

// gemfileFor returns the Gemfile bundler uses for a project (the nearest one at or above it).
func (b *builder) gemfileFor(dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if g, ok := b.gemfiles[d]; ok {
			return g
		}
		if d == "." || d == "" {
			return ""
		}
	}
}

func (b *builder) edge(from, to string, kind model.EdgeKind, spec string, line int) {
	if from == to {
		return
	}
	k := edgeKey{from, to, kind}
	if b.seen[k] {
		return
	}
	b.seen[k] = true
	b.a.Edges = append(b.a.Edges, model.Edge{From: from, To: to, Kind: kind, Spec: spec, Line: line, Provenance: model.ProvScan})
}

func (b *builder) unres(from, spec string, class model.UnresolvedClass, line int, detail string) {
	u := model.Unresolved{File: from, Spec: spec, Class: class, Line: line, Detail: detail}
	switch class {
	case model.UnresolvedDynamic, model.UnresolvedUncertainScan:
		u.ScopeTier, u.ScopeDir, u.TriggerGlobs = model.TierModule, ".", triggers
	default:
		u.ScopeTier = model.TierNone
	}
	b.a.Unresolved = append(b.a.Unresolved, u)
}

// otherRoles gives roles to the non-Ruby files the ecosystem owns.
func (b *builder) otherRoles(files []string) {
	for _, p := range files {
		if skipPath(p) || b.isRuby[p] {
			continue
		}
		base := path.Base(p)
		switch {
		case base == "Gemfile.lock" || base == "gems.locked":
			b.a.Roles[p] = model.RoleLock
		case base == ".rspec" || base == ".rspec-local" || base == ".rspec-parallel" || base == ".simplecov" ||
			strings.HasSuffix(p, "spec/spec.opts") && base == "spec.opts":
			b.a.Roles[p] = model.RoleConfig
		case dataExt(p) && b.inRuby(p):
			b.a.Roles[p] = model.RoleData
		}
	}
}

func dataExt(p string) bool {
	switch path.Ext(p) {
	case ".erb", ".haml", ".slim", ".builder", ".jbuilder", ".yml", ".yaml":
		return true
	}
	return false
}

// inRuby: p lies in a Ruby project and not in a sub-tree that holds another ecosystem's manifest and no Ruby file.
func (b *builder) inRuby(p string) bool {
	pr := b.project(path.Dir(p))
	if !b.markers[pr] {
		return false
	}
	for d := path.Dir(p); eco.Within(d, pr); d = path.Dir(d) {
		if b.foreign[d] && !b.rbDirs[d] {
			return false
		}
		if d == pr || d == "." {
			break
		}
	}
	return true
}

// testish: a test or a file below a spec/test/tests directory (helpers, support files).
func testish(p string, unit bool) bool {
	if unit {
		return true
	}
	for _, s := range strings.Split(path.Dir(p), "/") {
		if s == "spec" || s == "test" || s == "tests" {
			return true
		}
	}
	return false
}

// escapeGlob escapes the glob metacharacters of a literal path.
func escapeGlob(s string) string {
	if !strings.ContainsAny(s, "*?[]{}\\!") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte("*?[]{}\\!", s[i]) >= 0 {
			sb.WriteByte('\\')
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// outside: a cleaned repo-relative path that leaves the repository.
func outside(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/")
}
