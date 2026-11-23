package ruby

import (
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/glob"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

type rspecReq struct{ spec string }

// projCfg is what a project's Rakefiles, task files and .rspec files say about its tests.
type projCfg struct {
	rakePatterns  []string // relative to the project directory
	rspecPatterns []string
	rakeTask      bool // a Rake::TestTask is defined
	rakeHasPat    bool // ... and some task sets pattern / test_files
	libs          []string
	rubyOpts      []string
	rspecRequires []rspecReq
	defaultPath   string // .rspec --default-path
	computed      string // file whose test pattern is computed ("" when none)
	computedBy    string // "rake" or "rspec"
}

func (c *projCfg) collect(fi *fileInfo) {
	for _, im := range fi.scan.Imports {
		switch im.Kind {
		case model.ImportRbTestTask:
			if im.Spec == "rake" {
				c.rakeTask = true
			}
		case model.ImportRbTestPattern:
			if im.Suffix == "rake" {
				c.rakeHasPat = true
			}
			if im.Dynamic {
				if c.computed == "" || im.Suffix == "rake" {
					c.computed, c.computedBy = fi.p, im.Suffix
				}
				continue
			}
			if im.Suffix == "rspec" {
				c.rspecPatterns = append(c.rspecPatterns, im.Spec)
			} else {
				c.rakePatterns = append(c.rakePatterns, im.Spec)
			}
		case model.ImportRbTestLib:
			c.libs = append(c.libs, im.Spec)
		case model.ImportRbRubyOpt:
			c.rubyOpts = append(c.rubyOpts, im.Spec)
		}
	}
}

// readRSpec reads the .rspec option files of project dir (RSpec reads ./.rspec, ./.rspec-local; parallel_tests reads
// .rspec-parallel; old RSpec spec/spec.opts).
func (c *projCfg) readRSpec(t tree.Tree, dir string) {
	for _, n := range []string{".rspec", ".rspec-local", ".rspec-parallel", "spec/spec.opts"} {
		p := path.Join(dir, n)
		if !t.Has(p) {
			continue
		}
		b, err := t.Read(p)
		if err != nil {
			c.computed, c.computedBy = p, "rspec"
			continue
		}
		s := string(b)
		if strings.Contains(s, "<%") {
			// ERB: the options are computed.
			if c.computed == "" {
				c.computed, c.computedBy = p, "rspec"
			}
		}
		words := optionWords(s)
		for i := 0; i < len(words); i++ {
			w := words[i]
			val := func(long, short string) (string, bool) {
				switch {
				case w == long || short != "" && w == short:
					if i+1 < len(words) {
						i++
						return words[i], true
					}
				case strings.HasPrefix(w, long+"="):
					return strings.TrimPrefix(w, long+"="), true
				case short != "" && strings.HasPrefix(w, short) && len(w) > len(short) && !strings.HasPrefix(w, "--"):
					return strings.TrimPrefix(w, short), true
				}
				return "", false
			}
			if v, ok := val("--require", "-r"); ok {
				c.rspecRequires = append(c.rspecRequires, rspecReq{v})
			} else if v, ok := val("--pattern", "-P"); ok {
				for _, pat := range strings.Split(v, ",") {
					if pat = strings.TrimSpace(pat); pat != "" {
						c.rspecPatterns = append(c.rspecPatterns, pat)
					}
				}
			} else if v, ok := val("--default-path", ""); ok {
				c.defaultPath = strings.TrimSuffix(strings.TrimPrefix(v, "./"), "/")
			}
		}
	}
}

// optionWords splits an options file into words (quotes group words; # starts a comment line).
func optionWords(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		var cur strings.Builder
		var q byte
		in := false
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case q != 0 && c == q:
				q = 0
			case q == 0 && (c == '"' || c == '\''):
				q, in = c, true
			case q == 0 && (c == ' ' || c == '\t' || c == '\r'):
				if in {
					out = append(out, cur.String())
					cur.Reset()
					in = false
				}
			default:
				cur.WriteByte(c)
				in = true
			}
		}
		if in {
			out = append(out, cur.String())
		}
	}
	return out
}

// neverUnit: files below these directories are data, never test units.
func neverUnit(p string) bool {
	for _, s := range strings.Split(path.Dir(p), "/") {
		switch s {
		case "fixtures", "fixture", "vendor", "tmp", "node_modules", ".bundle":
			return true
		}
	}
	return false
}

// testName: the conventional test file names.
func testName(base string) bool {
	return strings.HasSuffix(base, "_spec.rb") || strings.HasSuffix(base, "_test.rb") ||
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".rb") || strings.HasPrefix(base, "tc_") && strings.HasSuffix(base, ".rb")
}

