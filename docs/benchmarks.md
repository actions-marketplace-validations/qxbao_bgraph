# Comparison with code-graph tools

Before bgraph was built, three general-purpose code-graph tools were measured as test selectors, to see whether an
existing dependency graph would be enough. This page reports that comparison. The evaluation of bgraph itself is in
[Evaluation](evaluation.md).

## Fairness note

- codegraph, GitNexus and graphify are **general code-graph tools**, not test selectors. None of them claims to select
  tests, and none has a way to say "unsure".
- They were measured with **bgraph's test-selection criteria**: a required test that is not selected is a violation.
- For each tool, file-level dependency edges were **extracted from the tool's own index** (its database or graph export),
  and the selection was computed the same way for every tool: walk backwards from the changed files, with no depth limit,
  over the union of the graphs of the old and the new tree. Results therefore measure the graph each tool builds, not a
  feature the tool advertises.
- Single run per task, on Windows, no repeats.

## Tools

| Tool | Version | Index command | Edges used |
|---|---|---|---|
| codegraph | 1.6.2 | `codegraph init -y .` | every edge of its SQLite index, mapped to the files of both end nodes |
| GitNexus | 1.6.12 | `gitnexus clean --force`, then `gitnexus analyze . --skip-agents-md --skip-skills` | every `CodeRelation` except `CONTAINS` (imports, calls, inheritance...), from a Cypher query, mapped to file paths |
| graphify | 0.8.47 | `graphify update . --no-cluster` | every link of `graphify-out/graph.json`, mapped to the source files of both end nodes |

GitNexus ran with a 1 GiB buffer pool (the default made indexing fail on one repository) and its query row limit raised
so that edges were not truncated. GitNexus is distributed under the PolyForm Noncommercial license; it was only run
locally as a comparison tool.

## Part A: trap fixtures

