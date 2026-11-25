package swift

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	swiftscan "github.com/qxbao/bgraph/internal/scan/swift"
)

type manifest struct {
	name       string
	local      []string
	targets    []target
	programs   map[string]string
	canonical  string
	nonTest    string
	opaqueTool bool
	flatLayout bool
	whole      bool
}

type target struct {
	name, kind, path             string
	sources, excludes, resources []string
	deps                         []string
}

type expression struct {
	Kind  string
	Text  string
	Items []argument
}

type argument struct {
	Label string
	Value expression
}

type manifestParser struct {
	tokens []swiftscan.Token
	i      int
}

var toolsVersion = regexp.MustCompile("^//[ \t]*swift-tools-version:[ \t]*([0-9]+\\.[0-9]+(?:\\.[0-9]+)?)\\r?$")

func parseManifest(src []byte) (*manifest, error) {
	first, code, _ := strings.Cut(string(src), "\n")
	v := toolsVersion.FindStringSubmatch(first)
	if v == nil {
		return nil, fmt.Errorf("missing or unsupported swift-tools-version directive")
	}
	tokens, err := swiftscan.Lex([]byte(code))
	if err != nil {
		return nil, err
	}
	p := manifestParser{tokens: tokens}
	if !p.take("import") || !p.take("PackageDescription") {
		return nil, fmt.Errorf("expected import PackageDescription")
	}
	if !p.take(";") && (p.i == len(tokens) || !tokens[p.i].LineBreak) {
		return nil, fmt.Errorf("expected a Swift statement break after import")
	}
	if (!p.take("let") && !p.take("var")) || !p.take("package") {
		return nil, fmt.Errorf("expected a literal package declaration")
	}
	if p.take(":") && !p.take("Package") {
		return nil, fmt.Errorf("unexpected Swift package declaration type")
	}
	if !p.take("=") || !p.take("Package") || !p.take("(") {
		return nil, fmt.Errorf("expected Package(...) initializer")
	}
	args, err := p.list(")", true, 0)
	if err != nil {
		return nil, err
	}
	p.take(";")
	if p.i != len(tokens) {
		return nil, fmt.Errorf("additional Swift manifest statements")
	}
	return decodePackage(v[1], expression{Kind: "call", Text: "Package", Items: args})
}

func (p *manifestParser) take(text string) bool {
	if p.i >= len(p.tokens) || p.tokens[p.i].Kind > swiftscan.Punctuation || p.tokens[p.i].Text != text {
		return false
	}
	p.i++
	return true
}

func (p *manifestParser) list(end string, labels bool, depth int) ([]argument, error) {
	if depth > 100 {
		return nil, fmt.Errorf("nesting limit exceeded in Swift manifest")
	}
	var out []argument
	for !p.take(end) {
		var label string
		if labels && p.i+1 < len(p.tokens) && p.tokens[p.i].Kind == swiftscan.Word &&
			p.tokens[p.i+1].Kind == swiftscan.Punctuation && p.tokens[p.i+1].Text == ":" {
			label = p.tokens[p.i].Text
			p.i += 2
		}
		x, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, argument{Label: label, Value: x})
		if p.take(end) {
			return out, nil
		}
		if !p.take(",") {
			return nil, fmt.Errorf("expected comma or %s in Swift manifest", end)
		}
	}
	return out, nil
}

func (p *manifestParser) value(depth int) (expression, error) {
	if p.take("[") {
		items, err := p.list("]", false, depth)
		return expression{Kind: "array", Items: items}, err
	}
	if p.take(".") && p.i < len(p.tokens) && p.tokens[p.i].Kind == swiftscan.Word {
		name := p.tokens[p.i].Text
		p.i++
		if p.take("(") {
			items, err := p.list(")", true, depth)
			return expression{Kind: "call", Text: name, Items: items}, err
		}
		return expression{Kind: "member", Text: name}, nil
	}
	if p.i < len(p.tokens) {
		t := p.tokens[p.i]
		p.i++
		if t.Kind == swiftscan.String {
			return expression{Kind: "string", Text: t.Text}, nil
		}
		if t.Kind == swiftscan.Word && slices.Contains([]string{"nil", "true", "false"}, t.Text) {
			return expression{Kind: "literal", Text: t.Text}, nil
		}
	}
	return expression{}, fmt.Errorf("computed or unsupported Swift manifest expression")
}

