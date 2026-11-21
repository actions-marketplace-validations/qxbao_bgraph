package dotnet

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/qxbao/bgraph/internal/tree"
)

// msFile is what bgraph reads out of one MSBuild file (project, .props, .targets, .projitems): the property definitions, the items and
// the imports, with every Condition ignored (union of all branches).
type msFile struct {
	path    string
	dir     string // directory of the file, repo-relative ("." for the root)
	props   []propDef
	items   []itemDef
	imports []importDef
	strs    []strVal // every other attribute value and element text (task parameters, property values...)
	err     error
}

// strVal is a string of the file that may name a file or directory the build reads (Copy SourceFiles, Exec Command...).
type strVal struct {
	value    string
	inTarget bool
}

type propDef struct{ name, value string }

type itemDef struct {
	typ      string // item type (Compile, ProjectReference...) or "HintPath" for a HintPath child element
	include  string
	cond     bool // the element or one of its ancestors has a Condition
	inTarget bool // declared inside a <Target>: computed at build time
}

type importDef struct {
	project string
	cond    bool
	sdk     bool // Sdk="..." attribute: an SDK import, not a repo file
}

// parseMSBuild reads an MSBuild file. A file that is not well-formed XML returns msFile.err.
func parseMSBuild(t tree.Tree, p string) *msFile {
	f := &msFile{path: p, dir: path.Dir(p)}
	b, err := t.Read(p)
	if err != nil {
		f.err = err
		return f
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	type frame struct {
		name string
		cond bool
	}
	var stack []frame
	inAnc := func(name string) bool {
		for _, fr := range stack {
			if fr.name == name {
				return true
			}
		}
		return false
	}
	condAnc := func() bool {
		for i, fr := range stack {
			if i > 0 && fr.cond {
				return true
			}
		}
		return false
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.err = err
			return f
		}
		switch el := tok.(type) {
		case xml.StartElement:
			name := el.Name.Local
			attr := map[string]string{}
			hasCond := false
			for _, a := range el.Attr {
				l := strings.ToLower(a.Name.Local)
				attr[l] = a.Value
				if l == "condition" {
					hasCond = true
				}
			}
			for _, a := range el.Attr {
				switch strings.ToLower(a.Name.Local) {
				case "condition", "include", "update", "remove", "name", "sdk", "label", "version", "project", "xmlns":
					continue
				}
				if v := strings.TrimSpace(a.Value); v != "" && len(stack) > 0 {
					f.strs = append(f.strs, strVal{v, inAnc("Target")})
				}
			}
			parent := ""
			if n := len(stack); n > 0 {
				parent = stack[n-1].name
			}
			switch {
			case name == "Import" && len(stack) > 0:
				_, sdk := attr["sdk"]
				if pr, ok := attr["project"]; ok {
					f.imports = append(f.imports, importDef{project: pr, cond: hasCond || condAnc(), sdk: sdk})
				}
				stack = append(stack, frame{name, hasCond})
			case parent == "PropertyGroup":
				val, ok := readText(dec)
				if !ok {
					f.err = errors.New("unterminated element " + name)
					return f
				}
				f.props = append(f.props, propDef{name: name, value: strings.TrimSpace(val)})
				if v := strings.TrimSpace(val); v != "" {
					f.strs = append(f.strs, strVal{v, inAnc("Target")})
				}
			case parent == "ItemGroup":
				it := itemDef{typ: name, cond: hasCond || condAnc(), inTarget: inAnc("Target")}
				if inc, ok := attr["include"]; ok {
					it.include = inc
					f.items = append(f.items, it)
				}
				stack = append(stack, frame{name, hasCond})
			case name == "HintPath" && len(stack) > 0 && inAnc("ItemGroup"):
				val, ok := readText(dec)
				if !ok {
					f.err = errors.New("unterminated element " + name)
					return f
				}
				f.items = append(f.items, itemDef{typ: "HintPath", include: strings.TrimSpace(val), cond: condAnc(), inTarget: inAnc("Target")})
			default:
				stack = append(stack, frame{name, hasCond})
			}
		case xml.CharData:
			if v := strings.TrimSpace(string(el)); v != "" && len(stack) > 0 {
				f.strs = append(f.strs, strVal{v, inAnc("Target")})
			}
		case xml.EndElement:
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		}
	}
	return f
}

// readText reads the character data of the current element up to its end tag, skipping nested elements.
func readText(dec *xml.Decoder) (string, bool) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		switch x := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 1 {
				sb.Write(x)
			}
		}
	}
	return sb.String(), true
}

// ---------------------------------------------------------------------------------------------------------------------------------
// Expression evaluation: the subset of MSBuild property syntax that names repository files.

