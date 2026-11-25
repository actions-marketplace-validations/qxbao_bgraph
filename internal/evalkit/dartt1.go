package evalkit

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/eco"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/scancache"
)

// The Dart T1 oracle (file-granular, modeled on the Ruby one): FAIL_TO_PASS names are located by LocateDartTests in P (base + test
// patch); a located test file requires the unit that runs it: the file unit (ID = its repo-relative path) or, when the package
// has a whole-project unit ("<dart-all-tests:DIR>"), that unit. Ambiguous names require every candidate (strict oracle); the
// touched-candidate variant (only the candidates the test patch touches) is recorded per violation (PatchOnlyGone,
// PatchOnlyMissing). Hooks ((setUpAll)/(tearDownAll) without a path) and names located outside a Dart test file are dropped and
// listed; names not found count as not_found. The units come from the "dart" ecosystem registered in bgraph.Ecosystems, looked up
// by name so that this package builds before the ecosystem exists.

// DartT1Info records how the Dart oracle treated one task (same shape as the Ruby oracle's).
type DartT1Info = RbT1Info

const dartWholePrefix = "<dart-all-tests:"

// dartParseWholeUnitID returns the package directory of a whole-project unit ID "<dart-all-tests:DIR>" ("" for the repo root, as
// the contract writes "." or "").
func dartParseWholeUnitID(id string) (string, bool) {
	if !strings.HasPrefix(id, dartWholePrefix) || !strings.HasSuffix(id, ">") {
		return "", false
	}
	d := strings.TrimSuffix(strings.TrimPrefix(id, dartWholePrefix), ">")
	if d == "." {
		d = ""
	}
	return d, true
}

// dartEcosystem returns the registered Dart ecosystem.
func dartEcosystem() (eco.Ecosystem, error) {
	for _, e := range bgraph.Ecosystems(nil) {
		if e.Name() == "dart" {
			return e, nil
		}
	}
	return nil, fmt.Errorf("the dart ecosystem is not registered in bgraph.Ecosystems")
}

// dartPrefetchWant: the blobs the Dart analysis reads, plus the sources other ecosystems scan in mixed repos.
func dartPrefetchWant(f string) bool {
	b := path.Base(f)
	switch path.Ext(b) {
	case ".dart", ".yaml", ".yml", ".js", ".mjs", ".cjs", ".ts", ".py", ".go", ".rs", ".toml", ".rb", ".swift":
		return true
	}
	switch b {
	case "pubspec.lock", "package.json", "bgraph.toml", ".fvmrc", ".flutter-version":
		return true
	}
	return false
}

// dartWithin reports whether the whole-project unit of package dir runs f: f's nearest package is dir (a nested package has its
// own suite). f may also be the directory of a required whole unit.
func dartWithin(info *DartT1Info) func(f, dir string) bool {
	norm := func(d string) string {
		if d == "." {
			return ""
		}
		return d
	}
	return func(f, dir string) bool {
		p := f
		if info != nil {
			if d, ok := info.pkgOf[f]; ok {
				p = d
			}
		}
		return norm(p) == norm(dir)
	}
}

// dartUnitsFor maps a located file to the units running it: its file unit, else the whole-project unit of its package.
func dartUnitsFor(a *eco.Analysis, within func(f, dir string) bool) func(string) []string {
	unitOf := map[string]string{}
	var wholes []model.TestUnit
	for _, u := range a.Tests {
		if u.Whole {
			wholes = append(wholes, u)
			continue
		}
		for _, f := range u.Files {
			unitOf[f] = u.ID
		}
	}
	return func(f string) []string {
		if u, ok := unitOf[f]; ok {
			return []string{u}
		}
		var out []string
		for _, w := range wholes {
			if within(f, w.Project) {
				out = append(out, w.ID)
			}
		}
		return out
	}
}

