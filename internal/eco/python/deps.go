package python

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/qxbao/bgraph/internal/tree"
)

// declared is the set of distribution names declared as dependencies anywhere in the repo (a superset: one set for the whole repo),
// normalized per PEP 503.
type declared map[string]bool

func normDist(s string) string {
	var b strings.Builder
	prev := false
	for _, c := range strings.ToLower(s) {
		if c == '-' || c == '_' || c == '.' {
			if !prev {
				b.WriteByte('-')
			}
			prev = true
			continue
		}
		prev = false
		b.WriteRune(c)
	}
	return b.String()
}

// importToDist maps top-level import names whose distribution has a different name (normalized) to that distribution.
var importToDist = map[string][]string{
	"yaml": {"pyyaml"}, "PIL": {"pillow"}, "sklearn": {"scikit-learn"}, "cv2": {"opencv-python", "opencv-python-headless", "opencv-contrib-python", "opencv-contrib-python-headless"},
	"bs4": {"beautifulsoup4"}, "dateutil": {"python-dateutil"}, "attr": {"attrs"}, "attrs": {"attrs"}, "jwt": {"pyjwt"}, "OpenSSL": {"pyopenssl"},
	"MySQLdb": {"mysqlclient"}, "psycopg2": {"psycopg2-binary", "psycopg2"}, "serial": {"pyserial"}, "usb": {"pyusb"}, "dotenv": {"python-dotenv"},
	"git": {"gitpython"}, "jose": {"python-jose"}, "magic": {"python-magic"}, "docx": {"python-docx"}, "pptx": {"python-pptx"},
	"skimage": {"scikit-image"}, "zmq": {"pyzmq"}, "Crypto": {"pycryptodome", "pycrypto", "pycryptodomex"}, "Cryptodome": {"pycryptodomex"},
	"nacl": {"pynacl"}, "wx": {"wxpython"}, "gi": {"pygobject"}, "fitz": {"pymupdf"}, "mpl_toolkits": {"matplotlib"},
	"pkg_resources": {"setuptools"}, "_distutils_hack": {"setuptools"}, "_pytest": {"pytest"}, "pytest_cov": {"pytest-cov"}, "markdown": {"markdown"},
	"ruamel": {"ruamel-yaml"}, "google": {"protobuf"}, "grpc": {"grpcio"}, "tensorflow": {"tensorflow", "tensorflow-cpu", "tensorflow-gpu"},
	"lxml": {"lxml"}, "Image": {"pillow"}, "simplejson": {"simplejson"}, "thrift": {"thrift"}, "kafka": {"kafka-python"}, "pymysql": {"pymysql"},
	"slugify": {"python-slugify"}, "socks": {"pysocks"}, "win32api": {"pywin32"}, "win32con": {"pywin32"}, "pythoncom": {"pywin32"}, "pywintypes": {"pywin32"},
	"yaml_include": {"pyyaml-include"}, "ldap": {"python-ldap"}, "jenkins": {"python-jenkins"}, "consul": {"python-consul"}, "etcd": {"python-etcd"},
	"xdist": {"pytest-xdist"}, "tomllib": {"tomli"}, "tomli_w": {"tomli-w"}, "zope": {"zope-interface", "zope-component"}, "jaraco": {"jaraco-functools"},
	"msgpack": {"msgpack"}, "Levenshtein": {"levenshtein", "python-levenshtein"}, "faiss": {"faiss-cpu", "faiss-gpu"}, "torch": {"torch"},
	"umap": {"umap-learn"}, "hdbscan": {"hdbscan"}, "dns": {"dnspython"}, "httplib2": {"httplib2"}, "oauth2client": {"oauth2client"},
	"apiclient": {"google-api-python-client"}, "googleapiclient": {"google-api-python-client"}, "flask_sqlalchemy": {"flask-sqlalchemy"},
	"bson": {"pymongo"}, "gridfs": {"pymongo"}, "pymongo": {"pymongo"}, "redis": {"redis"}, "elasticsearch": {"elasticsearch"},
	"markupsafe": {"markupsafe"}, "jinja2": {"jinja2"}, "werkzeug": {"werkzeug"}, "click": {"click"}, "itsdangerous": {"itsdangerous"},
	"typing_extensions": {"typing-extensions"}, "importlib_metadata": {"importlib-metadata"}, "importlib_resources": {"importlib-resources"},
	"sphinx_rtd_theme": {"sphinx-rtd-theme"}, "sphinxcontrib": {"sphinxcontrib-websupport"}, "pygments": {"pygments"},
}