func decodePackage(version string, x expression) (*manifest, error) {
	if err := x.fields("name", "defaultLocalization", "platforms", "pkgConfig", "providers", "products", "traits",
		"dependencies", "targets", "swiftLanguageVersions", "swiftLanguageModes", "cLanguageStandard", "cxxLanguageStandard"); err != nil {
		return nil, err
	}
	if err := knownExpressions(x, true); err != nil {
		return nil, err
	}
	name, err := x.stringField("name", true)
	if err != nil {
		return nil, err
	}
	m := &manifest{
		name: name, programs: map[string]string{}, whole: containsCall(x, "unsafeFlags"),
		flatLayout: versionAtLeast(version, 5, 9), opaqueTool: containsCall(x, "headerSearchPath"),
	}
	deps, err := x.arrayField("dependencies")
	if err != nil {
		return nil, err
	}
	for _, dep := range deps {
		if dep.Kind != "call" || dep.Text != "package" {
			return nil, fmt.Errorf("expected .package dependency")
		}
		local, err := localDependency(dep)
		if err != nil {
			return nil, err
		}
		if local != "" {
			m.local = append(m.local, local)
		}
	}

	targets, err := x.arrayField("targets")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, item := range targets {
		t, err := decodeTarget(item)
		if err != nil {
			return nil, err
		}
		if seen[t.name] {
			return nil, fmt.Errorf("duplicate Swift target %q", t.name)
		}
		seen[t.name] = true
		if len(item.field("plugins").Items) > 0 {
			m.opaqueTool = true
		}
		switch t.kind {
		case "executableTarget":
			m.programs[t.name] = t.name
		case "plugin", "macro", "systemLibrary", "binaryTarget":
			m.opaqueTool = true
		}
		m.targets = append(m.targets, t)
	}
	prods, err := x.arrayField("products")
	if err != nil {
		return nil, err
	}
	for _, pr := range prods {
		if pr.Kind != "call" || !slices.Contains([]string{"library", "executable", "plugin"}, pr.Text) {
			return nil, fmt.Errorf("unsupported Swift product")
		}
		if err := pr.fields("name", "type", "targets"); err != nil {
			return nil, err
		}
		name, err := pr.stringField("name", true)
		if err != nil {
			return nil, err
		}
		targets, err := pr.stringsField("targets")
		if err != nil || len(targets) == 0 {
			return nil, fmt.Errorf("invalid targets for Swift product %q", name)
		}
		for _, target := range targets {
			if !seen[target] {
				return nil, fmt.Errorf("unknown target %q for Swift product %q", target, name)
			}
		}
		if pr.Text == "executable" {
			m.programs[name] = targets[0]
		}
	}
	for _, t := range m.targets {
		for _, dep := range t.deps {
			if !seen[dep] && len(deps) == 0 {
				return nil, fmt.Errorf("unknown Swift target or external product %q", dep)
			}
		}
	}

	m.canonical = version + canonical(x)
	for i := range x.Items {
		if x.Items[i].Label == "targets" {
			a := x.Items[i].Value
			a.Items = slices.DeleteFunc(slices.Clone(a.Items), func(v argument) bool { return v.Value.Text == "testTarget" })
			x.Items[i].Value = a
			break
		}
	}
	m.nonTest = version + canonical(x)
	return m, nil
}