// dartRequired derives the required unit IDs of a task. dir is a repo directory holding commit p (base + test patch); tp is the
// set of files the test patch touches; cmd is the dataset's test command.
func dartRequired(ctx context.Context, dir, p string, ids []string, tp map[string]bool, kind, cmd string) ([]string, *DartT1Info, error) {
	if kind == "" {
		kind = "f2p"
	}
	info := &DartT1Info{Kind: kind, Names: len(ids)}
	e, err := dartEcosystem()
	if err != nil {
		return nil, info, err
	}
	gt, err := newGitTreeWant(ctx, dir, p, dartPrefetchWant)
	if err != nil {
		return nil, info, err
	}
	defer gt.Close()
	lt, err := dartPrefetch(ctx, dir, gt)
	if err != nil {
		return nil, info, err
	}
	a, err := e.Analyze(ctx, lt, scancache.New())
	if err != nil {
		return nil, info, err
	}
	info.a, info.files = a, lt.Files()
	locs := LocateDartTests(lt, ids, tp, cmd)
	info.pkgOf = map[string]string{}
	for _, l := range locs {
		for _, f := range l.Files {
			if d, ok := DartPackageDir(lt, f); ok {
				info.pkgOf[f] = d
			}
		}
	}
	req, err := dartRequirements(locs, dartUnitsFor(a, dartWithin(info)), kind, info)
	return req, info, err
}

// dartRequirements turns located names into requirements (split from dartRequired for tests).
func dartRequirements(locs []DartTestLoc, unitsFor func(string) []string, kind string, info *DartT1Info) ([]string, error) {
	set := map[string]bool{}
	noOwn := map[string]bool{}
	for _, l := range locs {
		switch {
		case l.Hook:
			info.Dropped = append(info.Dropped, "hook: "+l.Name)
			continue
		case l.NotUnit:
			info.Dropped = append(info.Dropped, "not a test file: "+l.Name)
			continue
		case l.NotFound:
			info.NotFound++
			continue
		case kind == "n2p" && !l.InPatch:
			info.Untouched++
			continue
		}
		req := RbReq{Name: l.Name, Form: l.Form, Via: l.Via, Files: l.Files, Ambiguous: l.Ambiguous}
		us, pus := map[string]bool{}, map[string]bool{}
		for _, f := range l.Files {
			uf := unitsFor(f)
			if len(uf) == 0 {
				noOwn[f] = true
			}
			for _, u := range uf {
				us[u] = true
			}
		}
		for _, f := range l.PatchFiles {
			for _, u := range unitsFor(f) {
				pus[u] = true
			}
		}
		req.Units, req.PatchUnits = sortedKeys(us), sortedKeys(pus)
		if len(req.Units) == 0 {
			info.NoOwner++
			continue
		}
		info.Kept++
		if l.Ambiguous {
			info.Ambiguous++
		}
		for _, u := range req.Units {
			set[u] = true
		}
		info.Reqs = append(info.Reqs, req)
	}
	info.NoOwnerFiles = sortedKeys(noOwn)
	return sortedKeys(set), nil
}

// dartMissing returns the required unit IDs the selection lacks (a selected whole-project unit covers the files of its package)
// and records the violations and the patch-only variant.
func dartMissing(req []string, sel []SelUnit, info *DartT1Info, changed []string) []string {
	return fileUnitMissing(req, sel, info, changed, dartParseWholeUnitID, dartWithin(info))
}

// dartDiagnose fills the reach fields and Class of the violations from the Dart graph at P.
func dartDiagnose(info *DartT1Info) { fileUnitDiagnose(info, dartParseWholeUnitID) }

// WriteDartReport writes dart.md next to t1.md (same layout as ruby.md).
func WriteDartReport(dir string, rs []T1Result) error {
	return writeFileOracleReport(dir, "dart.md", "Dart", rs, func(r T1Result) (*RbT1Info, *RbWide) { return r.Dart, r.DartW })
}
