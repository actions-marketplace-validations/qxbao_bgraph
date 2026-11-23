// Package ruby is bgraph's Ruby scanner (docs/ecosystems/ruby.md). It is a lexer (comments, =begin/=end, __END__, every string
// form with nested interpolation, heredocs, percent literals, regex-versus-division and character literals decided like Ruby's
// parser) plus a token-level extractor, not a parser. The result does not depend on the file's path, so it is cached by content.
//
// Every reference is a model.ImportRef; Suffix holds the base of a path: "" (a feature name or a path relative to an unknown
// working directory: resolved by suffix), "rel" (relative to the scanned file's directory) or "root" (relative to the project
// root, Rails.root). Dynamic=true means only a static prefix (Spec, possibly empty) of the path is known.
//
//   - A partly computed path (require "a/#{x}/version") is Dynamic with Spec = the static prefix, followed, when static text
//     follows the computed part, by "#{}" and that suffix ("a/#{}/version").
//   - require / require_dependency / autoload (any receiver for the last two; Kernel.require; send(:require, ...)) →
//     rb_require. require_relative / autoload_relative → rb_require with base "rel". Path expressions are evaluated:
//     File.expand_path("x", __dir__), File.expand_path("../x", __FILE__), File.join(__dir__, "a"), File.dirname(__FILE__) +
//     "/x", "#{__dir__}/x", Pathname(__dir__).join("x"), Rails.root.join("x"), Dir.pwd.
//   - load "x" → rb_load (the exact file name; load adds no extension); eval / instance_eval / class_eval of File.read(...) →
//     rb_load of the file read.
//   - Dir[...] / Dir.glob / Pathname.glob / Dir.children / each_child / entries / foreach whose files are required or loaded
//     by a block over them (`.each { |f| require f }`, `.each(&method(:require))`) → rb_glob (Spec the glob); other Dir globs →
//     rb_data_glob. A require of the loop variable is then not reported as dynamic.
//   - Zeitwerk::Loader.for_gem / for_gem_extension → rb_zeitwerk; loader.push_dir(x) → rb_push_dir.
//   - `< Rails::Application` → rb_rails "app"; `< Rails::Engine` → rb_rails "engine".
//   - Rake::TestTask / RSpec::Core::RakeTask (rb_test_task) `x.pattern =` / `x.test_files =` → rb_test_pattern (Dynamic when the
//     value is computed), `x.ruby_opts` "-rX" → rb_ruby_opt, `x.libs` → rb_test_lib.
//   - Gemfile: gem "name", require: ... → rb_gem; gemspec → rb_gemspec; eval_gemfile "x" → rb_load relative to the file. Bundler.require → rb_bundler. Gem.bin_path(g, x) →
//     rb_bin_path. fixtures :all → rb_fixtures_all. $LOAD_PATH / $: → rb_load_path (recorded only).
//   - String literals (and literal parts of interpolated strings) that may name a path (they contain '/' or '.'; a literal
//     with white space contributes its words containing '/'), and evaluated path expressions → rb_string.
//
// An argument that cannot be evaluated is a Dynamic ref with an empty Spec. Uncertain=true when the token stream cannot be
// trusted (an unterminated literal, heredoc or =begin; a heuristic regex/percent-literal decision that spans lines).
package ruby

import (
	"strings"

	"github.com/qxbao/bgraph/internal/model"
)

const maxRefs = 200000

// Scan scans one Ruby file.
func Scan(_ string, src []byte) model.ScanResult {
	lx, why := lex(src)
	if why != "" {
		return model.ScanResult{Imports: []model.ImportRef{}, Uncertain: true, UncertainReason: why}
	}
	s := &scanner{src: src, strs: lx.strs, seen: map[refKey]bool{}, used: map[int32]bool{}, top: lx.toks, ctx: -1}
	s.collectVars(lx.toks)
	s.run(lx.toks)
	// Interpolations: every code part is scanned too (a require inside "#{...}" is still a require).
	for i := 0; i < len(s.strs); i++ {
		for _, p := range s.strs[i].parts {
			if p.isCode && len(p.code) > 0 {
				s.run(p.code)
			}
		}
	}
	if s.res == nil {
		s.res = []model.ImportRef{}
	}
	return model.ScanResult{Imports: s.res}
}

type refKey struct {
	kind         model.ImportKind
	spec, suffix string
	dyn          bool
}

type scanner struct {
	src  []byte
	strs []strInfo
	res  []model.ImportRef
	seen map[refKey]bool
	// used: start offsets of tokens consumed by a recognized construct (their strings are not reported again as rb_string).
	used map[int32]bool
	// handled: start offsets of require/load calls resolved through a Dir loop.
	handled map[int32]bool
	rake    string // "rake" / "rspec" once a Rake::TestTask / RSpec::Core::RakeTask is seen
	loadP   bool
	top     []token            // the top-level token list
	vars    map[string]*varDef // single-assignment path variables (vars.go)
	scopes  []int              // scope of each top-level token (vars.go); nil when unknown
	// defsLoad: the file defines a method named load; a bare `load(x)` call there is that method, not Kernel#load.
	defsLoad bool
	// ctx: while a string's interpolations are evaluated, the index of that string in the top-level list (-1: none).
	ctx int
}

