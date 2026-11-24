package evalkit

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/dynamic"
	"github.com/qxbao/bgraph/internal/dynamic/phpdyn"
	"github.com/qxbao/bgraph/internal/eco/php"
	"github.com/qxbao/bgraph/internal/model"
	"github.com/qxbao/bgraph/internal/tree"
)

// Phase 12 (docs/dynamic-layer.md §11): the PHP per-test-file layer checked without the collector as oracle.
//
//   - Hybrid T2 (RunPHPDynT2): records made once at R in a copy of the T2 checkout (`bgraph record --eco php`); for every
//     killed mutant the whole suite runs at M with a JUnit report, and the red test files are read per file from it (minus the
//     files red at R). The static selection and the selection with the PHP layer on R→M follow; a violation is a red test file
//     the hybrid selection does not run (its unit is narrowed and the file is not among the kept ones).
//   - Guard rate (RunPHPGuardRate): over first-parent commits, the selection parent→commit with the layer on and no records: the
//     guard refuses (bucketed by reason) or passes and stops at "no records". For accepted commits the function-level
//     refinement (body guard and PHP's tokenizer) is run on the changed files, as the layer would.
//   - Replay (RunPHPReplay): fresh records at each parent of the commits the guard accepts, the selection parent→commit, and
//     every removed test file run alone at the commit as the runner adapter runs it (and at the parent when it fails there).
//
// Everything runs in one Docker container (the PHP image with Xdebug) so that records and selections see the same PHP.

const phpDynSuiteTimeout = 20 * time.Minute

// PHPDynConfig configures the PHP layer evaluation of one repo.
type PHPDynConfig struct {
	Repo     T2Repo
	Clone    string // the working copy (vendor installed; its working tree is modified and restored)
	Work     string // state and index directories of this evaluation
	Out      string // reports and the per-file oracle store
	Oracle   string // the cached PHP T2 oracle (oracle.jsonl, unit level): which mutants were killed
	Jobs     int
	Rerecord bool
	Logf     func(string, ...any)
}

func (c PHPDynConfig) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf("%s: "+f, append([]any{c.Repo.Name}, a...)...)
	}
}

// phpCheckout checks dir out at rev with a clean tree, keeping the untracked Composer install (vendor/, composer.lock).
func phpCheckout(ctx context.Context, dir, rev string) error {
	if _, err := git(ctx, dir, "checkout", "-q", "-f", "--detach", rev); err != nil {
		return err
	}
	_, err := git(ctx, dir, "clean", "-q", "-fdx", "-e", "/vendor/", "-e", "/composer.lock")
	return err
}

// phpClean reports whether dir has no tracked change and no untracked file outside the Composer install.
func phpClean(ctx context.Context, dir string) error {
	out, err := git(ctx, dir, "status", "--porcelain", "--ignored", "--untracked-files=all")
	if err != nil {
		return err
	}
	var dirty []string
	for _, ln := range strings.Split(out, "\n") {
		if len(ln) < 4 {
			continue
		}
		p := strings.TrimPrefix(ln[3:], `"`) // quoted when it has spaces or special characters
		if p == "vendor/" || strings.HasPrefix(p, "vendor/") || p == "composer.lock" {
			continue
		}
		dirty = append(dirty, strings.TrimSpace(ln))
	}
	if len(dirty) > 0 {
		return fmt.Errorf("checkout not clean: %s", strings.Join(firstNs(dirty, 8), "; "))
	}
	return nil
}

func firstNs(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("+%d more", len(s)-n))
}

// ---- JUnit per file ----

type phpJUnitNode struct {
	XMLName  xml.Name
	File     string         `xml:"file,attr"`
	Errors   int            `xml:"errors,attr"`
	Failures int            `xml:"failures,attr"`
	Failure  []struct{}     `xml:"failure"`
	Error    []struct{}     `xml:"error"`
	Suites   []phpJUnitNode `xml:"testsuite"`
	Cases    []phpJUnitNode `xml:"testcase"`
}

