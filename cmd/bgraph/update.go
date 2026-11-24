package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// releaseBase is the GitHub releases URL of bgraph; tests point it at a local server.
var releaseBase = "https://github.com/qxbao/bgraph/releases"

const updateUsage = `Usage:
  bgraph update [--check] [--version VERSION] [--force]
`

// updater replaces a bgraph binary with a release build. Fields are set by runUpdate; tests set them directly.
type updater struct {
	base    string // releases URL
	exe     string // binary to replace
	current string // version of the running binary ("dev" for an unversioned build)
	goos    string
	goarch  string
	client  *http.Client
}

// runUpdate implements `bgraph update`: replace the running binary with the latest (or a given) release.
func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bgraph update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	want := fs.String("version", "", "release to install, e.g. 0.2.0 (default: the latest)")
	force := fs.Bool("force", false, "install even when the version is the same or the binary is a development build")
	fs.Usage = func() { fmt.Fprint(stderr, updateUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "update: extra arguments: %v\n", fs.Args())
		return 2
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: locating the bgraph binary:", err)
		return 1
	}
	u := &updater{base: releaseBase, exe: exe, current: buildVersion(), goos: runtime.GOOS, goarch: runtime.GOARCH,
		client: &http.Client{Timeout: 5 * time.Minute}}
	if err := u.run(context.Background(), *want, *check, *force, stdout); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func (u *updater) run(ctx context.Context, want string, check, force bool, stdout io.Writer) error {
	_ = os.Remove(u.exe + ".old") // left by a previous update on Windows

	target := strings.TrimPrefix(want, "v")
	if target == "" {
		v, err := u.latest(ctx)
		if err != nil {
			return fmt.Errorf("finding the latest release: %w", err)
		}
		target = v
	}
	dev := u.current == "dev" || strings.Contains(u.current, "-0.") // dev or a Go pseudo-version
	if check {
		switch {
		case dev:
			fmt.Fprintf(stdout, "bgraph %s (development build); latest release: %s\n", u.current, target)
		case compareVersions(target, u.current) > 0:
			fmt.Fprintf(stdout, "bgraph %s → %s available; run `bgraph update`\n", u.current, target)
		default:
			fmt.Fprintf(stdout, "bgraph %s is up to date\n", u.current)
		}
		return nil
	}
	if !force {
		if dev {
			return fmt.Errorf("bgraph %s is a development build; `bgraph update --force` replaces it with release %s", u.current, target)
		}
		if want == "" && compareVersions(target, u.current) <= 0 {
			fmt.Fprintf(stdout, "bgraph %s is up to date\n", u.current)
			return nil
		}
		if want != "" && target == u.current {
			fmt.Fprintf(stdout, "bgraph %s is already installed\n", u.current)
			return nil
		}
	}

	ext := ".tar.gz"
	if u.goos == "windows" {
		ext = ".zip"
	}
	asset := fmt.Sprintf("bgraph_%s_%s_%s%s", target, u.goos, u.goarch, ext)
	dir := u.base + "/download/v" + target + "/"
	sums, err := u.get(ctx, dir+"checksums.txt")
	if err != nil {
		return err
	}
	sum, err := checksumOf(sums, asset)
	if err != nil {
		return err
	}
	archive, err := u.get(ctx, dir+asset)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != sum {
		return fmt.Errorf("%s: SHA-256 mismatch (checksums.txt says %s)", asset, sum)
	}
	bin, err := extractBinary(archive, ext, u.goos)
	if err != nil {
		return fmt.Errorf("%s: %w", asset, err)
	}
	if err := replaceBinary(u.exe, bin, u.goos); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "bgraph %s → %s (%s)\n", u.current, target, u.exe)
	return nil
}

// latest returns the version of the latest release, read from the redirect of /releases/latest
// (no GitHub API call, so no API rate limit).
func (u *updater) latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.base+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := *u.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	_, tag, ok := strings.Cut(loc, "/tag/")
	if !ok || tag == "" {
		return "", fmt.Errorf("%s/latest: unexpected response %s (Location %q)", u.base, resp.Status, loc)
	}
	return strings.TrimPrefix(path.Base(tag), "v"), nil
}

func (u *updater) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

// checksumOf finds the SHA-256 of name in a checksums.txt ("<hex>  <name>" per line).
func checksumOf(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s (no build for this platform?)", name)
}

// extractBinary returns the bgraph executable from a release archive.
func extractBinary(archive []byte, ext, goos string) ([]byte, error) {
	name := "bgraph"
	if goos == "windows" {
		name = "bgraph.exe"
	}
	if ext == ".zip" {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == name && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("no %s in the archive", name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no %s in the archive", name)
		}
		if err != nil {
			return nil, err
		}
		if path.Base(h.Name) == name && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// replaceBinary writes bin next to exe and swaps it in. Windows cannot overwrite a running executable but can
// rename it, so the old binary is moved to exe+".old" (removed by the next update).
func replaceBinary(exe string, bin []byte, goos string) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".bgraph-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (%w); rerun with the permissions that installed bgraph there", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	_, err = tmp.Write(bin)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmpName, 0o755)
	}
	if err != nil {
		return err
	}
	if goos == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(tmpName, exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}
		_ = os.Remove(old) // fails while the old binary runs; the next update removes it
		return nil
	}
	return os.Rename(tmpName, exe)
}

// compareVersions compares two semantic versions (without "v"): -1, 0 or 1. A pre-release sorts before its release.
func compareVersions(a, b string) int {
	ac, apre, _ := strings.Cut(a, "-")
	bc, bpre, _ := strings.Cut(b, "-")
	ap, bp := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < max(len(ap), len(bp)); i++ {
		var x, y int
		if i < len(ap) {
			x, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			y, _ = strconv.Atoi(bp[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	default:
		return 1
	}
}
