package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("license")}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipOf(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(body)
	zw.Close()
	return buf.Bytes()
}

// fakeReleases serves /latest (redirect to tag v<latest>), checksums.txt and one archive per version.
func fakeReleases(t *testing.T, latest string, assets map[string][]byte, corrupt bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/v"+latest, http.StatusFound)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		if parts[1] == "checksums.txt" {
			for name, data := range assets {
				sum := sha256.Sum256(data)
				if corrupt {
					sum[0] ^= 1
				}
				fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
			}
			return
		}
		data, ok := assets[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestUpdater(t *testing.T, srv *httptest.Server, current, goos string) *updater {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "bgraph")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &updater{base: srv.URL, exe: exe, current: current, goos: goos, goarch: "amd64", client: srv.Client()}
}

func TestUpdateReplacesBinary(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			assets := map[string][]byte{"bgraph_0.3.0_linux_amd64.tar.gz": tarGz(t, "bgraph", []byte("new linux")),
				"bgraph_0.3.0_windows_amd64.zip": zipOf(t, "bgraph.exe", []byte("new windows"))}
			u := newTestUpdater(t, fakeReleases(t, "0.3.0", assets, false), "0.2.0", goos)
			var out bytes.Buffer
			if err := u.run(context.Background(), "", false, false, &out); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(u.exe)
			if string(got) != "new "+goos || !strings.Contains(out.String(), "0.2.0 → 0.3.0") {
				t.Fatalf("binary %q, output %q", got, out.String())
			}
			if _, err := os.Stat(u.exe + ".old"); err == nil {
				t.Error(".old left behind")
			}
			if entries, _ := os.ReadDir(filepath.Dir(u.exe)); len(entries) != 1 {
				t.Errorf("leftover files: %v", entries)
			}
		})
	}
}

func TestUpdateUpToDateAndCheck(t *testing.T) {
	srv := fakeReleases(t, "0.3.0", nil, false)
	for _, tc := range []struct {
		current string
		check   bool
		want    string
	}{
		{"0.3.0", false, "up to date"},
		{"0.4.0", false, "up to date"},
		{"0.3.0", true, "up to date"},
		{"0.2.0", true, "0.2.0 → 0.3.0 available"},
		{"dev", true, "development build"},
	} {
		u := newTestUpdater(t, srv, tc.current, "linux")
		var out bytes.Buffer
		if err := u.run(context.Background(), "", tc.check, false, &out); err != nil || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%+v: err %v, output %q", tc, err, out.String())
		}
		if got, _ := os.ReadFile(u.exe); string(got) != "old" {
			t.Errorf("%+v: binary changed", tc)
		}
	}
}

func TestUpdateRefusals(t *testing.T) {
	assets := map[string][]byte{"bgraph_0.3.0_linux_amd64.tar.gz": tarGz(t, "bgraph", []byte("new"))}
	for _, tc := range []struct {
		name, current string
		corrupt       bool
		want          string
	}{
		{"dev build", "dev", false, "--force"},
		{"pseudo-version", "0.2.1-0.20261120000000-abcdef123456", false, "--force"},
		{"checksum", "0.2.0", true, "SHA-256 mismatch"},
	} {
		u := newTestUpdater(t, fakeReleases(t, "0.3.0", assets, tc.corrupt), tc.current, "linux")
		err := u.run(context.Background(), "", false, false, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
		if got, _ := os.ReadFile(u.exe); string(got) != "old" {
			t.Errorf("%s: binary changed", tc.name)
		}
	}
	u := newTestUpdater(t, fakeReleases(t, "0.3.0", assets, false), "0.2.0", "darwin")
	if err := u.run(context.Background(), "", false, false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Errorf("missing platform: %v", err)
	}
}

func TestUpdateExplicitVersionAndForce(t *testing.T) {
	assets := map[string][]byte{"bgraph_0.1.0_linux_amd64.tar.gz": tarGz(t, "bgraph", []byte("v0.1.0")),
		"bgraph_0.3.0_linux_amd64.tar.gz": tarGz(t, "bgraph", []byte("v0.3.0"))}
	srv := fakeReleases(t, "0.3.0", assets, false)
	u := newTestUpdater(t, srv, "0.2.0", "linux")
	if err := u.run(context.Background(), "v0.1.0", false, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(u.exe); string(got) != "v0.1.0" {
		t.Fatalf("downgrade: %q", got)
	}
	u = newTestUpdater(t, srv, "dev", "linux")
	if err := u.run(context.Background(), "", false, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(u.exe); string(got) != "v0.3.0" {
		t.Fatalf("force: %q", got)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"0.2.0", "0.2.0", 0}, {"0.10.0", "0.9.1", 1}, {"1.0.0", "1.0.0-rc.1", 1}, {"1.0.0-rc.1", "1.0.0-rc.2", -1}, {"0.2", "0.2.1", -1}} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCLIUpdateUsage(t *testing.T) {
	var o, e bytes.Buffer
	if code := run([]string{"update", "extra"}, &o, &e); code != 2 {
		t.Errorf("exit %d", code)
	}
	if code := run([]string{"update", "--bogus"}, &o, &e); code != 2 {
		t.Errorf("exit %d", code)
	}
}
