# Dynamic layer (Phase 6) — design contract

Status: implementation contract for Phase 6 (docs/plan.md §4.5, §10.2, §10.3, §11). Python first; other ecosystems reuse the core.

## Goal and non-goal

The static layer is sound but widens where it cannot see (Python name-based `importlib`, `__import__`, dynamic `sys.path`: 266/270 SWE-bench
tasks widened, median selection 1.0 instead of ~0.06). The dynamic layer **only removes units that the static layer selected because of a
widening**, using per-unit records of what each test actually loaded or opened. It is never a safety net: a unit the static graph reaches
through ordinary edges is never removed.

```text
Selected = S_precise ∪ { t ∈ S_widened \ S_precise : not (record(t) usable AND Dep(t) ∩ Changed = ∅) }
```

Off by default; enabled per ecosystem (`[dynamic] ecosystems = ["python"]` in bgraph.toml, or `--dynamic` on the CLI) and only recommended
after the ecosystem passes the §10.3 gate for the hybrid configuration.

## 1. Which units can be narrowed (selector)

The selector classifies every chosen unit:

- **precise**: reached by the backward walk from the seeds of changed files only (ordinary and pattern edges), or chosen by a user `[[rule]]`
  target, `previously_failed`, a new test file (`AlwaysRun`).
- **widened**: chosen only because of (a) an Unresolved seed (dynamic / unknown / uncertain_scan import whose scope contains a changed file),
  (b) a module/package scope widening, or (c) a `Whole` unit rule.

Implementation: walk twice (changed-file seeds; then all seeds) or tag seeds and propagate the "precise" bit; a unit is precise if any of its
files is reached from a changed-file seed. `SelectedTest` gains `Widened bool` (json `widened,omitempty`). Full-scope selections are never
narrowed in v1 (the result stays full).

## 2. When narrowing is allowed at all (per selection)

Narrowing is skipped for the whole selection (with a `dynamic_skipped` note giving the reason) unless **every** changed file is:

- status M (modified, existing at base and head; no A/D/R/C, no mode-only or type changes, no submodule),
- a source file of the narrowed ecosystem as classified by its analysis role `RoleSource` or `RoleTest` (for Python: `.py`; NOT `.pyi`, `.pyx`,
  `.pxd`, `.so`/`.pyd`, data files, `conftest.py` counts as source),
- not a manifest, lockfile, build script, runner configuration or toolchain pin (any role other than source/test, any global-table hit, any
  file a `[[config]]` or `[[dynamic]]` declaration names),

and no widening of the selection has a reason from the global table (`classify` global categories) or tier full.

Rationale (plan §4.5): the collector only observes reads of files that exist; additions can shadow modules (negative dependencies), deletions
and config changes alter loading itself.

## 3. Records

```go
type Record struct {
    Eco, Unit      string            // unit ID (Python: the test file path)
    Collector      string            // collector name+version, e.g. "python-audit-1"
    Commit         string            // HEAD when recorded (informational)
    Deps           map[string]string // repo-relative file -> git blob OID of the content the test saw
    Env            string            // environment fingerprint (section 5)
    Complete       bool              // false when the collector could not see everything (section 4)
    Incomplete     string            // why not complete
    Pass           bool              // the unit's run passed (pytest exit 0); any other exit code is not usable
    Recorded       time.Time
}
```

A record is **usable** for unit t at base B iff: `Complete && Pass`; `Env` equals the current fingerprint; the collector version is current;
and for every `d` in `Deps`, the blob of `d` at B equals the recorded OID (a dep missing at B → not usable). The unit's own test file is always
in `Deps`. Then t is removed iff no changed file is in `Deps`.

Store: `<state dir>/dynamic/<eco>.jsonl` (the scancache state dir, never inside the repo), latest record per (unit, env) wins; atomic rewrite;
corrupt store → ignore + warning, never an error. `bgraph record --out FILE` / `bgraph dynamic import FILE` move records between machines
(CI records on main, developers import).

## 4. Python collector (`bgraph record`)