// nearestTestDir returns "spec" or "test" for the nearest ancestor directory named spec, test or tests ("test"), "" for none.
func nearestTestDir(p string) string {
	segs := strings.Split(path.Dir(p), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		switch segs[i] {
		case "spec", "test":
			return segs[i]
		case "tests":
			return "test"
		}
	}
	return ""
}

// topTestDir returns the first spec/test/tests directory of p below project proj ("" for none), relative to proj.
func topTestDir(p, proj string) string {
	rel := p
	if proj != "." {
		rel = strings.TrimPrefix(p, proj+"/")
	}
	segs := strings.Split(path.Dir(rel), "/")
	for i, s := range segs {
		if s == "spec" || s == "test" || s == "tests" {
			return strings.Join(segs[:i+1], "/")
		}
	}
	return ""
}

// compilePatterns compiles test patterns relative to the project directory (and below alsoUnder); ok=false when one cannot be
// interpreted.
func compilePatterns(dir string, pats []string, alsoUnder string) (out []*glob.Pattern, ok bool) {
	ok = true
	for _, pat := range pats {
		pat = strings.TrimPrefix(strings.ReplaceAll(pat, "\\", "/"), "./")
		if pat == "" || strings.HasPrefix(pat, "/") {
			continue
		}
		cands := []string{path.Join(dir, pat)}
		if alsoUnder != "" && !strings.HasPrefix(pat, alsoUnder+"/") {
			cands = append(cands, path.Join(dir, alsoUnder, pat))
		}
		for _, c := range cands {
			if g, err := glob.Compile(c); err == nil {
				out = append(out, g)
			} else {
				ok = false
			}
		}
	}
	return out, ok
}

// units decides the test units.
func (b *builder) units(cfg map[string]*projCfg) map[string]*model.TestUnit {
	type compiled struct{ rake, rspec []*glob.Pattern }
	comp := map[string]*compiled{}
	get := func(proj string) *compiled {
		if c, ok := comp[proj]; ok {
			return c
		}
		c := &compiled{}
		if pc := cfg[proj]; pc != nil {
			rake := pc.rakePatterns
			if pc.rakeTask && !pc.rakeHasPat {
				rake = append(rake, "test/test*.rb") // Rake::TestTask's default pattern
			}
			var ok1, ok2 bool
			c.rake, ok1 = compilePatterns(proj, rake, "")
			c.rspec, ok2 = compilePatterns(proj, pc.rspecPatterns, pc.defaultPath)
			if (!ok1 || !ok2) && pc.computed == "" {
				pc.computed, pc.computedBy = path.Join(proj, "Rakefile"), "rake"
				if !ok2 {
					pc.computedBy = "rspec"
				}
			}
		}
		comp[proj] = c
		return c
	}
	out := map[string]*model.TestUnit{}
	for _, p := range b.rbFiles {
		if path.Ext(p) != ".rb" || neverUnit(p) || helperName(path.Base(p)) {
			continue
		}
		fi := b.infos[p]
		proj := fi.proj
		c := get(proj)
		pc := cfg[proj]
		base := path.Base(p)
		near := nearestTestDir(p)
		byName := near != "" && testName(base)
		rspec := false
		for _, g := range c.rspec {
			if g.Match(p) {
				rspec = true
			}
		}
		rake := false
		for _, g := range c.rake {
			if g.Match(p) {
				rake = true
			}
		}
		if pc != nil && pc.defaultPath != "" && strings.HasSuffix(base, "_spec.rb") && withinDir(p, path.Join(proj, pc.defaultPath)) {
			rspec = true
		}
		if !byName && !rspec && !rake {
			continue
		}
		runner := RunnerMinitest
		switch {
		case rspec || near == "spec" && strings.HasSuffix(base, "_spec.rb") && !rake:
			runner = RunnerRSpec
		case b.rails[proj] && strings.HasPrefix(topTestDir(p, proj), "test"):
			runner = RunnerRails
		}
		u := &model.TestUnit{Eco: Name, ID: p, Project: proj, Files: []string{p}, Runner: runner, Config: b.gemfileFor(proj)}
		if runner == RunnerMinitest {
			libs := []string{"lib"}
			if pc != nil && len(pc.libs) > 0 {
				libs = append(libs, pc.libs...)
			} else if td := topTestDir(p, proj); td != "" {
				libs = append(libs, td)
			} else {
				libs = append(libs, "test")
			}
			u.Config += "|" + strings.Join(dedupe(libs), ",")
		}
		out[p] = u
	}
	return out
}

// helperName: test helpers are sources even when a test name pattern matches them (test_helper.rb matches test_*.rb): as
// sources, a change nobody is known to load widens instead of selecting only the helper.
func helperName(base string) bool {
	switch base {
	case "test_helper.rb", "spec_helper.rb", "rails_helper.rb", "helper.rb":
		return true
	}
	return false
}

func withinDir(p, dir string) bool { return dir == "." || strings.HasPrefix(p, dir+"/") }

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "./"), "/")
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
