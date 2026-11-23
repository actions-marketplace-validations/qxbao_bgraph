package lockfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Formats of yarn.lock and bun.lock graphs.
const (
	FormatYarn  = "yarn"       // yarn classic (v1)
	FormatBerry = "yarn-berry" // yarn 2 and later
	FormatBun   = "bun"        // text bun.lock
)

// Files gives a parser read access to the tree that holds the lockfile, for formats whose lockfile alone does not
// describe the projects: yarn v1 records no workspace (their manifests declare the projects and their direct
// dependencies), yarn Berry applies the root manifest's resolutions and .yarnrc.yml catalogs and package extensions,
// and bun.lock drops some workspace declarations. Paths are repo-relative. tree.Tree implements it.
type Files interface {
	Read(p string) ([]byte, error)
	// Files returns every file of the tree.
	Files() []string
}

// ParseFiles is Parse with access to the lockfile's tree (see Files); yarn.lock and bun.lock need it (nil is an
// error).
func ParseFiles(name, dir string, data []byte, files Files) (*Graph, error) {
	switch path.Base(name) {
	case "yarn.lock":
		return ParseYarn(dir, data, files)
	case "bun.lock":
		return ParseBun(dir, data, files)
	}
	return Parse(name, dir, data)
}

// ParseYarn reads a yarn.lock: classic (v1, "# yarn lockfile v1") or Berry (a __metadata entry).
func ParseYarn(dir string, data []byte, files Files) (*Graph, error) {
	if berryHeader(data) {
		return ParseBerry(dir, data, files)
	}
	return ParseYarnV1(dir, data, files)
}

// berryHeader reports whether a yarn.lock is a Berry lockfile (YAML with a top-level __metadata key).
func berryHeader(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		l := strings.TrimRight(string(line), "\r")
		if l == "__metadata:" || strings.HasPrefix(l, `"__metadata":`) {
			return true
		}
	}
	return false
}

// yarnBlock is one entry of a yarn v1 lockfile: the patterns that resolve to it and its fields.
type yarnBlock struct {
	keys   []string
	fields map[string]any // version, resolved, integrity, uid, dependencies, optionalDependencies...
}

// parseYarnV1Syntax parses the yarn v1 lockfile syntax (an indentation-based key/value format) into blocks indexed by
// pattern.
func parseYarnV1Syntax(data []byte) (map[string]*yarnBlock, []*yarnBlock, error) {
	type frame struct {
		indent int
		obj    map[string]any
	}
	top := map[string]any{}
	stack := []frame{{-1, top}}
	blockOf := map[string]*yarnBlock{}
	var blocks []*yarnBlock
	lines := strings.Split(string(data), "\n")
	sawHeader := false
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trim := strings.TrimLeft(line, " ")
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "#") {
			if strings.TrimSpace(trim) == "# yarn lockfile v1" {
				sawHeader = true
			}
			continue
		}
		indent := len(line) - len(trim)
		if strings.HasPrefix(trim, "\t") || indent%2 != 0 {
			return nil, nil, fmt.Errorf("yarn.lock line %d: bad indentation", i+1)
		}
		toks, err := yarnTokens(trim)
		if err != nil {
			return nil, nil, fmt.Errorf("yarn.lock line %d: %v", i+1, err)
		}
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		if indent != parent.indent+2 && !(len(stack) == 1 && indent == 0) {
			return nil, nil, fmt.Errorf("yarn.lock line %d: unexpected indentation", i+1)
		}
		if n := len(toks); n >= 2 && toks[n-1] == yarnColon {
			// "key[, key...]:" opens an object.
			var keys []string
			for j := 0; j < n-1; j++ {
				if j%2 == 1 {
					if toks[j] != yarnComma {
						return nil, nil, fmt.Errorf("yarn.lock line %d: expected a comma", i+1)
					}
					continue
				}
				if toks[j] == yarnComma || toks[j] == yarnColon {
					return nil, nil, fmt.Errorf("yarn.lock line %d: expected a key", i+1)
				}
				keys = append(keys, toks[j])
			}
			obj := map[string]any{}
			for _, k := range keys {
				if _, dup := parent.obj[k]; dup {
					return nil, nil, fmt.Errorf("yarn.lock line %d: duplicate key %q", i+1, k)
				}
				parent.obj[k] = obj
			}
			if len(stack) == 1 {
				b := &yarnBlock{keys: keys, fields: obj}
				blocks = append(blocks, b)
				for _, k := range keys {
					blockOf[k] = b
				}
			}
			stack = append(stack, frame{indent, obj})
			continue
		}
		if len(toks) != 2 || toks[0] == yarnColon || toks[0] == yarnComma || toks[1] == yarnColon || toks[1] == yarnComma {
			return nil, nil, fmt.Errorf("yarn.lock line %d: expected \"key value\"", i+1)
		}
		if len(stack) == 1 {
			return nil, nil, fmt.Errorf("yarn.lock line %d: value at the top level", i+1)
		}
		if _, dup := parent.obj[toks[0]]; dup {
			return nil, nil, fmt.Errorf("yarn.lock line %d: duplicate key %q", i+1, toks[0])
		}
		parent.obj[toks[0]] = toks[1]
	}
	if !sawHeader {
		return nil, nil, fmt.Errorf("yarn.lock: no \"# yarn lockfile v1\" header: lockfile version not supported")
	}
	return blockOf, blocks, nil
}