func (s *scanner) text(t token) string { return string(s.src[t.start:t.end]) }

func (s *scanner) add(kind model.ImportKind, spec, suffix string, dyn bool, line int32) {
	k := refKey{kind, spec, suffix, dyn}
	if s.seen[k] || len(s.res) >= maxRefs {
		return
	}
	s.seen[k] = true
	s.res = append(s.res, model.ImportRef{Kind: kind, Spec: spec, Suffix: suffix, Dynamic: dyn, Line: int(line)})
}

func baseTag(b base) string {
	switch b {
	case bRel:
		return "rel"
	case bRoot:
		return "root"
	}
	return ""
}

func (s *scanner) isOp(ts []token, i int, o string) bool {
	return i >= 0 && i < len(ts) && ts[i].kind == tOp && s.text(ts[i]) == o
}

func (s *scanner) isIdent(ts []token, i int, o string) bool {
	return i >= 0 && i < len(ts) && ts[i].kind == tIdent && s.text(ts[i]) == o
}

// closeOf returns the index of the bracket closing ts[i] ("(", "[" or "{"), or -1.
func (s *scanner) closeOf(ts []token, i, hi int) int {
	d := 0
	for j := i; j < hi && j < len(ts); j++ {
		if ts[j].kind != tOp {
			continue
		}
		switch s.text(ts[j]) {
		case "(", "[", "{":
			d++
		case ")", "]", "}":
			d--
			if d == 0 {
				return j
			}
			if d < 0 {
				return -1
			}
		}
	}
	return -1
}

// split splits ts[lo:hi] at top-level commas.
func (s *scanner) split(ts []token, lo, hi int) [][2]int {
	var out [][2]int
	d := 0
	start := lo
	for j := lo; j < hi; j++ {
		t := ts[j]
		if t.kind == tNL && d == 0 && j == start {
			start = j + 1
			continue
		}
		if t.kind != tOp {
			continue
		}
		switch s.text(t) {
		case "(", "[", "{":
			d++
		case ")", "]", "}":
			d--
		case ",":
			if d == 0 {
				out = append(out, [2]int{start, trimNL(ts, start, j)})
				start = j + 1
			}
		}
	}
	if start < hi {
		out = append(out, [2]int{start, trimNL(ts, start, hi)})
	}
	return out
}

func trimNL(ts []token, lo, hi int) int {
	for hi > lo && ts[hi-1].kind == tNL {
		hi--
	}
	return hi
}

// stmtEnders end a command argument list at depth 0.
var stmtEnders = map[string]bool{"if": true, "unless": true, "while": true, "until": true, "rescue": true, "and": true, "or": true,
	"then": true, "do": true, "end": true}

// continues: a line ending with this operator goes on on the next line.
var continues = map[string]bool{",": true, "+": true, "-": true, "*": true, "/": true, "||": true, "&&": true, "=": true, "(": true,
	"[": true, "{": true, "|": true, ".": true, "&.": true, "?": true, ":": true, "=>": true, "<<": true, "::": true, "+=": true, "||=": true}

// cmdEnd returns the end of a command argument list (or right-hand side) starting at lo.
func (s *scanner) cmdEnd(ts []token, lo int) int {
	d := 0
	for j := lo; j < len(ts); j++ {
		t := ts[j]
		switch t.kind {
		case tNL:
			if d > 0 {
				continue
			}
			if j > lo && ts[j-1].kind == tOp && continues[s.text(ts[j-1])] {
				continue
			}
			if j+1 < len(ts) && (s.isOp(ts, j+1, ".") || s.isOp(ts, j+1, "&.")) {
				continue
			}
			return j
		case tIdent:
			if d == 0 && stmtEnders[s.text(t)] && !(j > lo && (s.isOp(ts, j-1, ".") || s.isOp(ts, j-1, "&."))) &&
				!(j+1 < len(ts) && s.isOp(ts, j+1, ":") && !ts[j+1].sp) {
				return j
			}
		case tOp:
			switch s.text(t) {
			case "(", "[":
				d++
			case "{":
				if d == 0 && j > lo {
					return j // a block
				}
				d++
			case ")", "]", "}":
				if d == 0 {
					return j
				}
				d--
			case ";":
				if d == 0 {
					return j
				}
			}
		}
	}
	return len(ts)
}

// callRanges returns the argument ranges of a call whose name is at i, and the index after the call (-1: no arguments).
func (s *scanner) callRanges(ts []token, i int) ([][2]int, int) {
	j := i + 1
	if j >= len(ts) {
		return nil, -1
	}
	t := ts[j]
	if s.isOp(ts, j, "(") && !t.sp {
		c := s.closeOf(ts, j, len(ts))
		if c < 0 {
			return nil, -1
		}
		return s.split(ts, j+1, c), c + 1
	}
	if !argStart(s, ts, j) {
		return nil, -1
	}
	e := s.cmdEnd(ts, j)
	if e <= j {
		return nil, -1
	}
	return s.split(ts, j, e), e
}

