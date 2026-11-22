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

Decision (2026-11-22, Phase 6 evaluation): a changed file may also be a **documentation or harmless file** (`classify.Doc` /
`classify.Harmless`: `*.md`, `*.rst`, `*.adoc`, CHANGES/LICENSE-style files, `docs/...`) when it is status M with an unchanged
regular-file mode, no graph edge (direct or glob) points to it, no ecosystem gives it a role in either tree, no ecosystem claims
it, it is not in the global table, and no `[[config]]`, `[[dynamic]]` or `[[rule]]` names it. It stays a changed file for the
per-unit check: a unit whose record lists it as a dependency is kept. In the replay, 70–85 % of the widened commits were refused
because a changelog or doc file changed next to the `.py` files. Residual risk, the same as for `.py` files and accepted with
it: a test that sees the file only through native code (an extension module or ctypes under `trust_native_env`), a subprocess
(already incomplete) or `os.stat` alone (size/mtime, not audited) is not observed. Doc files that are themselves test items
(`--doctest-glob`) are covered by the static layer's directory unit, which is precise and never removed.

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
- Files read before the plugin is imported are not observed: pytest's own ini/pyproject.toml/setup.cfg/tox.ini discovery. Their
  pytest sections are in the fingerprint (§5) and changing those files is refused by §2, so no narrowing depends on them.
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
  frame. CPython audits only library loads and first symbol lookups, never the foreign calls themselves (`ctypes.call_function` is
  raised only by the private `_ctypes.call_function` helper), so who calls a foreign function cannot be observed. The strict
  default (`trust_native_env = false`) applies the rule as written: any of these events, or ctypes already imported when the plugin
  loads (symbols resolved earlier can be called silently), makes the record incomplete. With `trust_native_env = true` the
  native code of the system and the environment is trusted whoever calls it, as installed extension modules are (which already
  accepts that native code given a path by repository code, `lxml.etree.parse(path)`-style, may read it unobserved); only
  `ctypes.dlopen` of a library that is a file of the repository still makes the record incomplete. Finding (Windows): pytest's
  terminal setup imports colorama, which loads kernel32 through ctypes, and click's `_winconsole` does the same at import; with the
  strict default every record of the three T2 repositories is incomplete. Whether trust mode should be the recommended setting on
  Windows is an open question for the evaluation.
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

`sha256` of: fingerprint version; collector version; OS + arch; Python `sys.version` + `sys.implementation` + platform tag; the sorted list
of installed distributions (`importlib.metadata`, name==version) of the interpreter; and, for every pytest configuration file at B
(pytest.ini, .pytest.ini, pytest.toml, .pytest.toml, tox.ini, setup.cfg, pyproject.toml), a digest of the part that configures pytest:
the `[tool.pytest]` table of pyproject.toml (`ini_options` and the native table; re-encoded canonically), the `[tool:pytest]` section
of setup.cfg, the `[pytest]` section of tox.ini, the whole file for pytest.ini/pytest.toml and whenever the part cannot be extracted (a pyproject.toml that does not parse). At selection
time bgraph computes the same fingerprint by running the configured interpreter once (`[dynamic_layer] python = "..."`, default
`python` on PATH; cached per Index by executable path+mtime). If the interpreter cannot run → no narrowing (note).

Decision (2026-11-22, Phase 6 evaluation): manifests, lockfiles, requirements files and toolchain pins are **not** in the fingerprint
(fingerprint version 3; older records no longer match). The installed distributions already fingerprint the environment the tests actually
ran in, and with lockfiles in it every routine lock bump invalidated every record (replay: all records unusable after 26–60 of 150
commits). A diff that changes a manifest, lockfile or configuration still blocks narrowing through §2. conftest.py files are
dependencies in the records, not part of the fingerprint.

Clarification (implementation): the probe runs a script from an otherwise empty temporary directory (no repository module can shadow
what it imports), from the repo root, without isolation flags (PYTHONPATH and user site count, as for the tests). The cached probe is
also dropped when the modification time of one of the interpreter's `sys.path` directories changes, so that an install into the same
interpreter during a long-lived session (MCP) is seen. Distribution names are normalized (PEP 503). Configuration files are found at
any depth outside `node_modules`/`site-packages`. `trust_native_env = true` changes the collector ID (`python-audit-1+trust-native`), so records made under one setting are not
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