// Token markers (strings that cannot be a decoded token value: they contain a NUL byte).
const (
	yarnColon = "\x00:"
	yarnComma = "\x00,"
)

// yarnTokens splits one line into strings, colons and commas (yarn's lockfile tokenizer: quoted JSON strings, bare
// words up to a space, colon or comma; booleans and numbers are kept as their text).
func yarnTokens(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ':
			i++
		case c == ':':
			out = append(out, yarnColon)
			i++
		case c == ',':
			out = append(out, yarnComma)
			i++
		case c == '"':
			j := i + 1
			for ; j < len(s); j++ {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == '"' {
					break
				}
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string")
			}
			v, err := strconv.Unquote(s[i : j+1])
			if err != nil {
				var x string
				if json.Unmarshal([]byte(s[i:j+1]), &x) != nil {
					return nil, fmt.Errorf("bad string %s", s[i:j+1])
				}
				v = x
			}
			out = append(out, v)
			i = j + 1
		case c == '#':
			return out, nil
		default:
			j := i
			for ; j < len(s) && s[j] != ' ' && s[j] != ':' && s[j] != ','; j++ {
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out, nil
}

// splitPattern splits "name@range" (a scoped name starts with @).
func splitPattern(p string) (name, rng string, ok bool) {
	i := strings.IndexByte(p[min(1, len(p)):], '@')
	if i < 0 {
		return p, "", false
	}
	i++
	return p[:i], p[i+1:], true
}

// yarnManifest is the part of a package.json yarn v1 reads for a workspace.
type yarnManifest struct {
	dir     string
	name    string
	version string
	deps    map[string]map[string]string // section → name → range
}

var yarnManifestSections = []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"}

func readYarnManifest(files Files, dir string) (*yarnManifest, error) {
	p := path.Join(dir, "package.json")
	b, err := files.Read(p)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	ym := &yarnManifest{dir: dir, deps: map[string]map[string]string{}}
	if v, ok := m["name"]; ok {
		_ = json.Unmarshal(v, &ym.name)
	}
	if v, ok := m["version"]; ok {
		_ = json.Unmarshal(v, &ym.version)
	}
	for _, s := range yarnManifestSections {
		v, ok := m[s]
		if !ok || string(v) == "null" {
			continue
		}
		var d map[string]string
		if err := json.Unmarshal(v, &d); err != nil {
			return nil, fmt.Errorf("%s: %s is not a map of strings", p, s)
		}
		ym.deps[s] = d
	}
	return ym, nil
}

// yarnWorkspacePatterns reads the "workspaces" field of a root manifest (an array, or an object with "packages").
func yarnWorkspacePatterns(files Files, dir string) ([]string, error) {
	p := path.Join(dir, "package.json")
	b, err := files.Read(p)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	raw, ok := m["workspaces"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list, nil
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%s: workspaces is neither a list nor an object with packages", p)
	}
	return obj.Packages, nil
}

// expandWorkspaces returns the directories (repo-relative) matched by workspace glob patterns relative to dir: a
// directory matches when it holds a package.json and is not inside node_modules. Supported glob syntax: *, ?, **,
// character classes and {a,b}; anything else (negations, extglobs) is an error.
func expandWorkspaces(files Files, dir string, patterns []string) ([]string, error) {
	var res []*regexp.Regexp
	for _, p := range patterns {
		p = strings.TrimSuffix(strings.TrimPrefix(path.Clean(strings.TrimPrefix(p, "./")), "./"), "/")
		if strings.HasPrefix(p, "!") || strings.HasPrefix(p, "../") || p == ".." || p == "." {
			return nil, fmt.Errorf("workspace pattern %q not supported", p)
		}
		for _, q := range expandBraces(p) {
			re, err := globRegexp(q)
			if err != nil {
				return nil, err
			}
			res = append(res, re)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files.Files() {
		if path.Base(f) != "package.json" {
			continue
		}
		d := path.Dir(f)
		rel := d
		if dir != "." {
			r, ok := strings.CutPrefix(d, dir+"/")
			if !ok {
				continue
			}
			rel = r
		} else if d == "." {
			continue
		}
		if strings.Contains("/"+rel+"/", "/node_modules/") || seen[d] {
			continue
		}
		for _, re := range res {
			if re.MatchString(rel) {
				seen[d] = true
				out = append(out, d)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// expandBraces expands {a,b} alternatives (not nested).
func expandBraces(p string) []string {
	i := strings.IndexByte(p, '{')
	if i < 0 {
		return []string{p}
	}
	j := strings.IndexByte(p[i:], '}')
	if j < 0 {
		return []string{p}
	}
	j += i
	var out []string
	for _, alt := range strings.Split(p[i+1:j], ",") {
		out = append(out, expandBraces(p[:i]+alt+p[j+1:])...)
	}
	return out
}

// globRegexp compiles a slash-separated glob ("**" crosses zero or more directories; a wildcard never matches a
// leading dot).
func globRegexp(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	segs := strings.Split(g, "/")
	for i, s := range segs {
		if s == "**" {
			if i == 0 {
				if len(segs) == 1 {
					return nil, fmt.Errorf("workspace pattern %q not supported", g)
				}
				b.WriteString(`(?:[^/.][^/]*/)*`) // ends with its own "/"
				continue
			}
			b.WriteString(`(?:/[^/.][^/]*)*`) // starts with its own "/"
			continue
		}
		if i > 0 && !(i == 1 && segs[0] == "**") {
			b.WriteString("/")
		}
		for k := 0; k < len(s); k++ {
			c := s[k]
			switch c {
			case '*':
				if k == 0 {
					b.WriteString(`(?:[^/.][^/]*)?`)
				} else {
					b.WriteString(`[^/]*`)
				}
			case '?':
				if k == 0 {
					b.WriteString(`[^/.]`)
				} else {
					b.WriteString(`[^/]`)
				}
			case '[':
				e := strings.IndexByte(s[k:], ']')
				if e < 0 {
					return nil, fmt.Errorf("workspace pattern %q: unterminated class", g)
				}
				cls := s[k+1 : k+e]
				if strings.HasPrefix(cls, "!") {
					cls = "^" + cls[1:]
				}
				b.WriteString("[" + cls + "]")
				k += e
			case '(', ')', '|':
				return nil, fmt.Errorf("workspace pattern %q not supported", g)
			default:
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// exoticRange: a yarn v1 range that is not resolved by the registry (and so never by a workspace).
func exoticRange(r string) bool {
	for _, p := range []string{"file:", "link:", "git:", "git+", "git@", "http:", "https:", "github:", "gitlab:", "bitbucket:", "gist:", "portal:", "workspace:", "patch:", "exec:"} {
		if strings.HasPrefix(r, p) {
			return true
		}
	}
	// GitHub shorthand "user/repo[#ref]"
	if strings.Contains(r, "/") && !strings.HasPrefix(r, "npm:") && !strings.ContainsAny(r, " <>=^~|") {
		return true
	}
	return false
}

// ParseYarnV1 reads a yarn classic (v1) lockfile. The projects are the root manifest in dir and its workspaces;
// their direct dependencies come from their manifests (files is required). A dependency resolves, like yarn does, to
// a workspace when its name is a workspace's and the range accepts the workspace's version, else to the lockfile entry
// whose patterns contain "name@range". A dependency that resolves to nothing is an error (level L0).
func ParseYarnV1(dir string, data []byte, files Files) (*Graph, error) {
	blockOf, blocks, err := parseYarnV1Syntax(data)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return nil, fmt.Errorf("yarn.lock v1: the workspace manifests are not available")
	}
	g := &Graph{Format: FormatYarn, Version: "1", Nodes: map[string]*Node{}, Projects: map[string]string{}, Extra: map[string]string{}}
	root, err := readYarnManifest(files, dir)
	if err != nil {
		return nil, fmt.Errorf("yarn.lock v1: root manifest: %v", err)
	}
	pats, err := yarnWorkspacePatterns(files, dir)
	if err != nil {
		return nil, fmt.Errorf("yarn.lock v1: %v", err)
	}
	wsDirs, err := expandWorkspaces(files, dir, pats)
	if err != nil {
		return nil, fmt.Errorf("yarn.lock v1: %v", err)
	}
	manifests := []*yarnManifest{root}
	byName := map[string]*yarnManifest{}
	for _, d := range wsDirs {
		m, err := readYarnManifest(files, d)
		if err != nil {
			return nil, fmt.Errorf("yarn.lock v1: workspace %s: %v", d, err)
		}
		if m.name == "" {
			return nil, fmt.Errorf("yarn.lock v1: workspace %s has no name", d)
		}
		if _, dup := byName[m.name]; dup {
			return nil, fmt.Errorf("yarn.lock v1: two workspaces are named %q", m.name)
		}
		byName[m.name] = m
		manifests = append(manifests, m)
	}
	projectID := func(d string) string { return "yarn-project:" + d }
	for _, m := range manifests {
		id := projectID(m.dir)
		g.Nodes[id] = &Node{ID: id, Name: m.name, Package: m.name, Version: m.version, Ident: "project " + m.dir, Project: m.dir,
			Local: true, Deps: map[string]string{}}
		g.Projects[m.dir] = id
	}
	blockID := map[*yarnBlock]string{}
	for _, b := range blocks {
		k := append([]string(nil), b.keys...)
		sort.Strings(k)
		blockID[b] = k[0]
	}
	var nodeFor func(name string, b *yarnBlock) (string, error)
	// resolve returns the node a dependency resolves to; from is the directory of the declaring manifest ("" for a
	// lockfile entry). ok=false: nothing resolves it.
	resolve := func(name, rng, from string) (string, bool, error) {
		if b, ok := blockOf[name+"@"+rng]; ok && (exoticRange(rng) || byName[name] == nil) {
			id, err := nodeFor(name, b)
			return id, err == nil, err
		}
		if rest, ok := strings.CutPrefix(rng, "link:"); ok && from != "" {
			p, ok := join(from, rest)
			if !ok {
				return "", false, fmt.Errorf("link %s outside the repo", rng)
			}
			if id, ok := g.Projects[p]; ok {
				return id, true, nil
			}
			id := "yarn-link:" + p
			if _, ok := g.Nodes[id]; !ok {
				g.Nodes[id] = &Node{ID: id, Name: name, Package: name, Ident: "link " + p, Local: true, Deps: map[string]string{}}
			}
			return id, true, nil
		}
		if exoticRange(rng) {
			return "", false, nil
		}
		if ws := byName[name]; ws != nil {
			sat, err := semverSatisfies(ws.version, rng)
			if err != nil {
				return "", false, fmt.Errorf("range %q of %s: %v", rng, name, err)
			}
			if sat {
				return projectID(ws.dir), true, nil
			}
		}
		if b, ok := blockOf[name+"@"+rng]; ok {
			id, err := nodeFor(name, b)
			return id, err == nil, err
		}
		return "", false, nil
	}
	nodeFor = func(name string, b *yarnBlock) (string, error) {
		id := "yarn:" + name + "@" + blockID[b]
		if _, ok := g.Nodes[id]; ok {
			return id, nil
		}
		n := &Node{ID: id, Name: name, Package: name, Deps: map[string]string{}, Scripts: true}
		g.Nodes[id] = n
		n.Version, _ = b.fields["version"].(string)
		for _, k := range b.keys {
			if kn, r, ok := splitPattern(k); ok && kn == name {
				if alias, ok := strings.CutPrefix(r, "npm:"); ok {
					if an, _, ok := splitPattern(alias); ok {
						n.Package = an
					} else {
						n.Package = alias
					}
				}
			}
		}
		idm := map[string]any{}
		for k, v := range b.fields {
			if k != "dependencies" && k != "optionalDependencies" {
				idm[k] = v
			}
		}
		n.Ident = "yarn " + name + " " + canon(idm)
		if r, _ := b.fields["resolved"].(string); !strings.HasPrefix(r, "http://") && !strings.HasPrefix(r, "https://") {
			n.Local = true
		}
		for _, s := range []string{"dependencies", "optionalDependencies"} {
			if b.fields[s] == nil {
				continue
			}
			m, ok := b.fields[s].(map[string]any)
			if !ok {
				return id, fmt.Errorf("yarn.lock: %s of %s is not a map", s, id)
			}
			for dn, r := range m {
				rs, ok := r.(string)
				if !ok {
					return id, fmt.Errorf("yarn.lock: bad range %s → %s", id, dn)
				}
				t, found, err := resolve(dn, rs, "")
				if err != nil {
					return id, err
				}
				if !found {
					return id, fmt.Errorf("yarn.lock: %s: no entry for %s@%s", id, dn, rs)
				}
				n.Deps[dn] = t
			}
		}
		return id, nil
	}
	for _, m := range manifests {
		n := g.Nodes[projectID(m.dir)]
		for _, s := range yarnManifestSections {
			names := make([]string, 0, len(m.deps[s]))
			for dn := range m.deps[s] {
				names = append(names, dn)
			}
			sort.Strings(names)
			for _, dn := range names {
				rng := m.deps[s][dn]
				t, found, err := resolve(dn, rng, m.dir)
				if err != nil {
					return nil, fmt.Errorf("yarn.lock v1: %s: %v", m.dir, err)
				}
				if !found {
					if s == "peerDependencies" {
						continue // not installed for the declaring project itself; its dependents provide it
					}
					return nil, fmt.Errorf("yarn.lock v1: %s: %s %s@%s resolves to no entry", m.dir, s, dn, rng)
				}
				key := dn
				if prev, dup := n.Deps[dn]; dup {
					if prev == t {
						continue
					}
					key = dn + " (" + s + ")"
				}
				n.Deps[key] = t
			}
		}
	}
	return g, nil
}
