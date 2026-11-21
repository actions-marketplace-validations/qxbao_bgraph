package evalkit

import (
	"context"
)

// PyLocateRow is the locator outcome for one task.
type PyLocateRow struct {
	ID     string      `json:"instance_id"`
	Repo   string      `json:"repo"`
	Source string      `json:"source"`
	F2P    int         `json:"f2p_count"`
	Err    string      `json:"error,omitempty"`
	Locs   []PyTestLoc `json:"-"`
	Unique int         `json:"unique"`
	Ambig  int         `json:"ambiguous"`
	NotFnd int         `json:"not_found"`
}

// PyLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task in the clone repoDir and runs LocatePythonTests
// over the P tree. Blobs are prefetched in one batch, and only when some name needs file contents.
// TODO(python-phase-2): map the located files to python units with internal/eco/python and feed T1 (see RustLocateTask / rustt1.go).
func PyLocateTask(ctx context.Context, repoDir, idxFile string, t Task) PyLocateRow {
	row := PyLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source}
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
	want := func(string) bool { return false }
	if pyNeedsScan(ids) {
		want = pyIsTestFile
	}
	gt, err := newGitTreeWant(ctx, repoDir, p, want)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	defer gt.Close()
	row.Locs = LocatePythonTests(gt, t.Repo, ids)
	for _, l := range row.Locs {
		switch {
		case l.NotFound:
			row.NotFnd++
		case l.Ambiguous:
			row.Ambig++
		default:
			row.Unique++
		}
	}
	return row
}