// argStart: ts[j] can start the first argument of a parenless call.
func argStart(s *scanner, ts []token, j int) bool {
	t := ts[j]
	switch t.kind {
	case tStr, tSym, tIVar, tGVar, tNum:
		return t.sp || t.kind == tStr
	case tIdent:
		return t.sp && !stmtEnders[s.text(t)]
	case tOp:
		o := s.text(t)
		return t.sp && (o == "::" || o == "(" || o == "[" || o == "*" || o == "&" && !s.isOp(ts, j+1, "&"))
	}
	return false
}

// stmtStart: ts[i] starts a statement.
func (s *scanner) stmtStart(ts []token, i int) bool {
	if i == 0 {
		return true
	}
	p := ts[i-1]
	if p.kind == tNL {
		return true
	}
	if p.kind == tOp {
		o := s.text(p)
		return o == ";" || o == "{" || o == "("
	}
	if p.kind == tIdent {
		switch s.text(p) {
		case "do", "then", "else", "begin":
			return true
		}
	}
	// `{ |x| stmt` and `do |x| stmt`
	return s.isOp(ts, i-1, "|")
}

func (s *scanner) markUsed(ts []token, lo, hi int) {
	for j := lo; j < hi && j < len(ts); j++ {
		s.used[ts[j].start] = true
	}
}

func (s *scanner) run(ts []token) {
	if s.handled == nil {
		s.handled = map[int32]bool{}
	}
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		switch t.kind {
		case tIdent:
			if n := s.ident(ts, i); n > i {
				i = n - 1
			}
		case tGVar:
			if txt := s.text(t); (txt == "$LOAD_PATH" || txt == "$:") && !s.loadP {
				s.loadP = true
				s.add(model.ImportRbLoadPath, "", "", false, t.line)
			}
		case tStr:
			if !s.used[t.start] {
				s.stringRef(t)
			}
		}
	}
}

// receiver returns the receiver name of a method call at i ("" when there is none, "?" for an expression).
func (s *scanner) receiver(ts []token, i int) string {
	if !(s.isOp(ts, i-1, ".") || s.isOp(ts, i-1, "&.")) {
		return ""
	}
	if i >= 2 && ts[i-2].kind == tIdent {
		return s.text(ts[i-2])
	}
	return "?"
}

