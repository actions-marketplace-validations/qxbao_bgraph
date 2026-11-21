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

## License

Apache License 2.0 — see `LICENSE` and `THIRD_PARTY_LICENSES`.