func localDependency(dep expression) (string, error) {
	if err := dep.fields("", "name", "path", "id", "url", "from", "exact", "branch", "revision", "traits"); err != nil {
		return "", err
	}
	if v := dep.field("path"); v.Kind != "" {
		if v.Kind != "string" || v.Text == "" || dep.field("url").Kind != "" || dep.field("id").Kind != "" {
			return "", fmt.Errorf("invalid local Swift dependency path")
		}
		return v.Text, nil
	}
	if v := dep.field("url"); v.Kind != "" {
		if v.Kind != "string" || v.Text == "" {
			return "", fmt.Errorf("invalid Swift dependency URL")
		}
		s := v.Text
		// Bare Git paths and file URLs can name another checkout in this repository.
		if strings.HasPrefix(strings.ToLower(s), "file:") {
			u, err := url.Parse(s)
			if err != nil {
				return "", fmt.Errorf("unsupported file URL for Swift dependency")
			}
			if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
				return "/" + u.Host + u.Path, nil
			}
			if u.Path != "" {
				return u.Path, nil
			}
			if u.Opaque != "" {
				return u.Opaque, nil
			}
			return "", fmt.Errorf("missing path in Swift dependency file URL")
		}
		if !strings.Contains(s, ":") || (len(s) > 1 && s[1] == ':') {
			return s, nil
		}
		return "", nil
	}
	if v := dep.field("id"); v.Kind != "string" || v.Text == "" {
		return "", fmt.Errorf("invalid Swift dependency")
	}
	return "", nil
}

func decodeTarget(x expression) (target, error) {
	t := target{kind: x.Text}
	if x.Kind != "call" || !slices.Contains([]string{"target", "testTarget", "executableTarget", "systemLibrary", "binaryTarget", "plugin", "macro"}, x.Text) {
		return t, fmt.Errorf("unsupported Swift target declaration")
	}
	if err := x.fields("name", "dependencies", "path", "exclude", "sources", "resources", "publicHeadersPath",
		"cSettings", "cxxSettings", "swiftSettings", "linkerSettings", "plugins", "packageAccess", "pkgConfig", "providers",
		"capability", "url", "checksum"); err != nil {
		return t, err
	}
	var err error
	if t.name, err = x.stringField("name", true); err != nil {
		return t, err
	}
	if strings.ContainsAny(t.name, "/\\") || t.name == "." || t.name == ".." {
		return t, fmt.Errorf("invalid Swift target name %q", t.name)
	}
	if v := x.field("path"); v.Kind == "string" && v.Text == "" {
		t.path = "."
	} else if t.path, err = x.stringField("path", false); err != nil {
		return t, err
	}
	if t.sources, err = x.stringsField("sources"); err != nil {
		return t, err
	}
	if t.excludes, err = x.stringsField("exclude"); err != nil {
		return t, err
	}
	if h, err := x.stringField("publicHeadersPath", false); err != nil {
		return t, err
	} else if h != "" {
		t.resources = append(t.resources, h)
	}
	rs, err := x.arrayField("resources")
	if err != nil {
		return t, err
	}
	for _, r := range rs {
		if r.Kind != "call" || !slices.Contains([]string{"copy", "process", "embedInCode"}, r.Text) {
			return t, fmt.Errorf("unsupported Swift target resource declaration")
		}
		if err := r.fields("", "localization"); err != nil {
			return t, err
		}
		d, err := r.stringField("", true)
		if err != nil {
			return t, err
		}
		t.resources = append(t.resources, d)
	}
	ds, err := x.arrayField("dependencies")
	if err != nil {
		return t, err
	}
	for _, d := range ds {
		if d.Kind == "string" && d.Text != "" {
			t.deps = append(t.deps, d.Text)
			continue
		}
		if d.Kind != "call" || !slices.Contains([]string{"target", "byName", "product"}, d.Text) {
			return t, fmt.Errorf("unsupported Swift target dependency")
		}
		if err := d.fields("name", "package", "condition", "moduleAliases"); err != nil {
			return t, err
		}
		n, err := d.stringField("name", true)
		if err != nil {
			return t, err
		}
		if d.Text != "product" {
			t.deps = append(t.deps, n)
		}
	}
	ps, err := x.arrayField("plugins")
	if err != nil {
		return t, err
	}
	for _, p := range ps {
		if p.Kind == "string" && p.Text != "" {
			t.deps = append(t.deps, p.Text)
			continue
		}
		if p.Kind != "call" || p.Text != "plugin" {
			return t, fmt.Errorf("unsupported Swift plugin usage")
		}
		n, err := p.stringField("name", true)
		if err != nil {
			return t, err
		}
		if p.field("package").Kind == "" {
			t.deps = append(t.deps, n)
		}
	}
	return t, nil
}

