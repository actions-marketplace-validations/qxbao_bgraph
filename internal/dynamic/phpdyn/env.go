// Package phpdyn is the PHP part of the dynamic layer (docs/dynamic-layer.md §11): per-test-file records of what each test file
// of a Composer project included, read, stat'ed or listed (one process tree per test file, Xdebug's line coverage, a file://
// stream wrapper proxy and a function monitor, injected through an ini file on PHP_INI_SCAN_DIR), a file-list record per project,
// and the guards and narrowing that run only the test files of a selected PHP unit that can see a changed file, refined to
// the changed functions when every change lies inside function bodies.
package phpdyn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
)

// Eco is the ecosystem name of the records.
const Eco = "php"

// CollectorID identifies the collector and its record format (docs/dynamic-layer.md §11.3): bump it whenever what the collector
// observes or writes changes (records of other versions are not used). The embedded collector carries the same ID.
const CollectorID = "php-xdebug-1"

// ResolvePHP turns the configured PHP into an executable path: a name without separator is looked up on PATH, anything else is
// relative to the repo root. Empty: "php".
func ResolvePHP(root, configured string) (string, error) {
	if configured == "" {
		configured = "php"
	}
	if !strings.ContainsAny(configured, `/\`) {
		return exec.LookPath(configured)
	}
	p := configured
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, filepath.FromSlash(p))
	}
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// probeScript prints the facts of the PHP executable that change how tests run (docs/dynamic-layer.md §11.5), as JSON.
const probeScript = `$e = [];
foreach (get_loaded_extensions(false) as $x) { $e[] = $x . '=' . phpversion($x); }
foreach (get_loaded_extensions(true) as $x) { $e[] = 'zend:' . $x . '=' . phpversion($x); }
sort($e);
$ini = [];
foreach (['zend.assertions', 'error_reporting', 'memory_limit', 'precision', 'serialize_precision', 'date.timezone',
  'default_charset', 'opcache.enable_cli', 'auto_prepend_file', 'auto_append_file', 'include_path', 'short_open_tag',
  'pcov.enabled'] as $k) { $v = ini_get($k); $ini[$k] = $v === false ? null : $v; }
echo json_encode(['version' => PHP_VERSION, 'os' => PHP_OS, 'machine' => php_uname('m'), 'extensions' => $e, 'ini' => $ini,
  'xdebug' => phpversion('xdebug') ?: '', 'scan_dir' => PHP_CONFIG_FILE_SCAN_DIR, 'binary' => PHP_BINARY]);`

// Probe is what the probe script reports about a PHP executable.
type Probe struct {
	Version    string             `json:"version"`
	OS         string             `json:"os"`
	Machine    string             `json:"machine"`
	Extensions []string           `json:"extensions"`
	Ini        map[string]*string `json:"ini"`
	Xdebug     string             `json:"xdebug"`
	ScanDir    string             `json:"scan_dir"`
	Binary     string             `json:"binary"`
}

// ini returns an ini value of the probe ("" when unset).
func (p Probe) ini(k string) string {
	if v := p.Ini[k]; v != nil {
		return *v
	}
	return ""
}

// Lines returns the fingerprint lines of the probe (docs/dynamic-layer.md §11.5).
func (p Probe) Lines() []string {
	out := []string{"php " + p.Version, "php-os " + p.OS, "machine " + p.Machine, "xdebug " + p.Xdebug}
	for _, e := range p.Extensions {
		out = append(out, "ext "+e)
	}
	keys := make([]string, 0, len(p.Ini))
	for k := range p.Ini {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := "<unset>"
		if p.Ini[k] != nil {
			v = *p.Ini[k]
		}
		out = append(out, "ini "+k+"="+v)
	}
	return out
}

// probeEnv returns the process environment without the variables the recording sets (the collector must not run in the probe).
func probeEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "XDEBUG_MODE", "XDEBUG_CONFIG", "BGRAPH_PHP_OUT", "BGRAPH_PHP_ROOT", "BGRAPH_PHP_VENDOR", "BGRAPH_PHP_RUNNER",
			"BGRAPH_PHP_PARENT":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "XDEBUG_MODE=off")
}

// RunProbe runs the probe script with the PHP executable exe from an empty temporary directory.
func RunProbe(ctx context.Context, exe string) (Probe, error) {
	dir, err := os.MkdirTemp("", "bgraph-phpenv-")
	if err != nil {
		return Probe{}, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-r", probeScript)
	cmd.Dir = dir
	cmd.Env = probeEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Probe{}, fmt.Errorf("%s: %v %s", exe, err, strings.TrimSpace(errb.String()))
	}
	var p Probe
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &p); err != nil || p.Version == "" {
		return Probe{}, fmt.Errorf("%s: unexpected probe output %q", exe, firstN(out.String(), 200))
	}
	return p, nil
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ProbeCache caches RunProbe per executable path and modification time.
type ProbeCache struct {
	mu sync.Mutex
	m  map[string]Probe
}

// Get returns the probe of exe, running it when its path or modification time is new.
func (c *ProbeCache) Get(ctx context.Context, exe string) (Probe, error) {
	st, err := os.Stat(exe)
	if err != nil {
		return Probe{}, err
	}
	key := exe + "\x00" + st.ModTime().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	if v, ok := c.m[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := RunProbe(ctx, exe)
	if err != nil {
		return Probe{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]Probe{}
	}
	c.m[key] = v
	return v, nil
}

// envVars are the environment variables that change how PHP test processes load code or configure the runtime: the ini
// search (PHPRC, PHP_INI_SCAN_DIR), application environments read by test bootstraps (APP_ENV, APP_DEBUG), Composer's runtime
// environment and vendor directory, and every SYMFONY_* variable (symfony/runtime, phpunit-bridge and dotenv read them).
var envVars = []string{"PHPRC", "PHP_INI_SCAN_DIR", "APP_ENV", "APP_DEBUG", "COMPOSER_RUNTIME_ENV", "COMPOSER_VENDOR_DIR"}

// envVarLines returns the environment variables of the fingerprint.
func envVarLines() []string {
	var out []string
	for _, k := range envVars {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, "env "+k+"="+v)
		}
	}
	var sym []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SYMFONY_") {
			sym = append(sym, "env "+kv)
		}
	}
	sort.Strings(sym)
	return append(out, sym...)
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// installedFiles are the files of a Composer vendor directory that describe the installed dependency state.
var installedFiles = []string{"composer/installed.json", "composer/installed.php", "composer/autoload_classmap.php",
	"composer/autoload_files.php", "composer/autoload_namespaces.php", "composer/autoload_psr4.php", "composer/autoload_real.php",
	"composer/autoload_static.php", "autoload.php"}

// InstalledParts returns, for every vendor directory (repo-relative), the digests of its installed state on disk (docs/dynamic-layer.md
// §11.5): installed.json or installed.php is required, the autoload files are digested when present. A project without an
// installed vendor directory has no fingerprint.
func InstalledParts(root string, vendors []string) (map[string]string, error) {
	files := map[string]string{}
	for _, v := range vendors {
		for _, f := range installedFiles {
			rel := path.Join(v, f)
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			files[rel] = digest(b)
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(v), "composer", "installed.json"))
		if err != nil {
			return nil, fmt.Errorf("%s: no composer/installed.json on disk (run composer install): the dependency state is unknown", v)
		}
		if why := mirroredPathPackage(filepath.Join(root, filepath.FromSlash(v), "composer"), b); why != "" {
			return nil, fmt.Errorf("%s: %s", v, why)
		}
	}
	return files, nil
}

// mirroredPathPackage returns why the installed state cannot be fingerprinted when a package installed from a Composer path
// repository is a copy rather than a symbolic link: its files in the vendor directory change only when Composer mirrors them
// again, and installed.json does not change with the source. Symbolic links are followed by the collector to the repository
// files they point to.
func mirroredPathPackage(composerDir string, installed []byte) string {
	type pkg struct {
		Name string `json:"name"`
		Dist struct {
			Type string `json:"type"`
		} `json:"dist"`
		InstallPath string `json:"install-path"`
	}
	var pkgs []pkg
	var v2 struct {
		Packages []pkg `json:"packages"`
	}
	if err := json.Unmarshal(installed, &v2); err == nil && v2.Packages != nil {
		pkgs = v2.Packages
	} else if err := json.Unmarshal(installed, &pkgs); err != nil {
		return "unreadable composer/installed.json"
	}
	for _, p := range pkgs {
		if p.Dist.Type != "path" {
			continue
		}
		if p.InstallPath == "" {
			return "package " + p.Name + " comes from a path repository and its install path is unknown"
		}
		st, err := os.Lstat(filepath.Join(composerDir, filepath.FromSlash(p.InstallPath)))
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			return "package " + p.Name + " is mirrored from a path repository (not a symbolic link): its installed copy is not fingerprinted"
		}
	}
	return ""
}

// Fingerprint is the environment fingerprint of docs/dynamic-layer.md §11.5: the collector, OS and architecture, the PHP probe,
// the environment variables that change loading, and the installed dependency state of the given vendor directories.
func Fingerprint(p Probe, root string, vendors []string) (string, error) {
	files, err := InstalledParts(root, vendors)
	if err != nil {
		return "", err
	}
	interp := append([]string{"os " + runtime.GOOS + "/" + runtime.GOARCH}, p.Lines()...)
	interp = append(interp, envVarLines()...)
	return dynamic.Fingerprint(CollectorID, interp, files), nil
}

// Vendors returns the vendor directories of the PHP units that run from projects (repo-relative), sorted.
func Vendors(units []Unit) []string {
	var out []string
	for _, u := range units {
		if u.Vendor != "" {
			out = append(out, u.Vendor)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// VendorOf returns the Composer vendor directory of a runner bin directory ("vendor/bin" → "vendor"; "" when the bin directory
// is not inside a vendor directory).
func VendorOf(binDir string) string {
	if binDir == "" || binDir == "/" {
		return ""
	}
	if path.Base(binDir) == "bin" {
		return path.Dir(binDir)
	}
	return ""
}

// errNoVendor is returned when a unit has no vendor directory to fingerprint.
var errNoVendor = errors.New("no Composer vendor directory: the installed dependency state is unknown")

// UnitFingerprint is Fingerprint for the vendor directory of one unit.
func UnitFingerprint(p Probe, root string, binDir string) (string, error) {
	v := VendorOf(binDir)
	if v == "" {
		return "", errNoVendor
	}
	return Fingerprint(p, root, []string{v})
}