## 8. Node

Status: implemented (2026-11-22, `internal/dynamic/nodedyn`; evaluation in `research/phase-6-20261122/README.md`). Sections 1–3 and 6 apply unchanged (eco `node`, unit ID = the test file path,
`TestUnit.Project` = the directory the runner adapter runs from). This section gives the Node counterparts of §2 (source files),
§4 (collector), §5 (fingerprint) and §7 (evaluation). The experiments behind every assumption are listed in §8.7.

### 8.1 Per-unit isolation (one runner process tree per test file)

A record is the union of what **every process and thread of one runner invocation that runs only that test file** loaded, read or
stat'ed. The union is what makes `complete` possible; attribution inside one process is not:

- vitest transforms sources in the **main process** (vite reads every inlined module with `fs` there and keeps the result in an
  in-memory module graph). Two test files in one invocation share that graph: the second file's imports are cache hits, never
  read again. Workers only receive transformed code over RPC.
- jest keeps a per-invocation haste map and transform results; mocha, node:test without isolation and AVA share `require.cache`
  and ESM module maps between files of one process.

So `bgraph record` starts one invocation per unit (in parallel across units, `--jobs`), from `TestUnit.Project` exactly as the
runner adapter does, with the runner's JS entry point run by the configured `node` (no `npx`/shell in between):

| runner | command (cwd = unit project) | why these flags |
|---|---|---|
| vitest | `node <vitest>/vitest.mjs run <file> --coverage.enabled=false --update=none --reporter=dot` (+ `--fsModuleCache=false`/`--experimental.fsModuleCache=false` when the installed version has the option) | the project's pool is kept (forks, threads, vmForks, vmThreads all observed, §8.2); file isolation flags do not matter with one file; no coverage (it writes into the checkout and its thresholds fail single files); `--update=none` never writes snapshots into the checkout; the fs module cache is keyed by the file content, but a cache hit skips files a plugin inlined while transforming |
| jest | `node <jest>/bin/jest.js --runTestsByPath <file> -i --ci --no-cache --no-watchman --coverage=false` | `-i`: tests in the runner process; `--ci`: no snapshot writes; `--no-cache`: no transform cache (a hit skips files a transformer inlines: immutable-js's preprocessor bundles the whole `src/` in a child process keyed only on the entry file) and a haste map rebuilt from scratch; `--no-watchman`: no watchman child |
| mocha | `node <mocha>/bin/mocha.js <file>` | (`--parallel` from a config spawns workerpool children: observed) |
| node:test | `node --test <file>` | the orchestrator and the per-file child are both observed (§8.2) |
| ava | `node <ava>/entrypoints/cli.mjs <file>` | worker threads or child processes: observed |
| playwright, karma, vitest browser mode | not recorded (unit skipped, or the browser spawn makes the record incomplete) | tests run in a browser |

vitest's positional argument is a substring filter (as in the runner adapter): `a.test.ts` also runs `aa.test.ts`. The record is
then a superset (the union of both files), which is sound. Whole-project units (opaque test patterns) are skipped, as for Python.
`Pass` = exit code 0; "no test files found" exits non-zero and is not usable.

The record command adds only flags that do not change what a test does; it does not turn off what the runner adapter's command
does (vitest `typecheck`, setup files, projects). A vitest typecheck runs `tsc`, which reads every file of the program (and is
spawned through a shell or shim: incomplete, §8.3) — such units are never narrowed, which is the sound outcome for type tests.

### 8.2 Collector

A CommonJS preload (`bgraph_collect.cjs`, embedded, written per record session with a `config.json` next to it giving the
repository root and the unit's log directory) is put on `NODE_OPTIONS=--require=<file>` (appended to the user's `NODE_OPTIONS`).
`--require` and not `--import`: Node 22's `node --test` orchestrator skips `--import` preloads but runs `--require` ones, and a
`--require` preload also runs in eval workers. The configuration is read from the file next to the preload, not from the
environment, so threads started with another `env` still find it.

**Where it runs.** Every Node process inheriting `NODE_OPTIONS` (the runner's main process; vitest forks, jest/mocha/ava workers,
`node --test` children, children the tests spawn) and every worker thread of those processes (preloads run per thread: vitest
`threads`/`vmThreads` tinypool workers, test-created workers, also with explicit `execArgv`). It also runs in the ESM loader's
hooks thread (where Node reads ESM sources once a hook is registered). Each process/thread writes its own
`<pid>-<threadId>.jsonl` in the unit's log directory, appending synchronously per event (killed or `terminate()`d workers lose
nothing written so far); the hooks thread writes `<pid>-<threadId>-hooks.jsonl`. The Go side merges every file of the directory.
The first line of each file is a start event (pid, ppid, thread id, argv, parent spawn token).