Six small repositories, one per ecosystem (E1 TypeScript, E2 JavaScript, E3 Python, E4 Rust, E5 Java/Maven, E6 C#/.NET),
each with a few test units and one "guard" test that no change should select. Six change kinds per fixture, 36 cells:

| Cell | Change |
|---|---|
| C1 | edit a leaf source file |
| C2 | edit a file at the bottom of an import chain |
| C3 | edit a test (or a test module) |
| C4 | edit the build configuration (`tsconfig.json`, `package.json`, `pyproject.toml`, `Cargo.toml`, `pom.xml`, `.csproj`) |
| C5 | add a source file and a test that uses it |
| C6 | delete a file |

The edits are mechanical: a comment appended to code, TOML or XML files, whitespace appended to JSON files. The
expectation lists the tests that must run (`need`) and the guard test (`exclude`); C4 expects every test of the fixture.
A cell has **no miss** when every `need` test is selected (or the scope is `full`).

| Tool | No miss (C1–C3, C5–C6) | C4 (build config) | Guard test selected | Index time, 10-file fixture |
|---|---|---|---|---|
| codegraph 1.6.2 | 25/30 | 0/6 | 0 | 2–4 s |
| GitNexus 1.6.12 | 24/30 | 0/6 | 0 | 40–57 s |
| graphify 0.8.47 | 21/30 | 0/6 | 0 | about 1 s |
| bgraph (Node only, first measurement) | 30/30 (20 via `full`) | 5/6 | 0 | 0.4–1.7 s per `affected` call |
| bgraph (all six ecosystems, current) | **30/30** (none via `full`) | 2/6 | 3 (Rust) | 0.1–1.0 s per `affected` call |

The first bgraph measurement was taken when only Node was supported: every non-Node cell widened to `full`
(`unknown_file`), which counts as no miss but is not a selection. The current row is a re-run of the same harness with
all six ecosystems; for Rust, JVM and .NET units the harness maps a unit ID to the paths it covers (a Cargo
`<crate>::test:<name>` target to `<crate>/tests/<name>.rs`, the `::lib` target to the crate's `src/` files, a Maven
module or .NET project to its directory).

bgraph's C4 misses, stated plainly:

- **E2 C4** (also in the first measurement): whitespace appended to `package.json`. bgraph compares `package.json` by
  top-level keys, so a whitespace-only edit selects nothing (scope `none`). This is deliberate (the edit has no effect),
  but under this criterion it is a miss.
- **E4 C4**: a comment appended to the root `Cargo.toml`. Manifests are diffed structurally; a comment changes nothing
  (scope `none`).
- **E5 C4**: a comment appended to the root `pom.xml`. Only the root module, which owns the file, is selected.
- **E6 C4**: a comment appended to `Core/Core.csproj`. The projects that reference `Core` are selected; the `Watch` test
  project, which does not reference it, is not.

All four are build files edited without any semantic change; a real dependency or setting change in the same files
widens as described in the [ecosystem pages](README.md#ecosystems). The three guard selections are Rust: a change to a
library file selects the library's whole unit-test target, which also holds the guard test (bgraph's Rust granularity is
the Cargo target).

## Part B: real repositories, FAIL_TO_PASS oracle

Node only. 59 tasks sampled from the Node T1 dataset ([Evaluation](evaluation.md#t1-fail_to_pass)), 58 evaluable (one
task's required test could not be mapped to a file). For each task the tool indexed the old and the new tree, and a test
unit counted as selected when any of its files was in the backward closure of the changed files. Violation = a required
test not selected; `sel/units` = selected units / all test units.

Scope cuts, stated honestly:

- 10 minutes per index; after two consecutive timeouts on a repository, its remaining tasks were recorded as `timeout`.
  graphify timed out on all 4 material-ui tasks.
- To bound the run time, graphify was not run on babel (4 tasks), three.js (3) and vuejs/core (4): 11 tasks
  `skipped:budget`.
- GitNexus was stopped after 43 of 58 tasks: 14 tasks `skipped:budget` (preact 5, axios 4, express 2, dayjs 3) and one
  native crash on a material-ui task (`error`).

### All tasks each tool finished

| Selector | Tasks | With result | ok | Violations | Timeout / error | skipped:budget | median sel/units (ok) | p90 (ok) | median sel/units (ok + violations) | median index time, old + new tree |
|---|---|---|---|---|---|---|---|---|---|---|
| run everything | 58 | 58 | 58 | 0 | 0 | 0 | 1.000 | 1.000 | 1.000 | – |
| bgraph | 58 | 58 | 58 | 0 | 0 | 0 | 0.956 | 1.000 | 0.956 | under 1 s |
| codegraph | 58 | 58 | 50 | 8 | 0 | 0 | 0.748 | 0.956 | 0.567 | 10 s |
| graphify | 58 | 43 | 27 | 16 | 4 / 0 | 11 | 0.211 | 0.708 | 0.125 | 16 s |
| GitNexus | 58 | 43 | 24 | 19 | 0 / 1 | 14 | 0.785 | 0.953 | 0.491 | 166 s |

### Intersection: the 29 tasks all three tools finished

| Selector | Tasks | ok | Violations | median sel/units (ok) | p90 (ok) |
|---|---|---|---|---|---|
| bgraph | 29 | 29 | 0 | 0.956 | 1.000 |
| codegraph | 29 | 27 | 2 | 0.792 | 0.956 |
| graphify | 29 | 19 | 10 | 0.255 | 0.708 |
| GitNexus | 29 | 19 | 10 | 0.779 | 0.953 |

### Per repository (ok / violations / timeout or error, skipped:budget in parentheses)

| Repository | Tasks | Test units | codegraph | graphify | GitNexus |
|---|---|---|---|---|---|
| anuraghazra/github-readme-stats | 4 | 16 | 4/0/0 | 4/0/0 | 4/0/0 |
| axios/axios | 5 | 57 | 5/0/0 | 4/1/0 | 1/0/0 (+4) |
| babel/babel | 5 | 215 | 4/1/0 | 0/1/0 (+4) | 1/4/0 |
| darkreader/darkreader | 2 | 24 | 2/0/0 | 2/0/0 | 2/0/0 |
| expressjs/express | 4 | 90 | 4/0/0 | 0/4/0 | 1/1/0 (+2) |
| facebook/docusaurus | 4 | 268 | 4/0/0 | 4/0/0 | 0/4/0 |
| iamkun/dayjs | 4 | 72 | 4/0/0 | 4/0/0 | 1/0/0 (+3) |
| immutable-js/immutable-js | 2 | 44 | 2/0/0 | 0/2/0 | 0/2/0 |
| mrdoob/three.js | 3 | 245 | 3/0/0 | (+3) | 3/0/0 |
| mui/material-ui | 4 | 587 | 2/2/0 | 0/0/4 | 2/1/1 |
| preactjs/preact | 12 | 112 | 7/5/0 | 4/8/0 | 7/0/0 (+5) |
| sveltejs/svelte | 5 | 47 | 5/0/0 | 5/0/0 | 2/3/0 |
| vuejs/core | 4 | 196 | 4/0/0 | (+4) | 0/4/0 |

### Violations by cause

| Tool | Violations | Causes |
|---|---|---|
| codegraph | 8 | preact ×5: tests import `preact` or `preact/hooks`, the package's own name mapped to its sources (package self-reference); material-ui ×2; babel ×1: fixture-directory tests (`input.js` / `output.js` are data read by the runner, not imported) |
| graphify | 16 | express ×4: `require('../')` (a directory / `main` import); immutable-js ×2 and preact ×8: package self-reference; axios ×1; babel ×1: fixture-directory tests |
| GitNexus | 19 | in 15 of them the closure held no test file at all (babel ×4, docusaurus ×4, immutable-js ×2, vuejs/core ×4, svelte ×1; one docusaurus test imports a plain relative `../options`, so this is not only alias resolution; the cause inside the tool was not investigated); express ×1, material-ui ×1, svelte ×2 (runner-driven `tests/*/test.ts` harnesses that load fixtures dynamically) |

Every violation is silent: none of the tools signals that it is unsure.

### Index cost

Indexing time grows quickly with repository size: codegraph took 90–320 s per tree on material-ui; GitNexus 290–590 s
on material-ui and 90–290 s on docusaurus even for tiny diffs, and about 1.5 min per tree on a repository as small as
dayjs; graphify exceeded 600 s on material-ui. bgraph's selection took under a second (median) with its content-keyed
cache.

## What the comparison shows

- Each code-graph tool misses required tests silently on a sizable share of tasks (codegraph 8/58, graphify 16/43,
  GitNexus 19/43 of what they finished). The misses fall in known classes: package self-reference, directory or `main`
  imports, tests driven by fixture directories.
- bgraph had no violation, but its selectivity on this Node sample is low (median 0.956): most tasks widen to the project
  because of dynamic imports, uncertain scans or unknown imports. It is safe, with little saving on these repositories.
- The sample is Node-only, so Part B says nothing about selection quality in other ecosystems; those are covered by
  [Evaluation](evaluation.md).