// isDeclared reports whether the top-level import name is provided by a declared distribution.
func (d declared) isDeclared(top string) bool {
	if len(d) == 0 {
		return false
	}
	if d[normDist(top)] || d[normDist(strings.TrimPrefix(top, "_"))] {
		return true
	}
	for _, dist := range importToDist[top] {
		if d[dist] {
			return true
		}
	}
	// namespace distributions: google-cloud-x, azure-mgmt-y, sphinxcontrib-z ... provide `google`, `azure`, `sphinxcontrib`
	pre := normDist(top) + "-"
	for k := range d {
		if strings.HasPrefix(k, pre) {
			return true
		}
	}
	return false
}

var reqName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)

// addReq adds the distribution named by one requirement line ("pkg[extra]>=1; python_version<'3'", "name @ url", "git+...#egg=name").
func (d declared) addReq(line string) {
	line = strings.TrimSpace(line)
	if i := strings.Index(line, " #"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	if strings.HasPrefix(line, "-") {
		// -e git+https://...#egg=name / -e ./local ; every other option is not a requirement
		if (strings.HasPrefix(line, "-e ") || strings.HasPrefix(line, "--editable")) && strings.Contains(line, "#egg=") {
			e := line[strings.Index(line, "#egg=")+5:]
			if m := reqName.FindString(e); m != "" {
				d[normDist(m)] = true
			}
		}
		return
	}
	if i := strings.Index(line, "#egg="); i >= 0 {
		if m := reqName.FindString(line[i+5:]); m != "" {
			d[normDist(m)] = true
		}
		return
	}
	if m := reqName.FindString(line); m != "" {
		d[normDist(m)] = true
	}
}

// loadDeclared reads every dependency declaration in the tree.
func loadDeclared(t tree.Tree, files []string) declared {
	d := declared{}
	for _, p := range files {
		base := path.Base(p)
		switch {
		case base == "pyproject.toml":
			d.fromPyproject(t, p)
		case base == "setup.cfg":
			d.fromSetupCfg(t, p)
		case base == "setup.py":
			if b, err := t.Read(p); err == nil && len(b) < 1<<20 {
				for _, m := range setupPyReq.FindAllSubmatch(b, -1) {
					d[normDist(string(m[1]))] = true
				}
			}
		case base == "tox.ini":
			d.fromTox(t, p)
		case base == "Pipfile":
			if b, err := t.Read(p); err == nil {
				var doc map[string]any
				if toml.Unmarshal(b, &doc) == nil {
					for _, k := range []string{"packages", "dev-packages"} {
						if m, ok := doc[k].(map[string]any); ok {
							for name := range m {
								d[normDist(name)] = true
							}
						}
					}
				}
			}
		case isRequirementsFile(p):
			if b, err := t.Read(p); err == nil && len(b) < 4<<20 {
				for _, l := range strings.Split(string(b), "\n") {
					d.addReq(l)
				}
			}
		}
	}
	return d
}

var setupPyReq = regexp.MustCompile(`["']([A-Za-z][A-Za-z0-9._-]*)(?:\[[^\]"']*\])?\s*(?:[<>=!~;@][^"']*)?["']`)

// isRequirementsFile: requirements*.txt, *-requirements.txt, constraints*.txt, *.in next to them, or any .txt/.in in a requirements directory.
func isRequirementsFile(p string) bool {
	base := path.Base(p)
	ext := path.Ext(base)
	if ext != ".txt" && ext != ".in" {
		return false
	}
	stem := strings.TrimSuffix(base, ext)
	ls := strings.ToLower(stem)
	if strings.HasPrefix(ls, "requirements") || strings.HasPrefix(ls, "constraints") || strings.HasSuffix(ls, "-requirements") ||
		strings.HasSuffix(ls, "_requirements") || strings.HasPrefix(ls, "requirement-") {
		return true
	}
	switch strings.ToLower(path.Base(path.Dir(p))) {
	case "requirements", "reqs", "requirement":
		return true
	}
	return false
}

func (d declared) fromPyproject(t tree.Tree, p string) {
	b, err := t.Read(p)
	if err != nil {
		return
	}
	var doc map[string]any
	if toml.Unmarshal(b, &doc) != nil {
		return
	}
	strs := func(v any) {
		if l, ok := v.([]any); ok {
			for _, x := range l {
				if s, ok := x.(string); ok {
					d.addReq(s)
				}
			}
		}
	}
	tbl := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	if pr := tbl(doc["project"]); pr != nil {
		strs(pr["dependencies"])
		for _, v := range tbl(pr["optional-dependencies"]) {
			strs(v)
		}
	}
	for _, v := range tbl(doc["dependency-groups"]) {
		strs(v) // inline tables ({include-group = ...}) are skipped by strs
	}
	if bs := tbl(doc["build-system"]); bs != nil {
		strs(bs["requires"])
	}
	tool := tbl(doc["tool"])
	if po := tbl(tool["poetry"]); po != nil {
		for _, k := range []string{"dependencies", "dev-dependencies"} {
			for name := range tbl(po[k]) {
				d[normDist(name)] = true
			}
		}
		for _, g := range tbl(po["group"]) {
			for name := range tbl(tbl(g)["dependencies"]) {
				d[normDist(name)] = true
			}
		}
	}
	if pdm := tbl(tool["pdm"]); pdm != nil {
		for _, v := range tbl(pdm["dev-dependencies"]) {
			strs(v)
		}
	}
	if uv := tbl(tool["uv"]); uv != nil {
		strs(uv["dev-dependencies"])
	}
	if h := tbl(tbl(tool["hatch"])["envs"]); h != nil {
		for _, e := range h {
			strs(tbl(e)["dependencies"])
		}
	}
	delete(d, "python")
}

func (d declared) fromSetupCfg(t tree.Tree, p string) {
	b, err := t.Read(p)
	if err != nil {
		return
	}
	ini := parseINI(string(b))
	for sec, kv := range ini.sections {
		switch {
		case sec == "options":
			for _, k := range []string{"install_requires", "tests_require", "setup_requires"} {
				for _, l := range splitLines(kv[k]) {
					d.addReq(l)
				}
			}
		case strings.HasPrefix(sec, "options.extras_require"):
			for _, v := range kv {
				for _, l := range splitLines(v) {
					d.addReq(l)
				}
			}
		}
	}
}

var toxFactor = regexp.MustCompile(`^[\w{},.!-]+:\s*`)

func (d declared) fromTox(t tree.Tree, p string) {
	b, err := t.Read(p)
	if err != nil {
		return
	}
	for sec, kv := range parseINI(string(b)).sections {
		if !strings.HasPrefix(sec, "testenv") && sec != "tox" {
			continue
		}
		for _, l := range splitLines(kv["deps"]) {
			l = toxFactor.ReplaceAllString(strings.TrimSpace(l), "")
			if strings.HasPrefix(l, "{") || strings.HasPrefix(l, "-r") || strings.HasPrefix(l, "-c") {
				continue
			}
			d.addReq(l)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// RunnerConfigFile reports whether p is a file pytest may read its configuration from, part of the dynamic layer's environment
// fingerprint (docs/dynamic.md §5). Manifests and lockfiles are not: the installed distributions fingerprint the environment.
func RunnerConfigFile(p string) bool {
	if skipPath(p) {
		return false
	}
	switch path.Base(p) {
	case "pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg", "pyproject.toml":
		return true
	}
	return false
}

// RunnerConfigPart returns the part of runner config file p (RunnerConfigFile) that configures pytest: the [tool.pytest] table
// of pyproject.toml (re-encoded canonically), the [tool:pytest] section of setup.cfg and the [pytest] section of tox.ini; the
// whole content for the pytest.ini/pytest.toml files,
// and whenever the part cannot be extracted reliably (pyproject.toml that does not parse).
func RunnerConfigPart(p string, content []byte) string {
	switch path.Base(p) {
	case "pyproject.toml":
		var doc map[string]any
		if err := toml.Unmarshal(content, &doc); err != nil {
			return "whole:" + string(content)
		}
		t := tbl(tbl(doc["tool"])["pytest"])
		if len(t) == 0 {
			return ""
		}
		b, err := json.Marshal(t) // map keys are sorted: canonical
		if err != nil {
			return "whole:" + string(content)
		}
		return "tool.pytest:" + string(b)
	case "setup.cfg", "tox.ini":
		section := "tool:pytest"
		if path.Base(p) == "tox.ini" {
			section = "pytest"
		}
		var out []string
		in := false
		for _, l := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
				in = strings.EqualFold(strings.TrimSpace(t[1:len(t)-1]), section)
				continue
			}
			if in {
				out = append(out, strings.TrimRight(l, " \t"))
			}
		}
		if len(out) == 0 {
			return ""
		}
		return section + ":" + strings.Join(out, "\n")
	}
	return "whole:" + string(content)
}
