package jvmdyn

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// External artifacts (docs/dynamic.md §9.9): the build-definition digest of the fingerprint does not see an external
// dependency that changes without a file change (a SNAPSHOT, a version range, a rewritten local repository). Each class record
// therefore lists the archives outside the repository and the JDK that its JVM loaded classes from or read (the resolved
// external class path as the agent observed it), with their size and SHA-1; at selection time every listed archive must still
// exist with the same content, otherwise the record is not usable.

// External returns the archives (absolute paths) outside the repository and outside the fork's JDK that fork f loaded classes
// from or read, and the reasons its record is incomplete because an external class-path entry cannot be checked (a class
// directory outside the repository).
func (m *Mapper) External(f *Fork) (paths []string, why []string) {
	seen := map[string]bool{}
	loaded := true
	add := func(p string) {
		p = filepath.Clean(p)
		if !loaded {
			// Read only, never a class source, and gone after the run: a temporary class-path manifest jar (surefire's
			// booter jar); the classes it points to are recorded with their own archives.
			if _, err := os.Stat(p); err != nil {
				return
			}
		}
		if p == "" || seen[p] || m.rel(p) != "" || f.JavaHome != "" && under(p, f.JavaHome) {
			return
		}
		seen[p] = true
		lp := strings.ToLower(p)
		if strings.HasSuffix(lp, ".jar") || strings.HasSuffix(lp, ".zip") {
			paths = append(paths, p)
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			why = append(why, "loads classes from a directory outside the repository: "+p)
		}
	}
	for _, c := range f.Classes {
		if lp := locationPath(c.Location); lp != "" {
			add(lp)
		}
	}
	loaded = false
	for _, r := range f.Reads {
		lr := strings.ToLower(r)
		if strings.HasSuffix(lr, ".jar") || strings.HasSuffix(lr, ".zip") {
			add(r)
		}
	}
	return paths, why
}

var artifactCache sync.Map // "path|size|mtime" → "size:sha1"

// ArtifactDigest returns "size:sha1" of file p (cached by path, size and modification time).
func ArtifactDigest(p string) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	key := fmt.Sprintf("%s|%d|%d", p, st.Size(), st.ModTime().UnixNano())
	if v, ok := artifactCache.Load(key); ok {
		return v.(string), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	d := fmt.Sprintf("%d:%s", st.Size(), hex.EncodeToString(h.Sum(nil)))
	artifactCache.Store(key, d)
	return d, nil
}

// ArtifactsUnchanged returns "" when every recorded artifact exists with its recorded digest, otherwise why not.
func ArtifactsUnchanged(arts map[string]string) string {
	for p, want := range arts {
		got, err := ArtifactDigest(p)
		switch {
		case err != nil:
			return "external artifact " + p + " is missing"
		case got != want:
			return "external artifact " + p + " changed since the recording"
		}
	}
	return ""
}