// vroot is the virtual absolute root: a path "<vroot>/a/b" is the repo-relative path "a/b". Strings that start with another "/" or with a
// drive letter are absolute paths outside the repository.
const vroot = "/\x00repo"

func absDir(dir string) string {
	if dir == "." || dir == "" {
		return vroot + "/"
	}
	return vroot + "/" + dir + "/"
}

// Sentinel errors of eval.
var (
	errIgnore   = errors.New("build output or generated location")
	errExternal = errors.New("toolchain location outside the repository")
)

type propVal struct {
	raw  string
	file string // repo-relative file that defines it (for $(MSBuildThisFile...))
}

// env is the evaluation context of one project: every property defined anywhere in its import closure (all values, since conditions
// are ignored), the project itself and the solution directories.
type env struct {
	t       tree.Tree
	project string // repo-relative project file
	props   map[string][]propVal
	slnDirs []string // directories (repo-relative) of the .sln/.slnx files in the ancestors of the project
	// lenient: the expression sits under a Condition, so a property defined nowhere in the repository (it may come from the
	// environment, the command line or the SDK) that is glued to literal text of its path segment is taken to be any value ("*"),
	// not an error.
	lenient bool
}

const maxCombos = 64

// scalarProps take one of a few values per build (Configuration...): they are replaced by "*" so that a path built from them becomes a
// glob that matches every possible value.
var scalarProps = map[string]bool{"configuration": true, "platform": true, "targetframework": true, "runtimeidentifier": true,
	"targetframeworkidentifier": true, "buildconfiguration": true, "platformname": true}

// outputProps name build output locations: an item that lives there is generated, not a repository input.
var outputProps = map[string]bool{"outdir": true, "outputpath": true, "intermediateoutputpath": true, "baseintermediateoutputpath": true,
	"baseoutputpath": true, "targetdir": true, "targetpath": true, "projectextensionspath": true, "msbuildprojectextensionspath": true,
	"targetfilename": true, "documentationfile": true, "packageoutputpath": true, "publishdir": true, "nugetpackageroot": true,
	"nugetpackagesdirectory": true, "artifactspath": true, "artifactsbinoutput": true, "userprofile": true, "homedrive": true, "homepath": true, "home": true}

// externalProps are the locations of the installed toolchain.
var externalProps = map[string]bool{"msbuildtoolspath": true, "msbuildbinpath": true, "msbuildextensionspath": true,
	"msbuildextensionspath32": true, "msbuildextensionspath64": true, "msbuildsdkspath": true, "vstoolspath": true,
	"msbuildprogramfiles32": true, "msbuildruntimetype": true, "dotnetroot": true, "netcoretargetingpackroot": true,
	"msbuildtoolsversion": true, "visualstudioversion": true, "vsinstalldir": true}

// eval expands every $(...) of s in the scope of file this. It returns every possible value (union over multiple definitions).
func (e *env) eval(s string, this string, depth int) ([]string, error) {
	if depth > 8 {
		return nil, errors.New("property recursion too deep")
	}
	if strings.Contains(s, "@(") || strings.Contains(s, "%(") {
		return nil, errors.New("item list or metadata reference")
	}
	results := []string{""}
	for len(s) > 0 {
		i := strings.Index(s, "$(")
		if i < 0 {
			results = appendAll(results, []string{s})
			break
		}
		if i > 0 {
			results = appendAll(results, []string{s[:i]})
		}
		j := matchParen(s, i+1)
		if j < 0 {
			return nil, errors.New("unbalanced $(")
		}
		// A path fragment glued to literal text of its segment ("eng/$(ProjectType).targets") can stand for "*"; a whole segment
		// ("$(HumanizerProject)") may hold a path with separators, which "*" does not cover.
		bounded := (i > 0 && s[i-1] != '/' && s[i-1] != '\\') || (j+1 < len(s) && s[j+1] != '/' && s[j+1] != '\\')
		vals, err := e.evalExpr(s[i+2:j], this, depth, bounded)
		if err != nil {
			return nil, err
		}
		results = appendAll(results, vals)
		if len(results) > maxCombos {
			return nil, errors.New("too many property combinations")
		}
		s = s[j+1:]
	}
	return results, nil
}

func appendAll(prefixes, suffixes []string) []string {
	out := make([]string, 0, len(prefixes)*len(suffixes))
	for _, p := range prefixes {
		for _, s := range suffixes {
			out = append(out, p+s)
		}
	}
	return out
}

