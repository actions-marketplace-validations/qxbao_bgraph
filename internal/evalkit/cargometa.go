package evalkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/eco/rust"
	"github.com/qxbao/bgraph/internal/scancache"
)

// CargoMetaResult compares bgraph's Rust analysis of one commit with `cargo metadata --no-deps`.
type CargoMetaResult struct {
	Repo       string   `json:"repo"`
	Commit     string   `json:"commit"`
	Error      string   `json:"error,omitempty"`
	Packages   int      `json:"packages"`
	Units      int      `json:"units"`
	Match      bool     `json:"match"`
	Mismatches []string `json:"mismatches,omitempty"`
}

type cmTarget struct {
	Kind    []string `json:"kind"`
	Name    string   `json:"name"`
	SrcPath string   `json:"src_path"`
	Test    bool     `json:"test"`
	Doctest bool     `json:"doctest"`
}

type cmMeta struct {
	Packages []struct {
		Name         string     `json:"name"`
		ManifestPath string     `json:"manifest_path"`
		Targets      []cmTarget `json:"targets"`
	} `json:"packages"`
}

// CargoMetaCompare runs the comparison for repo (a clone under work/repos) at each commit, using a temporary worktree under work/wt.
func CargoMetaCompare(ctx context.Context, work, repo string, commits []string) []CargoMetaResult {
	var out []CargoMetaResult
	dir := RustRepoDir(work, repo)
	for i, c := range commits {
		r := CargoMetaResult{Repo: repo, Commit: c}
		wt := filepath.Join(work, "wt", fmt.Sprintf("cm_%s_%d", repoDirName(repo), i))
		os.RemoveAll(wt)
		git(ctx, dir, "worktree", "prune")
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			r.Error = err.Error()
			out = append(out, r)
			continue
		}
		if _, err := git(ctx, dir, "worktree", "add", "--detach", wt, c); err != nil {
			r.Error = "worktree: " + err.Error()
			out = append(out, r)
			continue
		}
		func() {
			defer func() {
				git(context.Background(), dir, "worktree", "remove", "--force", wt)
				os.RemoveAll(wt)
				git(context.Background(), dir, "worktree", "prune")
			}()
			cmd := exec.CommandContext(ctx, "cargo", "metadata", "--no-deps", "--format-version", "1", "--offline")
			cmd.Dir = wt
			var so, se bytes.Buffer
			cmd.Stdout, cmd.Stderr = &so, &se
			if err := cmd.Run(); err != nil {
				msg := strings.TrimSpace(se.String())
				if len(msg) > 800 {
					msg = msg[:800]
				}
				r.Error = "cargo metadata: " + err.Error() + ": " + msg
				return
			}
			var m cmMeta
			if err := json.Unmarshal(so.Bytes(), &m); err != nil {
				r.Error = "decode: " + err.Error()
				return
			}
			gt, err := newGitTree(ctx, dir, c)
			if err != nil {
				r.Error = "tree: " + err.Error()
				return
			}
			defer gt.Close()
			a, err := rust.New(nil).Analyze(ctx, gt, scancache.New())
			if err != nil {
				r.Error = "analyze: " + err.Error()
				return
			}
			r.Packages, r.Units = len(m.Packages), len(a.Tests)
			r.Mismatches = compareCargoMeta(wt, m, a)
			r.Match = len(r.Mismatches) == 0
		}()
		out = append(out, r)
	}
	return out
}

// relToRoot makes an absolute path reported by cargo relative to the worktree root with forward slashes.
func relToRoot(root, p string) string {
	norm := func(s string) string { return strings.TrimPrefix(filepath.ToSlash(s), "//?/") }
	r, q := strings.TrimSuffix(norm(root), "/"), norm(p)
	if len(q) >= len(r) && strings.EqualFold(q[:len(r)], r) {
		q = strings.TrimPrefix(q[len(r):], "/")
	}
	return q
}

