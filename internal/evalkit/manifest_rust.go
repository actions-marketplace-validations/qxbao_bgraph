package evalkit

import (
	"sort"
	"strings"
)

// RustSplitRule describes the Rust dev/holdout split (by repo, ~70/30 by T1 task count, fixed before looking at any result).
const RustSplitRule = "sort repos (lowercased) by ascending sha256(name); walk that order and mark repos holdout until holdout holds >=30% of the T1 tasks and at least 3 repos, the rest dev"

// BuildRustManifest builds the deterministic Rust manifest. Unlike Node's every-third-repo rule, Rust has a few repos with many tasks
// and many with a handful, so the split walks the sha256 order accumulating task counts (T1 = tasks after SelectTasks).
func BuildRustManifest(t1 map[string]*ManifestRepo, ds []DatasetInfo, capPer int) Manifest {
	all := map[string]*ManifestRepo{}
	total := 0
	for k, v := range t1 {
		c := *v
		all[strings.ToLower(k)] = &c
		total += c.T1
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return sha(keys[i]) < sha(keys[j]) })
	m := Manifest{Ecosystem: "rust", Version: 1, SplitRule: RustSplitRule, CapPer: capPer, Datasets: ds}
	held, nHold := 0, 0
	for _, k := range keys {
		r := all[k]
		r.Role = "dev"
		if nHold < 3 || held*10 < total*3 {
			r.Role = "holdout"
			held += r.T1
			nHold++
		}
		sort.Strings(r.Sources)
		m.Repos = append(m.Repos, *r)
	}
	return m
}
