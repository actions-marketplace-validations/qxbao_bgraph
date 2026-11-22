# Command-line reference

`bgraph <command> [flags]`. Every command that reads a repository takes `--root DIR` (any directory inside the
repository; default `.`).

| Command | Purpose |
|---|---|
| [`affected`](#affected) | the selection: scope, tests, widenings |
| [`command`](#command) | the selection as test-runner commands |
| [`explain`](#explain) | why a test is selected, or where a file leads |
| [`unresolved`](#unresolved) | unresolved imports with suggested `bgraph.toml` entries |
| [`index`](#index) | analyze the working tree and print counts |
| [`audit`](#audit) | shadow mode: check a full-suite run against the selection |
| [`record`](#record) | record per-test dependencies for the dynamic layer |
| [`dynamic import`, `dynamic status`](#dynamic) | manage dynamic-layer records |
| [`mcp`](#mcp) | MCP server on stdio |
| `version`, `help` | print the version or the usage |

`bgraph doctor` is reserved: it prints "not implemented" and exits with code `2`.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success (for `audit`: clean) |
| `1` | tool error (git cannot run, unreadable file, write failure...) |
| `2` | usage error (unknown command or flag, wrong arguments) |
| `3` | `audit` only: a failed test lies outside the selection (with `--strict`, also an unmapped failure) |

An uncertain selection is not an error: it is reported as a widening and the exit code is `0`.

## Base and head

`affected`, `command`, `explain` and `audit` compare a **base** with a **head**:

- `--base REF`: a ref or commit. Default: the merge-base of `HEAD` with the first of `origin/HEAD`, `origin/main`,
  `main`, `master` that exists. When none exists, bgraph warns and the selection is `full` (`no_base`). A base that cannot
  be resolved (for example in a shallow clone) also gives `full` (`no_base`).
- `--head REF` (`affected`, `audit`): the target commit. Default: the working tree, including staged, unstaged and
  untracked (non-ignored) files.

## Dynamic-layer flags

`affected`, `command`, `explain` and `audit` accept:

| Flag | Effect |
|---|---|
| `--dynamic` | enable the [dynamic layer](dynamic-layer.md) for this run (every supported ecosystem) |
| `--dynamic=false` | disable it even if `bgraph.toml` enables it |
| `--python EXE` | interpreter (overrides `[dynamic_layer] python`) |
| `--node EXE` | node executable (overrides `[dynamic_layer] node`) |
| `--trust-native-env` | as `[dynamic_layer] trust_native_env = true` |

Without `--dynamic`, `bgraph.toml` decides.

## `affected`

```text
bgraph affected [--root DIR] [--base REF] [--head REF] [--json] [--failed ID,...] [dynamic flags]
```

| Flag | Meaning |
|---|---|
| `--json` | print the [selection as JSON](#json-output) |
| `--failed ID,...` | test unit IDs that failed in the previous run; they are always selected if they still exist |

Text output lists the scope, the base, the number of changed files, each widening (reason, path, tier, scope, detail
and suggestion), warnings, the selected tests with their first reason, and the units removed by the dynamic layer.

```sh
bgraph affected --base origin/main
bgraph affected --base A --head B --json
```

### JSON output

`affected --json` prints a `Selection`:

| Field | Type | Meaning |
|---|---|---|
| `scope` | string | `none`, `graph`, `package`, `module` or `full` ([tiers](architecture.md#scope-tiers)) |
| `base`, `head` | string | resolved commits (`head` empty for the working tree) |
| `changed` | string[] | changed paths |
| `tests` | object[] | selected test units (below); absent when empty, and always empty for `full` |
| `widening` | object[] | widenings (below) |
| `warnings` | object[] | widenings that do not change the scope (`case_mismatch`, `rule_rejected`, `dynamic_skipped`) |
| `always_run` | string[] | new test files and previously failed units |
| `narrowed` | object[] | widened units removed by the dynamic layer: `unit`, `recorded` (time), `commit`, `reason` |
| `index_stats` | object | counters: `units`, `seeds`, `reached`, `unresolved` (and `narrowed_classes` with the JVM class layer) |

A selected test:

| Field | Meaning |
|---|---|
| `unit` | unit ID (a test file, `crate::target`, `maven:<dir>`, `go:<dir>`...; see the [ecosystem pages](README.md#ecosystems)) |
| `project` | the directory the runner runs from |
| `runner` | `jest`, `vitest`, `mocha`, `ava`, `node-test`, `playwright`, `pytest`, `django`, `cargo`, `maven`, `gradle`, `dotnet`, `go`... |
| `files` | the unit's files (a virtual node for build-level units) |
| `reasons` | reason chains, each step joined by ` ; `, e.g. `src/a.test.ts --import-> src/util.ts ; src/util.ts (changed)` |
| `whole` | run the project's whole test suite (the test file set is unknown) |
| `package`, `config` | ecosystem-specific runner addressing details (for example the Cargo package for `-p`, the Go import path, the pytest config file or Django driver; see the ecosystem pages) |
| `widened` | selected only because of a widening; only such units can be removed by the dynamic layer |
| `dynamic` | why the dynamic layer kept the unit |
| `class_filter`, `classes` | the JVM class layer narrowed the unit: only these test classes run (the module is still built) |

A widening:

| Field | Meaning |
|---|---|
| `tier` | the tier it widens to |
| `reason` | a [reason code](architecture.md#widening-reasons) |
| `path` | the file that caused it |
| `scope` | the widened directory (empty for `full`) |
| `detail` | human-readable detail |
| `suggest` | a `bgraph.toml` snippet that could narrow it |
| `eco` | restricts a module/package widening to one ecosystem's units |

## `command`

```text
bgraph command [--root DIR] [--base REF] [--list-dir DIR] [--json] [dynamic flags]
```

Turns the selection into runner commands. Text output starts with `# scope: <scope>` and `# <note>` lines, then one
shell line per command (`cd <project> && ...` when the project is not the root, environment variables as a prefix).

| Flag | Meaning |
|---|---|
| `--list-dir DIR` | write list files there (Playwright `--test-list`, pytest `@argfile`, Gradle init scripts for class filters) instead of long argument lists |
| `--json` | print the plan as JSON |

JSON plan: `scope`, `commands` (each with `runner`, `project` (working directory), `argv`, optional `env`, `list_file`,
`batch` / `batches` when split, `note`) and `notes`.

Rules:

- scope `none`: no command (note "no test is affected: nothing to run");
- scope `full`: no command (note "run the full suite");
- a non-`none` scope without tests, or a test unit whose runner has no adapter: the plan's scope becomes `full`, with no
  command and a note saying to run the full suite;
- a command is never emitted with an empty list; long lists are split into batches (8000 characters per command line);
- a runner that cannot express a list safely (a file name with glob characters for Mocha, AVA or `node --test`) falls
  back to a whole-project command, with a note.

The per-runner command shapes are in the [ecosystem pages](README.md#ecosystems).

```sh
$ bgraph command --base origin/main
# scope: graph
# vitest: the argument is a substring of the path, so a.test.ts also matches aa.test.ts (over-selection, safe)
cd packages/core && npx vitest run src/a.test.ts src/b.test.ts
```

## `explain`

```text
bgraph explain [--root DIR] [--base REF] [dynamic flags] <test|file>
```

For a selected test: every reason chain that selected it. For a file: where it leads. For a unit removed by the dynamic
layer: why it was removed or kept.

## `unresolved`

```text
bgraph unresolved [--root DIR] [--class unknown|dynamic|ambiguous|uncertain_scan]
```

Lists the unresolved imports of the working tree, one per line (`file:line "spec" [class] widens to <tier> <scope>`),
with a detail line and a suggested `bgraph.toml` entry (`[[rule]]`, `[[config]]` for computed runner-config keys, or
`[[dynamic]]` for Python dynamic imports). Stale `[[dynamic]]` declarations are listed too. Ends with `total: N`.

## `index`

```text
bgraph index [--root DIR]
```

Analyzes the working tree and prints `files`, `edges`, `test units`, `unresolved` and `elapsed`. It also warms the
[caches](architecture.md#index-and-caches).

## `audit`

```text
bgraph audit --results FILE [--results FILE ...] [--format auto|junit|jest-json|playwright-json]
             [--root DIR] [--base REF] [--head REF] [--failed ID,...] [--json] [--log FILE] [--strict] [dynamic flags]
```

Shadow mode: CI runs the **full** suite as usual, then `audit` computes the selection for the same change and reports
every failed test that lies outside it. Such a failure is an unsafe miss, a flaky test or a failure that already existed
on the base; the audit cannot tell which, so it reports and a human judges.

| Flag | Meaning |
|---|---|
| `--results FILE` | test results (repeatable): JUnit XML, Jest/Vitest JSON, Playwright JSON |
| `--format` | `auto` (default) or a fixed format |
| `--json` | print the report as JSON: `scope`, `base`, `head`, `selected`, `failed`, `missed[]`, `unmapped[]`, `clean` |
| `--log FILE` | append one JSON line per run and print the number of clean runs in a row |
| `--strict` | also exit `3` when a failure cannot be mapped to a test file |

- Failures that cannot be attributed to a known test file (for example JUnit from Mocha with describe names only) are
  listed as `unmapped`, never dropped.
- A failure in a unit removed by the dynamic layer is a miss like any other.
- A run whose scope is `full` is trivially clean.

Switch selection on after N clean shadow runs in a row. A ready-to-adapt GitHub Actions workflow is in
[`ci/shadow-github-actions.yml`](ci/shadow-github-actions.yml).

## `record`

```text
bgraph record [--root DIR] [--eco python|node|jvm] [--units GLOB] [--jobs N] [--timeout D] [--allow-dirty]
              [--out FILE] [--build-args ARGS] [--keep-logs] [--python EXE] [--node EXE] [--trust-native-env]
              [--json] [--quiet]
```

Runs test units with a dependency collector and stores the records of the [dynamic layer](dynamic-layer.md).

| Flag | Default | Meaning |
|---|---|---|
| `--eco` | `[dynamic_layer] ecosystems`, else every ecosystem with test units | record one ecosystem |
| `--units GLOB` | all | only units matching the glob |
| `--jobs N` | Python: half the CPUs; Node: a quarter; JVM: min(4, CPUs/2) parallel test JVMs | parallelism |
| `--timeout D` | `10m` | per unit (per build for JVM); a unit that times out gets an incomplete record |
| `--allow-dirty` | off | record a working tree that differs from `HEAD` |
| `--out FILE` | the state directory | write the records to a JSONL file instead (import elsewhere with `bgraph dynamic import`) |
| `--build-args ARGS` | – | JVM: extra Maven/Gradle arguments, space-separated |
| `--keep-logs` | off | JVM: keep the collector logs |
| `--json` | off | print the summary as JSON |
| `--quiet` | off | no per-unit progress |

The summary lists the commit, units, recorded, complete (and passed), incomplete, failed, the store, elapsed time, the native
toolchain addons accepted (Node) and the incomplete reasons. Exit code `0` even when some units failed or are incomplete.

## `dynamic`

```text
bgraph dynamic import [--root DIR] FILE
bgraph dynamic status [--root DIR] [--eco python|node|jvm] [--python EXE] [--node EXE] [--trust-native-env] [--json]
```

- `import` merges a records file (from `record --out`) into the store, for example records made by CI on the main branch.
- `status` reports, per ecosystem, the store, the number of records and recorded units, records of the current
  environment, records usable at `HEAD` (and why the others are not) and the incomplete reasons. Without `--eco` it
  reports Python and Node when they have records or test units; with `--json`, an object for `--eco`, else an array.

## `mcp`

```text
bgraph mcp [--root DIR]
```

Runs a [Model Context Protocol](https://modelcontextprotocol.io) server on stdio, with one index per session so repeated
queries are fast. Tools:

| Tool | Arguments | Result |
|---|---|---|
| `affected_tests` | `base?`, `head?`, `previously_failed?`, `limit?`, `dynamic?` | the selection: scope, tests with reasons, widening, warnings, narrowed units |
| `dependents_of` | `files`, `rev?`, `limit?` | the files that depend on the given files (tests first), each with its dependency chain, the selected test units, and the unresolved imports that may depend on them |
| `explain_selection` | `target`, `base?`, `head?`, `limit?`, `dynamic?` | why a test is selected, or where a file leads |

Results are size-bounded (`limit`, default 200, max 5000): when a list is cut, `truncated` is true and the `*_total`
counts give the real size. The `widening`, `warnings`, `narrowed` and `unresolved` sections are always present;
`scope: full` means run the whole suite.

```json
{
  "mcpServers": {
    "bgraph": { "command": "bgraph", "args": ["mcp", "--root", "/path/to/repo"] }
  }
}
```

With Claude Code: `claude mcp add bgraph -- bgraph mcp --root /path/to/repo`.
