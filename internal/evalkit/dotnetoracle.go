package evalkit

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// DotnetTestLoc is where one .NET FAIL_TO_PASS test name is defined, as far as a source scan can tell.
//
// Phase 1 (this file) only locates test source files. TODO(dotnet-phase-2): map Files to project units with internal/eco/dotnet
// (file -> owning .csproj, T1 oracle); ambiguous names (partial classes, same class in several projects) should require the
// units of ALL candidate files, NotFound names must be reported as unmapped, never silently dropped.
type DotnetTestLoc struct {
	Name string
	// Form: "dotted" (Namespace.Class.Method[(args)]), "nested" (Namespace.Outer+Inner.Method), "prefixed" (project/dll hint,
	// "src/Foo.Tests::Namespace.Class.Method"), "bare" (method name only: needs a method search), "other".
	Form      string
	Hint      string   // text before "::" for the prefixed form (a project directory or a test assembly/result file)
	Class     string   // resolved Namespace.Class ("" when not resolved)
	Files     []string // candidate .cs files (sorted)
	Ambiguous bool
	NotFound  bool
	Scanned   bool // resolved only by the full scan of test-like .cs files (file name differs from the class name, or a bare method name)
	InPatch   bool // some candidate file is touched by the test patch (set by DotnetLocateTask)
}

var (
	dotnetNsRe   = regexp.MustCompile(`(?m)^[ \t]*namespace[ \t]+([\w.]+)`)
	dotnetDeclRe = regexp.MustCompile(`\b(?:class|struct|record|interface)[ \t]+([A-Za-z_]\w*)`)
	dotnetArity  = regexp.MustCompile("`\\d+$")
	// A method declaration: modifiers, one type token, the name, optional generics, "(".
	dotnetMethodKeywords = map[string]bool{"await": true, "return": true, "new": true, "throw": true, "yield": true, "else": true, "in": true, "using": true}
	dotnetMethodLineRe   = regexp.MustCompile(`^[ \t]*(?:(?:public|internal|private|protected|static|async|virtual|override|sealed|unsafe|new|partial|extern)[ \t]+)*([\w<>\[\],.?]+)[ \t]+(\w+)[ \t]*(?:<[^>\n]*>)?[ \t]*\(`)
)

func dotnetIsSrc(p string) bool { return strings.HasSuffix(p, ".cs") }

// dotnetTestish: some directory is named like a test project (UnitTests, Foo.Tests, test/, tests/, ...). Bounds the full scan.
func dotnetTestish(p string) bool {
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		l := strings.ToLower(s)
		if strings.Contains(l, "test") || strings.Contains(l, "spec") {
			return true
		}
	}
	return false
}

// dotnetStems are the class-name stems of a file: "Foo.cs" and the partial-class spellings "Foo.Bar.cs" / "Foo.Designer.cs" both index "Foo".
func dotnetStems(p string) []string {
	b := strings.TrimSuffix(path.Base(p), ".cs")
	out := []string{b}
	if i := strings.IndexByte(b, '.'); i > 0 {
		out = append(out, b[:i])
	}
	return out
}

// dotnetCand is one reading of a test name: type cls in namespace ns.
type dotnetCand struct{ ns, cls string }

func parseDotnetName(name string) (form, hint string, cands []dotnetCand, method string) {
	n := strings.TrimSpace(name)
	form = "dotted"
	// "::" is a project/assembly prefix only before the argument list ("ipv6: \"2001:db8::\"" is an argument).
	if i := strings.Index(n, "::"); i >= 0 && (strings.IndexByte(n, '(') < 0 || i < strings.IndexByte(n, '(')) {
		hint, n, form = n[:i], n[i+2:], "prefixed"
	}
	// Cut theory arguments and generic arguments: Method(a: 1), Method<T>(...). Both can contain dots, so cut before splitting.
	if i := strings.IndexAny(n, "(<"); i >= 0 {
		n = n[:i]
	}
	n = strings.TrimSpace(n)
	if n == "" {
		return "other", hint, nil, ""
	}
	segs := nonEmpty(strings.Split(n, "."))
	if len(segs) == 1 && !strings.Contains(segs[0], "+") {
		return "bare", hint, nil, segs[0]
	}
	if strings.Contains(n, "+") && form == "dotted" {
		form = "nested"
	}
	typ := func(s string) string { // Outer+Inner -> Outer; Foo`1 -> Foo
		if i := strings.IndexByte(s, '+'); i >= 0 {
			s = s[:i]
		}
		return dotnetArity.ReplaceAllString(s, "")
	}
	method = segs[len(segs)-1]
	// The class is the segment before the method; also try shorter prefixes (nested types spelled with '.') and the full name (class only).
	for k := len(segs) - 1; k >= 1; k-- {
		cands = append(cands, dotnetCand{strings.Join(segs[:k-1], "."), typ(segs[k-1])})
	}
	cands = append(cands, dotnetCand{strings.Join(segs[:len(segs)-1], "."), typ(segs[len(segs)-1])})
	return form, hint, cands, method
}

// dotnetFile holds what a file declares: its namespaces and type names.
type dotnetFile struct {
	ns    []string
	types map[string]bool
	src   string
}

func readDotnetFile(t tree.Tree, p string, cache map[string]*dotnetFile) *dotnetFile {
	if f, ok := cache[p]; ok {
		return f
	}
	f := &dotnetFile{types: map[string]bool{}}
	if b, err := t.Read(p); err == nil {
		f.src = string(b)
		for _, m := range dotnetNsRe.FindAllStringSubmatch(f.src, -1) {
			f.ns = append(f.ns, m[1])
		}
		for _, m := range dotnetDeclRe.FindAllStringSubmatch(f.src, -1) {
			f.types[m[1]] = true
		}
	}
	cache[p] = f
	return f
}

