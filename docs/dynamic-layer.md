# Dynamic layer

The static layer is sound but widens where it cannot see: name-based `importlib` and `__import__` in Python, computed
`import()` in Node, everything inside a JVM module. The **dynamic layer** narrows such selections with per-test records of
what each test actually loaded or read. It is **off by default**, enabled per ecosystem, and this page is its contract:
what it may remove, when it refuses, what a record is, and what each collector observes.

| Ecosystem | What is narrowed | Collector | Status |
|---|---|---|---|
| Python | widened test files | audit hook in a pytest plugin, one process per test file | implemented |
| Node | widened test files | `--require` preload in every process and thread of the runner, one runner invocation per test file | implemented |
| JVM | test classes inside a selected Maven module / Gradle project | Java agent, one fresh test JVM per class | implemented |
| .NET | – | – | not implemented (see [9.9](#99-decisions-and-implementation)) |

Section numbers are stable: code comments refer to them.

## Contents

- [Goal and non-goal](#goal-and-non-goal)
- [1. Which units can be narrowed](#1-which-units-can-be-narrowed)
- [2. When narrowing is allowed](#2-when-narrowing-is-allowed)
- [3. Records](#3-records)
- [4. Python collector](#4-python-collector)
- [5. Environment fingerprint](#5-environment-fingerprint)
- [6. Output and tooling](#6-output-and-tooling)
- [7. Evaluation](#7-evaluation)
- [8. Node](#8-node)
- [9. JVM and .NET: per-test-class narrowing](#9-jvm-and-net-per-test-class-narrowing)

## Goal and non-goal

The dynamic layer **only removes units that the static layer selected because of a widening**. It is never a safety
net: a unit the static graph reaches through ordinary edges is never removed.

```text
Selected = S_precise ∪ { t ∈ S_widened \ S_precise : not (record(t) usable AND Dep(t) ∩ Changed = ∅) }
```

(The JVM class layer is the exception: it narrows inside precise units, see [9.1](#91-what-is-narrowed).)

Enabling it:

```toml
[dynamic_layer]
ecosystems = ["python"]          # "python", "node", "jvm"
```

`--dynamic` turns the layer on for every supported ecosystem for one run; `--dynamic=false` turns it off even when
`bgraph.toml` enables it. Library callers use `Change.Dynamic` (`DynamicOn` / `DynamicOff` / default) and
`Options.Python`, `Options.Node`, `Options.TrustNativeEnv`. See [Configuration](configuration.md#dynamic_layer).

The layer has passed mutation and replay checks (see [7](#7-evaluation) and [Evaluation](evaluation.md#dynamic-layer))
but not a release gate: enable it per ecosystem after evaluating it on your repository, for example with
[shadow audits](cli.md#audit).

## 1. Which units can be narrowed

The selector classifies every chosen unit:

- **precise**: reached by the backward walk from the changed files only (ordinary and pattern edges), or chosen by a
  `[[rule]]` target, `previously_failed`, or as a new test file (`always_run`);
- **widened**: chosen only because of (a) an unresolved-import seed (dynamic, unknown or uncertain-scan import whose
  scope contains a changed file), (b) a module or package scope widening, or (c) a whole-project unit.

A unit is precise if any of its files is reached from a changed-file seed. Selected tests carry `widened: true` in JSON.
A `full` selection is never narrowed.

## 2. When narrowing is allowed

Narrowing is skipped for the whole selection (with a `dynamic_skipped` warning giving the reason) unless **every**
changed file is:

- status `M` (exists at base and head; no addition, deletion, rename, mode-only or type change, no submodule);
- a source file of the narrowed ecosystem, by its analysis role (source or test). Python: `.py` (`conftest.py`
  included); not `.pyi`, `.pyx`, `.pxd`, `.so` / `.pyd`, data files;
- not a manifest, lockfile, build script, runner configuration or toolchain pin (any other role, any global-table file,
  any file a `[[config]]` or `[[dynamic]]` declaration names);
- not a symbolic link, not seen through one (a path under a directory link), and not the target of a link that is
  part of the change: a recorder may have logged the link path instead of the target's, so link changes are never
  narrowed (the JVM class layer applies the same guard);

and no widening of the selection comes from the global table or has tier `full`.

**Documentation files.** A changed file may also be a documentation or harmless file (`*.md`, `*.rst`, `*.adoc`,
`LICENSE`-style files...) when it has status `M` with an unchanged regular-file mode, no graph edge points to it, no
ecosystem gives it a role in either tree or claims it, it is not in the global table, and no `[[config]]`, `[[dynamic]]`
or `[[rule]]` names it. It stays a changed file for the per-unit check: a unit whose record lists it is kept. Residual
risk, accepted: a test that sees the file only through native code, a subprocess (already incomplete) or `os.stat`
alone is not observed. Documentation files that are themselves test items (`--doctest-glob`) belong to a directory unit,
which is precise and never removed.

Why these guards: the collector only observes reads of files that exist. Additions can shadow modules (negative
dependencies); deletions and configuration changes alter loading itself.

## 3. Records

```go
type Record struct {
    Eco, Unit  string            // unit ID (Python, Node: the test file path)
    Collector  string            // collector name and version, e.g. "python-audit-1"
    Commit     string            // HEAD when recorded (informational)
    Deps       map[string]string // repo-relative file -> git blob OID of the content the test saw
    Env        string            // environment fingerprint (section 5)
    Complete   bool              // false when the collector could not see everything (section 4)
    Incomplete string            // why not complete
    Pass       bool              // the unit's run passed; any other outcome is not usable
    Recorded   time.Time
}
```

A record is **usable** for unit `t` at base `B` if and only if: `Complete && Pass`; `Env` equals the current
fingerprint; the collector version is current; and for every `d` in `Deps`, the blob of `d` at `B` equals the recorded
OID (a dependency missing at `B` makes it unusable). The unit's own test file is always in `Deps`. Then `t` is removed if
and only if no changed file is in `Deps`.

**Store.** `<state dir>/dynamic/<eco>.jsonl` (the user cache directory, never inside the repository); the latest record
per (unit, environment) wins; rewritten atomically; a corrupt store is ignored with a warning, never an error.
`bgraph record --out FILE` and `bgraph dynamic import FILE` move records between machines: CI records the main branch,
developers import. Records are content-keyed, so they stay valid across commits until a dependency changes; in practice
they must be refreshed on the base branch to stay useful.

**Recording in CI.** [`ci/record-github-actions.yml`](ci/record-github-actions.yml) is a ready-to-adapt GitHub Actions
workflow: a nightly job on the default branch runs the `qxbao/bgraph` action with `mode: record` and saves the records
file in the Actions cache (one entry per run); the pull-request job restores the newest entry, runs the full suite as
usual and audits it twice (`--dynamic=false` and `--dynamic`, the action importing the records first), keeping both
shadow logs. `bgraph audit --summary --log FILE` compares them: misses (a test the records removed that failed) and the
share of the suite each selection would run. Both jobs must set up the same toolchain, since records are tied to the
environment fingerprint (section 5); keep the records file outside the working tree, where it would count as a change.

## 4. Python collector

`bgraph record` runs each Python test unit (test file) **in its own process**. Per-file isolation is what makes
`complete` possible: in one process, module caching and test order hide dependencies.

- **Command**: `python -m pytest -p bgraph_collect -p no:cacheprovider -q --rootdir R [-c CONFIG] FILE`, from the
  unit's project directory, exactly as the runner adapter pins rootdir and config, with `PYTHONDONTWRITEBYTECODE=1` (no
  cache writes into the checkout). A configuration whose `addopts` needs the cache provider makes the run fail, so its
  records are never used. Django units run their driver through a bootstrap that imports the plugin and runs the driver
  as `__main__`.
- **Plugin** (embedded in the binary, written to a temporary directory put on `PYTHONPATH`): installs
  `sys.addaudithook` as early as possible and records `open` events (any mode), `import` events,
  `os.listdir` / `os.scandir` / `glob.glob` (directory dependencies), `compile` / `exec` with a file name, and
  `sqlite3.connect`. At session end it adds every `sys.modules[*].__file__` and `__spec__.origin`, which covers modules
  imported before the hook (pytest plugins, conftests).
- **Paths**: only paths inside the repository are kept (after `realpath`); outside paths are covered by the fingerprint.
  A `__pycache__/*.pyc` stands for its `.py`. Caches and build metadata (`__pycache__`, `.pytest_cache`, `.hypothesis`,
  `.mypy_cache`, `.ruff_cache`, `.tox`, `.nox`, `.eggs`, `.git`, `*.egg-info`, `*.dist-info`) are dropped; paths that do
  not exist are dropped (additions are refused by [2](#2-when-narrowing-is-allowed)); devices are ignored; any other
  existing untracked file makes the record incomplete ("reads untracked file").
- **Listings**: listings made by the import system or by pytest's collection are not dependencies (they only decide which
  modules and test files exist, which changes only by additions, deletions and renames). Listings by any other code add
  every tracked file under the listed directory, recursively, at record time.
- **Not observed**: files read before the plugin loads, i.e. pytest's own configuration discovery. Their pytest sections
  are in the fingerprint ([5](#5-environment-fingerprint)) and changing those files is refused by
  [2](#2-when-narrowing-is-allowed).
- **Incomplete** when: a subprocess is spawned (`subprocess.Popen`, `os.system`, `os.exec*`, `os.posix_spawn`,
  `os.startfile`); ctypes is used (`ctypes.dlopen`, `ctypes.dlsym*`, `ctypes.call_function`, `ctypes.cdata*`,
  `ctypes.string_at`, `ctypes.wstring_at`, `ctypes.seh_exception`, or ctypes already imported when the plugin loads); a
  native extension module is imported from the repository; a native extension from the environment other than CPython's
  own (unless `trust_native_env`); `sys.settrace` is replaced; the hook saw an exception in itself; pytest-xdist or
  forked workers are active; the run timed out or its output is missing.
- **`trust_native_env = true`**: the native code of the system and the environment is trusted whoever calls it, as
  installed extension modules are; only `ctypes.dlopen` of a library that is a repository file still makes the record
  incomplete. CPython audits only library loads and first symbol lookups, not foreign calls themselves, so who calls a
  foreign function cannot be observed. On Windows, pytest's terminal setup loads `kernel32` through ctypes (colorama), so
  with the strict default no record is ever complete there; enable `trust_native_env` on Windows.
- **Extension modules** are seen through `importlib.machinery.ExtensionFileLoader.create_module` and the final
  `sys.modules` sweep; CPython's own are those under the base installation outside any `site-packages`.
- **Pass** = pytest exit code 0. Exit 5 (no tests) is not usable; failed or errored units get `Pass=false`.
- **Blob OIDs** come from the working tree at `HEAD` (index OIDs for unmodified files). `bgraph record` refuses a dirty
  tree unless `--allow-dirty`; then a modified file is keyed by the blob hash of its bytes on disk, without eol or filter
  conversion: if that differs from what git would store, the record is only stale, never wrong.
- **Options**: `--jobs N`, `--units GLOB`, `--timeout`. The summary reports recorded, complete, incomplete by reason and
  failed units.

## 5. Environment fingerprint

`sha256` of: the fingerprint version; the collector version; OS and architecture; Python `sys.version`,
`sys.implementation` and platform tag; the sorted list of installed distributions (`importlib.metadata`, `name==version`,
names normalized per PEP 503); and, for every pytest configuration file at the base (`pytest.ini`, `.pytest.ini`,
`pytest.toml`, `.pytest.toml`, `tox.ini`, `setup.cfg`, `pyproject.toml`, at any depth outside `node_modules` and
`site-packages`), a digest of the part that configures pytest: the `[tool.pytest]` table of `pyproject.toml` (re-encoded
canonically), `[tool:pytest]` of `setup.cfg`, `[pytest]` of `tox.ini`, the whole file for `pytest.ini` / `pytest.toml`
and whenever the part cannot be extracted.

At selection time bgraph computes the same fingerprint by running the configured interpreter once
(`[dynamic_layer] python`, default `python` on PATH). The probe runs from an empty temporary directory (no repository
module can shadow what it imports), without isolation flags (`PYTHONPATH` and the user site count, as for the tests). It
is cached by executable path and modification time, and dropped when one of the interpreter's `sys.path` directories
changes (an install into the same interpreter during a long-lived MCP session is seen). If the interpreter cannot run,
nothing is narrowed (warning).

Manifests, lockfiles, requirements files and toolchain pins are **not** in the fingerprint: the installed distributions
already describe the environment the tests ran in, and with lockfiles in it every routine lock bump invalidated every
record. A diff that changes a manifest, lockfile or configuration still blocks narrowing through
[2](#2-when-narrowing-is-allowed). `conftest.py` files are dependencies in the records, not part of the fingerprint.
`trust_native_env = true` changes the collector ID (`python-audit-1+trust-native`), so records made under one setting
are not used under the other.

## 6. Output and tooling

- `dynamic_skipped` is a selection warning (`warnings`, reason `dynamic_skipped`); its detail names the ecosystem and
  the reason. Unreadable or corrupt record lines are reported the same way.
- A kept widened unit carries `dynamic` (why the layer kept it). Removed units are listed in `narrowed`
  (`unit`, `recorded`, `commit`, `reason`) and are not in `tests`. When every unit is removed the scope becomes `none`.
- `bgraph explain <unit>` shows why a unit was removed or kept (which changed file is in its dependencies, or why the
  record is unusable); `bgraph affected` prints a "narrowed by dynamic records" section; the MCP `explain_selection`
  tool includes it.
- `bgraph audit`: a failure in a narrowed unit is a miss (exit `3`) like any other unselected failure.
- `bgraph dynamic status`: records per ecosystem, records usable at `HEAD`, histogram of incomplete reasons.

See [CLI](cli.md#record) for the commands.

## 7. Evaluation

The oracles never come from the collector itself:

- **Hybrid mutation (T2)**: record at a commit R, then for every mutant run the whole suite; violations = units the
  oracle saw fail that are not in the hybrid selection (must be 0). Report the median selected/total units, static
  versus hybrid.
- **Replay**: 150 recent first-parent commits per repository, with records made once at an older commit and, separately,
  fresh at each parent commit; report the widening rate and the median selection on widened commits, static versus
  hybrid. On commits without a widened unit the hybrid selection must equal the static one (asserted). Narrowed commits
  are verified by running the removed units at the commit.
- **Fault injection of the guards**: unit tests and fixtures for each forbidden change kind (addition, deletion, rename,
  `.pyi`, configuration, lockfile, conftest with changed dependencies), stale records (a dependency blob differs at the
  base), environment mismatch, incomplete and failed records: none may narrow.

Results are summarized in [Evaluation](evaluation.md#dynamic-layer).

## 8. Node

Sections 1–3 and 6 apply unchanged (ecosystem `node`, unit ID = the test file path, the unit's project = the directory
the runner adapter runs from). This section gives the Node counterparts of [2](#2-when-narrowing-is-allowed) (source
files), [4](#4-python-collector) (collector), [5](#5-environment-fingerprint) (fingerprint) and [7](#7-evaluation)
(evaluation). The experiments behind each assumption are listed in [8.7](#87-experiments-behind-this-design).

### 8.1 Per-unit isolation (one runner process tree per test file)

A record is the union of what **every process and thread of one runner invocation that runs only that test file**
loaded, read or stat'ed. The union is what makes `complete` possible; attribution inside one process is not:

- Vitest transforms sources in the **main process** (Vite reads every inlined module there and keeps the result in an
  in-memory module graph). Two test files in one invocation share that graph: the second file's imports are cache hits,
  never read again. Workers only receive transformed code.
- Jest keeps a per-invocation haste map and transform results; Mocha, `node:test` without isolation and AVA share
  `require.cache` and ESM module maps between files of one process.

So `bgraph record` starts one invocation per unit (in parallel across units, `--jobs`), from the unit's project directory
exactly as the runner adapter does, with the runner's JS entry point run by the configured `node` (no `npx` or shell in
between):

| Runner | Command (working directory = unit project) | Why these flags |
|---|---|---|
| Vitest | `node <vitest>/vitest.mjs run <file> --coverage.enabled=false --update=none --reporter=dot` (+ `--fsModuleCache=false` / `--experimental.fsModuleCache=false` when the installed version has it) | the project's pool is kept (forks, threads, vmForks, vmThreads are all observed, [8.2](#82-collector)); no coverage (it writes into the checkout and its thresholds fail single files); `--update=none` never writes snapshots; the fs module cache is keyed by file content, but a hit skips files a plugin inlined while transforming |
| Jest | `node <jest>/bin/jest.js --runTestsByPath <file> -i --ci --no-cache --no-watchman --coverage=false` | `-i`: tests in the runner process; `--ci`: no snapshot writes; `--no-cache`: no transform cache (a hit skips files a transformer inlines) and a haste map rebuilt from scratch; `--no-watchman`: no watchman child |
| Mocha | `node <mocha>/bin/mocha.js <file>` | `--parallel` from a config spawns worker children: observed |
| `node:test` | `node --test <file>` | the orchestrator and the per-file child are both observed |
| AVA | `node <ava>/entrypoints/cli.mjs <file>` | worker threads or child processes: observed |
| Playwright, Karma, Vitest browser mode | not recorded (unit skipped, or the browser spawn makes the record incomplete) | tests run in a browser |

Vitest's positional argument is a substring filter: `a.test.ts` also runs `aa.test.ts`. The record is then a superset
(the union of both files), which is sound. Whole-project units are skipped. `Pass` = exit code 0; "no test files found"
exits non-zero and is not usable.

The record command adds only flags that do not change what a test does; it does not turn off Vitest `typecheck`, setup
files or projects. A Vitest typecheck runs `tsc`, which reads every file of the program and is spawned through a shell or
shim (incomplete, [8.3](#83-what-makes-a-record-incomplete)): such units are never narrowed, the sound outcome for type
tests.

### 8.2 Collector

A CommonJS preload (`bgraph_collect.cjs`, embedded, written per record session with a `config.json` next to it giving the
repository root and the unit's log directory) is put on `NODE_OPTIONS=--require=<file>` (appended to the user's
`NODE_OPTIONS`, quoted when the path has spaces). `--require` and not `--import`: Node 22's `node --test` orchestrator
skips `--import` preloads but runs `--require` ones, and a `--require` preload also runs in eval workers. The
configuration is read from the file next to the preload, not from the environment, so threads started with another
`env` still find it.

**Where it runs.** Every Node process inheriting `NODE_OPTIONS` (the runner's main process; Vitest forks, Jest / Mocha /
AVA workers, `node --test` children, children the tests spawn) and every worker thread of those processes (Vitest
`threads` / `vmThreads` workers, test-created workers, also with explicit `execArgv`), plus the ESM loader's hooks thread.
Each process or thread writes its own `<pid>-<threadId>.jsonl` in the unit's log directory, appending synchronously per
event (killed workers lose nothing written so far); the hooks thread writes `<pid>-<threadId>-hooks.jsonl`. The first
line of each file is a start event (pid, parent pid, thread id, argv, parent spawn token).

**What it hooks.** Paths are made absolute, `realpath`ed (pnpm and workspace links resolve to repository files) and kept
only when inside the repository and outside `node_modules`; per path the first event of each kind and caller class is
kept.

- loads: `Module.prototype.load` (every CommonJS module, also `require` of `.json` / `.node`), and an ESM `load` hook
  registered with `module.register` (`file:` URLs of every `import` / `import()`, JSON modules, the entry point). Not
  `module.registerHooks`: in Node 22.17 a synchronous `load` hook routes CommonJS through the ESM translators, whose
  `require` lacks `require.resolve.paths`, and Jest fails to start.
- reads and opens, any mode: `readFile(Sync)`, `open(Sync)`, `createReadStream`, `copyFile(Sync)` / `cp(Sync)`
  (source), `openAsBlob`, `fs/promises` `readFile` / `open` / `copyFile` / `cp`, and writes that open a path
  (`writeFile(Sync)`, `appendFile(Sync)`, `createWriteStream`; an existing untracked file makes the record incomplete,
  an absent one is dropped). Patching the `fs` module object reaches graceful-fs, and `module.syncBuiltinESMExports()`
  reaches `import { readFileSync } from 'node:fs'`.
- directory listings: `readdir(Sync)`, `opendir(Sync)`, `fs/promises` `readdir` / `opendir`, `glob(Sync)`.
- metadata: `stat(Sync)`, `lstat(Sync)`, `fs/promises` `stat` / `lstat` of repository paths.
- native code: `process.dlopen` (every `.node` addon), `node:wasi` instances (their preopened directories).
- processes: `ChildProcess.prototype.spawn` (all asynchronous APIs: `spawn`, `fork`, `exec`, `execFile`, `cluster`)
  and the synchronous `spawnSync` / `execSync` / `execFileSync`. Each spawn gets a token in the child's environment
  (`BGRAPH_COLLECT_PARENT`) and is logged with its executable, arguments and caller frames. Spawned processes get the
  preload added to their `NODE_OPTIONS` when the caller passed an `env` without it. A spawn whose asynchronous `spawn`
  failed (no pid) ran nothing and is ignored.
- threads: `worker_threads.Worker` (subclassed and re-exported); a plain-object `env` option gets the preload added to
  its `NODE_OPTIONS`; each construction is logged with the new thread id.

**Caller classification.** Reads, listings and stats carry the caller's stack frames (file paths only, at most 40). The
first frame that is neither a `node:` internal nor a traversal helper (`graceful-fs`, `fdir`, `tinyglobby`, `fast-glob`,
`glob`, `globby`, `readdirp`, `picomatch`, `micromatch`) decides:

- loads and reads are dependencies, except reads whose frames include `jest-haste-map`: its crawl reads every file of the
  project on a cold cache to extract imports; that index decides which files exist, not what a test runs. A config naming
  `haste.hasteImplModulePath` (content-derived haste names) makes the record incomplete.
- a listing is a dependency (every tracked file under the directory, recursively) only when that frame is a repository
  file outside `node_modules` (test code, sources, helpers). The result of a listing changes only through additions,
  deletions and renames, which [2](#2-when-narrowing-is-allowed) refuses, and reads of the listed files are observed
  separately. So listings by the runner, the resolver, config lookups or any package are not dependencies, nor are
  listings whose frames are all helpers (Vitest's test discovery, `import.meta.glob`). `fs.glob` called by repository
  code adds every file under its `cwd`. (Python differs: listings by any non-import, non-pytest code count, because Python
  has no stat hook.)
- a stat is a dependency (the stat'ed file) only when that frame is a repository file; resolvers and config lookups stat
  candidates for existence.

**Not hooked, and why that is acceptable.** Node's own module loader reads through internal bindings: CommonJS and ESM
loads are seen through the two load hooks instead; `package.json` files read by the loader are manifests (refused by
[2](#2-when-narrowing-is-allowed), fingerprinted by [8.4](#84-source-files-and-environment-fingerprint)).
`process.binding('fs')` used by test code is not observed (not a public API; documented residual).

**Merging** (per unit): every log file of the unit's directory; the root process (the runner) must have a start event
(else the collector did not load: incomplete). Spawn tokens are matched to child start events, worker constructions to
`<pid>-<threadId>` start events. Paths map to repository paths as in [4](#4-python-collector): tracked → dependency with
its blob OID; derived and cache paths dropped (`node_modules/**`, `.git/**`, Vite's `*.timestamp-*.mjs` bundled
configs, Vite and Vitest cache directories); absent → dropped; existing untracked → incomplete.

### 8.3 What makes a record incomplete

A record is incomplete (never used) when:

1. **a spawned process is not observed**: a spawn whose token has no child start event with the spawned pid (a non-Node
   executable, a shell, a `.cmd` / shell shim, a Node child started without the inherited `NODE_OPTIONS`), except the
   runner-internal spawns below. A Node child reached through a shell is not enough (the shell may run anything else).
2. **a worker thread is not observed**: a `Worker` construction without a start event for its thread id (an explicit
   `env` without `NODE_OPTIONS` that the wrapper could not extend, `env: SHARE_ENV` with a stripped variable...).
3. **native code**: a `.node` addon that is a repository file, always; an addon from the environment (`node_modules`,
   global installs) unless `trust_native_env = true` or it is on the toolchain list below; a `node:wasi` instance with
   a preopened directory inside the repository.
4. **the runner configuration has imports bgraph cannot resolve**: Vite ≥ 8 bundles `vite.config.*` /
   `vitest.config.*` (and project configs) natively with rolldown, Vite ≤ 7 with esbuild: their reads are not observed.
   Their static imports are graph edges config → file, and every test of the project has an edge to its config, so a
   change to such a file is reached by precise edges and never narrowed. That argument fails when the config has an
   unresolved import (an alias or computed specifier the scanner cannot resolve but the bundler can): records of that
   project are incomplete. Imports the bundler leaves dynamic run after bundling and are observed by the load hooks.
5. **cached or bundled dependency output is read**: files under Vite's optimized-deps directories
   (`node_modules/.vite/**/deps*`, bundled natively and cached across runs); the optimizer is off by default in Vitest.
6. reads of an existing untracked repository file (build outputs such as `packages/x/dist/**`, new snapshot files).
7. the run timed out or crashed, the collector output is missing or from another collector version, the root process
   logged no start event, `module.register` is missing (Node < 20.6: ESM loads unobservable), or Yarn Plug'n'Play is
   used (modules load from zip archives through a patched `fs`; not supported).

Runner-internal spawns that do not make a record incomplete (matched by the first non-helper frame's package, the
executable and its arguments; each only receives contents or lists files):

| Spawned by | Executable / arguments | Why it is harmless |
|---|---|---|
| `esbuild` | `esbuild --service=<v> --ping` | Vite's transform service: code is passed over stdin; its config bundling is covered by rule 4 |
| `vite` (Windows) | `cmd /d /s /c "net use"` | maps network drives for `realpath` |
| `jest-haste-map` | `watchman ...`, `find ...` (also as an `execSync` command line when it is one plain command: Jest 22 runs `execSync('watchman --version')` even with `--no-watchman`) | file crawl (existence) |

Not incomplete by themselves: `vm` contexts (Jest's sandbox gets its code from observed reads, and the `fs` it sees is
the patched module), observed worker threads and child Node processes, transform caches disabled by the flags of
[8.1](#81-per-unit-isolation-one-runner-process-tree-per-test-file), Node's own `process.report` probe.

**Native toolchain list.** Every Vitest run loads a native addon from `node_modules` (Vite 8: the rolldown binding;
Vite 7: `@rollup/rollup-<platform>`) and Jest 30 loads `@unrs/resolver-binding-<platform>`: with the Python rule as
written no such record would ever be complete. The addons of these packages are accepted without `trust_native_env`:

| Packages | Assumption |
|---|---|
| `@rollup/rollup-*` | rollup's parser: parses the code JS hands it |
| `@rolldown/binding-*`, `rolldown` | Vite 8's bundler: bundles the runner configuration (rule 4) and the dependency optimizer (rule 5); Vite's transforms hand it the code |
| `@oxc-parser/*`, `@oxc-transform/*`, `oxc-parser`, `oxc-transform` | parse or transform the code handed to them |
| `@oxc-resolver/*`, `oxc-resolver`, `@unrs/resolver-binding-*`, `unrs-resolver` | module resolution: existence checks and reads of `package.json` / `tsconfig.json`, which are manifest or config changes that [2](#2-when-narrowing-is-allowed) refuses and [8.4](#84-source-files-and-environment-fingerprint) fingerprints |
| `@swc/core`, `@swc/core-*` | transform the code handed to them; the `.swcrc` lookup reads a configuration file (refused, fingerprinted) |
| `lightningcss`, `lightningcss-*` | transform the CSS handed to them |
| `fsevents`, `@parcel/watcher`, `@parcel/watcher-*` | file watchers, unused by the recording commands |

Conditions: the addon is matched by the package it lives in (the last `node_modules/<name>` of its realpath), that
directory's `package.json` names that package, and the addon is not a repository file. The loaded allowlisted addons are
recorded (`name@version`) and summed up by `bgraph record`. Any other native addon makes the record incomplete unless
`trust_native_env = true`; under `trust_native_env` the record carries the trusting collector ID. (`@esbuild/*` is a
binary, not an addon: its service is the spawn row above.)

### 8.4 Source files and environment fingerprint

**Source files the guard accepts for `node`**: `.js`, `.mjs`, `.cjs`, `.jsx`, `.ts`, `.tsx`, `.mts`, `.cts`, with role
source or test in the Node analysis. Not `.d.ts` / `.d.mts` / `.d.cts` (never loaded at run time), not `.vue` /
`.svelte` / `.astro` / `.mdx` (their compilers may read `src=` / `@import` targets through native preprocessors), not
JSON or other data, not runner configs (they have the config role), not snapshots. The documentation-file rule of
[2](#2-when-narrowing-is-allowed) applies unchanged.

**Fingerprint** (`sha256`, fingerprint version, collector ID, as in [5](#5-environment-fingerprint)):

- OS, architecture; `process.version`, `process.versions.modules` and `napi` of the configured `node`
  (`[dynamic_layer] node`, default `node` on PATH, also `--node`; probed once per index and cached by executable path
  and modification time).
- **Installed packages** (what is installed, not lockfiles): for the repository root and every project directory that
  has a `node_modules`, the digest of the package manager's record of the installed tree (npm
  `node_modules/.package-lock.json`, pnpm `node_modules/.pnpm/lock.yaml`, Yarn `node_modules/.yarn-state.yml` or
  `node_modules/.yarn-integrity`); without one, the sorted `name@version` of `node_modules/*/package.json` and
  `node_modules/@*/*/package.json`. Not pnpm's `.modules.yaml` (it holds timestamps). Computed at every selection; Yarn
  Plug'n'Play (`.pnp.cjs`) is an error (no fingerprint, no narrowing).
- **Runner configuration at the base** (read unobserved by Node's loader and the native bundlers): the content digest
  of every file with the config role in the Node analysis (Vitest / Vite / Jest / Babel / Mocha / AVA / SWC configs,
  `tsconfig*.json` / `jsconfig*.json`, `.babelrc*`, `.mocharc*`, `.swcrc`, `.browserslistrc`...), and of every
  `package.json` outside `node_modules` with the keys that only concern installation or publication removed
  (`dependencies`, `devDependencies`, `peerDependencies*`, `optionalDependencies`, `bundle(d)Dependencies`,
  `overrides`, `resolutions`, `pnpm`, `scripts`, `engines`, `packageManager`, `files`, and harmless keys such as
  `version` and `description`). `type`, `main`, `exports`, `imports`, `jest`, `mocha`, `ava`, `babel`, `workspaces` and
  `name` stay: they change how modules resolve and how runners are configured.

Lockfiles are not in the fingerprint (installed state instead); a diff changing a lockfile, manifest or config is
refused by [2](#2-when-narrowing-is-allowed).

### 8.5 Configuration and CLI

`[dynamic_layer] ecosystems = ["python", "node"]`; `node = "..."` (executable); `trust_native_env` applies to both
ecosystems (collector ID `node-hook-1` / `node-hook-1+trust-native`). `--node` and `Options.Node` override the
executable. `bgraph record` records the ecosystems of `[dynamic_layer] ecosystems`, else every ecosystem with test units
(`--eco` to pick one; Node default `--jobs` is a quarter of the CPUs). Records of each ecosystem go to their own store
(`dynamic/node.jsonl`).

The runner's JS entry point is the `bin` of `vitest` / `jest` / `mocha` / `ava` found in `node_modules` from the unit's
project up to the repository root; `node --test` needs none. Vitest ≥ 4.1 gets `--update=none` and the fs module cache
off (`--experimental.fsModuleCache=false` in 4.1, `--fsModuleCache=false` from 5); older versions get neither (a new
snapshot file then makes the record incomplete as an untracked file).

### 8.6 Evaluation

As [7](#7-evaluation), one repository at a time:

- **Records**: complete / incomplete reasons / record time per repository, in the strict default (with the toolchain
  list) and with `trust_native_env`.
- **Hybrid mutation**: the oracle is the whole suite through the runner's JSON reporter (Vitest, Jest, Mocha), never the
  collector. Violations = units the oracle saw fail that are not in the hybrid selection (must be 0).
- **Replay**: 150 first-parent commits per repository, in full clones with one install per repository; records made once
  at an old commit and fresh at each parent of a guard-passing widened commit; equality assertion on non-widened commits;
  narrowed commits verified by running the removed units.
- **Fault injection**: unit tests for every [8.3](#83-what-makes-a-record-incomplete) reason from synthetic logs, the
  frame classification, each forbidden change kind (`.d.ts`, `.vue`, `package.json`, lockfile, `tsconfig.json`, Vitest
  config, addition / deletion / rename), stale records and environment mismatches (Node version, installed state, config
  digest). An integration test records and narrows a small `node:test` project end to end (needs only `node`; skipped
  without it).

Results: [Evaluation](evaluation.md#dynamic-layer).

### 8.7 Experiments behind this design

Node 22.17, Windows 11.

| Claim | Observation |
|---|---|
| Vite reads sources in the Vitest main process | Vitest 4.1 / Vite 8 and Vitest 5.0 / Vite 8: sources, the test file and a dynamically imported plugin file are `fs` reads of the main process; the fork or thread worker reads no source file. `import.meta.glob` is a listing (fdir, no caller frame) and only the imported plugin is read |
| `NODE_OPTIONS` preload reaches workers | forks pool children, threads and vmThreads workers (explicit `execArgv`), test-created `Worker`s, child `node` processes of tests: all logged with their reads |
| preload gaps | a `Worker` with `env: {}` or `env: { NODE_OPTIONS: '' }` runs no preload (hence rule 2 and the `env` wrapper); with `--import`, eval workers and the `node --test` orchestrator run no preload, with `--require` both do |
| `registerHooks` breaks Jest | a synchronous `load` hook in Node 22.17 makes Jest 30 fail (`require.resolve.paths is not a function`); `module.register` + `Module.prototype.load` works for Jest 22/30, Vitest, Mocha, `node:test` |
| config bundling is native | Vite 8 reads neither `vitest.config.ts` nor its imported helper through `fs` (rolldown); Vite 7 reads configs through `fs` |
| Jest cold versus warm cache | cold haste map: every project file is read from `jest-haste-map` worker frames; warm: only the test's real dependencies; `--no-cache` rebuilds the haste map and re-transforms |
| natives and spawns in ordinary runs | rolldown binding (Vite 8), `@rollup/rollup-win32-x64-msvc` (Vite 7), `@unrs/resolver-binding` (Jest 30); spawns: `esbuild --service --ping` and `cmd /c "net use"` (Vite), `watchman --version` (jest-haste-map), `node -p process.report` (rollup); a Vitest typecheck spawns `tsc` through `cmd.exe` |
| stat callers | Vite (`package.json`, `.env*` lookups), Vitest (project directories), jest-haste-map, jest-resolve, jest-runtime, `@babel/core`, browserslist, locate-path: no repository-code stat in the observed suites |
| overhead | median of 5 runs, collector versus none: one Vitest file 1.80 s → 2.09 s; one Jest file (`--no-cache`) 5.1 s → 7.2 s |

## 9. JVM and .NET: per-test-class narrowing

The JVM class layer is implemented (`ecosystems = ["jvm"]`, off by default). The .NET counterpart was designed but is
not implemented ([9.9](#99-decisions-and-implementation)). Sections 9.1–9.8 are the design; 9.9 records the decisions
and the implementation where it differs or adds detail; 9.10 the evaluation. Sections [3](#3-records) and
[6](#6-output-and-tooling) apply with the changes below; this section replaces [1](#1-which-units-can-be-narrowed),
[2](#2-when-narrowing-is-allowed), [4](#4-python-collector) and [5](#5-environment-fingerprint) for the JVM.

### 9.1 What is narrowed

The static JVM unit is the Maven module or Gradle project ([JVM](ecosystems/jvm.md)); a change to a module's own
sources reaches it through **precise** edges. Unlike [1](#1-which-units-can-be-narrowed), the class layer therefore
narrows *precise* units, inside the unit: a selected unit U may run only the test classes K(U) instead of all of them.

```text
K(U) = { C ∈ Classes(U) : no usable record(C)  OR  Dep(C) ∩ Changed ≠ ∅ }  ∪  Declared(Changed ∩ TestRoots(U))
```

- Sub-unit ID `<unit ID>#<class>`: the JVM binary name of a top-level class (`maven:gson#com.google.gson.GsonTest`).
  Nested classes run with their outer class.
- `Classes(U)` comes from a **class-list record** of U (the classes the runner reported in the recording session) whose
  dependencies are every file under U's test source roots. It is usable only if all of them are unchanged at the base;
  otherwise U is not narrowed (a class added since the recording would never run).
- `Declared`: a changed test-root file may declare a new class. Its top-level types at the head are extracted lexically
  and always kept; extraction failure means U is not narrowed.
- **Compilation is never narrowed**: every unit the static layer selected is still built, even when K(U) is empty.
  Compile-only effects (signature changes) are caught by the build.
- Units without class-level support (Groovy or Scala sources, whole-project units) stay whole.

### 9.2 Guards

Per selection, as [2](#2-when-narrowing-is-allowed): every changed file has status `M` and an unchanged regular mode;
global or `full` widenings refuse. Then each changed file must be one of:

1. **A code file in a compiled source root** of a unit, at base and head: `.java` / `.kt` under the default or literal
   declared main and test roots (`src/{main,test}/{java,kotlin}`, Kotlin multiplatform `src/*{Main,Test}/kotlin`, POM
   `<sourceDirectory>` / `<testSourceDirectory>`). Not: templates (`src/main/java-templates`, filtered),
   `module-info.java`, generated-source directories, build scripts (`*.gradle.kts`), `buildSrc/`, `build-logic/` or
   included-build sources.
2. **A documentation file**, under the JVM variant of the documentation rule ([9.9](#99-decisions-and-implementation),
   decision 3).

Anything else (resources, test data, `.proto`, `META-INF/services`, build files, configuration) **refuses class
narrowing for the whole selection**: a resource of module A reaches the tests of every downstream module through the
class path, as a jar read the collector cannot attribute to an entry. Modules using Lombok are never narrowed: Lombok
rewrites declarations (it can create constants) where the lexical guard below cannot see them.

**Declaration guard.** Compile-time inlining copies information into other class files without a run-time reference: a
test using `A.CONST` never loads `A`. Per changed code file, the constant-capable declarations must be untouched (exact
rule in [9.9](#99-decisions-and-implementation), decision 2). Method bodies, signatures, imports, annotations and enum
constants are free: whatever uses them loads the declaring class. Kotlin `inline` function bodies are covered by the
collector (SMAP, [9.5](#95-collectors)).

Rejected alternative: adding every `CONSTANT_Class` entry of every loaded class to the record. javac ≥ 9 leaves the owner
class of each inlined constant in the constant pool, so it is sound for javac, but it costs selectivity on every
narrowing (mean share of classes kept per changed main file 0.51 → 0.65 on one repository) while the constant guard cost
4 of 143 code-only commits there.

### 9.3 Records and fingerprint

A record per sub-unit as in [3](#3-records) (collector ID `jvm-agent-2`), plus the class-list record of
[9.1](#91-what-is-narrowed). `Pass` = every test of the class passed in its isolated run; classes that fail alone
(order-dependent classes that pass in a shared JVM) are never narrowed. A record that loaded a class compiled from a
generated source is kept on every change; a loaded repository class that maps to no tracked source makes the record
incomplete.

Fingerprint: see [9.9](#99-decisions-and-implementation). SNAPSHOT and version-range dependencies, which can change
without a file change, are handled by checking the external artifacts each record used.

### 9.4 Isolation and recording commands

One fresh JVM per test class; the record is everything that process loaded or read:

| Build | Recording | Notes |
|---|---|---|
| Maven | `mvn test -DforkCount=N -DreuseForks=false` (the whole reactor or the selected modules), agent through `JAVA_TOOL_OPTIONS=-javaagent:...` | `-DargLine` cannot inject the agent: a plugin `<argLine>` wins. POM-level `<forkCount>` / `<reuseForks>` also win over `-D`; detected by the per-fork check below |
| Gradle | `gradlew test -I <init script>`; the init script sets `forkEvery = 1`, `maxParallelForks = N` and the agent in `jvmArgs` on every `Test` task | no environment variable; the daemon is not instrumented |

Fork attribution: the test class of a fork is the first class defined from a test output directory
(`target/test-classes`, `build/classes/*/test`); the runner loads it by name before anything else of the test tree.
Cross-check per module: every class the runner reported has exactly one fork per execution and every fork exactly one
class, else the module's records are incomplete. (The check caught a real loss during prototyping: Windows reused a PID
among short-lived forks and a log was overwritten; log names now carry a nonce.)

### 9.5 Collectors

**JVM agent** (Java, embedded; one log per JVM; the build tool's own JVMs are skipped by main class):

- *Class loading*: a `ClassFileTransformer` that never transforms sees every class defined from bytes with its code
  source; `SourceFile` and the SMAP `*F` section (Kotlin inlining: file name and class) are read from those bytes. At exit,
  `Instrumentation.getAllLoadedClasses()` cross-checks: a non-hidden application class the hook never saw makes the
  record incomplete. Reflection, `Class.forName`, `ServiceLoader` and enum switch maps all end in class loading.
- *Class → source*: (package of the class, `SourceFile`) is looked up in an index of tracked `.java` / `.kt` files keyed
  by their `package` declaration and file name. This covers nested, anonymous and lambda-host classes, Kotlin file
  facades (`FooKt` → `Foo.kt`) and several top-level classes per file. Several candidates → all of them. SMAP entries map
  the same way (inline bodies from files whose classes were never loaded).
- *Files*: JFR `jdk.FileRead` / `jdk.FileWrite` with threshold 0 and stack traces. Observed: `FileInputStream`,
  `RandomAccessFile`, `Files.readAllBytes` / `lines` / `newInputStream` / `newBufferedReader`, `transferTo`,
  empty-file reads, class files read from directories. **Not observed**: an open without a read, `Files.size` /
  `exists`, listings (the accepted residuals of [2](#2-when-narrowing-is-allowed) and [8](#8-node)). Memory-mapped reads
  (`FileChannel.map`) are not JFR events, so the agent instruments `sun.nio.ch.FileChannelImpl.map` and any mapping
  makes the record incomplete ([9.9](#99-decisions-and-implementation)).
- *Incomplete*: a child process (`jdk.ProcessStart`), a native library outside the JDK and OS directories (unless
  `trust_native_env`; a repository library always), unmapped repository classes, missing logs, fork attribution
  failures, timeouts.

**.NET (designed, not implemented)**: EventPipe through environment variables (loader, JIT and inlining events),
dependencies = methods JIT-compiled or inlined in repository assemblies, mapped to documents through portable PDBs. Type
loads cannot be used (xUnit discovery loads every test class and the types of their members), and EventPipe has **no
file-read events**, so tests that read sources, assemblies or data files cannot be observed. Both facts force a stricter
guard (only method-body changes) and leave a larger residual than any accepted elsewhere.

### 9.6 Running a narrowed selection

| Build | Command | Length limits |
|---|---|---|
| Maven | narrowed modules run in their own invocation with the class filter `-Dtest=<C>,<C>$*,... -Dsurefire.failIfNoSpecifiedTests=false`; modules kept whole need a separate invocation (`-Dtest` applies to every module and overrides `includes` / `excludes`) | `mvn.cmd` runs through `cmd.exe` (8,191 characters): the list is passed in `MAVEN_ARGS` (Maven ≥ 3.9), in the environment |
| Gradle | an init script adds `filter.includeTestsMatching` per selected project, with `failOnNoMatchingTests = false` | no command-line growth |

Executions whose POM configuration fixes `<test>` ignore `-Dtest` and still run (a harmless superset). A narrowed run
changes the set and order of classes in a shared JVM, so order-dependent classes may fail spuriously (false alarms, not
misses); classes that fail in isolation are never narrowed.

### 9.7 Prototype evidence

Windows 11, JDK 17/21, .NET SDK 10 running net8.0; repositories gson (Maven), moshi (Gradle, Kotlin), Polly and RestSharp
(.NET).

| Claim | Observation |
|---|---|
| constant inlining, Java | an added test asserting `JsonReader.DEFAULT_NESTING_LIMIT == 255` loads no `JsonReader`; changing the constant to 256 fails it; the constant guard refuses. Natural case: a test using `GsonBuildConfig.VERSION` from a filtered template in `src/main/java-templates` (refused as a non-code root) |
| constant inlining, C# | a test of a const-only class records only its own file; the class (no method, no PDB document) is never loaded; changing the constant fails the test |
| javac constant-pool owners | javac 17 adds a `CONSTANT_Class` for the owner of every inlined constant (qualified name, static import, case label, annotation value, string concatenation) |
| gson mapping | 120 classes, 120 forks, 0 unmapped classes; nested and anonymous classes map to their file; median 59 files per class |
| moshi mapping (Gradle, Kotlin) | 25 classes, 0 unmapped; SMAP added a file not otherwise loaded 21 times (inline extension functions) |
| guard cost (500 first-parent commits) | gson: 143 code-only commits; constant guard passes 139, a body-only guard 42. Polly: 37 code-only; constant guard 37, body-only 14 |
| selectivity (mean share of a module's classes kept per changed file; static: 1.0) | gson main file 0.51 (median 0.68), test file 0.01; Polly source file 0.09 (median 0.03), test file 0.01 |
| recording cost | gson 15 s (suite) → 37 s (fresh fork per class, 4 forks) → 74 s with the agent; moshi 5 s → 18 s with the agent; Polly 8.5 s → 51 s isolated → 361 s traced and parsed; RestSharp 41 s → 45 s isolated → 51 s traced |

### 9.8 Evaluation method

- **Hybrid mutation** on the JVM mutation sets (gson, moshi): records at R; the oracle is the whole suite per class,
  read from surefire / Gradle XML reports (class verdicts, never the collector). Violations = failing classes not in
  K(U) (must be 0); report selected classes / module classes, static versus hybrid, and mutants refused per guard.
- **Replay**: first-parent commits with records fresh at each parent; verification by running the removed classes at
  the commit.
- **Fault injection**: one fixture or unit test per guard (resource, build file, template root, constant change, Kotlin
  `const val`, Lombok module, a test class added inside a modified file), per incomplete reason (child process, native
  library, unmapped class, mmap, missing fork log, attribution mismatch) and per stale case (class-list drift,
  environment mismatch).
- **Cost**: record time per repository.

### 9.9 Decisions and implementation

Decisions:

1. **JVM only. .NET is not implemented**: EventPipe has no file-read events, so a .NET record cannot see tests that read
   sources, assemblies or data files, and xUnit discovery pollutes type loads, which forces a body-only guard that passes
   only 38 % of Polly's and 29 % of gson's code-only commits (constant guard: 100 % / 97 %). It may be reopened with
   stack-attributed type loads or a file-read source.
2. **Constant guard, lexical and fail-closed**: a line diff splits the change into hunks; narrowing is refused when a
   hunk's base lines touch a guarded declaration of the base parse or its head lines one of the head parse. Guarded (any
   visibility, private constants included): Java `final` fields (static or instance) with an initializer whose type is
   primitive, boxed, `String` or cannot be extracted (multi-declarator fields count as unknown); every field of an
   interface; every member of an annotation type (element defaults); Kotlin `const val` (span extended to the next
   declaration line) and `annotation class` headers and bodies. Kotlin `inline` functions are free (the collector maps
   the SMAP of the classes they were inlined into). Unicode escapes are translated first; a file that does not lex or
   place (unbalanced brackets, unterminated literal or comment, an escape producing a line break, unexpected structure)
   refuses. Whitespace inside a guarded span counts as a change.
3. **Any changed non-code file refuses class narrowing for the whole selection**, except documentation files
   (status `M`, no other ecosystem's role, JVM role data or none, not global, not claimed, declared or named by a
   `[[rule]]`; graph edges to it do not matter because the collector observes reads). Tests read resources from build
   output and module jars, not from the source path, so the exception is denied for any file under a `src/` directory of
   any module, and for every file of the repository when a build file declares a non-conventional resource root (Maven
   `<resources>` / `<testResources>` `<directory>` outside `src/`, any Gradle `resources ... srcDir(s)`). Reads of a
   resource's build-output copy are mapped back to the tracked file only when byte-identical; a filtered copy makes the
   record incomplete.
4. **Lombok**: no narrowing when any build-definition file of the base or head tree mentions `lombok` or a
   `lombok.config` exists (repository-wide).
5. **Collector**: as [9.5](#95-collectors), with the mmap hook required; a native library outside the JDK of the fork
   and the OS directories makes the record incomplete, a repository one always; any child process makes it incomplete;
   log names are `<pid>-<nanotime>-<random>`; a class needs exactly one log (several executions or forks of one class:
   incomplete); a class that fails in its isolated run is recorded with `Pass=false`.
6. The class list is trusted only while the module's test roots are unchanged at the base.
7. Runner: Maven `-Dtest` through `MAVEN_ARGS`, Gradle filter init script ([9.6](#96-running-a-narrowed-selection)).

Implementation:

- **Agent**: embedded Java sources, compiled at record time with the build's JDK (`javac --release 11` + `jar`) and
  cached in the state directory per source hash and JDK release. Build-tool JVMs (Maven's launcher, Gradle daemon,
  wrapper and launcher, Kotlin daemon) write nothing. Log lines: `start`, `jfr ok|fail`, `hook ok|fail`, `class`
  (internal name, code source, SourceFile, SMAP entries), `unseen` (shutdown cross-check), `mmap`, `end`. JFR
  (`jdk.FileRead` / `FileWrite` with stacks, `jdk.ProcessStart`, `jdk.NativeLibrary`) is dumped on exit and read
  afterwards, together with the SourceFile / SMAP of every `.class` file a fork read.
- **mmap hook**: `sun.nio.ch.FileChannelImpl.map` is retransformed by a minimal class-file rewriter (a short prologue
  calling the hook, with the exception table, line and local tables and the first StackMapTable frame shifted); the
  agent jar is appended to the boot class path. JFR is started first (its own instrumentation redefines
  `FileChannelImpl`), then the hook is installed, self-tested by mapping 16 bytes of the agent jar, and re-inserted on any
  later redefinition. Any failure: every record of that JVM is incomplete. (Side effect: the JVM prints a CDS warning
  because the boot class path was appended.)
- **Recording** (`bgraph record --eco jvm [--units GLOB] [--jobs N] [--build-args ARGS] [--keep-logs]`), one invocation
  per build. Maven: `mvn -B -ntp -DforkCount=N -DreuseForks=false -Dmaven.test.failure.ignore=true -DfailIfNoTests=false
  -Dsurefire.failIfNoSpecifiedTests=false [-pl <units> -am] test` with the agent in `JAVA_TOOL_OPTIONS` (paths must not
  contain spaces). Gradle: `gradlew --console=plain --continue -I <init> test` (or explicit `:p:test` for a subset); the
  init script sets `forkEvery = 1`, `maxParallelForks = N`, the agent in `jvmArgs`, `ignoreFailures = true` and disables
  up-to-date checks and the build cache for test tasks. Default N = min(4, CPUs/2). `--timeout` applies per build. Class
  verdicts come from the JUnit XML reports of the run; the class is the **suite name**, cut at `$`.
- **Mapping a fork**: the fork's test class is the first class defined from a test output directory, which also gives the
  module. Every loaded class is looked up by (package, SourceFile); SMAP entries by (package of the owning class, file
  name). A class with a repository code source that maps to no file makes the record incomplete (generated sources,
  renamed classes), except Kotlin's regenerated inline classes, which map to the file of their host class. Reads: a
  tracked file is a dependency; a `.class` maps through its SourceFile; a byte-identical copy of a tracked resource in a
  build output maps to that resource; an archive built by a module (`<m>/build/libs/*.jar`, `<m>/target/*.jar`) depends
  on every tracked file of that module's non-test source sets; any other archive, build output or untracked file makes
  the record incomplete; reads outside the repository belong to the environment.
- **Records**: `<unit>#<class>` per top-level class and the class-list record `<unit>`. The class list is incomplete
  when the build failed, when a fork ran a class the runner did not report, or when the module has test sources but no
  reported class: such a module is never narrowed.
- **Fingerprint** (`jvm-agent-2`, `+trust-native` with `trust_native_env`): OS, architecture, a digest of the JDK's
  `release` file (`JAVA_HOME`, else `java` on PATH), the installed Maven or Gradle when the repository has no wrapper,
  and the content of every build-definition file (POMs, `.mvn/**`, Gradle scripts, settings, `gradle.properties`,
  version catalogs, wrapper properties, `buildSrc/**`, `build-logic/**`).
- **External artifacts**: each class record lists every archive outside the repository and the fork's JDK that its JVM
  loaded classes from or read, as absolute path → size and SHA-1. At selection time every listed archive is re-hashed
  (cached by path, size and modification time): a missing or changed archive, or a record without artifacts, is not
  usable. A re-resolved SNAPSHOT or version range, or a rewritten local repository, therefore invalidates exactly the
  records that used it. Records are tied to the machine's artifact paths: a CI record imported on another machine is
  unusable unless the paths match.
- **Selection**: the guards of [9.2](#92-guards) as decided above, plus: any JVM build-definition widening refuses. A
  selected unit gets `class_filter` and `classes` (the kept classes, possibly none) and a `dynamic` note; removed classes
  are `narrowed` entries `<unit>#<class>`; `index_stats.narrowed_classes` counts them. The scope never becomes `none`
  (the modules are still built).
- **Runner** (`bgraph command`): Maven narrowed modules run in their own step-2 invocation
  `mvn -pl <mods> -Dsurefire.failIfNoSpecifiedTests=false verify` with `MAVEN_ARGS=-Dtest=<C>,<C>$*,...` in the
  command's environment; modules with no kept class are only built by step 1 (`install -DskipTests` compiles their
  tests). Gradle: an init script written to `--list-dir` adds `filter.includeTestsMatching` for each kept class and its
  nested classes with `failOnNoMatchingTests = false`; modules with no kept class run `:p:testClasses`; without
  `--list-dir` the narrowed projects run whole (with a note).

### 9.10 Evaluation

- Hybrid mutation on the JVM oracle (gson, moshi; 40 killed mutants): **0 violations** (339 failing classes, all kept);
  33 mutants narrowed, 4 refused by the constant guard (annotation defaults, Kotlin annotation classes); hybrid classes /
  module classes median 0.70 (gson) and 0.80 (moshi), static 1.0.
- Records (4 forks): gson 130 classes in 149 s (122 complete and passed), moshi 39 classes in 105 s (32), against a
  suite of 104–194 s.
- Constant guard over 500 first-parent commits: gson 137 of 146 code-only commits pass, moshi 53 of 59.
- Replay (gson, 10 code-only commits, fresh records at each parent): 9 narrowed, 1 refused by the constant guard,
  0 misses (383 removed classes run at their commit, all green); median classes run / module classes 0.70.