// PHPJUnitFiles reads a PHPUnit or Pest JUnit report: the number of test cases, every test file seen, and the red ones (a
// failure or error in a test case, or error/failure counts on a test file's suite: setUpBeforeClass and data provider errors
// have no test case). File attributes are absolute (PHPUnit) or relative to the project directory (Pest, with "::<test>"
// appended); they are returned relative to root.
func PHPJUnitFiles(r io.Reader, root, project string) (tests int, seen, red map[string]bool, err error) {
	var n phpJUnitNode
	if err := xml.NewDecoder(r).Decode(&n); err != nil {
		return 0, nil, nil, err
	}
	if n.XMLName.Local != "testsuites" && n.XMLName.Local != "testsuite" {
		return 0, nil, nil, fmt.Errorf("unexpected JUnit root %q", n.XMLName.Local)
	}
	seen, red = map[string]bool{}, map[string]bool{}
	var walk func(n phpJUnitNode, file string, isCase bool)
	walk = func(n phpJUnitNode, file string, isCase bool) {
		if f := phpJUnitFile(n.File, root, project); f != "" {
			file = f
		}
		if file != "" {
			seen[file] = true
		}
		if isCase {
			tests++
			if len(n.Failure) > 0 || len(n.Error) > 0 {
				red[file] = true
			}
		} else if file != "" && n.File != "" && n.Errors+n.Failures > 0 {
			red[file] = true
		}
		for _, s := range n.Suites {
			walk(s, file, false)
		}
		for _, c := range n.Cases {
			walk(c, file, true)
		}
	}
	walk(n, "", n.XMLName.Local == "testcase")
	delete(red, "")
	return tests, seen, red, nil
}

func phpJUnitFile(attr, root, project string) string {
	if attr == "" {
		return ""
	}
	if i := strings.Index(attr, ".php::"); i >= 0 {
		attr = attr[:i+4]
	}
	attr = filepath.ToSlash(attr)
	if path.IsAbs(attr) || filepath.IsAbs(attr) {
		rel, err := filepath.Rel(root, filepath.FromSlash(attr))
		if err != nil {
			return attr
		}
		return filepath.ToSlash(rel)
	}
	return path.Clean(path.Join(project, attr))
}

// phpSuiteRun is one run of the whole suite.
type phpSuiteRun struct {
	Tests   int
	Seen    map[string]bool
	Red     map[string]bool
	Ms      int64
	Timeout bool
	Exit    string // non-zero exit (with a usable report: warnings, deprecations or failures)
}

// phpWholeSuite runs the repo's whole suite in clone with a JUnit report (the runner as the T2 oracle runs it).
func (c PHPDynConfig) phpWholeSuite(ctx context.Context) (phpSuiteRun, error) {
	report := filepath.Join(c.Work, "junit-"+c.Repo.Dir+".xml")
	os.MkdirAll(c.Work, 0o755)
	os.Remove(report)
	args := append([]string{c.Repo.Bin, "--do-not-cache-result", "--log-junit", report}, c.Repo.Args...)
	t0 := time.Now()
	out, tl, to, runErr := runProc(ctx, c.Clone, append([]string{"XDEBUG_MODE=off"}, c.Repo.Env...), phpDynSuiteTimeout, "php", args...)
	run := phpSuiteRun{Ms: time.Since(t0).Milliseconds(), Timeout: to}
	if ctx.Err() != nil {
		return run, ctx.Err()
	}
	if to {
		return run, nil
	}
	f, err := os.Open(report)
	if err != nil {
		return run, fmt.Errorf("no JUnit report (%v): %s %s", runErr, tail(out, 600), tail(tl, 600))
	}
	defer f.Close()
	run.Tests, run.Seen, run.Red, err = PHPJUnitFiles(f, c.Clone, ".")
	if err != nil {
		return run, fmt.Errorf("JUnit report: %w (%v): %s", err, runErr, tail(tl, 600))
	}
	if run.Tests == 0 {
		return run, fmt.Errorf("no test in the JUnit report (%v): %s %s", runErr, tail(out, 600), tail(tl, 600))
	}
	if runErr != nil {
		run.Exit = runErr.Error()
	}
	return run, nil
}

// PHPDynBaseline is the whole suite at R, twice.
type PHPDynBaseline struct {
	Repo  string   `json:"repo"`
	R     string   `json:"r"`
	Tests int      `json:"tests"`
	Files int      `json:"files"` // test files in the JUnit report
	Red   []string `json:"red,omitempty"`
	RunMs []int64  `json:"run_ms"`
	Exit  string   `json:"exit,omitempty"`
	Error string   `json:"error,omitempty"`
}