- Runs each Python test unit (test file) **in its own process**: `python -m pytest -p bgraph_collect -q <file>` from the unit's pytest rootdir
  (`TestUnit.Project`, config `TestUnit.Config`; Django driver units use their driver). Per-file isolation is what makes `complete` possible
  (plan: same-process tests are not complete because of module caching and test order).
- The plugin (embedded in the binary, written to a temp dir put on `PYTHONPATH`) installs `sys.addaudithook` as early as possible and records:
  `open` events (any mode; paths resolved to absolute), `import` events, `os.listdir`/`os.scandir`/`glob.glob` (directory deps → recorded as
  every file under the listed directory at record time), `compile`/`exec` with a filename; at session end it also adds every
  `sys.modules[*].__file__` and `__spec__.origin` (covers modules imported before the hook, e.g. by pytest plugins and conftest).
- Keeps only paths inside the repository (after `realpath`); paths outside are covered by the env fingerprint.
- Marks the record **incomplete** when: a subprocess is spawned (`subprocess.Popen`, `os.system`, `os.exec*`, `os.posix_spawn`,
  `os.startfile`), `ctypes.dlopen`/`ctypes` usage, a native extension module is imported from inside the repository, or (unless
  `[dynamic] trust_native_env = true`) a native extension from the environment other than CPython's own `lib-dynload`/DLLs; `sys.settrace`
  is replaced; the audit hook saw an exception in itself; pytest-xdist/forked workers are active; the run timed out.
- Pass = pytest exit code 0. Exit 5 (no tests) → not usable. Failed/errored units get `Pass=false` (never narrowed).
- Blob OIDs are computed from the working tree content (git blob hash of the bytes, honouring the repo's eol/filter config through
  `git hash-object --stdin-paths` or the existing gitx helpers); `bgraph record` refuses a dirty tree unless `--allow-dirty`.
- Parallel `--jobs N`, `--units glob`, `--timeout`. Output summary: recorded / complete / incomplete by reason / failed.

## 5. Environment fingerprint

`sha256` of: collector version; OS + arch; Python `sys.version` + `sys.implementation` + platform tag; the sorted list of installed
distributions (`importlib.metadata`, name==version) of the interpreter; and the blob OIDs at B of the ecosystem's manifests, lockfiles and runner
configs (pyproject.toml, setup.cfg, setup.py, tox.ini, pytest.ini, requirements*.txt, *.lock...). At selection time bgraph computes the same
fingerprint by running the configured interpreter once (`[dynamic] python = "..."`, default `python` on PATH; cached per Index by executable
path+mtime). If the interpreter cannot run → no narrowing (note).

## 6. Output and tooling

- `Selection.Narrowed []NarrowedTest{Unit, Recorded, Commit, Reason}` lists removed units; `bgraph explain <unit>` shows why a unit was
  removed or kept (which changed file is in its deps, or why the record is unusable); `bgraph affected` prints a "narrowed by dynamic records:
  N" section; MCP `explain_selection` includes it.
- `bgraph audit`: a failure in a narrowed unit is a miss (exit 3) like any other unselected failure.
- `bgraph dynamic status`: records per eco, usable at HEAD, incomplete reasons histogram.

## 7. Evaluation (gate for the hybrid configuration, plan §10.2/§10.3)

Oracles must not come from the collector itself:

- **T2 hybrid**: on the Python T2 repos (click, marshmallow, attrs; envs under `D:\bench\bgraph-eval\t2\python`) record at R, then for every
  mutant: violations = killed units not in the hybrid selection (must be 0); report median sel/units static vs hybrid.
- **Replay**: 100–300 recent commits of each T2 repo plus flask/requests (envs to set up), records made once at an older commit R0 and kept
  fresh by content keying: report widening rate and median selection on widened commits, static vs hybrid; selection on non-widened commits
  must be identical (by construction; assert).
- **Fault injection of the guards**: fixtures/unit tests for each forbidden change kind (add, delete, rename, .pyi, config, lockfile,
  conftest with changed deps), stale record (dep blob differs at base), env mismatch, incomplete record, failed record → no narrowing.
- T1 on SWE-bench Python needs per-task environments; out of scope for v1 (documented).

Report in `research/phase-6-<date>/README.md`.