**What it hooks** (paths are made absolute against the cwd, `realpath`ed — pnpm/workspace links resolve to repository files — and
kept only when inside the repository and outside `node_modules`; per path the first event of each kind and caller class is kept):

- loads: `Module.prototype.load` (every CommonJS module, also `require` of `.json`/`.node`), and an ESM `load` hook registered with
  `module.register` (`file:` URLs of every `import`/`import()`, JSON modules, the entry point). Not `module.registerHooks`: in Node
  22.17 a synchronous `load` hook routes CommonJS through the ESM translators, whose `require` lacks `require.resolve.paths`, and
  jest fails to start.
- reads and opens, any mode: `readFile(Sync)`, `open(Sync)`, `createReadStream`, `copyFile(Sync)`/`cp(Sync)` (source),
  `openAsBlob`, `fs/promises` `readFile`/`open`/`copyFile`/`cp`, and writes that open a path (`writeFile(Sync)`, `appendFile(Sync)`,
  `createWriteStream`; as in §4: an existing untracked file makes the record incomplete, an absent one is dropped). Patching the
  `fs` module object reaches graceful-fs (it clones `fs` after the preload ran, jest-runtime and jest-haste-map read through it)
  and `module.syncBuiltinESMExports()` reaches `import { readFileSync } from 'node:fs'`.
- directory listings: `readdir(Sync)`, `opendir(Sync)`, `fs/promises` `readdir`/`opendir`, `glob(Sync)`.
- metadata: `stat(Sync)`, `lstat(Sync)`, `fs/promises` `stat`/`lstat` of repository paths.
- native code: `process.dlopen` (every `.node` addon), `node:wasi` instances (their preopened directories).
- processes: `ChildProcess.prototype.spawn` (all asynchronous APIs: `spawn`, `fork`, `exec`, `execFile`, `cluster`) and the
  synchronous `spawnSync`/`execSync`/`execFileSync`. Each spawn gets a token in the child's environment
  (`BGRAPH_COLLECT_PARENT`) and is logged with its executable, arguments and caller frames.
- threads: `worker_threads.Worker` (subclassed and re-exported); a plain-object `env` option gets the preload added to its
  `NODE_OPTIONS`; each construction is logged with the new thread id.

Reads, listings and stats carry the caller's stack frames (file paths only, at most 40). The Go side classifies by the **first
frame that is neither `node:` internal nor a traversal helper** (`graceful-fs`, `fdir`, `tinyglobby`, `fast-glob`, `glob`,
`globby`, `readdirp`, `picomatch`, `micromatch`):

- loads and reads are dependencies, except reads whose frames include `jest-haste-map`: its crawl reads every file of the project
  on a cold cache to extract imports for `--findRelatedTests`; that index decides which files exist, not what a test runs
  (haste names come from paths). A config naming `haste.hasteImplModulePath` (content-derived haste names) makes the record
  incomplete.
