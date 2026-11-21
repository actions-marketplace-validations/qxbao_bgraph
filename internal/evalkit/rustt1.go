package evalkit

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qxbao/bgraph/internal/eco/rust"
	"github.com/qxbao/bgraph/internal/scancache"
)

// RustReq is the requirement derived from one FAIL_TO_PASS name: the units that run tests defined in its candidate files.
type RustReq struct {
	Name      string   `json:"name"`
	Files     []string `json:"files,omitempty"` // candidate files (Files, or MacroFiles when Files is empty; the doc-test file for doc-tests)
	Units     []string `json:"units"`
	Ambiguous bool     `json:"ambiguous,omitempty"`
	Macro     bool     `json:"macro,omitempty"`
	Doc       bool     `json:"doc,omitempty"`
}

// RustViolation is one required unit missing from the selection, with what a reviewer needs to classify it.
type RustViolation struct {
	Unit    string   `json:"unit"`
	Name    string   `json:"name"`
	Files   []string `json:"files"`
	Changed []string `json:"changed"`
}

// RustT1Info records how the Rust oracle treated one task.
type RustT1Info struct {
	Kind       string          `json:"kind"`  // f2p | n2p
	Names      int             `json:"names"` // FAIL_TO_PASS names in the task
	Kept       int             `json:"kept"`  // names that became requirements (n2p: only those located in a file the test patch touches)
	Untouched  int             `json:"untouched,omitempty"`
	NotFound   int             `json:"not_found,omitempty"` // no candidate file at all: skipped
	NoOwner    int             `json:"no_owner,omitempty"`  // candidate file(s) run by no unit: skipped
	Doc        int             `json:"doc,omitempty"`
	Ambiguous  int             `json:"ambiguous,omitempty"`
	MacroOnly  int             `json:"macro_only,omitempty"`
	Reqs       []RustReq       `json:"reqs,omitempty"`
	Violations []RustViolation `json:"violations,omitempty"`
}

// rustRequired derives the required unit IDs of a task. dir is a repo directory holding commit p (base + test patch); touched is the
// set of files the test patch adds or modifies.
func rustRequired(ctx context.Context, dir, p string, touched map[string]string, t Task, ids []string) ([]string, *RustT1Info, error) {
	info := &RustT1Info{Kind: t.F2PKind, Names: len(ids)}
	if info.Kind == "" {
		info.Kind = "f2p"
	}
	gt, err := newGitTree(ctx, dir, p)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	a, err := rust.New(nil).Analyze(ctx, gt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	isTouched := func(f string) bool {
		_, ok := touched[f]
		return ok
	}
	owningPkg := func(f string) string {
		best := ""
		for _, pr := range a.Projects {
			d := pr.Dir
			if (d == "." || f == d || strings.HasPrefix(f, d+"/")) && (best == "" || len(d) > len(best)) {
				best = d
			}
		}
		return best
	}
	hasUnit := map[string]bool{}
	for _, u := range a.Tests {
		hasUnit[u.ID] = true
	}
	set := map[string]bool{}
	for _, l := range LocateRustTests(gt, ids) {
		cand := l.Files
		if len(cand) == 0 && !l.Doc {
			cand = l.MacroFiles
		}
		if len(cand) == 0 {
			info.NotFound++
			continue
		}
		if info.Kind == "n2p" {
			any := false
			for _, f := range cand {
				any = any || isTouched(f)
			}
			if !any {
				info.Untouched++
				continue
			}
		}
		req := RustReq{Name: l.Name, Files: cand, Ambiguous: l.Ambiguous, Macro: len(l.Files) == 0 && !l.Doc, Doc: l.Doc}
		us := map[string]bool{}
		for _, f := range cand {
			if l.Doc {
				if pd := owningPkg(f); pd != "" && hasUnit[rust.UnitID(pd, rust.KindDoc, "")] {
					us[rust.UnitID(pd, rust.KindDoc, "")] = true
				}
				continue
			}
			for _, u := range rust.DirectOwners(a, f) {
				us[u] = true
			}
		}
		for u := range us {
			req.Units = append(req.Units, u)
		}
		sort.Strings(req.Units)
		if len(req.Units) == 0 {
			info.NoOwner++
			continue
		}
		info.Kept++
		switch {
		case l.Doc:
			info.Doc++
		case req.Macro:
			info.MacroOnly++
		}
		if l.Ambiguous {
			info.Ambiguous++
		}
		for _, u := range req.Units {
			set[u] = true
		}
		info.Reqs = append(info.Reqs, req)
	}
	var out []string
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, info, nil
}

// rustMissing returns the required unit IDs that the selection lacks and records the violations (per name) in info.
func rustMissing(req []string, sel []SelUnit, info *RustT1Info, changed []string) []string {
	have := map[string]bool{}
	for _, u := range sel {
		have[u.Unit] = true
	}
	var miss []string
	for _, u := range req {
		if !have[u] {
			miss = append(miss, u)
		}
	}
	if info != nil {
		for _, r := range info.Reqs {
			for _, u := range r.Units {
				if !have[u] {
					info.Violations = append(info.Violations, RustViolation{Unit: u, Name: r.Name, Files: r.Files, Changed: changed})
				}
			}
		}
	}
	return miss
}

func (v RustViolation) String() string {
	return fmt.Sprintf("unit %s name %q file %v changed %v", v.Unit, v.Name, v.Files, v.Changed)
}
