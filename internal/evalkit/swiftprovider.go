package evalkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/swift"
	"github.com/qxbao/bgraph/internal/scancache"
)

// SwiftProviderResult distinguishes target/file comparisons from package-wide coverage.
type SwiftProviderResult struct {
	Repo       string   `json:"repo"`
	Commit     string   `json:"commit"`
	Package    string   `json:"package"`
	Mode       string   `json:"mode,omitempty"`
	Targets    int      `json:"test_targets"`
	Files      int      `json:"test_files"`
	Error      string   `json:"error,omitempty"`
	Mismatches []string `json:"mismatches,omitempty"`
}

type swiftDescription struct {
	Targets []struct {
		Name    string
		Type    string
		Path    string
		Sources []string
	}
}

func swiftMetadata(ctx context.Context, dir string, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "swift", args...)
	cmd.Dir = dir
	swiftProviderProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	data, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", strings.Join(args, " "), ctx.Err())
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
		}
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return json.Unmarshal(data, out)
}

func swiftProviderPackage(ctx context.Context, worktree string, result SwiftProviderResult, a *eco.Analysis) SwiftProviderResult {
	dir := filepath.Join(worktree, filepath.FromSlash(result.Package))
	var declared, described swiftDescription
	if err := swiftMetadata(ctx, dir, &declared, "package", "dump-package"); err != nil {
		result.Error = err.Error()
		return result
	}
	if err := swiftMetadata(ctx, dir, &described, "package", "describe", "--type", "json"); err != nil {
		result.Error = err.Error()
		return result
	}
	projects := map[string]bool{}
	for _, p := range a.Projects {
		projects[p.Dir] = true
	}
	if !projects[result.Package] {
		result.Mismatches = append(result.Mismatches, "missing project "+result.Package)
	}
	units := map[string]bool{}
	var whole string
	for _, u := range a.Tests {
		if u.Project != result.Package {
			continue
		}
		units[u.ID] = true
		if swiftOpaqueUnit(u) {
			whole = u.ID
		}
	}
	result.Mode = "parsed"
	if whole != "" {
		result.Mode = "opaque"
	}
	owners := swiftFileOwners(a)
	want := map[string]bool{}
	for _, t := range declared.Targets {
		if t.Type == "test" {
			want[swift.UnitID(result.Package, t.Name)] = true
		}
	}
	result.Targets = len(want)
	for _, t := range described.Targets {
		if t.Type != "test" {
			continue
		}
		id := swift.UnitID(result.Package, t.Name)
		if !want[id] {
			result.Mismatches = append(result.Mismatches, "describe lists "+id+" absent from dump-package")
		}
		expected := id
		if whole != "" {
			expected = whole
		}
		for _, f := range t.Sources {
			result.Files++
			file := path.Join(result.Package, filepath.ToSlash(t.Path), filepath.ToSlash(f))
			if !containsStr(owners[file], expected) {
				result.Mismatches = append(result.Mismatches, expected+" does not own "+file)
			}
		}
	}
	if whole == "" {
		for id := range want {
			if !units[id] {
				result.Mismatches = append(result.Mismatches, "missing "+id)
			}
		}
		for id := range units {
			if !want[id] {
				result.Mismatches = append(result.Mismatches, "extra "+id)
			}
		}
	}
	sort.Strings(result.Mismatches)
	return result
}

// SwiftProviderCompare checks each base revision in a disposable worktree using
// the installed SwiftPM. dump-package supplies targets, describe supplies sources.
func SwiftProviderCompare(ctx context.Context, work, repo string, commits []string) []SwiftProviderResult {
	clone := RustRepoDir(work, repo)
	var results []SwiftProviderResult
	for _, commit := range commits {
		results = append(results, swiftProviderCommit(ctx, clone, work, repo, commit)...)
	}
	return results
}

func swiftProviderCommit(ctx context.Context, clone, work, repo, commit string) []SwiftProviderResult {
	failure := func(err error) []SwiftProviderResult {
		return []SwiftProviderResult{{Repo: repo, Commit: commit, Error: err.Error()}}
	}
	t, err := newGitTreeWant(ctx, clone, commit, swiftSource)
	if err != nil {
		return failure(err)
	}
	defer t.Close()
	analysis, err := swift.New().Analyze(ctx, t, scancache.New())
	if err != nil {
		return failure(err)
	}
	var dirs []string
	for _, file := range t.Files() {
		if path.Base(file) == "Package.swift" {
			dirs = append(dirs, path.Dir(file))
		}
	}
	if len(dirs) == 0 {
		return failure(fmt.Errorf("no committed Package.swift"))
	}
	parent := filepath.Join(work, "wt")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return failure(err)
	}
	tmp, err := os.MkdirTemp(parent, "swift-provider-")
	if err != nil {
		return failure(err)
	}
	defer os.RemoveAll(tmp)
	// SwiftPM uses a local dependency's directory basename as its package identity.
	name := path.Base(repo)
	if !filepath.IsLocal(name) || name == "." {
		return failure(fmt.Errorf("invalid Swift repository name %q", repo))
	}
	wt := filepath.Join(tmp, name)
	if _, err := git(ctx, clone, "worktree", "add", "--detach", wt, commit); err != nil {
		return failure(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		git(cleanup, clone, "worktree", "remove", "--force", wt)
	}()
	var out []SwiftProviderResult
	for _, dir := range dirs {
		r := SwiftProviderResult{Repo: repo, Commit: commit, Package: dir}
		out = append(out, swiftProviderPackage(ctx, wt, r, analysis))
	}
	return out
}