// ident handles the identifier at i; it may return the index after a construct it consumed (or i).
func (s *scanner) ident(ts []token, i int) int {
	t := ts[i]
	name := s.text(t)
	// A label (`require: false`) is never a call.
	if s.isOp(ts, i+1, ":") && !ts[i+1].sp {
		return i
	}
	afterDot := s.isOp(ts, i-1, ".") || s.isOp(ts, i-1, "&.")
	afterDef := s.isIdent(ts, i-1, "def") || s.isIdent(ts, i-1, "alias") || s.isIdent(ts, i-1, "alias_method")
	if afterDef {
		return i
	}
	switch name {
	case "require", "require_relative", "load", "require_dependency", "autoload", "autoload_relative":
		if afterDot {
			r := s.receiver(ts, i)
			ok := r == "Kernel" || name == "autoload" || name == "require_dependency" || name == "autoload_relative"
			if !ok {
				return i
			}
		}
		if s.handled[t.start] {
			return i
		}
		if s.isOp(ts, i+1, "=") || name == "load" && !afterDot && s.defsLoad {
			return i
		}
		ranges, end := s.callRanges(ts, i)
		if end < 0 {
			return i
		}
		s.requireCall(ts, name, ranges, t.line)
		return i + 1
	case "send", "public_send", "__send__":
		ranges, end := s.callRanges(ts, i)
		if end < 0 || len(ranges) < 2 {
			return i
		}
		r0 := ranges[0]
		if r0[1]-r0[0] != 1 {
			return i
		}
		m := ""
		if ts[r0[0]].kind == tSym {
			m = strings.TrimPrefix(s.text(ts[r0[0]]), ":")
		} else if ts[r0[0]].kind == tStr {
			m, _ = s.strs[ts[r0[0]].aux].static()
		}
		switch m {
		case "require", "require_relative", "load", "require_dependency", "autoload":
			s.requireCall(ts, m, ranges[1:], t.line)
		}
		return i
	case "Dir", "Pathname":
		if afterDot {
			return i
		}
		return s.dirCall(ts, i)
	case "Zeitwerk":
		if s.isOp(ts, i+1, "::") && s.isIdent(ts, i+2, "Loader") && s.isOp(ts, i+3, ".") &&
			(s.isIdent(ts, i+4, "for_gem") || s.isIdent(ts, i+4, "for_gem_extension")) {
			s.add(model.ImportRbZeitwerk, "", "", false, t.line)
		}
	case "push_dir":
		if !afterDot {
			return i
		}
		ranges, end := s.callRanges(ts, i)
		if end < 0 || len(ranges) == 0 {
			return i
		}
		r := ranges[0]
		v, ok := s.evalExact(ts, r[0], r[1])
		s.markUsed(ts, r[0], r[1])
		switch {
		case !ok || !v.usable():
			s.add(model.ImportRbPushDir, "", "", true, t.line)
		default:
			s.add(model.ImportRbPushDir, v.s, baseTag(v.b), v.dyn, t.line)
		}
		return end
	case "Rails":
		if s.isOp(ts, i+1, "::") && (s.isOp(ts, i-1, "<") || s.isOp(ts, i-1, "::") && s.isOp(ts, i-2, "<")) {
			switch {
			case s.isIdent(ts, i+2, "Application"):
				s.add(model.ImportRbRails, "app", "", false, t.line)
			case s.isIdent(ts, i+2, "Engine"):
				s.add(model.ImportRbRails, "engine", "", false, t.line)
			}
			return i
		}
		return s.pathRef(ts, i)
	case "TestTask", "RakeTask":
		if s.isOp(ts, i-1, "::") {
			s.rake = "rake"
			if name == "RakeTask" && s.isIdent(ts, i-2, "Core") {
				s.rake = "rspec"
			}
			s.add(model.ImportRbTestTask, s.rake, "", false, t.line)
		}
	case "pattern", "test_files", "ruby_opts", "libs":
		if afterDot && s.rake != "" {
			return s.rakeAttr(ts, i, name)
		}
	case "Bundler":
		if s.isOp(ts, i+1, ".") && s.isIdent(ts, i+2, "require") {
			s.add(model.ImportRbBundler, "", "", false, t.line)
			return i + 3
		}
	case "gem":
		if !afterDot && s.stmtStart(ts, i) {
			s.gemCall(ts, i)
		}
	case "gemspec":
		if !afterDot && s.stmtStart(ts, i) {
			spec := ""
			if ranges, end := s.callRanges(ts, i); end >= 0 {
				for _, r := range ranges {
					if v, ok := s.kwString(ts, r, "path"); ok {
						spec = v
					}
				}
			}
			s.add(model.ImportRbGemspec, spec, "", false, t.line)
		}
	case "eval_gemfile":
		// Bundler: the path is relative to the Gemfile's directory.
		if !afterDot {
			ranges, end := s.callRanges(ts, i)
			if end < 0 || len(ranges) == 0 {
				return i
			}
			r := ranges[0]
			v, ok := s.evalExact(ts, r[0], r[1])
			s.markUsed(ts, r[0], r[1])
			if !ok || !v.usable() || v.dyn {
				s.add(model.ImportRbLoad, "", "", true, t.line)
			} else {
				if v.b == bStr {
					v.b = bRel
				}
				s.add(model.ImportRbLoad, v.s, baseTag(v.b), false, t.line)
			}
			return end
		}
	case "Gem":
		if s.isOp(ts, i+1, ".") && s.isIdent(ts, i+2, "bin_path") {
			ranges, end := s.callRanges(ts, i+2)
			if end >= 0 && len(ranges) >= 2 {
				r := ranges[1]
				if r[1]-r[0] == 1 && ts[r[0]].kind == tStr {
					if v, ok := s.strs[ts[r[0]].aux].static(); ok {
						s.add(model.ImportRbBinPath, v, "", false, t.line)
						s.markUsed(ts, r[0], r[1])
					}
				}
			}
		}
	case "eval", "instance_eval", "class_eval", "module_eval", "binding_eval":
		s.evalCall(ts, i)
	case "fixtures":
		if !afterDot && i+1 < len(ts) && ts[i+1].kind == tSym && s.text(ts[i+1]) == ":all" {
			s.add(model.ImportRbFixturesAll, "", "", false, t.line)
		}
	case "File", "__dir__", "__FILE__":
		if !afterDot {
			return s.pathRef(ts, i)
		}
	}
	return i
}

// pathRef records an evaluated path expression starting at i as a string reference.
func (s *scanner) pathRef(ts []token, i int) int {
	v, end, ok := s.evalGreedy(ts, i, len(ts))
	if !ok || end <= i+1 || !v.usable() {
		return i
	}
	if v.b == bStr && !pathLike(v.s) && !v.dyn {
		return i
	}
	if v.dyn && v.s == "" && v.b == bStr {
		return i
	}
	if v.b == bRel && (v.s == "." || v.s == "") && !v.dyn {
		return end // the file's own directory
	}
	s.markUsed(ts, i, end)
	s.add(model.ImportRbString, v.s, baseTag(v.b), v.dyn, ts[i].line)
	return end
}

// requireCall records require-like call with argument ranges.
func (s *scanner) requireCall(ts []token, name string, ranges [][2]int, line int32) {
	arg := 0
	if name == "autoload" || name == "autoload_relative" {
		arg = 1
	}
	if len(ranges) <= arg {
		return
	}
	r := ranges[arg]
	kind := model.ImportRbRequire
	if name == "load" {
		kind = model.ImportRbLoad
	}
	relative := name == "require_relative" || name == "autoload_relative"
	v, ok := s.evalExact(ts, r[0], r[1])
	s.markUsed(ts, r[0], r[1])
	if !ok || !v.usable() || v.fullyDynamic() {
		s.add(kind, "", "", true, line)
		return
	}
	b := v.b
	if relative && b == bStr {
		b = bRel
	}
	if b == bRel && !v.dyn && (v.s == "." || v.s == "") {
		s.add(kind, "", "", true, line) // a directory is not a file: not understood
		return
	}
	s.add(kind, v.spec(), baseTag(b), v.dyn, line)
}