func (f *dotnetFile) declares(c dotnetCand) bool {
	if !f.types[c.cls] {
		return false
	}
	if c.ns == "" {
		return true
	}
	for _, n := range f.ns {
		if n == c.ns {
			return true
		}
	}
	return false
}

// LocateDotnetTests locates the .cs file(s) declaring the class of each FAIL_TO_PASS name in tree t (the P tree: base + test patch).
// Pass 1 looks at files named like the class (Foo.cs, partials Foo.*.cs) and verifies namespace + declaration by reading them; pass 2 reads
// every test-like .cs file (class in a differently named file; bare method names, which need a method-declaration search).
func LocateDotnetTests(t tree.Tree, names []string) []DotnetTestLoc {
	out := make([]DotnetTestLoc, len(names))
	cands := make([][]dotnetCand, len(names))
	methods := make([]string, len(names))
	byStem := map[string][]string{}
	for _, p := range t.Files() {
		if dotnetIsSrc(p) {
			for _, s := range dotnetStems(p) {
				byStem[s] = append(byStem[s], p)
			}
		}
	}
	forms := make([]string, len(names))
	hints := make([]string, len(names))
	for i, n := range names {
		forms[i], hints[i], cands[i], methods[i] = parseDotnetName(n)
	}
	// Prefetch (partial clones): only the files named like some class guess.
	if pf, ok := t.(jvmPrefetcher); ok {
		want := map[string]bool{}
		for i := range names {
			for _, c := range cands[i] {
				for _, p := range byStem[c.cls] {
					want[p] = true
				}
			}
		}
		pf.Prefetch(func(p string) bool { return want[p] })
	}
	cache := map[string]*dotnetFile{}
	for i, n := range names {
		l := DotnetTestLoc{Name: n, Form: forms[i], Hint: hints[i]}
		for _, c := range cands[i] {
			var files []string
			seen := map[string]bool{}
			for _, p := range byStem[c.cls] {
				if !seen[p] && readDotnetFile(t, p, cache).declares(c) {
					seen[p] = true
					files = append(files, p)
				}
			}
			if len(files) > 0 {
				l.Files, l.Class = files, strings.TrimPrefix(c.ns+"."+c.cls, ".")
				break
			}
		}
		out[i] = l
	}
	var scanned bool
	for i := range out {
		l := &out[i]
		if len(l.Files) > 0 || l.Form == "other" {
			continue
		}
		if !scanned {
			scanned = true
			if pf, ok := t.(jvmPrefetcher); ok {
				pf.Prefetch(func(p string) bool { return dotnetIsSrc(p) && dotnetTestish(p) })
			}
			for _, p := range t.Files() {
				if dotnetIsSrc(p) && dotnetTestish(p) {
					readDotnetFile(t, p, cache)
				}
			}
		}
		l.Files, l.Class = dotnetScan(t, cache, cands[i], methods[i], l.Form == "bare")
		l.Scanned = len(l.Files) > 0
	}
	for i := range out {
		l := &out[i]
		sort.Strings(l.Files)
		l.NotFound = len(l.Files) == 0
		l.Ambiguous = len(l.Files) > 1
	}
	return out
}

func dotnetScan(t tree.Tree, cache map[string]*dotnetFile, cands []dotnetCand, method string, bare bool) ([]string, string) {
	paths := make([]string, 0, len(cache))
	for p := range cache {
		if dotnetTestish(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if bare {
		var files []string
		for _, p := range paths {
			for _, ln := range strings.Split(cache[p].src, "\n") {
				if m := dotnetMethodLineRe.FindStringSubmatch(ln); m != nil && m[2] == method && !dotnetMethodKeywords[m[1]] {
					files = append(files, p)
					break
				}
			}
		}
		return files, ""
	}
	for _, c := range cands {
		var files []string
		for _, p := range paths {
			if cache[p].declares(c) {
				files = append(files, p)
			}
		}
		if len(files) > 0 {
			return files, strings.TrimPrefix(c.ns+"."+c.cls, ".")
		}
	}
	return nil, ""
}

// DotnetLocateRow is the locator outcome for one task.
type DotnetLocateRow struct {
	ID      string          `json:"instance_id"`
	Repo    string          `json:"repo"`
	Source  string          `json:"source"`
	F2P     int             `json:"f2p_count"`
	Err     string          `json:"error,omitempty"`
	Locs    []DotnetTestLoc `json:"-"`
	Unique  int             `json:"unique"`
	Ambig   int             `json:"ambiguous"`
	NotFnd  int             `json:"not_found"`
	InPatch int             `json:"in_patch"`
}

// DotnetLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task and runs LocateDotnetTests over it.
func DotnetLocateTask(ctx context.Context, repoDir, idxFile string, t Task) DotnetLocateRow {
	row := DotnetLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source}
	ids, err := ParseF2P(t.F2P)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	row.F2P = len(ids)
	ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		row.Err = "test_patch: " + err.Error()
		return row
	}
	gt, err := newGitTreeWant(ctx, repoDir, p, func(string) bool { return false })
	if err != nil {
		row.Err = err.Error()
		return row
	}
	defer gt.Close()
	row.Locs = LocateDotnetTests(jvmGitTree{gt, ctx, repoDir}, ids)
	tp := patchFiles(t.TestPatch)
	for i := range row.Locs {
		l := &row.Locs[i]
		for _, f := range l.Files {
			if tp[f] {
				l.InPatch = true
			}
		}
		if l.InPatch {
			row.InPatch++
		}
		switch {
		case l.NotFound:
			row.NotFnd++
		case l.Ambiguous:
			row.Ambig++
		default:
			row.Unique++
		}
	}
	return row
}