// matchParen returns the index of the ')' matching the '(' at s[open], honouring quotes; -1 if unbalanced.
func matchParen(s string, open int) int {
	depth := 0
	var quote byte
	for i := open; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (e *env) evalExpr(expr, this string, depth int, bounded bool) ([]string, error) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "[") {
		return e.evalFunc(expr, this, depth)
	}
	if strings.ContainsAny(expr, ".(:") {
		// Instance method on a scalar or output property ($(Configuration.ToLowerInvariant())): same classification as the property.
		if base, _, ok := strings.Cut(expr, "."); ok && !strings.ContainsAny(base, "(:") {
			switch lb := strings.ToLower(strings.TrimSpace(base)); {
			case scalarProps[lb]:
				return []string{"*"}, nil
			case outputProps[lb]:
				return nil, errIgnore
			}
		}
		return nil, errors.New("unsupported property function $(" + expr + ")")
	}
	name := strings.ToLower(expr)
	thisDir := path.Dir(this)
	projDir := path.Dir(e.project)
	switch name {
	case "msbuildthisfiledirectory":
		return []string{absDir(thisDir)}, nil
	case "msbuildthisfile":
		return []string{path.Base(this)}, nil
	case "msbuildthisfilename":
		return []string{strings.TrimSuffix(path.Base(this), path.Ext(this))}, nil
	case "msbuildthisfileextension":
		return []string{path.Ext(this)}, nil
	case "msbuildthisfilefullpath":
		return []string{vroot + "/" + this}, nil
	case "msbuildprojectdirectory":
		return []string{strings.TrimSuffix(absDir(projDir), "/")}, nil
	case "msbuildprojectfile":
		return []string{path.Base(e.project)}, nil
	case "msbuildprojectname":
		return []string{strings.TrimSuffix(path.Base(e.project), path.Ext(e.project))}, nil
	case "msbuildprojectextension":
		return []string{path.Ext(e.project)}, nil
	case "msbuildprojectfullpath":
		return []string{vroot + "/" + e.project}, nil
	case "solutiondir":
		if len(e.slnDirs) == 0 {
			return nil, errors.New("$(SolutionDir) with no solution file above the project")
		}
		out := make([]string, len(e.slnDirs))
		for i, d := range e.slnDirs {
			out[i] = absDir(d)
		}
		return out, nil
	}
	if vals, ok := e.props[name]; ok {
		var out []string
		for _, v := range vals {
			sub, err := e.eval(v.raw, v.file, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return dedupe(out), nil
	}
	switch {
	case scalarProps[name]:
		return []string{"*"}, nil
	case outputProps[name]:
		return nil, errIgnore
	case externalProps[name]:
		return nil, errExternal
	}
	if e.lenient && bounded {
		return []string{"*"}, nil
	}
	return nil, errors.New("undefined property $(" + expr + ")")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// splitArgs splits a function argument list at top-level commas.
func splitArgs(s string) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" || len(out) > 0 {
		out = append(out, s[start:])
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"' || s[0] == '`') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// evalFunc evaluates "[Class]::Name(args)".
func (e *env) evalFunc(expr, this string, depth int) ([]string, error) {
	end := strings.Index(expr, "]::")
	if end < 0 {
		return nil, errors.New("unsupported property function $(" + expr + ")")
	}
	class := strings.ToLower(strings.TrimSpace(expr[1:end]))
	rest := expr[end+3:]
	op := strings.Index(rest, "(")
	if op < 0 || matchParen(rest, op) != len(rest)-1 {
		return nil, errors.New("unsupported property function $(" + expr + ")")
	}
	fn := strings.ToLower(strings.TrimSpace(rest[:op]))
	var args [][]string
	for _, a := range splitArgs(rest[op+1 : len(rest)-1]) {
		vals, err := e.eval(unquote(a), this, depth+1)
		if err != nil {
			return nil, err
		}
		args = append(args, vals)
	}
	// Cartesian product of the arguments.
	combos := [][]string{{}}
	for _, vals := range args {
		var next [][]string
		for _, c := range combos {
			for _, v := range vals {
				next = append(next, append(append([]string(nil), c...), v))
			}
		}
		combos = next
		if len(combos) > maxCombos {
			return nil, errors.New("too many property combinations")
		}
	}
	var out []string
	for _, c := range combos {
		v, err := e.call(class, fn, c, this)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return dedupe(out), nil
}

func slashes(s string) string { return strings.ReplaceAll(s, "\\", "/") }

// combine is System.IO.Path.Combine: a rooted segment replaces what came before.
func combine(segs []string) string {
	cur := ""
	for _, s := range segs {
		s = slashes(s)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "/") || isDrive(s) || cur == "" {
			cur = s
			continue
		}
		if !strings.HasSuffix(cur, "/") {
			cur += "/"
		}
		cur += s
	}
	return cur
}

func isDrive(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

// fullPath resolves a relative path against the project directory and cleans it, keeping a trailing slash.
func (e *env) fullPath(s string) string {
	s = slashes(s)
	trail := strings.HasSuffix(s, "/")
	if !strings.HasPrefix(s, "/") && !isDrive(s) {
		s = strings.TrimSuffix(absDir(path.Dir(e.project)), "/") + "/" + s
	}
	s = path.Clean(s)
	if trail && s != "/" {
		s += "/"
	}
	return s
}

func (e *env) call(class, fn string, a []string, this string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("unsupported property function [%s]::%s with %d arguments", class, fn, len(a))
	}
	switch class + "::" + fn {
	case "msbuild::gettargetframeworkidentifier", "msbuild::gettargetframeworkversion", "msbuild::gettargetplatformidentifier",
		"msbuild::gettargetplatformversion":
		return "*", nil // one value per target framework: any
	case "msbuild::getpathoffileabove":
		if len(a) < 1 || len(a) > 2 {
			return bad()
		}
		start := absDir(path.Dir(path.Dir(this)))
		if len(a) == 2 && a[1] != "" {
			start = e.fullPath(a[1])
		}
		return e.fileAbove(start, a[0], true), nil
	case "msbuild::getdirectorynameoffileabove":
		if len(a) != 2 {
			return bad()
		}
		return e.fileAbove(e.fullPath(a[0]), a[1], false), nil
	case "msbuild::normalizepath", "msbuild::normalizedirectory":
		if len(a) < 1 {
			return bad()
		}
		p := e.fullPath(combine(a))
		if fn == "normalizedirectory" && !strings.HasSuffix(p, "/") {
			p += "/"
		}
		return p, nil
	case "msbuild::ensuretrailingslash":
		if len(a) != 1 {
			return bad()
		}
		p := slashes(a[0])
		if p != "" && !strings.HasSuffix(p, "/") {
			p += "/"
		}
		return p, nil
	case "msbuild::valueordefault":
		if len(a) != 2 {
			return bad()
		}
		if a[0] != "" {
			return a[0], nil
		}
		return a[1], nil
	case "system.io.path::combine":
		if len(a) < 1 {
			return bad()
		}
		return combine(a), nil
	case "system.io.path::getfullpath":
		if len(a) < 1 || len(a) > 2 {
			return bad()
		}
		if len(a) == 2 {
			return e.fullPath(combine(a)), nil
		}
		return e.fullPath(a[0]), nil
	case "system.io.path::getdirectoryname":
		if len(a) != 1 {
			return bad()
		}
		p := slashes(a[0])
		p = strings.TrimSuffix(p, "/")
		i := strings.LastIndex(p, "/")
		if i < 0 {
			return "", nil
		}
		return p[:i], nil
	}
	return bad()
}

// fileAbove searches start and its ancestors inside the repository for file. withName: return the file path, else the directory.
// Not found: "" (the file may live above the repository, which bgraph cannot see).
func (e *env) fileAbove(start, file string, withName bool) string {
	rel, ok := strings.CutPrefix(start, vroot)
	if !ok {
		return ""
	}
	d := path.Clean("/" + rel)
	d = strings.TrimPrefix(d, "/")
	if d == "" {
		d = "."
	}
	file = slashes(file)
	for {
		p := path.Join(d, file)
		if e.t.Has(p) {
			if withName {
				return vroot + "/" + p
			}
			return strings.TrimSuffix(absDir(d), "/")
		}
		if d == "." {
			return ""
		}
		d = path.Dir(d)
	}
}

// target is a repository location named by an evaluated path.
type target struct {
	rel     string // repo-relative, cleaned (may hold wildcards)
	outside bool   // absolute path outside the repository or above its root
}

// toTarget turns an evaluated string into repo-relative locations. A relative string is tried against every base directory
// (relative paths of items declared in an imported file are relative to the importing project in MSBuild, but a few tools read them
// relative to the file; the union is a superset).
func toTarget(s string, bases []string) []target {
	s = slashes(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	if rel, ok := strings.CutPrefix(s, vroot); ok {
		return []target{clampRel(path.Clean("." + rel))}
	}
	if strings.HasPrefix(s, "/") || isDrive(s) {
		return []target{{outside: true, rel: s}}
	}
	var out []target
	for _, b := range bases {
		out = append(out, clampRel(path.Join(b, s)))
	}
	return out
}

func clampRel(p string) target {
	if p == ".." || strings.HasPrefix(p, "../") {
		return target{outside: true, rel: p}
	}
	return target{rel: p}
}

func hasWildcard(s string) bool { return strings.ContainsAny(s, "*?") }

// globOf converts an MSBuild path with wildcards into a bgraph glob: the literal characters that are glob syntax are escaped.
func globOf(rel string) string {
	var sb strings.Builder
	for _, c := range rel {
		if strings.ContainsRune(`[]{}\`, c) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(c)
	}
	return sb.String()
}