// kwString returns the literal string of keyword argument `key: "x"` in range r.
func (s *scanner) kwString(ts []token, r [2]int, key string) (string, bool) {
	if r[1]-r[0] == 3 && s.isIdent(ts, r[0], key) && s.isOp(ts, r[0]+1, ":") && ts[r[0]+2].kind == tStr {
		return s.strs[ts[r[0]+2].aux].static()
	}
	return "", false
}

func (s *scanner) gemCall(ts []token, i int) {
	ranges, end := s.callRanges(ts, i)
	if end < 0 || len(ranges) == 0 {
		return
	}
	r0 := ranges[0]
	if r0[1]-r0[0] != 1 || ts[r0[0]].kind != tStr {
		return
	}
	name, ok := s.strs[ts[r0[0]].aux].static()
	if !ok || name == "" {
		return
	}
	req := ""
	for _, r := range ranges[1:] {
		if r[1]-r[0] == 3 && s.isIdent(ts, r[0], "require") && s.isOp(ts, r[0]+1, ":") {
			v := ts[r[0]+2]
			switch {
			case v.kind == tIdent && s.text(v) == "false":
				req = "-"
			case v.kind == tStr:
				if x, ok := s.strs[v.aux].static(); ok {
					req = x
				}
			}
		}
	}
	s.markUsed(ts, r0[0], r0[1])
	s.add(model.ImportRbGem, name, req, false, ts[i].line)
}

// evalCall: eval / instance_eval / class_eval of File.read(path) loads that file.
func (s *scanner) evalCall(ts []token, i int) {
	ranges, end := s.callRanges(ts, i)
	if end < 0 || len(ranges) == 0 {
		return
	}
	r := ranges[0]
	for j := r[0]; j < r[1]; j++ {
		if ts[j].kind != tIdent {
			continue
		}
		m := s.text(ts[j])
		if m != "read" && m != "binread" && m != "readlines" {
			continue
		}
		if !(s.isOp(ts, j-1, ".") && (s.isIdent(ts, j-2, "File") || s.isIdent(ts, j-2, "IO"))) {
			continue
		}
		args, e := s.callRanges(ts, j)
		if e < 0 || len(args) == 0 {
			s.add(model.ImportRbLoad, "", "", true, ts[i].line)
			return
		}
		a := args[0]
		v, ok := s.evalExact(ts, a[0], a[1])
		if !ok || !v.usable() || v.dyn {
			s.add(model.ImportRbLoad, "", "", true, ts[i].line)
			return
		}
		s.markUsed(ts, a[0], a[1])
		s.add(model.ImportRbLoad, v.s, baseTag(v.b), false, ts[i].line)
		return
	}
}

// rakeAttr handles `t.pattern = ...`, `t.test_files = ...`, `t.ruby_opts << ...`, `t.libs << ...` inside a test task.
func (s *scanner) rakeAttr(ts []token, i int, name string) int {
	j := i + 1
	if !(s.isOp(ts, j, "=") || s.isOp(ts, j, "<<") || s.isOp(ts, j, "+=") || s.isOp(ts, j, "||=") ||
		s.isOp(ts, j, ".") && (s.isIdent(ts, j+1, "push") || s.isIdent(ts, j+1, "concat") || s.isIdent(ts, j+1, "unshift") ||
			s.isIdent(ts, j+1, "<<"))) {
		return i
	}
	if s.isOp(ts, j, ".") {
		j++
	}
	lo := j + 1
	hi := s.cmdEnd(ts, lo)
	if s.isOp(ts, lo, "(") && !ts[lo].sp {
		if c := s.closeOf(ts, lo, len(ts)); c > 0 {
			hi = c + 1
		}
	}
	strs, dyn := s.literalList(ts, lo, hi)
	s.markUsed(ts, lo, hi)
	line := ts[i].line
	switch name {
	case "pattern", "test_files":
		for _, p := range strs {
			s.add(model.ImportRbTestPattern, p, s.rake, false, line)
		}
		if dyn {
			s.add(model.ImportRbTestPattern, "", s.rake, true, line)
		}
	case "ruby_opts":
		if dyn {
			// A computed option may load any file before every test: the project's suite is computed too.
			s.add(model.ImportRbTestPattern, "", s.rake, true, line)
		}
		var words []string
		for _, p := range strs {
			words = append(words, strings.Fields(p)...)
		}
		for k, w := range words {
			switch {
			case w == "-r" && k+1 < len(words):
				s.add(model.ImportRbRubyOpt, words[k+1], "", false, line)
			case strings.HasPrefix(w, "--require="):
				s.add(model.ImportRbRubyOpt, strings.TrimPrefix(w, "--require="), "", false, line)
			case strings.HasPrefix(w, "-r") && len(w) > 2:
				s.add(model.ImportRbRubyOpt, w[2:], "", false, line)
			}
		}
	case "libs":
		for _, p := range strs {
			s.add(model.ImportRbTestLib, p, "", false, line)
		}
	}
	return hi
}

