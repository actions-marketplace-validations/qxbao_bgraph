---
name: add-dynamic-layer
description: Add a dynamic layer (recorded per-test dependencies that narrow bgraph's static selection) for an ecosystem end to end — the worth-it check, design contract, collector, records and fingerprint, guards, narrowing, runner filters, then the evaluation (fault injection, hybrid T2, guard rate over history, replay) and the phase report. Use whenever starting or planning dynamic-layer work for any ecosystem (PHP, Dart, Swift, C/C++, Go, Rust, .NET on Linux...), extending an existing layer to a new runner or platform, or deciding whether a layer is worth building or ready to turn on by default; distilled from the Python, Node, JVM, .NET and Ruby layers.
---

# Adding a dynamic layer to bgraph

The static layer is fail-closed: it selects every test that could be affected, or widens. The dynamic layer only
**removes** tests the static layer selected, using records of what each test actually touched. It is never a safety net:
when anything is uncertain (no record, a stale or incomplete one, a change the guards cannot judge) the static selection
stands. A test removed that would have failed is the only unacceptable outcome; every decision below serves that first
and selectivity second.

Read before designing:

- `docs/dynamic-layer.md`: §1–7 are the generic model (precise vs widened units, guards, records, store, fingerprint,
  output, evaluation); §8 Node, §9 JVM and .NET (§9.11), §10 Ruby are the worked layers. Pick the closest one as a model.
- `docs/roadmap.md` (the item's row, its notes, the default-on gate), `docs/ecosystems/<lang>.md` (units, runner),
  `docs/evaluation.md#dynamic-layer`.
- The local phase reports of earlier layers when present (`docs/private/eval/phase-6-*` Python and Node, `phase-7-*`
  JVM with the prototype in `dyn-class.md`, `phase-8-*` .NET with `dyn-dotnet.md` and `f-research.md`, `phase-10-*` Ruby
  with `contract.md`). They hold the measurements and bug lists the public docs summarize.
- The `add-ecosystem` skill §0 for the shared working rules (worktrees, staging by path, everything CI runs before a push,
  evaluation clones with `gc.auto=0`, English-only repository, no private references in public files).

## 0. Is it worth it? Measure before writing code

Each layer cost days of work and keeps costing maintenance. Decide with numbers, and write the verdict into the contract:

- **Room to narrow**: the static median selection on T1/T2 for the ecosystem (`docs/evaluation.md`). Near 1.0 means room
  (Ruby 1.0 → 0.12–0.19 hybrid); Go at 0.13–0.38 leaves little. A `full` selection is never narrowed, so the full rate
  caps the benefit.
- **Guard pass rate**, estimated lexically over ~500 first-parent commits of 2–3 real repositories *without* records:
  the share of commits whose changes the guards would accept. The non-code rule usually dominates (gson: only 143 of 500
  commits touched code alone); class-level code refusals dominate for method layers (Ruby: 82 of 88 refusals on rack).
  A layer the guards refuse on 80 % of commits will not reach the default-on gate.
- **Recording cost**: one isolated process per record unit, times units, times platforms or target frameworks. .NET took
  15–18 min per repository and its replay stopped at 2 commits because each recording took ~50 min.
- **Platforms**: where the collector works (.NET instrumentation is x64-only; the dev host is Windows).
- **Default-on gate** (roadmap): per ecosystem ≥ 300 replayed pairs, 0 misses, ≥ 70 % complete records, ≥ 15 % fewer
  tests selected. No layer has passed it yet; say up front whether this one plausibly can.

If the numbers say no, report that to the user with them and propose a roadmap note instead of building.

## 1. Working mode

- **Prototype outside the repository first** (a throwaway directory beside the evaluation data): does the candidate
  collector see what it must, on the platforms that matter, at what cost, and does attribution to source files work? The
  JVM prototype reproduced constant inlining before any design; the .NET spike replaced EventPipe (4.5× slower, polluted
  by test discovery, needs inlining off) with the runtime's code coverage. When the platform is unfamiliar, run research in
  parallel (a web research agent, a spike) and cross-check every claim against a measurement.
- **Contract first**: the lead writes `docs/private/eval/phase-<n>-<date>/contract.md` (section 2) before any code, and
  puts it in every agent's prompt.
- **Agents**: one builds (collector, records, guards, narrowing, runner, fixtures; `internal/dynamic/<lang>dyn` plus the
  wiring), one evaluates after it lands (`internal/evalkit`, `cmd/bgraph-eval`). Then an **independent review agent** that
  changes no code reads the diff for fail-closed holes: on .NET it found 9 of the 15 bugs.
- The lead re-runs the guard rate and T2 on the final code, verifies every claimed oracle result by hand and owns the
  report. Changes to the static layer found along the way need that ecosystem's full T1 again.

## 2. Contract: decide and write down

1. **Granularity, from where static precision is lost.**
   - *Widened units only* (Python, Node): the static unit (test file) is fine, imports are dynamic. Remove a widened unit
     whose usable record contains no changed file. Precise units are never touched.
   - *Inside precise units* (JVM and .NET: test classes inside a module or project; Ruby: test files by executed method,
     because every test loads the whole library at boot). Needs its own guard and narrowing; compilation of a selected
     unit is never narrowed. Class lists come from a class-list record keyed on the unit's test roots; types declared in a
     changed test file are extracted lexically at the head and always kept (extraction failure: no narrowing).
   - State the sub-unit ID format (`<unit ID>#<class>`) and how a narrowed unit runs.
2. **Collector.** Prefer the runtime's own coverage when it maps to source lines and needs no extra package (.NET
   Microsoft.CodeCoverage, Ruby `Coverage` methods mode): probes are inserted before the JIT, so inlining cannot hide a
   callee. Coverage does not see file reads, directory listings, subprocesses or native code: add hooks for those (Python
   audit hooks, Node `--require` preload, the JVM agent with JFR and an mmap hook). Every record needs a **canary**: the
   unit's own file must appear, or the record is incomplete.
3. **Isolation**: one fresh process tree per record unit. Module caches, shared haste maps and test order hide
   dependencies in a shared process. A unit that fails when run alone gets `Pass=false` and is never narrowed.
4. **Incomplete when** (the record exists but cannot remove anything): an unobserved child process (non-runtime
   executable, shell, a runtime child without the preload) or worker; repository native code; environment native code
   unless trusted (`trust_native_env`, or a reviewed toolchain allowlist); mmap; a read of an existing untracked repository
   file (build output, new snapshots); a loaded class or generated source that maps to no tracked file; another coverage
   tool active in the process; timeout, crash, missing log, zero tests run, missing canary; a failed attribution
   cross-check. Paths that do not exist are dropped (the guard handles additions); paths outside the repository are the
   fingerprint's job.
5. **Guards**, per selection; a refusal keeps the static selection and warns `dynamic_skipped`:
   - every changed file has status `M` with an unchanged regular-file mode (additions can shadow modules; deletions and
     renames change loading); no symbolic link or path seen through one (a recorder may log the link, not the target);
   - no manifest, lockfile, build script, runner configuration, toolchain pin, global-table file, or file named by a
     `[[config]]`, `[[dynamic]]` or `[[rule]]`; no global or `full` widening;
   - only source files of the ecosystem, plus unreferenced documentation files (Python found that `CHANGES.rst` beside the
     code blocked 70–85 % of commits without this exception; tighten it where docs can be resources, as the JVM does under
     `src/`);
   - for layers inside precise units, a **declaration guard** for whatever the compiler copies into other files without a
     run-time reference: constants and annotation defaults (JVM), `const`, enum members, default parameters, attributes,
     signatures, `using`, `partial`, `#if` (.NET), class- and module-level code, signatures, nested `def` (Ruby); code
     generators, analyzers and Lombok-like rewriters refuse;
   - the guard's lexer fails closed and is **checked against the real compiler** at base and head: an unparsable head
     once removed 33 failing Ruby test files; method boundaries must match the compiler's own;
   - an **instrumented check** per unit: when coverage has no entry for a changed member (loaded before the collector,
     excluded from coverage, defined by `eval`), keep the unit.
6. **Fingerprint**: the toolchain probe (cached by executable path and modification time), the *installed* dependency
   state rather than lockfiles (lockfiles in the fingerprint invalidated every Python record within weeks), only the
   runner-relevant part of configuration files, environment variables that change loading, build arguments, and the
   trust mode (which also changes the collector ID). The same function runs at record time on the working tree and at
   selection time on the base. Manifest and lockfile changes are refused by the guard instead.
7. **Running a narrowed selection**: how kept sub-units become a runner filter, and what happens past the command-line
   limits (Windows cmd.exe 8,191 characters: Maven classes go through `MAVEN_ARGS`; a too-long .NET filter runs the
   project whole). A unit with no kept sub-unit is still built.
8. **Evaluation plan and gates** (section 5), with the repositories chosen and the per-mutant time budget agreed with the
   user.

## 3. Implementation checklist

`internal/dynamic/rbdyn` is the most complete template for a layer inside precise units; `pydyn`/`nodedyn` for a
widened-only layer. A `<lang>dyn` package provides `Eco`, `CollectorID`, the toolchain probe and `ProbeCache`,
`EnvParts`/`Fingerprint`, `Unit`/`RunOptions`/`Collect`/`DefaultJobs`/`RecordOf`, process-tree helpers
(`proc_windows.go`/`proc_other.go`) and, for precise layers, `Guard`, `Narrowable`, `Narrow`. An ecosystem-side helper in
`internal/eco/<lang>` (e.g. `node.DynamicSource`, `jvm.DynamicSource`) answers which files are code.

Registration and wiring (search for an existing layer's name to find each spot):

- `internal/dynamic/record.go`: `Record` gets new fields only as `omitempty`, each path-keyed field validated in `valid()`.
  There is no schema version: the **collector ID is the record-format version**; bump it whenever what the collector
  observes or writes changes (keep the embedded helper's own ID equal, and make a mismatch an incomplete record, as Ruby
  does). Never bump the global `bgraph-env-N` prefix of `dynamic.Fingerprint` for one ecosystem: it voids every record.
- `internal/dynamic/narrow.go`: the `sourceFile` entry (it also makes `dynamic.Supported()` true for record, import and
  status). Reuse `Usable`, `Pick`, `Declared`; express every refusal with `dynamic.Skip`.
- `internal/rules/rules.go`: `DynamicEcosystems`, plus a `[dynamic_layer] <lang>` interpreter key when the layer needs one.
- Root package: `ecoEnv` and the `applyDynamic` dispatch in `dynamic.go` (precise layers get their own branch and a
  `dynamic_<lang>.go` with `apply<Lang>`/`record<Lang>`); the `Record` switch and `reasonKey` prefixes in `record.go`;
  `DynamicStatusOf` when unit IDs are not files; an `Options` field and probe cache in `bgraph.go`. A precise layer also
  writes its skip reason into `Dynamic` of every narrowable unit (`rubySkip`), because `explain` only shows
  `dynamic_skipped` on widened units.
- CLI: `cmd/bgraph/record.go` (`--eco` help, `bgraph dynamic` usage, the default ecosystem list of `dynamic status`),
  `cmd/bgraph/main.go` usage, an interpreter flag in `main.go`, `audit.go` and `record.go` when the layer has one.
- Runner filters for sub-units: `model.SelectedTest.ClassFilter`/`Classes`, `runner.Command.Env`, the adapter in
  `internal/runner`, `commands.go` printing the env prefix.
- Explain, MCP, audit and the CI action are generic; they need no change, but check their output on the new layer.
- Tests beside the existing ones (`dynamic_test.go`, `record_<lang>_test.go`, `internal/dynamic/<lang>dyn/*_test.go`,
  `internal/rules/dynamic_test.go`). Tests that hard-code collector IDs change with a bump.

**Fault injection** (unit tests or fixtures, before any evaluation): one case per forbidden change kind (addition,
deletion, rename, mode change, symlink, manifest, lockfile, runner config, declaration-level change, unparsable head),
per incomplete reason, a stale record (a dependency blob differs at the base), an environment mismatch, a failed record,
a missing canary. None may narrow.

## 4. Collector pitfalls seen so far

- **The runner is code that spawns and reads**: esbuild `--service`, Vite's `net use`, jest-haste-map's `watchman` probe
  (even with `--no-watchman`), gemspec `git ls-files`. Exempt a runner-internal spawn only by its caller frame or one exact
  command line, never by name alone. Discovery reads (haste map, test collection, import-system listings) are not
  dependencies; listings by repository code are (every tracked file below the directory).
- **Discovery pollutes records**: xUnit pre-enumerated theories ran other classes' data (median 38 dependencies instead
  of 8); turn it off in the recording settings. Measure dependency counts per unit against a spike before trusting them.
- **Preloads get lost**: `--import` skipped by the `node --test` orchestrator (use `--require`), workers with `env: {}`,
  `-DargLine` overridden by the build's `<argLine>` (use `JAVA_TOOL_OPTIONS`), JFR's shutdown hook stopping the recording
  first. Anything that might have run unobserved makes the record incomplete.
- **Native toolchains**: Vitest loads rolldown natively, pytest on Windows loads kernel32 through ctypes. A strict default
  gave 0 % complete records on Windows; a reviewed allowlist or `trust_native_env` (separate collector ID) fixed it.
- **Builds leak into recording**: coverlet.msbuild rewrote assemblies on disk and raced between parallel hosts; Gradle put
  module jars on the test class path; `--settings` overrode recording settings. Switch such tools off, map archives back
  to their module's sources, and refuse configurations you cannot control.
- **Path attribution**: deterministic `/_/` paths from third-party packages, generated sources, `artifacts/obj` layouts,
  Windows PID reuse overwriting a fork log (add a nonce, cross-check one log per unit), records tied to machine paths.
- **Process control**: kill the whole process tree on timeout (`cmd.Cancel` with `taskkill /T /F` on Windows, process
  groups elsewhere, plus `WaitDelay`): a test host that inherits the pipes otherwise hangs `Wait` forever (18 min on .NET).
  A crashed host can still write its report: check the summary and counters, not only the file.
- **Multi-target builds**: record every target framework, union the dependencies over the frameworks where the class
  exists; a framework that cannot run makes the class list untrustworthy.
- **Isolation changes outcomes**: some tests fail alone (`Pass=false`); some need the build lifecycle, not just the test
  goal.

## 5. Evaluation

Oracles never come from the collector. Commands live in `cmd/bgraph-eval` (`t2dyn`, `replaydyn` for widened-only layers;
`jvmdyn`, `dotnetdyn`, `rbdyn t2|guard|replay` for the others); add `<lang>dyn` the same way, writing JSON plus a
Markdown summary.

1. **Hybrid T2**: record at R, run the whole suite per mutant; a violation is an oracle-red unit or sub-unit missing from
   the hybrid selection (must be 0). For sub-units, rerun red units whole and read verdicts from the runner's reports
   (JUnit, TRX). Report static vs hybrid median selection and narrowed/refused/kept counts. For widened-only layers,
   generate mutants that actually widen (`--prepare`), or nothing is exercised.
2. **Guard rate** over 500 first-parent commits per repository, refusals bucketed by reason: it shows where the layer can
   ever help, and found two lexer bugs in Ruby.
3. **Replay**: fresh records at each parent (records made once at an old commit stop matching within weeks; that is a
   deployment fact, report it), on 150 commits or the guard-accepted ones; assert hybrid == static where nothing is
   narrowable; run every removed unit at the commit (and at the parent when it fails) to prove no miss.
4. **Complete-record rate** with incomplete reasons, recording time vs the plain suite, collector overhead.
5. Harness hygiene that cost reruns before: per-clone `PYTHONPATH` / `node_modules` links / bundles so records match the
   clone's environment; per-repository environment (`CI=1` changed behaviour); restore the checkout after each mutant;
   long filters through the environment; a pinned toolchain the same at record and select time.
6. Regression: the Node quick suite after every change; the full T1 of any ecosystem whose static layer changed.

## 6. Closing the phase

- Phase report `docs/private/eval/phase-<n>-<date>/README.md`: the worth-it numbers, every table above, every bug found
  with how it was detected, raw output paths in the evaluation directory.
- Public docs, verified against the code: a new `docs/dynamic-layer.md` section in the shape of §10 (what is narrowed,
  guards, records, collector, fingerprint, residual risks accepted, evaluation); the ecosystem page's "Dynamic layer" row
  and section; `docs/configuration.md`, `docs/cli.md`, `docs/bgraph.example.toml`; `docs/evaluation.md#dynamic-layer`;
  the README and `docs/README.md` tables.
- `docs/roadmap.md` in the same commit: the dynamic-layer row `done (unreleased)` with its link, the "Supported today"
  Dynamic column, new rows for what was left open (platforms, runners, relaxed guards), and the default-on row's status.
- The layer stays off by default until the gate is met; say so in the docs.
- Ask the user whether to keep or delete the phase's evaluation data.
