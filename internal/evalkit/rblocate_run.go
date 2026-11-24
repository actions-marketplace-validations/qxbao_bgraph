package evalkit

import (
	"context"
)

// RbLocateRow is the locator outcome for one Ruby task.
type RbLocateRow struct {
	ID      string      `json:"instance_id"`
	Repo    string      `json:"repo"`
	Source  string      `json:"source"`
	Kind    string      `json:"f2p_kind,omitempty"`
	F2P     int         `json:"f2p_count"`
	Err     string      `json:"error,omitempty"`
	Locs    []RbTestLoc `json:"-"`
	Unique  int         `json:"unique"`
	Ambig   int         `json:"ambiguous"`
	NotFnd  int         `json:"not_found"`
	InPatch int         `json:"in_patch"`
}

// RbLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task in the clone repoDir, prefetches the Ruby
// test-side blobs that are not scanned yet in one batch (partial clone) and runs LocateRubyTests.
func RbLocateTask(ctx context.Context, repoDir, idxFile string, t Task) RbLocateRow {
	row := RbLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source, Kind: t.F2PKind}
	ids, err := ParseF2P(t.F2P)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	row.F2P = len(ids)
	ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
	defer ib.Close()
	p, err := ib.Apply(ctx, t.TestPatch, "eval: test patch")
	if err != nil {
		row.Err = "test_patch: " + err.Error()
		return row
	}
	gt, err := newGitTreeWant(ctx, repoDir, p, func(string) bool { return false })
	if err != nil {
		row.Err = err.Error()
		return row
	}
	defer gt.Close()
	if err := prefetchBlobs(ctx, repoDir, gt, func(f string) bool {
		if !rbScanned(f) {
			return false
		}
		_, cached := rbScanCache.Load(gt.oid[f])
		return !cached
	}); err != nil {
		row.Err = "prefetch: " + err.Error()
		return row
	}
	row.Locs = LocateRubyTests(gt, ids, patchFiles(t.TestPatch))
	for _, l := range row.Locs {
		switch {
		case l.NotFound:
			row.NotFnd++
		case l.Ambiguous:
			row.Ambig++
		default:
			row.Unique++
		}
		if l.InPatch {
			row.InPatch++
		}
	}
	return row
}