// literalAllowed: identifiers that may appear in a literal file list.
var literalAllowed = map[string]bool{"FileList": true, "Dir": true, "glob": true, "new": true, "sort": true, "uniq": true, "to_a": true,
	"ENV": true, "fetch": true, "freeze": true, "flatten": true, "compact": true, "select": true, "reject": true, "existing": true,
	"exclude": true, "nil": true}

// literalList collects the literal strings of a file list expression; dyn when part of it is computed. Strings inside ENV[...],
// as the key of ENV.fetch, in .exclude(...) and after a top-level `-` are not candidates (they only narrow the list).
func (s *scanner) literalList(ts []token, lo, hi int) ([]string, bool) {
	var out []string
	dyn := false
	for j := lo; j < hi; j++ {
		t := ts[j]
		switch t.kind {
		case tIdent:
			name := s.text(t)
			if !literalAllowed[name] {
				dyn = true
				continue
			}
			switch {
			case name == "ENV" && s.isOp(ts, j+1, "["):
				if c := s.closeOf(ts, j+1, hi); c > 0 {
					j = c
				}
			case name == "fetch" && s.isOp(ts, j+1, "("):
				// ENV.fetch("KEY", "default"): skip the key.
				if k := j + 2; k < hi && ts[k].kind == tStr && s.isOp(ts, k+1, ",") {
					j = k + 1
				}
			case name == "exclude" || name == "select" || name == "reject":
				if s.isOp(ts, j+1, "(") || s.isOp(ts, j+1, "{") {
					if c := s.closeOf(ts, j+1, hi); c > 0 {
						j = c
					}
				}
			}
		case tStr:
			si := &s.strs[t.aux]
			switch si.kind {
			case sWords:
				out = append(out, strings.Fields(si.parts[0].lit)...)
			case sSingle, sDouble:
				if v, ok := si.static(); ok {
					out = append(out, v)
				} else {
					dyn = true
				}
			default:
				dyn = true
			}
		case tOp:
			if s.text(t) == "-" {
				return out, dyn
			}
		case tIVar, tGVar, tSym:
			dyn = true
		}
	}
	return out, dyn
}

// dirCall handles Dir[...], Dir.glob(...), Pathname.glob(...), Dir.children/each_child/entries/foreach(dir) at i.
func (s *scanner) dirCall(ts []token, i int) int {
	name := s.text(ts[i])
	var ranges [][2]int
	end := -1
	dirList := false
	switch {
	case name == "Dir" && s.isOp(ts, i+1, "[") && !ts[i+1].sp:
		c := s.closeOf(ts, i+1, len(ts))
		if c < 0 {
			return i
		}
		ranges, end = s.split(ts, i+2, c), c+1
	case s.isOp(ts, i+1, ".") && s.isIdent(ts, i+2, "glob"):
		ranges, end = s.callRanges(ts, i+2)
	case name == "Dir" && s.isOp(ts, i+1, ".") && i+2 < len(ts) && ts[i+2].kind == tIdent:
		switch s.text(ts[i+2]) {
		case "children", "each_child", "entries", "foreach":
			dirList = true
			ranges, end = s.callRanges(ts, i+2)
		default:
			return i
		}
	default:
		return i
	}
	if end < 0 || len(ranges) == 0 {
		return i
	}
	line := ts[i].line
	// Evaluate the patterns; keyword base: joins them.
	var globs []pval
	var baseV *pval
	methodReq := ""
	unknown := false
	for k, r := range ranges {
		if r[1]-r[0] >= 3 && ts[r[0]].kind == tIdent && s.isOp(ts, r[0]+1, ":") && !ts[r[0]+1].sp {
			if s.text(ts[r[0]]) == "base" {
				if v, ok := s.evalExact(ts, r[0]+2, r[1]); ok && v.usable() && !v.dyn {
					baseV = &v
				} else {
					unknown = true
				}
			}
			continue
		}
		if m := s.methodRef(ts, r); m != "" {
			methodReq = m
			continue
		}
		if dirList && k > 0 {
			continue
		}
		if r[1]-r[0] == 1 && ts[r[0]].kind == tStr && s.strs[ts[r[0]].aux].kind == sWords {
			for _, w := range strings.Fields(s.strs[ts[r[0]].aux].parts[0].lit) {
				globs = append(globs, pval{b: bStr, s: w})
			}
			continue
		}
		if s.isOp(ts, r[0], "[") && s.closeOf(ts, r[0], r[1]) == r[1]-1 {
			for _, e := range s.split(ts, r[0]+1, r[1]-1) {
				if v, ok := s.evalExact(ts, e[0], e[1]); ok && v.usable() {
					globs = append(globs, v)
				} else {
					unknown = true
				}
			}
			continue
		}
		if v, ok := s.evalExact(ts, r[0], r[1]); ok && v.usable() && !(v.dyn && v.s == "" && v.b == bStr) {
			globs = append(globs, v)
		} else {
			unknown = true
		}
	}
	if baseV != nil {
		for k := range globs {
			if g, ok := join(*baseV, globs[k], true); ok {
				globs[k] = g
			} else {
				unknown = true
			}
		}
	}
	if dirList {
		for k := range globs {
			if !globs[k].dyn {
				globs[k].s = strings.TrimSuffix(globs[k].s, "/") + "/*"
			}
		}
	}
	s.markUsed(ts, i, end)
	// What consumes the listed files?
	reqKinds := map[string]bool{}
	if methodReq != "" {
		reqKinds[methodReq] = true
	}
	if !unknown && len(globs) > 0 {
		for _, k := range s.loopRequires(ts, end, dirList) {
			reqKinds[k] = true
		}
		// `paths = Dir[...]` then `paths.each { |f| require f }`: a single-assignment variable holding the list.
		if v := s.assignedTo(ts, i); v != "" {
			for j := end; j < len(ts); j++ {
				if s.isIdent(ts, j, v) && !s.isOp(ts, j-1, ".") && s.isOp(ts, j+1, ".") && s.sameScope(ts, i, j) {
					for _, k := range s.loopRequires(ts, j+1, dirList) {
						reqKinds[k] = true
					}
				}
			}
		}
	}
	if len(reqKinds) > 0 && (unknown || len(globs) == 0) {
		s.add(model.ImportRbRequire, "", "", true, line) // the files required are not known
		return end
	}
	for _, g := range globs {
		if len(reqKinds) == 0 {
			s.add(model.ImportRbDataGlob, g.spec(), baseTag(g.b), g.dyn, line)
			continue
		}
		s.add(model.ImportRbGlob, g.spec(), baseTag(g.b), g.dyn, line)
		if reqKinds["require_relative"] && g.b == bStr {
			// require_relative of a working-directory relative name resolves against the file's directory.
			s.add(model.ImportRbGlob, g.spec(), "rel", g.dyn, line)
		}
	}
	return end
}