// compareCargoMeta compares package set, per-package target set (kind+name -> unit ID) and src_path ownership.
// Mapping of cargo targets: lib -> "<pkg>::lib" when test=true and "<pkg>::doc" when doctest=true; bin/test -> unit when test=true;
// bench and example -> always a unit; test=false targets must have no unit but an ownership edge to their src_path.
func compareCargoMeta(root string, m cmMeta, a *eco.Analysis) []string {
	var diffs []string
	bg := map[string]bool{}
	for _, p := range a.Projects {
		bg[p.Dir] = true
	}
	units := map[string]bool{}
	for _, u := range a.Tests {
		units[u.ID] = true
	}
	edge := map[string]bool{}
	for _, e := range a.Edges {
		edge[e.From+"\x00"+e.To] = true
	}
	cargoPk := map[string]bool{}
	wantUnits := map[string]bool{}
	for _, p := range m.Packages {
		pdir := strings.TrimSuffix(strings.TrimSuffix(relToRoot(root, p.ManifestPath), "Cargo.toml"), "/")
		if pdir == "" {
			pdir = "."
		}
		cargoPk[pdir] = true
		if !bg[pdir] {
			diffs = append(diffs, "package "+pdir+" ("+p.Name+") in cargo but not in bgraph")
			continue
		}
		for _, t := range p.Targets {
			kind, ok := cargoKind(t.Kind)
			if !ok {
				continue
			}
			var id string
			unit := true
			switch kind {
			case rust.KindLib:
				id = rust.UnitID(pdir, kind, "")
				unit = t.Test
				if t.Doctest {
					wantUnits[rust.UnitID(pdir, rust.KindDoc, "")] = true
				}
			case rust.KindBin, rust.KindTest:
				id = rust.UnitID(pdir, kind, t.Name)
				unit = t.Test
			default:
				id = rust.UnitID(pdir, kind, t.Name)
			}
			if unit {
				wantUnits[id] = true
			}
			src := relToRoot(root, t.SrcPath)
			switch {
			case unit && !units[id]:
				diffs = append(diffs, fmt.Sprintf("target %s (kind %v, src %s): cargo runs it, bgraph has no unit", id, t.Kind, src))
			case unit && !containsStr(rust.DirectOwners(a, src), id):
				diffs = append(diffs, fmt.Sprintf("target %s: src_path %s is not owned by the unit in bgraph (owners %v)", id, src, rust.DirectOwners(a, src)))
			case !unit && units[id]:
				diffs = append(diffs, fmt.Sprintf("target %s (test=false): bgraph has a unit", id))
			case !unit && !edge[rust.Node(id)+"\x00"+src]:
				diffs = append(diffs, fmt.Sprintf("target %s (test=false): no ownership edge to %s in bgraph", id, src))
			}
		}
	}
	for d := range bg {
		if !cargoPk[d] {
			diffs = append(diffs, "package "+d+" in bgraph but not in cargo metadata (non-member?)")
		}
	}
	for id := range units {
		if !wantUnits[id] {
			if pd, _, _, ok := rust.ParseUnitID(id); ok && cargoPk[pd] {
				diffs = append(diffs, "unit "+id+" in bgraph but not a cargo test target")
			}
		}
	}
	sort.Strings(diffs)
	return diffs
}

func cargoKind(kinds []string) (string, bool) {
	for _, k := range kinds {
		switch k {
		case "lib", "rlib", "proc-macro", "dylib", "cdylib", "staticlib":
			return rust.KindLib, true
		case "bin":
			return rust.KindBin, true
		case "test":
			return rust.KindTest, true
		case "bench":
			return rust.KindBench, true
		case "example":
			return rust.KindExample, true
		}
	}
	return "", false // custom-build
}

func containsStr(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// CargoMetaCommits picks up to n distinct base commits of repo's tasks, in sha256 order (deterministic, not chronological).
func CargoMetaCommits(tasks []Task, repo string, n int) []string {
	seen := map[string]bool{}
	var cs []string
	for _, t := range tasks {
		if strings.EqualFold(t.Repo, repo) && !seen[t.BaseCommit] {
			seen[t.BaseCommit] = true
			cs = append(cs, t.BaseCommit)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return sha(cs[i]) < sha(cs[j]) })
	if len(cs) > n {
		cs = cs[:n]
	}
	return cs
}