func (c PHPDynConfig) baseline(ctx context.Context) PHPDynBaseline {
	b := PHPDynBaseline{Repo: c.Repo.Name, R: c.Repo.R}
	red := map[string]bool{}
	for i := 0; i < 2; i++ {
		run, err := c.phpWholeSuite(ctx)
		if err != nil || run.Timeout {
			b.Error = fmt.Sprintf("run %d: timeout=%v %v", i+1, run.Timeout, err)
			return b
		}
		if i > 0 && run.Tests != b.Tests {
			b.Error = fmt.Sprintf("test count changed from %d to %d", b.Tests, run.Tests)
		}
		b.Tests, b.Files, b.Exit = run.Tests, len(run.Seen), run.Exit
		b.RunMs = append(b.RunMs, run.Ms)
		for f := range run.Red {
			red[f] = true
		}
	}
	b.Red = sortedKeys(red)
	return b
}

// ---- per-file oracle ----

// PHPDynOracle is one mutant's verdict from the whole suite at M (never from the collector).
type PHPDynOracle struct {
	Repo   string   `json:"repo"`
	R      string   `json:"r"`
	Mutant T2Mutant `json:"mutant"`
	// Status: killed | survived | nocompile | timeout | error
	Status  string   `json:"status"`
	Red     []string `json:"red,omitempty"`      // red test files at M that are not red at R
	RedBase []string `json:"red_base,omitempty"` // red at M and at R (baseline failures)
	Tests   int      `json:"tests,omitempty"`
	Exit    string   `json:"exit,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Ms      int64    `json:"ms"`
}

func (o PHPDynOracle) key() string { return o.Repo + "|" + o.R + "|" + o.Mutant.ID }

func loadPHPDynOracle(file string) (map[string]PHPDynOracle, error) {
	out := map[string]PHPDynOracle{}
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		var o PHPDynOracle
		if json.Unmarshal(sc.Bytes(), &o) == nil {
			out[o.key()] = o
		}
	}
	return out, sc.Err()
}

func appendPHPDynOracle(file string, o PHPDynOracle) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// withMutant writes the mutated file into the clone, runs fn, and restores the checkout (the file's original content, then a
// clean check).
func (c PHPDynConfig) withMutant(ctx context.Context, m T2Mutant, mutated []byte, fn func() error) (err error) {
	file := filepath.Join(c.Clone, filepath.FromSlash(m.File))
	orig, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.WriteFile(file, orig, 0o644); rerr != nil && err == nil {
			err = rerr
		}
		if cerr := phpCheckout(ctx, c.Clone, c.Repo.R); cerr != nil && err == nil {
			err = cerr
		}
		if cerr := phpClean(ctx, c.Clone); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err := os.WriteFile(file, mutated, 0o644); err != nil {
		return err
	}
	return fn()
}

// verdict runs the whole suite at M (the mutated file written into the clone) and reads the red test files.
func (c PHPDynConfig) verdict(ctx context.Context, m T2Mutant, mutated []byte, baseRed map[string]bool) (o PHPDynOracle) {
	o = PHPDynOracle{Repo: c.Repo.Name, R: c.Repo.R, Mutant: m}
	t0 := time.Now()
	defer func() { o.Ms = time.Since(t0).Milliseconds() }()
	err := c.withMutant(ctx, m, mutated, func() error {
		out, tl, to, err := runProc(ctx, c.Clone, c.Repo.Env, 2*time.Minute, "php", "-l", m.File)
		if to {
			o.Status = "timeout"
			return nil
		}
		if err != nil {
			o.Status, o.Detail = "nocompile", firstN(strings.TrimSpace(out+tl), 300)
			return nil
		}
		run, err := c.phpWholeSuite(ctx)
		if err != nil {
			return err
		}
		if run.Timeout {
			o.Status = "timeout"
			return nil
		}
		o.Tests, o.Exit = run.Tests, run.Exit
		for _, f := range sortedKeys(run.Red) {
			if baseRed[f] {
				o.RedBase = append(o.RedBase, f)
			} else {
				o.Red = append(o.Red, f)
			}
		}
		o.Status = "survived"
		if len(o.Red) > 0 {
			o.Status = "killed"
		}
		return nil
	})
	if err != nil {
		o.Status, o.Detail = "error", err.Error()
	}
	return o
}

// ---- selections ----

// PHPDynSel summarizes one selection as test files run.
type PHPDynSel struct {
	Scope    string         `json:"scope"`
	Units    int            `json:"units"`             // selected PHP units
	Files    int            `json:"files"`             // test files the selection runs (a whole unit: its listed files)
	Narrowed bool           `json:"narrowed"`          // a PHP unit runs only some of its test files
	Removed  []string       `json:"removed,omitempty"` // removed test files (repo-relative)
	Skip     string         `json:"skip,omitempty"`    // the PHP layer's dynamic_skipped reason
	Notes    map[string]int `json:"notes,omitempty"`   // note class of the PHP units -> units
	Missed   []string       `json:"missed,omitempty"`  // red test files the selection does not run
}

func phpNarrowable(t model.SelectedTest) bool {
	return !t.Whole && (t.Runner == phpdyn.RunnerPHPUnit || t.Runner == phpdyn.RunnerPest)
}

func phpEnclosed(project, f string) bool {
	p := strings.Trim(project, "/")
	return p == "" || p == "." || f == p || strings.HasPrefix(f, p+"/")
}

// PHPRuns reports whether sel runs test file f: a selected PHP unit whose project encloses f runs it whole, or narrowed with f
// among its kept files.
func PHPRuns(sel model.Selection, f string) bool {
	for _, t := range sel.Tests {
		if !phpEnclosed(t.Project, f) || t.Runner != phpdyn.RunnerPHPUnit && t.Runner != phpdyn.RunnerPest {
			continue
		}
		if !t.ClassFilter || slices.Contains(t.Classes, f) {
			return true
		}
	}
	return false
}

// phpNoteClass reduces a PHP unit's dynamic note to a class.
func phpNoteClass(note string) string {
	switch {
	case strings.HasPrefix(note, "not narrowed by the PHP layer"):
		return "layer skipped"
	case strings.HasPrefix(note, "PHP layer: all"):
		return "all test files run (each saw a changed file or has no usable record)"
	case strings.HasPrefix(note, "PHP layer:") && strings.Contains(note, "test files run"):
		return "narrowed"
	case strings.HasPrefix(note, "PHP layer kept the whole unit: "):
		return "kept whole: " + RbReasonClass(strings.TrimPrefix(note, "PHP layer kept the whole unit: "))
	}
	return RbReasonClass(note)
}

// PHPSummarize summarizes sel; lists are the file lists of the PHP units (unit ID -> test files), red the oracle's red files.
func PHPSummarize(sel model.Selection, lists map[string][]string, red []string) PHPDynSel {
	d := PHPDynSel{Scope: string(sel.Scope), Skip: ecoSkipReason(sel, phpdyn.Eco)}
	for _, t := range sel.Tests {
		if !phpNarrowable(t) {
			continue
		}
		d.Units++
		if t.ClassFilter {
			d.Narrowed = true
			d.Files += len(t.Classes)
		} else {
			d.Files += len(lists[t.Unit])
		}
		if t.Dynamic != "" {
			if d.Notes == nil {
				d.Notes = map[string]int{}
			}
			d.Notes[phpNoteClass(t.Dynamic)]++
		}
	}
	for _, n := range sel.Narrowed {
		if _, f, ok := strings.Cut(n.Unit, "#"); ok {
			d.Removed = append(d.Removed, f)
		}
	}
	sort.Strings(d.Removed)
	for _, f := range red {
		if !PHPRuns(sel, f) {
			d.Missed = append(d.Missed, f)
		}
	}
	return d
}

// phpLists returns the file lists of the PHP units in the store of stateDir (latest file-list record per unit) and their sizes.
func phpLists(stateDir string) map[string][]string {
	recs, _ := dynamic.Load(dynamic.StorePath(stateDir, phpdyn.Eco))
	lists := map[string][]string{}
	when := map[string]time.Time{}
	for _, r := range recs {
		if strings.Contains(r.Unit, "#") || r.Eco != phpdyn.Eco {
			continue
		}
		if t, ok := when[r.Unit]; ok && r.Recorded.Before(t) {
			continue
		}
		when[r.Unit], lists[r.Unit] = r.Recorded, r.Classes
	}
	return lists
}

func listTotal(lists map[string][]string) int {
	n := 0
	for _, l := range lists {
		n += len(l)
	}
	return n
}

// ---- function level ----

// PHPLevel is how the PHP layer treats one changed file: "function" (the refinement and the tokenizer cross-check accepted it),
// "file" (with the reason), "test" (a test file: file level), "doc".
type PHPLevel struct {
	Path  string `json:"path"`
	Level string `json:"level"`
	Note  string `json:"note,omitempty"`
	Funcs int    `json:"funcs,omitempty"`
}

// phpTestFile mirrors the PHP analysis' test role (internal/eco/php): a *Test.php or .phpt file.
func phpTestFile(f string) bool {
	return strings.HasSuffix(path.Base(f), "Test.php") || strings.HasSuffix(strings.ToLower(f), ".phpt")
}

// phpChanged classifies the changed files of a selection the guard accepted as the layer does before narrowing (code at file
// level, documentation), then refines them as applyPHPFiles does: body guard, PHP's tokenizer cross-check, downgrade.
func phpChanged(ctx context.Context, phpExe string, files []string, base, head tree.Tree) []phpdyn.Changed {
	var ch []phpdyn.Changed
	for _, f := range files {
		if php.DynamicSource(f) {
			ch = append(ch, phpdyn.Changed{Path: f, Kind: phpdyn.KindFile, Note: "file level", Test: phpTestFile(f)})
		} else {
			ch = append(ch, phpdyn.Changed{Path: f, Kind: phpdyn.KindDoc})
		}
	}
	ch = phpdyn.Refine(ch, base, head)
	cf := phpdyn.CheckFiles(ch, base, head)
	checked := map[string]bool{}
	for _, f := range cf {
		checked[f.Path] = true
	}
	return phpdyn.Downgrade(ch, phpdyn.CrossCheck(ctx, phpExe, cf), checked)
}

// PHPLevels reports the treatment of each changed file.
func PHPLevels(ch []phpdyn.Changed) []PHPLevel {
	var out []PHPLevel
	for _, c := range ch {
		l := PHPLevel{Path: c.Path}
		switch {
		case c.Kind == phpdyn.KindLines:
			l.Level, l.Funcs = "function", len(c.Members)
		case c.Kind == phpdyn.KindDoc:
			l.Level = "doc"
		case c.Test:
			l.Level = "test"
		default:
			l.Level, l.Note = "file", c.Note
		}
		out = append(out, l)
	}
	return out
}

// PHPCommitLevel reduces the levels of a change: "function" when every changed non-test PHP file was refined, "file" when none
// was, "mixed", or "none" (only test files and documentation changed).
func PHPCommitLevel(ls []PHPLevel) string {
	fn, file := 0, 0
	for _, l := range ls {
		switch l.Level {
		case "function":
			fn++
		case "file":
			file++
		}
	}
	switch {
	case fn > 0 && file > 0:
		return "mixed"
	case fn > 0:
		return "function"
	case file > 0:
		return "file"
	}
	return "none"
}

// phpFileLevelOnly narrows the static selection st as the layer would with every changed PHP file at file level (the
// refinement off): the test files it would run. -1 when it cannot be computed.
func phpFileLevelOnly(st model.Selection, ch []phpdyn.Changed, recs []dynamic.Record, base tree.Tree, probe phpdyn.Probe, root string,
	lists map[string][]string) int {
	fl := make([]phpdyn.Changed, len(ch))
	for i, c := range ch {
		fl[i] = c
		if c.Kind == phpdyn.KindLines {
			fl[i] = phpdyn.Changed{Path: c.Path, Kind: phpdyn.KindFile, Note: "file level (refinement off)", Test: c.Test}
		}
	}
	unitEco := map[string]string{}
	for _, t := range st.Tests {
		if phpNarrowable(t) {
			unitEco[t.Unit] = phpdyn.Eco
		}
	}
	envs := map[string]string{}
	sel := phpdyn.Narrow(st, phpdyn.Params{Records: recs, Base: base, UnitEco: unitEco, Changed: fl,
		Env: func(t model.SelectedTest) (string, string) {
			if e, ok := envs[t.BinDir]; ok {
				return e, ""
			}
			e, err := phpdyn.UnitFingerprint(probe, root, t.BinDir)
			if err != nil {
				return "", err.Error()
			}
			envs[t.BinDir] = e
			return e, ""
		}})
	return PHPSummarize(sel, lists, nil).Files
}

// ---- recording ----

// PHPRecordStat is one recording.
type PHPRecordStat struct {
	Reused  bool                  `json:"reused,omitempty"`
	Summary *bgraph.RecordSummary `json:"summary,omitempty"`
	Ms      int64                 `json:"ms"`
	Error   string                `json:"error,omitempty"`
}

func phpRecord(ctx context.Context, ix *bgraph.Index, jobs int, rerecord bool) PHPRecordStat {
	if !rerecord {
		if cur, err := ix.DynamicStatusOf(ctx, phpdyn.Eco); err == nil && cur.CurrentEnv > 0 {
			return PHPRecordStat{Reused: true}
		}
	}
	t0 := time.Now()
	sum, err := ix.Record(ctx, bgraph.RecordOptions{Eco: phpdyn.Eco, Jobs: jobs, Timeout: 10 * time.Minute})
	st := PHPRecordStat{Ms: time.Since(t0).Milliseconds()}
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Summary = &sum
	return st
}

// PHPRecCount counts the records of a PHP store (the latest per unit): test-file records complete and passed, incomplete, failed
// (passed false), incomplete reasons, and the file-list records.
type PHPRecCount struct {
	Files      int            `json:"files"`
	Complete   int            `json:"complete"` // complete and passed
	Incomplete int            `json:"incomplete"`
	Failed     int            `json:"failed"`
	Reasons    map[string]int `json:"reasons,omitempty"`
	Lists      int            `json:"lists"`
	ListsOK    int            `json:"lists_ok"`
	Runners    []string       `json:"runners,omitempty"`
	ListWhy    []string       `json:"list_why,omitempty"`
}

func phpRecCount(stateDir string) PHPRecCount {
	recs, _ := dynamic.Load(dynamic.StorePath(stateDir, phpdyn.Eco))
	last := map[string]dynamic.Record{}
	for _, r := range recs {
		if cur, ok := last[r.Unit]; !ok || !r.Recorded.Before(cur.Recorded) {
			last[r.Unit] = r
		}
	}
	var c PHPRecCount
	for _, r := range last {
		if !strings.Contains(r.Unit, "#") {
			c.Lists++
			if r.Complete {
				c.ListsOK++
			} else {
				c.ListWhy = append(c.ListWhy, r.Unit+": "+r.Incomplete)
			}
			c.Runners = append(c.Runners, r.Runner)
			continue
		}
		c.Files++
		switch {
		case !r.Complete:
			c.Incomplete++
			if c.Reasons == nil {
				c.Reasons = map[string]int{}
			}
			c.Reasons[RbReasonClass(r.Incomplete)]++
		case !r.Pass:
			c.Failed++
		default:
			c.Complete++
		}
	}
	sort.Strings(c.Runners)
	return c
}

func phpOpen(ctx context.Context, clone, stateDir string) (*bgraph.Index, error) {
	return bgraph.Open(ctx, clone, bgraph.Options{StateDir: stateDir, PHP: "php"})
}

// ---- hybrid T2 ----

// PHPDynMutant is one mutant of the hybrid pass.
type PHPDynMutant struct {
	ID     string       `json:"id"`
	File   string       `json:"file"`
	Line   int          `json:"line"`
	Op     string       `json:"op"`
	Oracle PHPDynOracle `json:"oracle"`
	Total  int          `json:"total"` // listed test files of the PHP units
	Static PHPDynSel    `json:"static"`
	Hybrid PHPDynSel    `json:"hybrid"`
	// FileLevel: test files the layer would run with the function-level refinement off (-1: not computed).
	FileLevel int        `json:"file_level"`
	Levels    []PHPLevel `json:"levels,omitempty"`
	// Confirmed: the violations reproduced by a second whole-suite run at M (Unconfirmed: red only in the first run).
	Confirmed   []string `json:"confirmed,omitempty"`
	Unconfirmed []string `json:"unconfirmed,omitempty"`
	SelectMs    int64    `json:"select_ms"`
	Error       string   `json:"error,omitempty"`
}

// PHPDynT2Result is the hybrid pass over one repo.
type PHPDynT2Result struct {
	Repo     string               `json:"repo"`
	R        string               `json:"r"`
	PHP      string               `json:"php"` // PHP and Xdebug versions of the image (records and selections alike)
	Baseline PHPDynBaseline       `json:"baseline"`
	Record   PHPRecordStat        `json:"record"`
	Status   bgraph.DynamicStatus `json:"status"`
	Lists    map[string]int       `json:"lists"` // file-list size per PHP unit
	Records  PHPRecCount          `json:"records"`
	Mutants  []PHPDynMutant       `json:"mutants"`
	Tried    int                  `json:"tried"` // mutants whose verdict was computed or reused (killed or not)
	Ms       int64                `json:"ms"`
	Error    string               `json:"error,omitempty"`
}

// RunPHPDynT2 runs the hybrid pass on one repo: the cached oracle's killed mutants when the repo has one, otherwise mutants
// generated from the manifest entry until Repo.Mutants are killed.
func RunPHPDynT2(ctx context.Context, cfg PHPDynConfig, only map[string]bool) (res PHPDynT2Result) {
	t0 := time.Now()
	r := cfg.Repo
	res = PHPDynT2Result{Repo: r.Name, R: r.R}
	defer func() { res.Ms = time.Since(t0).Milliseconds() }()
	fail := func(f string, a ...any) PHPDynT2Result {
		res.Error = fmt.Sprintf(f, a...)
		cfg.logf("%s", res.Error)
		return res
	}
	if err := phpCheckout(ctx, cfg.Clone, r.R); err != nil {
		return fail("checkout R: %v", err)
	}
	if err := phpClean(ctx, cfg.Clone); err != nil {
		return fail("%v", err)
	}
	probe, err := phpdyn.RunProbe(ctx, "php")
	if err != nil {
		return fail("PHP probe: %v", err)
	}
	res.PHP = "PHP " + probe.Version + ", Xdebug " + probe.Xdebug
	res.Baseline = cfg.baseline(ctx)
	if res.Baseline.Error != "" {
		return fail("baseline: %s", res.Baseline.Error)
	}
	// The suite may leave files behind (Pest writes .phpunit.cache even with --do-not-cache-result): the recording starts clean.
	if err := phpCheckout(ctx, cfg.Clone, r.R); err != nil {
		return fail("checkout R after the baseline: %v", err)
	}
	baseRed := map[string]bool{}
	for _, f := range res.Baseline.Red {
		baseRed[f] = true
	}
	cfg.logf("baseline: %d tests in %d files, red at R: %v, %v ms", res.Baseline.Tests, res.Baseline.Files, res.Baseline.Red, res.Baseline.RunMs)
	state := filepath.Join(cfg.Work, "state", r.Dir)
	ix, err := phpOpen(ctx, cfg.Clone, state)
	if err != nil {
		return fail("open: %v", err)
	}
	defer ix.Close()
	res.Record = phpRecord(ctx, ix, cfg.Jobs, cfg.Rerecord)
	if res.Record.Error != "" {
		return fail("record: %s", res.Record.Error)
	}
	if s := res.Record.Summary; s != nil {
		cfg.logf("record: %d test files, %d complete, %d incomplete, %d failed in %s %v %v", s.Units, s.Complete, s.Incomplete, s.Failed,
			time.Duration(res.Record.Ms)*time.Millisecond, s.Reasons, s.Warnings)
	}
	if err := phpClean(ctx, cfg.Clone); err != nil {
		return fail("after recording: %v", err)
	}
	res.Status, _ = ix.DynamicStatusOf(ctx, phpdyn.Eco)
	res.Records = phpRecCount(state)
	lists := phpLists(state)
	res.Lists = map[string]int{}
	for u, l := range lists {
		res.Lists[u] = len(l)
	}
	recs, _ := dynamic.Load(dynamic.StorePath(state, phpdyn.Eco))

	// The mutants: the cached oracle's killed ones, or generated ones.
	var mutants []T2Mutant
	cached := false
	if cfg.Oracle != "" {
		old, err := loadOracle(cfg.Oracle)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail("oracle: %v", err)
		}
		seen := map[string]bool{}
		for _, o := range old {
			if o.Repo == r.Name && o.R == r.R && o.Status == "killed" && !seen[o.Mutant.ID] {
				seen[o.Mutant.ID] = true
				mutants = append(mutants, o.Mutant)
				cached = true
			}
		}
	}
	if !cached {
		if mutants, err = r.phpMutants(ctx, cfg.Clone); err != nil {
			return fail("mutants: %v", err)
		}
	}
	store := filepath.Join(cfg.Out, "oracle-phpdyn.jsonl")
	have, err := loadPHPDynOracle(store)
	if err != nil {
		return fail("per-file oracle: %v", err)
	}
	baseTree, err := newGitTreeWant(ctx, cfg.Clone, r.R, func(string) bool { return false })
	if err != nil {
		return fail("base tree: %v", err)
	}
	defer baseTree.Close()
	idx := filepath.Join(cfg.Work, "idx")
	killed := 0
	for _, m := range mutants {
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			return res
		}
		if only != nil && !only[m.ID] {
			continue
		}
		if !cached && killed >= r.Mutants {
			break
		}
		commit, mutated, err := r.MutantCommit(ctx, cfg.Clone, idx, m)
		if err != nil {
			res.Mutants = append(res.Mutants, PHPDynMutant{ID: m.ID, File: m.File, Line: m.Line, Op: m.Op, Error: "mutant commit: " + err.Error()})
			continue
		}
		key := PHPDynOracle{Repo: r.Name, R: r.R, Mutant: m}.key()
		o, ok := have[key]
		if !ok || o.Mutant != m || o.Status == "error" {
			o = cfg.verdict(ctx, m, mutated, baseRed)
			if ctx.Err() != nil {
				res.Error = ctx.Err().Error()
				return res
			}
			if err := appendPHPDynOracle(store, o); err != nil {
				return fail("oracle store: %v", err)
			}
		}
		res.Tried++
		if o.Status != "killed" {
			cfg.logf("%s %s:%d %s: %s %s", m.ID, m.File, m.Line, m.Op, o.Status, firstN(o.Detail, 200))
			if cached { // a cached killed mutant that is not killed here: reported
				res.Mutants = append(res.Mutants, PHPDynMutant{ID: m.ID, File: m.File, Line: m.Line, Op: m.Op, Oracle: o, FileLevel: -1})
			}
			continue
		}
		killed++
		pm := PHPDynMutant{ID: m.ID, File: m.File, Line: m.Line, Op: m.Op, Oracle: o, Total: listTotal(lists), FileLevel: -1}
		s0 := time.Now()
		st, err := ix.Select(ctx, bgraph.Change{Base: r.R, Head: commit, Dynamic: bgraph.DynamicOff})
		var hy model.Selection
		if err == nil {
			hy, err = ix.Select(ctx, bgraph.Change{Base: r.R, Head: commit, Dynamic: bgraph.DynamicOn})
		}
		pm.SelectMs = time.Since(s0).Milliseconds()
		if err != nil {
			pm.Error = err.Error()
		} else {
			pm.Static, pm.Hybrid = PHPSummarize(st, lists, o.Red), PHPSummarize(hy, lists, o.Red)
			if pm.Hybrid.Skip == "" {
				if head, err := newGitTreeWant(ctx, cfg.Clone, commit, func(p string) bool { return slices.Contains(st.Changed, p) }); err == nil {
					ch := phpChanged(ctx, "php", st.Changed, baseTree, head)
					head.Close()
					pm.Levels = PHPLevels(ch)
					pm.FileLevel = phpFileLevelOnly(st, ch, recs, baseTree, probe, cfg.Clone, lists)
				}
			}
			if len(pm.Hybrid.Missed) > 0 {
				pm.Confirmed, pm.Unconfirmed = cfg.confirm(ctx, m, mutated, pm.Hybrid.Missed)
			}
		}
		cfg.logf("%s %s:%d %s: red %d, static %d/%d, hybrid %d (file level only %d) %s%s%s", m.ID, m.File, m.Line, m.Op, len(o.Red),
			pm.Static.Files, pm.Total, pm.Hybrid.Files, pm.FileLevel, PHPCommitLevel(pm.Levels),
			map[bool]string{true: " VIOLATION " + strings.Join(pm.Hybrid.Missed, ","), false: ""}[len(pm.Hybrid.Missed) > 0],
			map[bool]string{true: "; skipped: " + pm.Hybrid.Skip, false: ""}[pm.Hybrid.Skip != ""]+pm.Error)
		res.Mutants = append(res.Mutants, pm)
	}
	if err := phpClean(ctx, cfg.Clone); err != nil {
		res.Error = "at the end: " + err.Error()
	}
	return res
}

// confirm reruns the whole suite at M and splits the missed files into reproduced and not reproduced.
func (c PHPDynConfig) confirm(ctx context.Context, m T2Mutant, mutated []byte, missed []string) (yes, no []string) {
	var run phpSuiteRun
	err := c.withMutant(ctx, m, mutated, func() error {
		var err error
		run, err = c.phpWholeSuite(ctx)
		return err
	})
	for _, f := range missed {
		if err == nil && !run.Timeout && run.Red[f] {
			yes = append(yes, f)
		} else if err == nil && !run.Timeout {
			no = append(no, f)
		} else {
			yes = append(yes, f) // not reproducible either way: counted
		}
	}
	return yes, no
}

// phpRunFile runs one test file in root as the runner adapter runs a narrowed unit with one operand (without the collector).
// Passed: exit status 0.
func phpRunFile(ctx context.Context, root string, u phpdyn.Unit, file, cacheDir string) (bool, string, error) {
	argv, err := phpdyn.Argv(u, file, "php", root, cacheDir)
	if err != nil {
		return false, "", err
	}
	out, tl, to, err := runProc(ctx, filepath.Join(root, filepath.FromSlash(u.Project)), []string{"XDEBUG_MODE=off"}, 15*time.Minute, argv[0], argv[1:]...)
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	if to {
		return false, "timeout: " + tl, nil
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return false, "", err
	}
	return err == nil, out + "\n" + tl, nil
}
