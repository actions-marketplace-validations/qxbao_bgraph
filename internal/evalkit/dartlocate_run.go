package evalkit

import (
	"context"

	"github.com/qxbao/bgraph/internal/tree"
)

// DartLocateRow is the locator outcome for one Dart task.
type DartLocateRow struct {
	ID      string        `json:"instance_id"`
	Repo    string        `json:"repo"`
	Source  string        `json:"source"`
	Kind    string        `json:"f2p_kind,omitempty"`
	F2P     int           `json:"f2p_count"`
	Err     string        `json:"error,omitempty"`
	Scope   DartCmdScope  `json:"cmd_scope"`
	Locs    []DartTestLoc `json:"-"`
	Unique  int           `json:"unique"`
	Ambig   int           `json:"ambiguous"`
	NotFnd  int           `json:"not_found"`
	Hooks   int           `json:"hooks"`
	InPatch int           `json:"in_patch"`
}

// dartPrefetch follows the symbolic links of gt and fetches, in one batch, the blobs of the files the locator indexes that are not
// scanned yet (partial clone).
func dartPrefetch(ctx context.Context, repoDir string, gt *gitTree) (tree.Tree, error) {
	lt := followGitLinks(gt)
	want := map[string]bool{}
	for f := range DartIndexFiles(lt.Files()) {
		r := dartRealPath(lt, f)
		if _, cached := dartScanCache.Load(gt.oid[r]); !cached {
			want[r] = true
		}
	}
	return lt, prefetchBlobs(ctx, repoDir, gt, func(f string) bool { return want[f] })
}

// DartLocateTask builds P (base + test patch, git plumbing only, no checkout) for one task in the clone repoDir, prefetches the
// test-side Dart blobs that are not scanned yet in one batch (partial clone) and runs the locator with the task's test command.
func DartLocateTask(ctx context.Context, repoDir, idxFile string, t Task) DartLocateRow {
	row := DartLocateRow{ID: t.InstanceID, Repo: t.Repo, Source: t.Source, Kind: t.F2PKind}
	ids, err := ParseF2P(t.F2P)
	if err != nil {
		row.Err = err.Error()
		return row
	}
	row.F2P = len(ids)
	ib := newIndexBuilder(repoDir, idxFile, t.BaseCommit)
	ib.ignoreWhitespace = true
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
	lt, err := dartPrefetch(ctx, repoDir, gt)
	if err != nil {
		row.Err = "prefetch: " + err.Error()
		return row
	}
	row.Scope = ParseDartCmd(lt, t.TestCmd)
	row.Locs = locateDart(lt, buildDartIndex(lt), ids, patchFiles(t.TestPatch), row.Scope)
	for _, l := range row.Locs {
		switch {
		case l.Hook:
			row.Hooks++
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
