---
name: add-ecosystem
description: Add a new language/ecosystem (or a new test framework/runner inside one) to bgraph end to end — design contract, scanner, eco package, ClassifyChange, runner adapter, fixtures, then the evaluation (T1 dataset + oracle, provider cross-check, T2 mutation, regression of existing ecosystems) and the phase report. Use when starting a bgraph phase for a new ecosystem (Python, JVM, .NET, Go...) or a large runner addition; distilled from how Rust (Phase 3) and Python (Phase 4) were integrated.
---

# Adding an ecosystem to bgraph

bgraph is a **fail-closed** affected-test selector: for every change the output contains every test that could be
affected, or it widens (package/module/full) with a machine-readable reason. Every decision below serves that
invariant first and selectivity second. A silent miss is the only unacceptable outcome.

The design source for each ecosystem is `docs/plan.md` §6.x (in Vietnamese; gitignored, local). Read the ecosystem's
section plus §6.7 (general rules), §6.8 (lockfiles), §10 (evaluation) and §11 (phase table: "done when") before
planning. The phase's done-criteria are usually "as Phase 1": 0 T1/T2 violations on the dev set, trap fixtures green,
widening rates reported by reason, plus an ecosystem-specific check (e.g. Rust: match `cargo metadata --no-deps`).

## 0. Working mode

- Split the work into two parallel agents that do not touch the same files:
  - **A — ecosystem**: `internal/scan/<lang>`, `internal/eco/<lang>`, `internal/runner`, `bgraph.go` (`Ecosystems()`),
    `internal/model` (minimal additions), `internal/e2e/testdata/fixtures/<lang>/`, README.
  - **B — evaluation data**: `internal/evalkit`, `cmd/bgraph-eval`, `evalkit/` only. Phase 1 (data, clones, manifest,
    test-name locator) runs in parallel with A; phase 2 (owners → units, T1, provider cross-check) starts after A lands.
  - Then **C — T2 mutation** on 2–3 small repos once T1 is clean.
- Both agents work in the same checkout: they must stage files **by path** (never `git add -A`, `commit -a`), must not
  run `git stash/reset/checkout --`, and must not push or tag. Expect the other agent's in-progress files to break the
  build of unrelated packages; they test their own packages and say so.
- Repo conventions: English for code comments, user-facing strings, docs and commit messages; commit messages end with
  the Co-Authored-By trailer the session specifies. Push only after review; CI (ubuntu + windows, vet for
  native/linux/windows, staticcheck 2025.1.1) must be green.
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
  - A repo without the ecosystem's files must return immediately (cost ~0).
  - **Bump `AnalyzerVersion` on every logic change** — analysis snapshots are keyed by it and stale snapshots hide fixes
    (this bit us repeatedly; never bump it *down*).
- **ClassifyChange** with a structural diff of manifests: harmless keys (metadata, docs, linters' config) → claim with
  no widening; dependency/feature changes → manifest_changed at the scope where unification happens; target layout →
  package + reverse deps; test-runner config → test_config_changed; lockfiles → lock_changed (L0).
- **Runner adapter** in `internal/runner` (+ tests), using `SelectedTest.Package`/`Project`/`Whole` as needed.
- **Registration** in `bgraph.Ecosystems()`; the e2e harness already runs all ecosystems.
- **Fixtures** (txtar: `base/`, `head/`, `delete/`, `want` with select/noselect/scope/widen/nowiden/full/notfull/warn),
  one per rule, each with both a `select` and a `noselect`. Always include: every widening path, a manifest harmless
  change (nothing), a lockfile change, unknown/dynamic constructs, files outside every project (orphan rule must fire,
  not silently skip), and a **mixed repo** with Node to prove isolation.
- **Smoke** on 2–3 real repos (shallow clones in the scratchpad): `bgraph index`, then `bgraph affected --base HEAD`
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
- General-rule changes made along the way (e.g. "test files named in a config are not implicit deps", gitlink pointer
  comparison) need the **full** T1 of every existing ecosystem, not just the quick suite.

## 3. Evaluation (agents B and C)

Data lives under `D:\bench\bgraph-eval` (durable). Never delete anything there on your own; at each phase close ask the
user whether to keep or delete the ecosystem's clones/caches.

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
   violations by hand).
6. **T1** (`bgraph-eval t1 --ecosystem <lang>`): P → C = gold patch; a task is OK if scope is full or every required unit
   is selected. Report per group/repo: ok, full (by reason), violations, full rate, median selected/units, timings, and
   the rule-of-three bound. **Every violation gets a hand check** (does the changed package really not reach the test?
   read the manifests at that base commit) and is classified real miss vs oracle artefact before anything is claimed.
7. **Provider cross-check** when the ecosystem has an exact graph provider (`cargo metadata --no-deps`, Gradle/MSBuild
   project graphs, `go list`): `bgraph-eval <provider>` on ~5 base commits per repo via temporary worktrees; projects,
   targets and root files must match; extra non-member projects are acceptable (they can only widen).
8. **T2 mutation** (agent C): one-line operator mutations in non-test source of 2–3 small repos, whole suite per
   mutant, red units (confirmed by a re-run, minus units red/flaky at R over two baseline runs) must be selected.
   Separate build cache per repo (`CARGO_TARGET_DIR` etc.) so mutants build incrementally; exclude non-compiling
   mutants; cache the oracle so `bgraph-eval t2 --ecosystem <lang>` re-checks bgraph alone. Drop repos whose suite
   cannot run on the host (symlink privilege, toolchain too old) and say why.
9. **Regression of existing ecosystems**: Node quick suite (`bgraph-eval quick ...`, ~1 min) after every change; full
   Node T1 (~16 min) after any general-rule or selector change.

Typical findings worth looking for in T1: whole-ecosystem full widenings caused by something generic (Rust: every
submodule in both trees counted as changed → bat always full; fixed by comparing gitlink OIDs, dev full rate 19% → 4%).

## 4. Closing the phase

- Write `research/phase-<n>-<date>/README.md` (gitignored, local): tables for T1 (dev/holdout), provider check, T2,
  smoke numbers, every violation with its classification, widening reasons, and the exact paths of raw outputs under
  `D:\bench\bgraph-eval`.
- Update README (status, supported runners, `bgraph.toml` additions documented in `docs/bgraph.example.toml`, which a
  test keeps parseable).
- Ask the user: keep or delete the phase's eval data; proceed to the next phase.