// methodRef recognizes `&method(:require)` (or :load / :require_relative) in r.
func (s *scanner) methodRef(ts []token, r [2]int) string {
	if r[1]-r[0] == 5 && s.isOp(ts, r[0], "&") && s.isIdent(ts, r[0]+1, "method") && s.isOp(ts, r[0]+2, "(") &&
		ts[r[0]+3].kind == tSym && s.isOp(ts, r[0]+4, ")") {
		switch m := strings.TrimPrefix(s.text(ts[r[0]+3]), ":"); m {
		case "require", "load", "require_relative", "require_dependency":
			return m
		}
	}
	return ""
}

// chainMethods may sit between a Dir glob and the block that iterates over its files.
var chainMethods = map[string]bool{"each": true, "sort": true, "sort_by": true, "map": true, "select": true, "reject": true,
	"each_with_index": true, "uniq": true, "to_a": true, "lazy": true, "reverse": true, "reverse_each": true, "each_entry": true,
	"filter": true, "filter_map": true, "grep": true, "grep_v": true, "flat_map": true, "sort!": true}

// loopRequires follows the method chain after a Dir call ending at `end` to its block, and marks the require/load calls in that
// block whose argument is the block's variable (handled). Returns the kinds of those calls.
func (s *scanner) loopRequires(ts []token, end int, dirList bool) []string {
	j := end
	var kinds []string
	for {
		if !(s.isOp(ts, j, ".") || s.isOp(ts, j, "&.")) || j+1 >= len(ts) || ts[j+1].kind != tIdent || !chainMethods[s.text(ts[j+1])] {
			break
		}
		j += 2
		if s.isOp(ts, j, "(") && !ts[j].sp {
			c := s.closeOf(ts, j, len(ts))
			if c < 0 {
				return nil
			}
			if m := s.methodRef(ts, [2]int{j + 1, c}); m != "" {
				return []string{m}
			}
			j = c + 1
		}
		// A block: { |f| ... } or do |f| ... end
		var lo, hi int
		switch {
		case s.isOp(ts, j, "{"):
			c := s.closeOf(ts, j, len(ts))
			if c < 0 {
				return nil
			}
			lo, hi = j+1, c
		case s.isIdent(ts, j, "do"):
			c := s.doEnd(ts, j)
			if c < 0 {
				return nil
			}
			lo, hi = j+1, c
		default:
			continue
		}
		vars := []string{"_1", "it"}
		k := lo
		if s.isOp(ts, k, "|") {
			vars = nil
			for k++; k < hi && !s.isOp(ts, k, "|"); k++ {
				if ts[k].kind == tIdent {
					vars = append(vars, s.text(ts[k]))
					break // the first parameter is the file
				}
			}
		}
		if len(vars) == 0 {
			return kinds
		}
		for q := lo; q < hi; q++ {
			if ts[q].kind != tIdent {
				continue
			}
			m := s.text(ts[q])
			switch m {
			case "require", "load", "require_relative", "require_dependency":
			default:
				continue
			}
			if s.isOp(ts, q-1, ".") && !s.isIdent(ts, q-2, "Kernel") {
				continue
			}
			ranges, e := s.callRanges(ts, q)
			if e < 0 || len(ranges) == 0 || e > hi {
				continue
			}
			if s.loopArg(ts, ranges[0], vars, dirList) {
				s.handled[ts[q].start] = true
				kinds = append(kinds, m)
			}
		}
		return kinds
	}
	return kinds
}

// loopWrappers may wrap the loop variable in the argument of a require of a globbed file.
var loopWrappers = map[string]bool{"File": true, "expand_path": true, "absolute_path": true, "to_s": true, "to_path": true,
	"sub": true, "chomp": true, "delete_suffix": true, "gsub": true, "realpath": true}