- a listing is a dependency (every tracked file under the directory, recursively, as in §4) only when that frame is a repository
  file outside `node_modules` (test code, sources, helpers). Decision (2026-11-22, lead): the result of a listing changes only
  through additions, deletions and renames, which §2 refuses, and reads of the listed files are observed separately (as reads,
  loads or, for metadata, stats of repository code). So listings by the runner, the resolver, config lookups or any package are
  not dependencies, nor are listings whose frames are all helpers (fdir and tinyglobby crawl through callbacks and lose the
  caller: vitest's test discovery and `import.meta.glob`). This differs from §4, where listings by any non-import, non-pytest
  code count: Python has no stat hook. `fs.glob` called by repository code adds every file under its `cwd`.
- a stat is a dependency (the stat'ed file) only when that frame is a repository file; resolvers and config lookups stat
  candidates for existence.

The Python collector has no stat hook (§2 decision: "`os.stat` alone is not observed", accepted). In Node, stats of repository
code are observed, which covers the case listings stood in for (enumerate a directory, then use sizes or mtimes).

**What is not hooked, and why that is acceptable.** Node's own module loader reads through internal bindings: CommonJS and ESM
loads are seen through the two load hooks instead; `package.json` files read by the loader are manifests (refused by §2,
fingerprinted by §8.4). `process.binding('fs')` used by test code is not observed (not a public API; documented residual).

**Merging** (Go side, per unit): every log file of the unit's directory; the root process (the runner) must have a start event
(else the collector did not load: incomplete). Spawn tokens are matched to child start events, worker constructions to
`<pid>-<threadId>` start events (§8.3). Paths are mapped to repository paths exactly as §4's clarifications (tracked → dependency
with its blob OID; derived/cache paths dropped: `node_modules/**`, `.git/**`, vite's `*.timestamp-*.mjs` bundled configs, vite and
vitest cache directories; absent → dropped; existing untracked → incomplete).

### 8.3 What makes a record incomplete

A record is incomplete (never used) when:

1. **a spawned process is not observed**: a spawn whose token has no child start event with the spawned pid (a non-Node
   executable, a shell, a `.cmd`/shell shim, a Node child started without the inherited `NODE_OPTIONS`), except the runner-internal
   spawns below. A Node child reached through a shell is not enough (the shell may run anything else).
2. **a worker thread is not observed**: a `Worker` construction without a start event for its thread id (an explicit `env`
   without `NODE_OPTIONS` that the wrapper could not extend, `env: SHARE_ENV` with a stripped variable...).
3. **native code**: a `.node` addon that is a repository file, always; an addon from the environment (`node_modules`, global
   installs) unless `trust_native_env = true` or it is on the toolchain list (below); a `node:wasi` instance with a preopened
   directory inside the repository.
4. **the runner configuration has imports bgraph cannot resolve**: vite ≥ 8 bundles `vite.config.*`/`vitest.config.*` (and
   project configs) with rolldown natively, vite ≤ 7 with esbuild's `build`: their reads are not observed. Their static imports are
   graph edges config → file, and every test of the project has an edge to its config, so a change to such a file is reached by
   precise edges and never narrowed. That argument fails when the config has an `Unresolved` import (an alias or a computed
   specifier the scanner cannot resolve but the bundler can): records of that project are incomplete. (Imports the bundler
   leaves dynamic are executed by Node after bundling and observed by the load hooks.)
5. **cached or bundled dependency output is read**: files under vite's optimized-deps directories (`node_modules/.vite/**/deps*`,
   bundled natively and cached across runs) — the optimizer is off by default in vitest.
6. reads of an existing untracked repository file (build outputs such as `packages/x/dist/**`, new snapshot files): as §4.
7. the run timed out, crashed, the collector output is missing or from another collector version, the root process logged no
   start event, `module.register` is missing (Node < 20.6: ESM loads unobservable), or Yarn Plug'n'Play is used (modules load
   from zip archives through a patched `fs`; not supported in v1).

Runner-internal spawns that do not make a record incomplete (matched by the first non-helper frame's package, the executable and
its arguments; each only receives contents or lists files):

| spawned by | executable / arguments | why it is harmless |
|---|---|---|
| `esbuild` | `esbuild --service=<v> --ping` | vite's transform service: code is passed over stdin; its `build` (config bundling) is covered by rule 4 |
| `vite` (Windows) | `cmd /d /s /c "net use"` | maps network drives for `realpath` |
| `jest-haste-map` | `watchman ...`, `find ...` (also as an `execSync` command line, when it is one plain command: jest 22 runs `execSync('watchman --version')` even with `--no-watchman`) | file crawl (existence) |

Not incomplete by themselves: `vm` contexts (jest's sandbox and `vm.runInContext` get their code from observed reads, and the
`fs` they see is the patched module), worker threads and child Node processes that are observed, transform caches disabled by
§8.1's flags, Node's own `process.report` probe (`node -p` from rollup, observed like any Node child).

**Native toolchain list** (decision 2026-11-22, lead). Every vitest run loads a native addon from `node_modules` (vite 8: the
rolldown binding; vite 7: `@rollup/rollup-<platform>`) and jest 30 loads `@unrs/resolver-binding-<platform>`: with the Python rule
as written no vitest or jest ≥ 30 record would ever be complete. The addons of these packages are accepted without
`trust_native_env`:

