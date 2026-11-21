# bgraph

Affected-test selection across languages, **fail-closed**: for every change, the output either contains every test
that could be affected, or widens the scope (project, full suite) with a machine-readable reason. It never silently
drops a test.

Status: the Node ecosystem (JS/TS, CJS/ESM; Jest, Vitest, Mocha, node:test, Playwright). Other ecosystems are not
supported yet: changes there widen to `full`.

## Usage

```sh
go install github.com/qxbao/bgraph/cmd/bgraph@latest   # or: go build -o bgraph ./cmd/bgraph

bgraph affected --base origin/main          # tests affected by the working tree relative to base
bgraph affected --base A --head B --json    # between two commits, JSON output
bgraph command --base origin/main           # runner commands (jest --runTestsByPath, vitest run, ...)
bgraph explain src/a.test.ts                # why a test was selected
bgraph unresolved                           # unresolved imports, with suggested bgraph.toml rules
```

Go library: `bgraph.Open(ctx, root, bgraph.Options{})`, then
`idx.Select(ctx, bgraph.Change{Base: "origin/main", Worktree: true})`.

Optional `bgraph.toml` at the repository root declares dependencies the tool cannot infer (`[[rule]]`) and generated
outputs (`[[generated]]`). Caches live in the user cache directory, never inside the repository.

## Shadow mode

Before trusting a selection, run it in shadow: CI runs the **full** suite as usual, then audits the results.

```sh
bgraph audit --results test-results/junit.xml --base origin/main --log shadow.jsonl
```

`audit` computes the selection for the same change and reports every **failed** test that lies outside it, that is, a
test the selection would have skipped although it failed. Such a failure is a potential unsafe miss, a flaky test or a
failure that already existed on the base; the audit cannot tell which, so it reports and a human judges. Switch
selection on after N clean shadow runs in a row (`--log` appends one JSON line per run and prints the streak).

- Results formats (`--format auto` by default; `--results` may be repeated): JUnit XML, Jest/Vitest JSON,
  Playwright JSON. Failures that cannot be attributed to a known test file (e.g. JUnit from Mocha with describe names
  only) are listed as `unmapped`, never dropped.
- Exit codes: `0` clean, `3` at least one missed failure (with `--strict`, also an unmapped one), `1` error
  (unreadable results, git error), `2` usage.
- A run whose scope is `full` (no base, shallow clone, global input changed) is trivially clean.

A ready-to-adapt GitHub Actions workflow is in [`docs/ci/shadow-github-actions.yml`](docs/ci/shadow-github-actions.yml).

## MCP

`bgraph mcp [--root DIR]` runs an MCP server on stdio (one index per session, so repeated queries are fast). Tools:

- `affected_tests` — `{base?, head?, previously_failed?, limit?}`: the selection (scope, tests with reasons, widening, warnings).
- `dependents_of` — `{files, rev?, limit?}`: who depends on the files, with the dependency chain and the unresolved imports that may too.
- `explain_selection` — `{target, base?, head?, limit?}`: why a test is selected / where a file leads.

Results are size-bounded (`limit`, default 200): when a list is cut, `truncated` is true and the `*_total` counts give the real
size. The `widening`, `warnings` and `unresolved` sections are always present; `scope: full` means run the whole suite.

```json
{
  "mcpServers": {
    "bgraph": { "command": "bgraph", "args": ["mcp", "--root", "/path/to/repo"] }
  }
}
```

With Claude Code: `claude mcp add bgraph -- bgraph mcp --root /path/to/repo`.

## License

Apache License 2.0 — see `LICENSE` and `THIRD_PARTY_LICENSES`.
