---
name: add-ecosystem
description: Add a new language/ecosystem (or a new test framework/runner inside one) to bgraph end to end — design contract, scanner, eco package, ClassifyChange, runner adapter, fixtures, then the evaluation (T1 dataset + oracle, provider cross-check, T2 mutation, regression of existing ecosystems) and the phase report. Use when starting a bgraph phase for a new ecosystem (Python, JVM, .NET, Go...) or a large runner addition; distilled from how Rust, Python, JVM, .NET and Go were integrated.
---

# Adding an ecosystem to bgraph

bgraph is a **fail-closed** affected-test selector: for every change the output contains every test that could be
affected, or it widens (package/module/full) with a machine-readable reason. Every decision below serves that
invariant first and selectivity second. A silent miss is the only unacceptable outcome.

Read the public docs before designing: `docs/roadmap.md` (the item, its priority and notes), `docs/architecture.md` (contract, graph, widening reasons, global table),
`docs/configuration.md`, `docs/lockfiles.md`, `docs/evaluation.md` (method and the bar each ecosystem met) and the
existing `docs/ecosystems/*.md` pages as models for the new one. The phase's done-criteria are usually: 0 T1/T2
violations on the dev set, trap fixtures green, widening rates reported by reason, plus an ecosystem-specific check
(e.g. Rust: match `cargo metadata --no-deps`).

## 0. Working mode

- Split the work into two parallel agents that do not touch the same files:
  - **A — ecosystem**: `internal/scan/<lang>`, `internal/eco/<lang>`, `internal/runner`, `bgraph.go` (`Ecosystems()`),
    `internal/model` (minimal additions), `internal/e2e/testdata/fixtures/<lang>/`, README.
  - **B — evaluation data**: `internal/evalkit`, `cmd/bgraph-eval`, `evalkit/` only. Phase 1 (data, clones, manifest,
    test-name locator) runs in parallel with A; phase 2 (owners → units, T1, provider cross-check) starts after A lands.
  - Then **C — T2 mutation** on 2–3 small repos once T1 is clean.
- Prefer one git worktree per agent (the Agent tool's worktree isolation): each commits on its own branch, the lead
  reviews and cherry-picks onto `main`, then re-runs every CI check on the merged result. Agents that share a framework
  file (`internal/lockfile/lockfile.go`, `cmd/bgraph-eval/main.go`, `internal/evalkit/lockdiff.go`) keep their edits
  additive (a `case`, a flag) and avoid refactoring shared helpers; two parallel refactors of the same helper are the
  merge conflicts that need real thought. An agent branched from an older `main` should fast-forward first when it
  depends on recent framework changes.
- When agents share one checkout instead, they must stage files **by path** (never `git add -A`, `commit -a`), must not
  run `git stash/reset/checkout --`, and must not push or tag. Expect the other agent's in-progress files to break the
  build of unrelated packages; they test their own packages and say so.
- Run git on the evaluation clones with `-c gc.auto=0 -c maintenance.auto=false`: an auto-gc on a large clone stalls
  for minutes and can prune unreachable objects the harness created.
- Repo conventions: English for code comments, user-facing strings, docs and commit messages; commit messages end with
  the Co-Authored-By trailer the session specifies. Public files never cite private material (`docs/private/` is
  gitignored); cite the public docs instead.
- Shared files (`cmd/bgraph-eval/main.go`, `internal/model`, `internal/selector`): keep edits minimal and stage only your
  hunks (`git apply --cached` of a partial diff) when another agent has uncommitted changes in the same file.
- Push only after review, and only after a local run of **everything CI runs**, in an isolated worktree: gofmt, `go vet`
  (native, `GOOS=linux`, `GOOS=windows`), `staticcheck ./...` 2025.1.1 (native and `GOOS=linux`), `go test ./...`. Tests
  that skip locally (real symlinks need a privilege on Windows, OS-specific paths) only run on CI: check `gh run list`
  after every push and fix a red run before anything else.
- The lead (you) writes the contract, reviews every agent's diff for fail-closed holes, verifies claimed oracle artefacts
  by hand, and owns the phase report.

## 1. Contract first (write it into both agent prompts)

Decide and fix before any code:

1. **The safe unit** — the smallest thing the runner can execute separately that is still sound to select. Node/Python:
   test file. Rust: Cargo target (the in-crate mod/use graph is NOT sound: trait impls, macros, `#[cfg]`). JVM/.NET:
   module/project. If intra-unit dependency analysis is unsound, use the graph only to decide *ownership*.
2. **Unit IDs** (the eval agent depends on them): stable strings, e.g. Rust `<pkgdir>::lib|doc|bin:<n>|test:<n>|bench:<n>|example:<n>`.
   Export `UnitID`/`ParseUnitID` helpers and document the format in the package doc.
3. **Graph encoding** with the existing file-level graph (edges From depends on To; reverse BFS from changed files):
   - file-granular ecosystems: edges between files, `TestUnit.Files = [the test file]`;
   - coarser units: a virtual node per unit (`<rust:ID>`) with `TestUnit.Files = [virtual node]`, edges virtual node →
     owned files, unit → unit (links), unit → dependency's library node;
   - package-level / unowned files → edges from every unit of the package (fail-closed default for "nobody owns it");
   - unresolved specifiers that might be in the repo → virtual node + suffix-glob pattern edges (`<bare:x>`,
     `<pymod:x>`) so a file added later is still caught; truly external → no edge (lockfile handles versions).
4. **Widening policy**, by reason code (`unknown_import`, `dynamic_import`, `config_is_code`, `manifest_changed`,
   `test_config_changed`, `lock_changed`, `unreferenced_file`, ...): which tier and scope dir for each uncertainty.
   Set `Widening.Eco` on module/package widenings whose scope dir can contain other ecosystems' projects (a root
   `Cargo.lock` must not select Node tests).
