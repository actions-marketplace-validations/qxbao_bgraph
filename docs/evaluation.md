# Evaluation

bgraph's claim is that it never drops an affected test. This page describes how that claim is tested and summarizes the
results per ecosystem. A comparison with general code-graph tools is on a separate page:
[Comparison with code-graph tools](benchmarks.md).

## Method

### In-repository fixtures

`internal/e2e/testdata/fixtures/<ecosystem>/*.txtar` holds several hundred "trap" fixtures: a base tree, a change and
hand-written expectations (`select`, `noselect`, `full`, `widen <reason>`, `warn <reason>`...). They cover negative
dependencies (a new file shadowing an old one), globs, case mismatches, renames, deletions, new files, data files,
configuration computed in code, lockfile levels and every ecosystem's manifest rules. Every soundness bug found by the
external evaluation below got a fixture that fails before the fix.

### T1: FAIL_TO_PASS

Real bug-fix tasks from SWE-bench-family datasets (SWE-bench Verified, SWE-bench Multilingual, Multi-SWE-bench,
SWE-bench-Live MultiLang, SWE-Sharp-Bench). Each task has a parent commit P, the fixed commit C (gold patch + test
patch) and the list of tests that fail at P and pass at C (FAIL_TO_PASS).

- bgraph selects for the change P → C. A task is a **violation** when a required test's unit is not selected and the
  scope is not `full`.
- Required test names are located in test files of C by a name locator; ambiguous names require every candidate.
- Tasks are capped per repository (40) and split into a **dev** set and a **holdout** set of repositories that are never
  used while developing.
- Every violation is classified by hand. Some are oracle artefacts: the required test's file is unrelated to the gold
  patch (flaky or environment tests listed as FAIL_TO_PASS), or a test patch edits a file whose pre-existing tests are
  then listed as new.
- With 0 real violations out of n tasks, the **rule of three** gives a 95 % upper bound of 3/n on the miss rate.

Metrics: the **full rate** (share of tasks whose scope is `full`) and the **median selection** (selected units / all
units, on non-full tasks).

### T2: mutation

Pinned repositories per ecosystem. One bug (an operator or literal mutation) is seeded into one line of a source file at
a commit R; the whole suite runs; every unit that turns red must be in bgraph's selection for the mutated change. Mutants
that do not compile or time out are excluded. Red units are confirmed by a rerun.

### Native-tool and provider checks

bgraph reads build definitions as text and never runs the build tool during selection. Its reading is checked against
the real tools:

| Check | Tool |
|---|---|
| Cargo packages, targets and target roots | `cargo metadata --no-deps` |
| Maven reactor and inter-module edges | effective POM |
| Gradle project list | `gradle projects` |
| .NET project references | `dotnet msbuild -getItem:ProjectReference` |
| Go packages, imports and embeds | `go list -e -json -test ./...` |
| JVM selection superset | Gradle/Maven incremental-build plugin (GIB) |
| .NET selection superset | dotnet-affected |
| Node selection superset | Vitest / Jest `--changed` |
| Lockfile closures | the package managers ([below](#lockfile-differential-check)) |

### Replay

Hundreds of historical commits per repository are replayed, checking invariants (a changed test is selected, a scope
other than `none` never yields an empty command, the result is deterministic) and recording the full rate, median
selection and latency.

### Dynamic layer

The [dynamic layer](dynamic-layer.md#7-evaluation) is evaluated in a hybrid configuration (static + records), with
oracles that never come from the collector: mutation (the whole suite on each mutant), replay of 150 commits with
records made once or fresh at each parent, an equality assertion on commits without a widened unit, and verification of
every narrowed commit by running the removed units.

### Lockfile differential check

A lockfile format is enabled at [level L1](lockfiles.md) only after this check: for sampled lockfile commits of real
repositories, the dependency closure bgraph computes for every project is compared with the package manager's own
resolution. Any mismatch blocks the format.

## Results

All numbers come from single runs on Windows 11. Median selection is selected units / all units on non-full tasks.

### T1 summary

| Ecosystem | Tasks evaluated | Real violations | Full rate | Median selection | 95 % bound |
|---|---|---|---|---|---|
| Node | 223 (dev 176, holdout 47) | 0 | 0 % | 1.000 (holdout 0.956) | 1.3 % (holdout 6.4 %) |
| Rust | 154 (dev 111, holdout 43) | 0 (1 oracle artefact) | 3 % | 0.985 (holdout 1.000) | 1.9 % (holdout 7.0 %) |
| Python | 270 (holdout 86) | 0 | 0 % | 1.000 (dev 0.992) | 1.1 % (holdout 3.5 %) |
| JVM | 243 (holdout 81) | 0 | 5 % (dev 7 %, holdout 1 %) | 0.94 (holdout 0.82) | holdout 3.7 % |
| .NET | 248 (holdout 75) | 0 (6 oracle artefacts) | 5 % | 0.89 (holdout 0.80) | holdout 4.0 % |
| Go | 243 (dev 169, holdout 74) | 0 (26 oracle artefacts, hand-classified) | 1 % | 0.380 (holdout 0.133) | holdout 4.1 % |

Notes per ecosystem:

- **Node** (SWE-bench Multilingual + Multi-SWE-bench). No task widens to `full` in the latest run. Low selectivity comes
  from real dependencies: dynamic `import()` in a test helper, barrel files, fixture runners; Vue and Svelte compute
  their runner config in code (a [`[[config]]`](configuration.md#config) declaration took one Vue change from 208 to 18
  selected tests).
- **Rust** (275 tasks fetched, 180 in the manifest; holdout tokio, nushell, axum). The violation was checked by hand:
  the gold patch only touches a crate the failing test's crate does not depend on, even as a dev-dependency; the
  required names are pre-existing tests listed as new because the test patch edits their file.
- **Python** (SWE-bench Verified, 12 repositories; holdout astropy, seaborn, xarray, sympy). Selectivity is about 1.0
  because name-based dynamic imports (Django settings, Sphinx autodoc, matplotlib backends, test runners) fire in 266 of
  270 tasks; without them the median would be 0.064. This is what the dynamic layer addresses.
- **JVM** (SWE-bench Multilingual Java + Multi-SWE-bench Java/Kotlin, 22 repositories). Narrowing works where the module
  structure allows it: 0.31 of 224 modules on druid, 0.15 of 41 on jib. Median selection time 0.47 s.
- **.NET** (SWE-Sharp-Bench and SWE-bench-Live, 55 repositories). The 6 violations were classified as oracle artefacts:
  the required test's file is untouched by the test patch and the gold patch changes a project outside the test
  project's transitive `ProjectReference` closure, or only documentation.
- **Go** (Multi-SWE-bench, SWE-bench-Live MultiLang, SWE-bench Multilingual; 97 repositories). All 26 violations were
  checked against `go list -deps -test` at C: 10 are name-ambiguity artefacts and 16 are FAIL_TO_PASS entries whose
  packages have no changed file in their closure (flaky or timing tests). An acceptance test that executes a binary built
  by a Makefile through a relative path (`../../../alertmanager`, from a helper package) was a real gap; it is now covered
  by tying such paths to the main package of that name. Median selection time 46 ms.

### T2 mutation summary

| Ecosystem | Repositories | Killed mutants | Violations | Median selection |
|---|---|---|---|---|
| Node | zod, hono, dayjs | 67 | 0 | 0.280 / 0.056 / 0.032 |
| Rust | bytes, bat, ripgrep | 60 | 0 | 1.000 / 1.000 / 0.259 |
| Python | click, marshmallow, attrs | 60 | 0 | 1.000 / 1.000 / 1.000 |
| JVM | gson (Maven), moshi (Gradle) | 40 | 0 | 0.875 / 0.708 |
| .NET | Polly, RestSharp | 40 | 0 | 0.239 / 0.923 |
| Go | hcl, prometheus/common, connect-go | 60 | 0 | 0.389 / 0.100 / 0.200 |

On Node, Vitest's and Jest's own `--changed` selection was a subset of bgraph's on every non-full mutant. The Go set
found one real miss during development (a test that builds and runs a command of the same module); test literals naming
in-repository import paths now depend on that package.

### Native-tool and provider checks

| Ecosystem | Result |
|---|---|
| Rust | 60 commits × 12 repositories: every member package, target and target root file matches `cargo metadata`; bgraph also indexes packages outside the workspace (fuzz crates, examples), which only adds units |
| JVM | Maven: 20/20 builds read correctly, 1,601 of 1,601 inter-module edges found. Gradle: 8/12 builds identical; the others differ by intermediate projects without files or by settings bgraph cannot evaluate (which fail closed with a full-scope widening). GIB: 34 tasks agree, 1 divergence classified as GIB over-approximation |
| .NET | `ProjectReference`: 55 repositories / 151 commits, 9,086 edges outside dotnet/runtime, 0 missing; dotnet/runtime: 0 missing (377 edges, 278 of them covered by an unresolved-construct widening). dotnet-affected: 217 tasks without a real violation; 1 divergence by design (it counts a Markdown file under a docs-site project) |
| Go | 25 repositories, 50 base commits, 656 modules, 7,234 packages with tests: every package with tests is a unit; 87,926 in-repo import edges and 6,602 embedded files covered. 107 extra units (packages excluded by build constraints on the host OS): over-selection only |
| Node | Vitest / Jest `--changed` ⊆ bgraph on every non-full T2 mutant. Comparison with general code-graph tools: [Benchmarks](benchmarks.md) |

### Replay

Node: 500 historical commits, 0 invariant violations, 0 full scopes on ordinary commits, median selection 0.954
overall (0.100 on hono), median 239 ms per commit (p95 1.0 s).

### Dynamic layer

| | Python | Node | JVM (class layer) |
|---|---|---|---|
| Repositories | click, marshmallow, attrs, Python-Markdown, pygments, werkzeug | dayjs, hono, express (+ immutable-js in replay) | gson, moshi |
| Records complete | 89–100 % with `trust_native_env` (0 % with the strict default on Windows) | 98–100 % on dayjs and express; hono 125 of 161 (26 incomplete: a native bundler binding not on the allowlist, workerd spawns; the rest fail on Windows: bun/deno/workerd runtime tests) | 122 of 130 (gson), 32 of 39 (moshi) |
| Hybrid mutation: violations caused by the layer | **0** (92 narrowed mutants) | **0** (62 narrowed mutants) | **0** (33 narrowed mutants, 339 failing classes all kept) |
| Median selection, static → hybrid | 1.000 → 0.118–0.194 on widening-heavy repositories | 0.032 → 0.021, 0.056 → 0.025, 0.957 → 0.946 | 1.0 → 0.70 (gson), 0.80 (moshi) |
| Replay: verified narrowed commits / removed units / violations | 29 / 734 / **0** | 28 / 829 / **0** | 9 / 383 classes / **0** |
| Equality on non-widened commits | 0 failures (1,200 checks) | 0 failures | – |

Findings:

- Records made once and kept for months almost never narrow: a recorded dependency changes (staleness) or the
  environment does. With records refreshed on the base branch, guard-passing widened commits are narrowed to a median of
  0.04–0.20 of the units (Python) and 0.03–0.04 (Node).
- On Windows, `trust_native_env = true` is needed for Python: pytest's terminal setup loads a system library through
  ctypes, so with the strict default no record is complete.
- The JVM constant guard passes 94 % (gson) and 90 % (moshi) of code-only commits.

These checks are not a release gate: the dynamic layer stays off by default.

### Lockfile differential check

| Format | Repositories | Commits | Project checks | Mismatches | Parse errors |
|---|---|---|---|---|---|
| pnpm 9.0 | 13 | 302 | ~2,900 | 0 | 0 |
| pnpm 6.0 | 3 | 60 | 1,294 | 0 | 0 |
| npm v2/v3 | 12 | 271 | 549 | 0 | 0 |
| Cargo v3/v4 | 8 | 176 | 5,137 | 0 | 4 (corrupt upstream lockfiles that cargo also rejects) |
| uv.lock v1 (revisions 0–5) | 8 | 169 | 740 | 0 | 0 |
| poetry.lock 1.1/2.0/2.1 | 3 | 87 | 87 | 0 | 0 (2 more commits whose `pyproject.toml` poetry itself rejects as out of sync) |

For uv the reference is `uv export --frozen --all-extras --all-groups` per workspace member (split per extra where
declared conflicts forbid exporting them together), and for poetry `poetry export --all-groups --all-extras`. Besides
the superset check, every pair of consecutive commits is checked the other way: a project whose exported package set
changed must be among the projects bgraph reports as changed (uv 674 project checks, poetry 76: no miss).

pnpm 5.4 and npm lockfile v1 are unsupported and stay at L0. The check found and fixed two parser bugs before L1 was
enabled (a two-document `pnpm-lock.yaml` whose closures were read as empty, and a Cargo path crate shadowing a registry
crate of the same name).

## Known limits of the evidence

- The datasets are bug-fix tasks: they rarely change manifests, lockfiles or CI files, so those rules are covered mainly
  by fixtures.
- Some ecosystems' holdout sets are small; the rule-of-three bounds above are wide.
- Selectivity is low where code computes what it loads (Python, some Node repositories). That is the sound outcome; the
  [dynamic layer](dynamic-layer.md) and `bgraph.toml` declarations are the ways to narrow it.