// knownExpressions admits only PackageDescription literal constructors. By construction
// there are no arbitrary identifiers, global calls, member chains, closures or operators.
func knownExpressions(x expression, root bool) error {
	if x.Kind == "call" && !root && !knownCalls[x.Text] {
		return fmt.Errorf("unsupported Swift manifest call .%s", x.Text)
	}
	for _, arg := range x.Items {
		if err := knownExpressions(arg.Value, false); err != nil {
			return err
		}
	}
	return nil
}

var knownCalls = func() map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Fields("library executable target testTarget executableTarget systemLibrary binaryTarget plugin macro " +
		"package target byName product copy process embedInCode when " +
		"macOS iOS tvOS watchOS visionOS macCatalyst driverKit linux windows android openbsd freebsd wasi " +
		"apt brew yum nuget version branch revision exact upToNextMajor upToNextMinor " +
		"define unsafeFlags linkedLibrary linkedFramework headerSearchPath enableUpcomingFeature enableExperimentalFeature interoperabilityMode swiftLanguageMode " +
		"buildTool command documentationGeneration sourceCodeFormatting custom writeToPackageDirectory allowNetworkConnections trait") {
		out[s] = true
	}
	return out
}()

func (x expression) fields(allowed ...string) error {
	seen := map[string]bool{}
	for _, a := range x.Items {
		if !slices.Contains(allowed, a.Label) || seen[a.Label] {
			return fmt.Errorf("unsupported or duplicate Swift argument %q in %s", a.Label, x.Text)
		}
		seen[a.Label] = true
	}
	return nil
}

func (x expression) field(label string) expression {
	for _, a := range x.Items {
		if a.Label == label {
			return a.Value
		}
	}
	return expression{}
}

func (x expression) stringField(label string, required bool) (string, error) {
	v := x.field(label)
	if !required && (v.Kind == "" || (v.Kind == "literal" && v.Text == "nil")) {
		return "", nil
	}
	if v.Kind != "string" || v.Text == "" || strings.ContainsRune(v.Text, 0) {
		return "", fmt.Errorf("expected nonempty Swift string for %s argument %q", x.Text, label)
	}
	return v.Text, nil
}

func (x expression) arrayField(label string) ([]expression, error) {
	v := x.field(label)
	if v.Kind == "" || (v.Kind == "literal" && v.Text == "nil") {
		return nil, nil
	}
	if v.Kind != "array" {
		return nil, fmt.Errorf("expected Swift array for %s argument %q", x.Text, label)
	}
	out := make([]expression, 0, len(v.Items))
	for _, a := range v.Items {
		out = append(out, a.Value)
	}
	return out, nil
}

func (x expression) stringsField(label string) ([]string, error) {
	args, err := x.arrayField(label)
	if err != nil {
		return nil, err
	}
	if args == nil {
		return nil, nil
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a.Kind != "string" || a.Text == "" || strings.ContainsRune(a.Text, 0) {
			return nil, fmt.Errorf("expected nonempty Swift string in %s argument %q", x.Text, label)
		}
		out = append(out, a.Text)
	}
	return out, nil
}

func canonical(x expression) string {
	// Encoding these structs and strings cannot fail.
	b, _ := json.Marshal(x)
	return string(b)
}

func containsCall(x expression, name string) bool {
	if x.Kind == "call" && x.Text == name {
		return true
	}
	for _, a := range x.Items {
		if containsCall(a.Value, name) {
			return true
		}
	}
	return false
}

func versionAtLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	vMajor, _ := strconv.Atoi(parts[0])
	vMinor, _ := strconv.Atoi(parts[1])
	return vMajor > major || (vMajor == major && vMinor >= minor)
}
