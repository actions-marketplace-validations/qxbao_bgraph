package nodedyn

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/tree"
)

// derived: paths whose content is derived from tracked files or the environment (docs/dynamic-layer.md §8.2 merging): installed
// packages, git metadata, vite's bundled configs and caches.
func derived(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "node_modules", ".git", ".vite", ".vitest-cache":
			return true
		}
	}
	base := path.Base(p)
	return strings.Contains(base, ".timestamp-") && (strings.HasSuffix(base, ".mjs") || strings.HasSuffix(base, ".cjs") ||
		strings.HasSuffix(base, ".js"))
}

// RecordOf turns an outcome into a record (docs/dynamic-layer.md §3, §8). t is the working tree at the recorded commit; root is the
// repository root on disk (to tell untracked files from absent ones).
func RecordOf(o Outcome, u Unit, t tree.Tree, root, env, collector, commit string, now time.Time) dynamic.Record {
	r := dynamic.Record{Eco: "node", Unit: u.ID, Collector: collector, Commit: commit, Deps: map[string]string{}, Env: env,
		Complete: true, Pass: o.Exit == 0 && !o.TimedOut && o.Err == "", Recorded: now.UTC(), Toolchain: o.Analysis.Toolchain}
	var why []string
	switch {
	case o.TimedOut:
		why = append(why, "timeout")
	case o.Err != "":
		why = append(why, o.Err)
	}
	why = append(why, o.Analysis.Incomplete...)
	lookup := func(p string) (string, bool) {
		if t.Has(p) {
			return p, true
		}
		if m := t.FoldLookup(p); len(m) == 1 {
			return m[0], true
		}
		return "", false
	}
	add := func(p string) {
		if k := t.Key(p); k != "" {
			r.Deps[p] = k
		} else {
			why = append(why, "cannot hash "+p)
		}
	}
	for _, f := range o.Analysis.Files {
		if p, ok := lookup(f); ok {
			add(p)
			continue
		}
		if derived(f) {
			continue
		}
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil || st.IsDir() {
			continue // absent (additions are never narrowed) or a directory
		}
		why = append(why, "reads untracked file "+f)
	}
	if len(o.Analysis.Dirs) > 0 {
		files := t.Files()
		for _, d := range o.Analysis.Dirs {
			pre := d + "/"
			if d == "." {
				pre = ""
			} else if !t.IsDir(d) {
				for _, f := range files {
					if len(f) > len(pre) && strings.EqualFold(f[:len(pre)], pre) {
						pre = f[:len(pre)]
						break
					}
				}
			}
			i := sort.SearchStrings(files, pre)
			for ; i < len(files) && strings.HasPrefix(files[i], pre); i++ {
				if !derived(files[i]) {
					add(files[i])
				}
			}
		}
	}
	for _, f := range u.Files {
		if p, ok := lookup(f); ok {
			add(p)
		} else {
			why = append(why, "the unit's own file "+f+" is not in the tree")
		}
	}
	if len(why) > 0 {
		r.Complete = false
		seen := map[string]bool{}
		var out []string
		for _, w := range why {
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
		r.Incomplete = strings.Join(out, "; ")
	}
	return r
}