// loopArg: the argument is the loop variable, possibly wrapped (File.expand_path(f), f.to_s, f.chomp(".rb")).
func (s *scanner) loopArg(ts []token, r [2]int, vars []string, dirList bool) bool {
	if dirList {
		return false // children/each_child yield bare names, joined by code we do not evaluate
	}
	seen := false
	for j := r[0]; j < r[1]; j++ {
		t := ts[j]
		switch t.kind {
		case tIdent:
			n := s.text(t)
			isVar := false
			for _, v := range vars {
				if n == v {
					isVar = true
				}
			}
			if isVar {
				if seen {
					return false
				}
				seen = true
				continue
			}
			if !loopWrappers[n] {
				return false
			}
		case tStr, tOp:
		default:
			return false
		}
	}
	return seen
}

// blockOpeners are keywords closed by `end`.
var blockOpeners = map[string]bool{"def": true, "class": true, "module": true, "case": true, "begin": true, "do": true}

// doEnd returns the index of the `end` closing the `do` at i (-1 when it cannot be found reliably).
func (s *scanner) doEnd(ts []token, i int) int {
	d := 1
	loopDo := false
	for j := i + 1; j < len(ts); j++ {
		t := ts[j]
		if t.kind == tNL || s.isOp(ts, j, ";") {
			loopDo = false
			continue
		}
		if t.kind != tIdent || s.isOp(ts, j-1, ".") || s.isOp(ts, j-1, "&.") || s.isOp(ts, j+1, ":") && !ts[j+1].sp {
			continue
		}
		switch n := s.text(t); {
		case n == "end":
			d--
			if d == 0 {
				return j
			}
		case n == "do" && loopDo:
			loopDo = false
		case blockOpeners[n]:
			if n == "def" && s.endlessDef(ts, j) {
				continue
			}
			d++
		case n == "if" || n == "unless" || n == "while" || n == "until" || n == "for":
			if s.stmtStart(ts, j) || s.isOp(ts, j-1, "=") || s.isOp(ts, j-1, "(") || s.isOp(ts, j-1, ",") {
				d++
				if n == "while" || n == "until" || n == "for" {
					loopDo = true
				}
			}
		}
	}
	return -1
}

// endlessDef: `def name(args) = expr` has no end.
func (s *scanner) endlessDef(ts []token, i int) bool {
	j := i + 1
	for j < len(ts) && ts[j].kind != tNL {
		if s.isOp(ts, j, "(") {
			c := s.closeOf(ts, j, len(ts))
			if c < 0 {
				return false
			}
			return s.isOp(ts, c+1, "=")
		}
		if s.isOp(ts, j, "=") {
			return true
		}
		j++
	}
	return false
}

// pathLike: a string that may name a file or directory of the repository.
func pathLike(v string) bool {
	if len(v) < 2 || len(v) > 256 || !strings.ContainsAny(v, "/.") || strings.Contains(v, "://") {
		return false
	}
	letter := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '"' || c == '\'' || c == '`' || c < 0x20 || c == '%' || c == '#':
			return false
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letter = true
		}
	}
	return letter
}

// stringRef records the path-like contents of a string literal.
func (s *scanner) stringRef(t token) {
	si := &s.strs[t.aux]
	switch si.kind {
	case sSingle, sDouble, sXStr, sWords, sIWords, sHeredoc:
	default:
		return
	}
	if si.kind == sDouble || si.kind == sHeredoc {
		if v, ok := s.strValue(t); ok && v.usable() && v.b != bStr && !(v.b == bRel && !v.dyn && (v.s == "." || v.s == "")) {
			s.add(model.ImportRbString, v.s, baseTag(v.b), v.dyn, t.line)
			return
		}
	}
	if si.kind == sHeredoc {
		return // a heredoc body is text or code, not a path
	}
	for _, p := range si.parts {
		if p.isCode {
			continue
		}
		v := p.lit
		if pathLike(v) && (si.kind != sWords && si.kind != sIWords) {
			s.add(model.ImportRbString, v, "", false, t.line)
			continue
		}
		for _, w := range strings.Fields(v) {
			if pathLike(w) && (strings.Contains(w, "/") || si.kind == sWords || si.kind == sIWords) {
				s.add(model.ImportRbString, w, "", false, t.line)
			}
		}
	}
}

// assignedTo returns the name of the single-assignment variable whose right-hand side starts at i (`name = Dir[...]`), or "".
func (s *scanner) assignedTo(ts []token, i int) string {
	if i < 2 || !s.isOp(ts, i-1, "=") || ts[i-2].kind != tIdent || !s.stmtStart(ts, i-2) || !s.sameList(ts) {
		return ""
	}
	n := s.text(ts[i-2])
	if d := s.vars[n]; d == nil || d.bad || d.n != 1 {
		return ""
	}
	return n
}

// sameScope: tokens i and j of the top-level list are in the same local-variable scope.
func (s *scanner) sameScope(ts []token, i, j int) bool {
	return s.sameList(ts) && s.scopes != nil && s.scopes[i] == s.scopes[j]
}