| packages | assumption |
|---|---|
| `@rollup/rollup-*` | rollup's parser: parses the code JS hands it |
| `@rolldown/binding-*`, `rolldown` | vite 8's bundler: bundles the runner configuration (reads covered by rule 4: its static imports are precise graph edges, configs with unresolvable imports make the record incomplete) and the dependency optimizer (rule 5); vite's transforms hand it the code |
| `@oxc-parser/*`, `@oxc-transform/*`, `oxc-parser`, `oxc-transform` | parse/transform the code handed to them |
| `@oxc-resolver/*`, `oxc-resolver`, `@unrs/resolver-binding-*`, `unrs-resolver` | module resolution: existence checks and reads of `package.json`/`tsconfig.json`, which are manifest/config changes that §2 refuses and §8.4 fingerprints |
| `@swc/core`, `@swc/core-*` | transform the code handed to them; the `.swcrc` lookup reads a configuration file (refused by §2, fingerprinted) |
| `lightningcss`, `lightningcss-*` | transform the CSS handed to them |
| `fsevents`, `@parcel/watcher`, `@parcel/watcher-*` | file watchers, unused by the recording commands (`run` modes) |

Conditions: the addon is matched by the package it lives in (the last `node_modules/<name>` of its realpath), that directory's
`package.json` names that package, and the addon is not a repository file — a renamed copy or an allowlisted name loaded from
anywhere else is an ordinary environment addon. The loaded allowlisted addons are recorded (`Record.Toolchain`, `name@version`)
and summed up by `bgraph record`. Any other native addon makes the record incomplete unless `trust_native_env = true`; under
`trust_native_env` the record carries the trusting collector ID, as for Python. (`@esbuild/*` is a binary, not an addon: its
service is the spawn row above.)

### 8.4 Source files (§2) and environment fingerprint (§5)

**Source files the guard accepts for `node`**: `.js`, `.mjs`, `.cjs`, `.jsx`, `.ts`, `.tsx`, `.mts`, `.cts`, with role source or
test in the Node analysis. Not `.d.ts`/`.d.mts`/`.d.cts` (never loaded at run time; like `.pyi`), not `.vue`/`.svelte`/`.astro`/
`.mdx` (their compilers may read `src=`/`@import` targets through native preprocessors such as `sass-embedded`; v1 refuses them),
not JSON or other data, not runner configs (`vitest.config.ts`, `babel.config.js`, `.mocharc.js` have the config role and are
refused by the existing role check), not snapshots. The documentation-file rule of §2 applies unchanged.

**Fingerprint** (`sha256`, fingerprint version, collector ID, as §5):

- OS, arch; `process.version`, `process.versions.modules` and `napi` of the configured `node` (`[dynamic_layer] node = "..."`,
  default `node` on PATH, also `--node`/`Options.Node`; probed once per Index and cached by executable path + mtime).
- **installed packages** (the Python decision: what is installed, not lockfiles): for the repository root and every project
  directory that has a `node_modules`, the digest of the package manager's record of the installed tree — npm
  `node_modules/.package-lock.json`, pnpm `node_modules/.pnpm/lock.yaml`, Yarn `node_modules/.yarn-state.yml` or
  `node_modules/.yarn-integrity`; without one, the sorted `name@version` of `node_modules/*/package.json` and
  `node_modules/@*/*/package.json`. Not pnpm's `.modules.yaml` (it holds timestamps). Computed at every selection (not cached);
  Yarn Plug'n'Play (`.pnp.cjs`) is an error (no fingerprint, no narrowing).