5. **Runner commands**: how a selected unit set becomes argv (per runner, Windows 8,191/32,767 char limits, list files,
   separate commands where the tool can't combine selectors, e.g. `cargo test --doc`). Notes for semantic caveats
   (feature unification with `-p`, pytest ≥ 8.2 for `@argfile`).

## 2. Ecosystem implementation (agent A)

- **Scanner** `internal/scan/<lang>`: a real lexer (comments incl. nested, every string form, raw strings, char vs
  lifetime, line continuations...) extracting only what ownership/imports need. Add a fuzz test and a
  comment-insertion property test like `internal/scan/js`. Hook into `internal/scancache` by extension; extend
  `model.ScanResult` minimally (an optional per-language field must stay serializable by the cache codec and keep old
  rows decodable); **bump `ScannerVersion`**.
- **Eco package** `internal/eco/<lang>` implementing `eco.Ecosystem`:
  - Projects/manifests read natively in Go (go-toml is available; no toolchain in the default path).
  - Roles for every file it claims (source, test, config, manifest, lock, data).
  - Resolution as a **union over all plausible interpretations** (all import roots, all conditions/targets); zero
    matches → unknown, never "probably external".
  - A repo without the ecosystem's files must return immediately (cost ~0). A stray file of the language in a repo
    with no project of the ecosystem is left to the general rules, never widened to full (a `.go` helper in a Java repo
    made a JVM task full).
  - A sub-tree holding another ecosystem's manifest and no file of this language is not owned by this ecosystem (a
    `web/` frontend under a root `go.mod` must not select every Go package).
  - A path the analysis evaluates (`File.join(File.dirname(__FILE__), "..", "lib")`, `os.path.join(...)`) must never end in
    "a confident target that does not exist, reported only": an evaluator bug then becomes a silent miss (Ruby: 390 T1 misses
    on asciidoctor from one `File.dirname` bug). Also depend on what the syntax alone guarantees (Ruby: the suffix union of
    the literal tail), and make an unclassifiable call site dynamic; assert "every load-like call yields an edge or an
    unresolved" in tests and in the fuzzer.
  - Test files are loaded by the runner, not by computed loads: a dynamic site need not fire on a change to a test file no
    real reference reaches (`Unresolved.SkipTests`); an uncertain scan still must (it may have hidden a literal reference).
  - Tests that build or run the repository's own programs (`go build ./cmd/x`, `exec.Command("../bin/x")`, import paths
    or relative paths in string literals, testscripts) depend on the built program's whole closure, not only its files.
    A program built before the tests (by a Makefile) and run by a relative path that is not in the tree is matched by
    the name the build tool gives it (Go: the main package's directory, or the module path's last element); the literal
    often sits in a non-test helper package the test imports, so the scanner must keep relative-path literals of
    non-test files too. Do not accept a bare `./name` (a working-directory data path such as caddy's `"./caddy"`
    selected every package once); check every task whose selection grows.
  - **Bump `AnalyzerVersion` on every logic change** — analysis snapshots are keyed by it and stale snapshots hide fixes
    (this bit us repeatedly; never bump it *down*). A change in a shared layer that alters what an analysis sees (the
    tree's symlink semantics, path rules) needs its own cache-key bump.
  - Symbolic links are resolved by the tree layer for every ecosystem; do not re-implement them per ecosystem.
  - Ownership edges (a project, module or package virtual node → the files it owns) are `implicit` edges from a virtual
    node; `Graph.Referenced` tells a real reference (import, embed, literal, glob) from ownership. Use it wherever "does
    anything use this file?" must not count mere ownership.
  - A project nested in another project's directory that builds separately (a Go module with its own `go.mod`) is not
    covered by the parent's module widening: set `Widening.Exact` and add the nested projects that really build with
    the parent (local replace, shared workspace) explicitly.
- **ClassifyChange** with a structural diff of manifests: harmless keys (metadata, docs, linters' config) → claim with
  no widening; dependency/feature changes → manifest_changed at the scope where unification happens; target layout →
  package + reverse deps; test-runner config → test_config_changed; lockfiles → lock_changed at L0. A lockfile format
  moves to L1 (closure comparison, `internal/lockfile`) only after the differential check against the package manager
  (`bgraph-eval lockdiff`, see `docs/lockfiles.md`): our per-project closure must be a superset of the tool's on ~10
  repos × 20–30 lockfile-changing commits, and, per consecutive pair, every project whose tool-side package set changed
  must be among the projects we mark changed. Lessons from the L1 formats:
  - Top-level keys go through `Graph.Extra` per key: a key whose effect already shows in the resolved entries
    (pnpm `overrides`, uv `options`, yarn `resolutions`) is compared through the closures; a key that changes
    installation in ways the graph does not show stays L0 and the detail names it. Decide each key with the gate.
  - Patches mark the patched packages changed instead of L0; a selector that cannot be resolved to entries is L0.
  - Some formats need the tree (yarn v1 and bun read workspace manifests; Berry applies `resolutions`, catalogs,
    `.yarnrc.yml` `packageExtensions` and yarn's built-in extensions at resolve time). Anything resolving to no entry,
    or an unknown format version, is L0.
  - Python: every project of one lockfile shares a virtual environment, so per-member closures are not enough (root
    dependency groups, pytest plugins loaded by entry point, imports nothing in the closure provides).
  - `go.sum` has no graph: lines only added change no build; anything else stays L0.
  - Pin the package-manager versions the reference uses; shared corepack caches drift.
- **Runner adapter** in `internal/runner` (+ tests), using `SelectedTest.Package`/`Project`/`Whole` as needed.
- **Registration** in `bgraph.Ecosystems()`; the e2e harness already runs all ecosystems.
- **Fixtures** (txtar: `base/`, `head/`, `delete/`, `link:base/`, `link:head/` (symbolic links, body = target), `want`
  with select/noselect/scope/widen/nowiden/full/notfull/warn),
  one per rule, each with both a `select` and a `noselect`. Always include: every widening path, a manifest harmless
  change (nothing), a lockfile change, unknown/dynamic constructs, files outside every project (orphan rule must fire,
  not silently skip), and a **mixed repo** with Node to prove isolation.
- **Smoke** on 2–3 real repos (clones with some history: a depth-1 clone has no base and every run is `full`):
  `bgraph index`, then `bgraph affected --base HEAD`
  after touching a leaf, a shared core file, a config/manifest, and a single test; report unit counts and timings, and
  compare discovered projects/targets with the native tool's metadata when one exists.

### Review checklist (lead)

- Any place that confines a file to a *subset* of units: could another unit read it at runtime? (Rust: unowned data
  under `tests/` was confined to integration tests, but `src/` unit tests can `fs::read` it → made package-level.)
- Any "unknown → skip" path. Unknown must widen or create a tracked virtual node.
- Default exclusions copied from the tool (ignored dirs, auto-discovery flags): cross-check with the native tool.
- Widenings without `Eco` that could select other ecosystems' tests; widenings with a tier that selects nothing
  (a module-tier Unresolved only seeds its own file — a broken manifest needs a real scope).
- Version bumps present; old cache rows still decode; Node behaviour unchanged (run the Node quick suite).
- A `[[rule]]` target that matches no unit makes the selection full (configuration error), in every ecosystem: new
  addressing forms (package directories, unit IDs) must keep that true.
- General-rule changes made along the way (e.g. "test files named in a config are not implicit deps", gitlink pointer
  comparison, symlink resolution in the tree layer) need the **full** T1 of every existing ecosystem, not just the
  quick suite; compare the per-task status sets with the previous runs, not only the totals.

## 3. Evaluation (agents B and C)

Data lives in a durable evaluation directory outside the repository (`--work`/`--out` of `bgraph-eval`). Never delete
anything there on your own; at each phase close ask the user whether to keep or delete the ecosystem's clones/caches.

1. **Tasks**: `evalkit/tools/fetch_<lang>.py` pulls the ecosystem's instances from pinned HF revisions (SWE-bench
   Multilingual, Multi-SWE-bench, SWE-bench Verified for Python; SWE-Gym / SWE-rebench for application-style repos)
   into `data/tasks-<lang>.jsonl` with the common schema; add (never remove) keys in `data/meta.json`. Normalize repo
   spelling, dedupe ids across datasets, keep `f2p_kind` (`f2p` vs `n2p` — n2p lists are often the whole suite: keep
   only names located in files the test patch touches).
2. **Clones**: `--filter=blob:none --no-checkout` partial clones with full history in `work/repos/<owner>__<name>`;
   verify every base commit exists.
3. **Manifest** `evalkit/manifest/<lang>.json` via `bgraph-eval manifest --ecosystem <lang>`: 40-per-repo cap,
   deterministic holdout (≥ 30% of tasks, ≥ 3 repos), `kind` library/framework/application. Published numbers come
   from the holdout only.
4. **Test-name locator** (`internal/evalkit/<lang>oracle.go` + `bgraph-eval <lang>-locate`): dataset test names →
   candidate files in the P tree (base + test patch, built via a temporary index, batched blob prefetch for partial
   clones). Report unique / ambiguous / not-found per repo with examples; macro-generated tests and docstring-named
   tests are the usual not-found causes.
5. **Owners → required units**: map located files to the units that *run* them (direct ownership edges; package-level
   files → every unit linked to the package node). Ambiguous names → require **all** candidates (strict oracle; review
   violations by hand). Also record, per violation, whether it disappears when an ambiguous name only requires the
   candidate the test patch touches: it separates name-ambiguity artefacts from real misses quickly.
6. **T1** (`bgraph-eval t1 --ecosystem <lang>`): P → C = gold patch; a task is OK if scope is full or every required unit
   is selected. Report per group/repo: ok, full (by reason), violations, full rate, median selected/units, timings, and
   the rule-of-three bound. **Every violation gets a hand check** (does the changed package really not reach the test?
   read the manifests at that base commit; where the toolchain can answer, take the dependency closure of each missing
   unit at C, e.g. `go list -deps -test`, and look for changed files in it) and is classified real miss vs oracle
   artefact before anything is claimed. SWE-bench-Live FAIL_TO_PASS lists often contain thousands of unrelated (flaky,
   timing) names: expect artefacts there, and still check each one.
7. **Provider cross-check** when the ecosystem has an exact graph provider (`cargo metadata --no-deps`, Gradle/MSBuild
   project graphs, `go list`): `bgraph-eval <provider>` on ~5 base commits per repo via temporary worktrees; projects,
   targets and root files must match; extra non-member projects are acceptable (they can only widen). Run it under
   more than one platform configuration when the tool is platform-dependent (`GOOS=linux` and the host). When a native
   affected-test tool exists (GIB, dotnet-affected, `--changed` of Vitest/Jest), check that bgraph's selection is a
   superset of it and explain every divergence.
8. **T2 mutation** (agent C): one-line operator mutations in non-test source of 2–3 small repos, whole suite per
   mutant, red units (confirmed by a re-run, minus units red/flaky at R over two baseline runs) must be selected.
   Separate build cache per repo (`CARGO_TARGET_DIR` etc.) so mutants build incrementally; exclude non-compiling
   mutants; cache the oracle so `bgraph-eval t2 --ecosystem <lang>` re-checks bgraph alone. Drop repos whose suite
   cannot run on the host (symlink privilege, toolchain too old) and say why. Pick repos with at least 5 test units;
   T2 is the check most likely to find a real miss (Go: a `TestMain` building a command by import path).
   Where one load error aborts the whole run (RSpec, Minitest), run each test file in its own process for per-file
   verdicts, and take the runner's own file list (`RSpec.configuration.files_to_run`, `Rake::TestTask#file_list`) as the
   provider check of the unit set. A per-mutant run of minutes (rake: 80 s) caps the repo's mutant count: decide the cap
   with the user up front.
9. **Regression of existing ecosystems**: Node quick suite (`bgraph-eval quick ...`, ~1 min) after every change; full
   Node T1 (~16 min) after any general-rule or selector change.

Typical findings worth looking for in T1: whole-ecosystem full widenings caused by something generic (Rust: every
submodule in both trees counted as changed → bat always full; fixed by comparing gitlink OIDs, dev full rate 19% → 4%).

Timing claims need a quiet machine and an A/B in one process: T1 `ms` moves by tens of percent between two runs of the
same binary when other evaluations run, and a scanner or analyzer bump makes the first run cold. Build both versions
against a small program that opens a fresh `Index` and calls `Select` N times on a warm state directory, alternate the
binaries, and count git processes with `GIT_TRACE`: on Windows a spawned git costs tens of milliseconds and is usually
the real cost (a `git cat-file` per run to read symlink targets was the "20 % slower select" of the symlink change).

### Dynamic layer for a new ecosystem (lessons from the JVM and .NET class layers)

- **Prefer the runtime's own coverage over custom tracing** when it exists and needs no extra package (.NET:
  Microsoft.CodeCoverage via `--collect "Code Coverage"`, Cobertura parsed in Go). Line coverage survives JIT inlining and
  avoids shipping a helper; an EventPipe + PDB prototype was 4.5× slower to record and polluted by test discovery.
- **Measure the record before trusting it**: compare per-class dependency counts against a spike. A median four times higher
  than expected was the test framework running other classes' data in discovery (xUnit pre-enumerated theories).
- **Kill the whole process tree on timeout** (`cmd.Cancel` with `taskkill /T` on Windows, `WaitDelay`): test runners start
  hosts that inherit the output pipes, and `Wait` otherwise blocks forever on a hung host.
- **Repository build tweaks leak into recording**: a repo-enabled coverage tool that rewrites assemblies on disk
  (coverlet.msbuild) must be switched off for parallel recording runs.
- **Multi-target projects**: record every framework, union the dependencies, and only over the frameworks where the class
  exists (its full-suite run per framework says so); a framework that cannot run makes the class list untrustworthy.
- **Path maps are shared**: deterministic builds of third-party packages also report `/_/...` paths; only map them into the
  repository when the tree has the file (or it is generated under one of its projects).
- **Evaluate with the T2 oracle of the phase** (killed mutants, class verdicts from the runner's reports), plus the guard
  over 500 first-parent commits; record the guard's refusals by category.

## 4. Closing the phase

- Write `docs/private/eval/phase-<n>-<date>/README.md` (gitignored, local): tables for T1 (dev/holdout), provider
  check, T2, smoke numbers, every violation with its classification, widening reasons, and the exact paths of raw
  outputs in the evaluation directory.
- Update the public docs (English, verified against the code, no private references): a new `docs/ecosystems/<lang>.md`
  with the same headings as the existing pages (At a glance, Supported layouts, Test units, What a unit depends on,
  Changes to manifests/configuration/lockfiles, Unresolved and uncertain imports, Running the selection, Addressing units
  in bgraph.toml, Known gaps and over-selection, See also); the ecosystem table and numbers of the root README and
  `docs/README.md`; the results in `docs/evaluation.md` (every number from the latest run, not an older report);
  `bgraph.toml` additions in `docs/configuration.md` and `docs/bgraph.example.toml` (a test keeps it parseable). Run
  `bgraph command` on a fixture to capture example output instead of writing it by hand.
- Update `docs/roadmap.md` in the same commit as the feature: set the item's status (`done (unreleased)` until a release
  contains it, then `done (vX.Y.Z)`), link the new page, add rows for what the phase left open (with status and priority)
  and for integrations discovered on the way, mark infeasible items `dropped` with the reason, and keep its "Supported
  today" table in step with the README and `docs/README.md`. Before starting a phase, read it to pick the item and its notes.
- Ask the user: keep or delete the phase's eval data; proceed to the next phase.
