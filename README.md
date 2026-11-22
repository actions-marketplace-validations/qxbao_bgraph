# bgraph

Affected-test selection across languages, **fail-closed**.

Given a change (a branch, a commit range or your working tree), bgraph tells you which tests can be affected. It reads
imports, manifests and runner configurations as text, builds a file-level dependency graph of the old and the new tree,
and walks it backwards from the changed files.

## The promise

For every change, the output either contains **every** test that could be affected, or widens the scope (a project, a
package, the full suite) with a machine-readable reason. bgraph never silently drops a test. When it cannot be sure (a
computed import, a build file it cannot evaluate, a CI definition), it selects more and says why; an optional
[`bgraph.toml`](docs/configuration.md) lets you supply what it could not infer.

## Supported ecosystems

| Ecosystem | Unit of selection | Runners / build tools | Details |
|---|---|---|---|
| Node (JS/TS, CJS/ESM) | test file | Vitest, Jest, Mocha, AVA, `node --test`, Playwright | [docs/ecosystems/node.md](docs/ecosystems/node.md) |
| Python | test file (or directory for collection hooks) | pytest, Django's runner | [docs/ecosystems/python.md](docs/ecosystems/python.md) |
| Rust | Cargo target (`lib`, `doc`, `bin`, `test`, `bench`, `example`) | `cargo test` | [docs/ecosystems/rust.md](docs/ecosystems/rust.md) |
| JVM | Maven module, Gradle project | Maven, Gradle (wrappers detected) | [docs/ecosystems/jvm.md](docs/ecosystems/jvm.md) |
| .NET | MSBuild project (`.csproj`, `.fsproj`, `.vbproj`) | `dotnet test` (VSTest, Microsoft.Testing.Platform) | [docs/ecosystems/dotnet.md](docs/ecosystems/dotnet.md) |
| Go | package | `go test` (modules, `go.work`, vendoring) | [docs/ecosystems/go.md](docs/ecosystems/go.md) |

Lockfiles (`pnpm-lock.yaml`, `package-lock.json`, `Cargo.lock`) are compared by resolved dependency closure, so a bump
selects only the projects it touches ([lockfiles](docs/lockfiles.md)). Changes in other languages widen to the project
or to the full suite.

An optional [dynamic layer](docs/dynamic-layer.md) (off by default) narrows widened selections with recorded per-test
dependencies for Python, Node and JVM test classes.

## Install

```sh
go install github.com/qxbao/bgraph/cmd/bgraph@latest
# or, from a checkout:
go build -o bgraph ./cmd/bgraph
```

bgraph needs `git` on PATH. It never runs your package manager, build tool or test runner to select tests.

## Quick start

```sh
bgraph affected --base origin/main          # tests affected by the working tree relative to base
bgraph affected --base A --head B --json    # between two commits, as JSON
bgraph command --base origin/main           # ready-to-run runner commands (vitest run ..., cargo test -p ..., go test ...)
bgraph explain src/a.test.ts                # why a test was selected
bgraph unresolved                           # what bgraph could not read, with suggested bgraph.toml entries
```

Before trusting the selection in CI, run it in **shadow mode**: keep running the full suite and let `bgraph audit` report
every failure the selection would have skipped ([CLI: audit](docs/cli.md#audit), and a
[GitHub Actions example](docs/ci/shadow-github-actions.yml)).

bgraph also runs as an MCP server (`bgraph mcp`) and as a Go library:

```go
idx, err := bgraph.Open(ctx, root, bgraph.Options{})
sel, err := idx.Select(ctx, bgraph.Change{Base: "origin/main", Worktree: true})
```

Caches live in the user cache directory, never inside the repository.

## Documentation

| Page | Content |
|---|---|
| [Architecture](docs/architecture.md) | the fail-closed contract, the graph, scope tiers, widening reasons |
| [Configuration](docs/configuration.md) | `bgraph.toml` reference ([annotated example](docs/bgraph.example.toml)) |
| [CLI](docs/cli.md) | commands, flags, JSON output, exit codes, shadow audits, MCP |
| [Lockfiles](docs/lockfiles.md) | levels L0 / L1 / L2 |
| [Dynamic layer](docs/dynamic-layer.md) | record-based narrowing |
| [Evaluation](docs/evaluation.md) | how soundness is tested, and the results per ecosystem |
| [Benchmarks](docs/benchmarks.md) | comparison with general code-graph tools |

The full index is [docs/README.md](docs/README.md).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES).