- **runner configuration at the base** (Node's loader and the native bundlers read these unobserved): the content digest of every
  file with the config role in the Node analysis (vitest/vite/jest/babel/mocha/ava/swc configs, `tsconfig*.json`/`jsconfig*.json`,
  `.babelrc*`, `.mocharc*`, `.swcrc`, `.browserslistrc`...), and of every `package.json` outside `node_modules` with the keys that
  only concern installation or publication removed (`dependencies`, `devDependencies`, `peerDependencies*`,
  `optionalDependencies`, `bundle(d)Dependencies`, `overrides`, `resolutions`, `pnpm`, `scripts`, `engines`, `packageManager`,
  `files`, and the harmless keys of `ClassifyChange`: `version`, `description`...). `type`, `main`, `exports`, `imports`, `jest`,
  `mocha`, `ava`, `babel`, `workspaces`, `name` stay: they change how modules resolve and how runners are configured.

Lockfiles are not in the fingerprint (installed state instead); a diff changing a lockfile, manifest or config is refused by §2.

### 8.5 Configuration and CLI

`[dynamic_layer] ecosystems = ["python", "node"]`; `node = "..."` (executable); `trust_native_env` applies to both ecosystems
(collector ID `node-hook-1` / `node-hook-1+trust-native`). `--node`/`Options.Node` override the executable. `bgraph record`
records the ecosystems of `[dynamic_layer] ecosystems`, else every ecosystem with test units (`--eco python|node` to pick one;
Node default `--jobs` is a quarter of the CPUs); `bgraph dynamic status` reports every ecosystem with records or units
(`--eco` for one; JSON: an object with `--eco`, else an array). Records of each ecosystem go to their own store
(`dynamic/node.jsonl`).

Clarifications (implementation):

- The collector files (`bgraph_collect.cjs`, `bgraph_hooks.mjs`, `config.json` with the root, the log directory and the exact
  `NODE_OPTIONS` entry) are written per unit; `NODE_OPTIONS` of the user is kept and the entry appended (quoted when the path has
  spaces). Spawned processes get the entry added to their `NODE_OPTIONS` when the caller passed an `env` without it, and
  `Worker`s with a plain-object `env` likewise, so that a test that builds its own environment is still observed.
- Each log line is written synchronously; reads, listings and stats of the same path are deduplicated per (event, path, caller,
  haste). Only repository paths outside `node_modules` are logged (after `realpath`), plus vite optimized-deps reads, addons,
  spawns and workers.
- A spawn whose asynchronous `spawn` failed (no pid) ran nothing and is ignored. A synchronous spawn is observed when a start
  event carries its token with the spawning process as parent.
- The runner's JS entry point is the `bin` of `vitest`/`jest`/`mocha`/`ava` found in `node_modules` from the unit's project up to
  the repository root; `node --test` needs none. vitest ≥ 4.1 gets `--update=none` and the fs module cache off
  (`--experimental.fsModuleCache=false` in 4.1, `--fsModuleCache=false` from 5); older versions get neither (a new snapshot file
  then makes the record incomplete as an untracked file).

### 8.6 Evaluation

Candidates (screening, 2026-11-22: a comment appended to N sampled source files, `bgraph affected --base HEAD`, count edits whose
selection is not full and has a widened unit):

| repo | runner | units | widening edits | widened units per edit | use |
|---|---|---|---|---|---|
| honojs/hono (T2 at 75ff8e3) | vitest 5.0 / vite 8 | 161 | 21/25 | ~7 of ~10 selected | T2 hybrid (existing oracle), replay |
| iamkun/dayjs (T2 at d3e43be) | jest 22 | 94 | 23/25 | 1–94 (locales loaded by name) | T2 hybrid (existing oracle), replay |
| expressjs/express | mocha 11 | 92 | 15/15 | ~87 | new T2 set `t2\node-dyn` (mocha JSON reporter added to the T2 harness), replay |
| immutable-js/immutable-js | jest 29 | 57 | 14/15 | 56 | control: its transformer bundles `src/` in a child process, records are expected to be broad |
| colinhacks/zod (T2) | vitest 4.1 | 207 | 7/20 | 1 | not used: typecheck spawns `tsc` (incomplete by design) |
| sveltejs/svelte | vitest 4.1 | 42 | 12/12 | 1 of 42 | not used (nothing to gain) |
| vuejs/core, darkreader, axios, github-readme-stats | | | 0/20, 0/15, 3/15, 2/15 | | not used |

prettier (dynamic `import()` in get-prettier) and babel's fixture runner were the Phase 1 candidates; their local copies are
partial clones (blobs not fetched) and babel's suite is long, so they are optional additions if time allows.

Protocol (as §7; one repo at a time, `--jobs` bounded, free memory checked between repos):

- **Records**: complete / incomplete reasons / record time per repo, in the strict default, with the toolchain list, and with
  `trust_native_env`.
- **T2 hybrid**: mutants only in files whose edit widens (`t2dyn --prepare` extended to Node, reusing `mutate.cjs`), the oracle is
  the whole suite through the runner's JSON reporter (vitest/jest existing, mocha added), never the collector. Violations =
  killed units not in the hybrid selection (must be 0; hybrid-only violations reported separately); median sel/units static vs
  hybrid, overall and on widened mutants.
- **Replay**: 150 first-parent commits of hono, dayjs, express (+ immutable-js), in full clones under `t2\node-dyn\` (the T2
  checkouts are shallow and stay untouched; `node_modules` from one install at R0 per repo). Records once at R0 and fresh at each
  P of a guard-passing widened commit; equality assertion on non-widened commits; verification of narrowed commits by running the
  removed units at C (and at P when they fail).
- **Guard and collector fault injection**: unit tests for every §8.3 reason from synthetic logs (unobserved spawn, shell spawn,
  runner-internal spawns, missing worker log, repository/environment/toolchain addon, WASI, config `Unresolved`, optimized-deps
  read, untracked read, missing root start event), the frame classification (haste-map reads, helper-only listings, repository
  listings and stats), each forbidden change kind (`.d.ts`, `.vue`, `package.json`, lockfile, `tsconfig.json`, vitest config,
  add/delete/rename), stale record, env mismatch (node version, installed state, config digest). Integration test: a tiny
  `node:test` project recorded and narrowed end to end (needs only `node`; skipped without it), and a vitest one when a prepared
  directory is given by environment variable.

### 8.7 Experiments behind this design (Node 22.17.1, Windows 11; `D:\bench\node-dyn-exp`)

| claim | observation |
|---|---|
| vite reads sources in the vitest main process | vitest 4.1.5 / vite 8.3.8 and vitest 5.0.1 / vite 8.3: `src/*.ts`, the test file and dynamically imported `src/plugins/p1.ts` are `fs` reads of the main process; the fork/thread worker reads no source file. `import.meta.glob` is a listing (fdir, no caller frame) and only the imported plugin is read |
| `NODE_OPTIONS` preload reaches workers | forks pool children (env inherited), threads and vmThreads tinypool workers (explicit `execArgv`), test-created `Worker`s, child `node` processes of tests: all logged with their reads (fixtures via `readFileSync`, `fs/promises`, `createReadStream`, CJS `require` of `.cjs`) |
| preload gaps | a `Worker` with `env: {}` or `env: { NODE_OPTIONS: '' }` runs no preload (hence rule 2 and the `env` wrapper); with `--import`, eval workers and the `node --test` orchestrator run no preload, with `--require` both do |
| `registerHooks` breaks jest | a sync `load` hook in Node 22.17 makes jest 30 fail (`require.resolve.paths is not a function` in jest-resolve); `module.register` + `Module.prototype.load` works for jest 22/30, vitest, mocha, node:test |
| config bundling is native | vite 8 reads neither `vitest.config.ts` nor its imported helper through `fs` (rolldown); with `--configLoader runner` both are read; vite 7 (zod) reads configs through `fs` |
| jest cold vs warm | cold haste map: every project file read from `jest-haste-map/build/worker.js` frames; warm: only the test's real dependencies; `--no-cache` rebuilds the haste map and re-transforms |
| natives and spawns in ordinary runs | rolldown binding (vite 8), `@rollup/rollup-win32-x64-msvc` (vite 7), `@unrs/resolver-binding` (jest 30); spawns: `esbuild --service --ping` and `cmd /c "net use"` (vite), `watchman --version` (jest-haste-map), `node -p process.report` (rollup); zod's vitest typecheck spawns `tsc` through `cmd.exe` and `tsc` reads 340 repository files |
| stat callers | hono: vite (package.json, `.env*` lookups), vitest (project dirs); dayjs: jest-haste-map, jest-resolve, jest-runtime, @babel/core, browserslist; express: locate-path (`.mocharc*`), mocha — no repository-code stat |
| real repos | hono `src/hono.test.ts`: 37 source files, no unobserved spawn but `net use`; dayjs `test/locale/keys.test.js` lists `src/locale` and loads all 143 locales (a repository listing), `display.test.js` loads only its locales; express `test/acceptance/route-map.js`: `examples/route-map/index.js` + 6 `lib/` files; immutable-js `flatten.ts`: the preprocessor's `make-synchronous` child runs rollup + TypeScript over every `src/` and `__tests__/` file |
| overhead | median of 5, collector vs none: hono one file 1.80 s → 2.09 s; dayjs `display.test.js` (`--no-cache`) 5.1 s → 7.2 s (noisy) |
