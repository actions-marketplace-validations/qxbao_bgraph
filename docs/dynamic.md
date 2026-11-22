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

Off by default; enabled per ecosystem (`[dynamic_layer] ecosystems = ["python"]` in bgraph.toml, or `--dynamic` on the CLI) and only
recommended after the ecosystem passes the §10.3 gate for the hybrid configuration.

Clarification (implementation): the table is `[dynamic_layer]`, not `[dynamic]`: `[[dynamic]]` is already the array of dynamic-import
declarations and TOML cannot hold a table and an array of tables under one key. Keys: `ecosystems`, `python`, `trust_native_env`
(documented in docs/bgraph.example.toml). `--dynamic` turns the layer on for every supported ecosystem for one run, `--dynamic=false`
turns it off even when bgraph.toml enables it; library callers use `Change.Dynamic` (`DynamicOn`/`DynamicOff`/default) and
`Options.Python` (also `--python` on the CLI) to override the interpreter.

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

Clarifications (implementation, `internal/dynamic/pydyn`):

- Command: `python -m pytest -p bgraph_collect -p no:cacheprovider -q --rootdir R [-c CONFIG] FILE` from `TestUnit.Project`, exactly
  as the runner adapter pins rootdir/config, with `PYTHONDONTWRITEBYTECODE=1` (no cache writes into the checkout; parallel units do
  not share `.pytest_cache`). A config whose addopts needs the cache provider makes the run fail, so its records are never used.
  Django units run their driver through a bootstrap script that imports the plugin and `runpy`s the driver as `__main__`.
- Files read before the plugin is imported are not observed: pytest's own ini/pyproject.toml/setup.cfg/tox.ini discovery. Those are
  environment files (§5) and changing them is refused by §2, so no narrowing depends on them.
- Directory listings made by the import system (`<frozen importlib...>` frames) or by pytest's collection (`_pytest`, `pluggy`
  frames, standard-library helpers skipped when looking for the caller) are not dependencies: they only decide which modules and
  test files exist, and that set changes only by additions, deletions and renames, which §2 refuses. Listings by any other code add
  every tracked file under the listed directory (recursively) at record time.
- In-repository paths that are not tracked: a `__pycache__/*.pyc` stands for its source `.py`; caches and build metadata derived
  from tracked files or the environment (`__pycache__`, `.pytest_cache`, `.hypothesis`, `.mypy_cache`, `.ruff_cache`, `.tox`,
  `.nox`, `.eggs`, `.git`, `*.egg-info`, `*.dist-info`) are dropped; paths that do not exist are dropped (negative dependencies are
  covered by §2's refusal of additions); devices (`nul`...) are ignored; any other existing untracked file makes the record
  incomplete ("reads untracked file").
- `sqlite3.connect` counts as an open. ctypes usage is the audit events `ctypes.dlopen`, `ctypes.dlsym*`, `ctypes.call_function`,
  `ctypes.cdata*`, `ctypes.string_at`, `ctypes.wstring_at`, `ctypes.seh_exception`; the reason names the first non-standard-library
  frame. With `trust_native_env = true`, ctypes usage with no repository frame on the stack is environment native code like an
  installed extension module and does not make the record incomplete; with a repository frame it always does. Finding: on Windows
  pytest's terminal setup imports colorama, which calls kernel32 through ctypes, so with the strict default every Windows record is
  incomplete.
- Extension modules are seen through `importlib.machinery.ExtensionFileLoader.create_module` (wrapped when the plugin loads) and
  the final `sys.modules` sweep; CPython's own are those under the base installation outside any `site-packages`.
- A run whose collector output is missing or unreadable, or that timed out, is recorded with `Complete=false, Pass=false`; exit 5
  sets `Pass=false` and adds "no tests ran (exit 5)" to the reasons.
- Blob OIDs come from the working tree snapshot at HEAD (`git` index OIDs for unmodified files). With `--allow-dirty`, a modified
  file is keyed by the blob hash of its bytes on disk without eol/filter conversion: if that differs from what git would store the
  record is only stale (unusable), never wrong.
- `--out FILE` writes the records to FILE instead of the state directory; `bgraph dynamic import FILE` merges them into the store.
  `bgraph dynamic status` counts records, records of the current environment, records usable at HEAD (and why the others are
  not) and incomplete reasons. `--trust-native-env` on the CLI (and `Options.TrustNativeEnv`) forces `trust_native_env = true`.

## 5. Environment fingerprint

`sha256` of: collector version; OS + arch; Python `sys.version` + `sys.implementation` + platform tag; the sorted list of installed
distributions (`importlib.metadata`, name==version) of the interpreter; and the blob OIDs at B of the ecosystem's manifests, lockfiles and runner
configs (pyproject.toml, setup.cfg, setup.py, tox.ini, pytest.ini, requirements*.txt, *.lock...). At selection time bgraph computes the same
fingerprint by running the configured interpreter once (`[dynamic] python = "..."`, default `python` on PATH; cached per Index by executable
path+mtime). If the interpreter cannot run → no narrowing (note).

Clarification (implementation): the probe runs a script from an otherwise empty temporary directory (no repository module can shadow
what it imports), from the repo root, without isolation flags (PYTHONPATH and user site count, as for the tests). The cached probe is
also dropped when the modification time of one of the interpreter's `sys.path` directories changes, so that an install into the same
interpreter during a long-lived session (MCP) is seen. Distribution names are normalized (PEP 503). The environment files are, at any
depth outside `node_modules`/`site-packages`: pyproject.toml, setup.cfg, setup.py, tox.ini, pytest.ini, .pytest.ini, pytest.toml,
.pytest.toml, Pipfile, noxfile.py, .python-version, requirements/constraints files and every `*.lock` / pylock file.
`trust_native_env = true` changes the collector ID (`python-audit-1+trust-native`), so records made under one setting are not
used under the other.

## 6. Output and tooling

- The `dynamic_skipped` note of §2 is a selection warning (`Selection.Warnings`, reason `dynamic_skipped`, the detail names the
  ecosystem and the reason); unreadable or corrupt record lines are reported the same way. A kept widened unit carries
  `SelectedTest.Dynamic` (json `dynamic`), saying why the layer kept it. When every unit is removed the scope becomes `none` (an
  empty selection with another scope means "run everything" to the runner adapter).
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
