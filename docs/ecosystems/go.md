# Go

bgraph selects Go tests at package granularity: every package directory with a `_test.go` file is a test unit. `go test` compiles a
package, its in-package tests and its external `_test` package together (shared `init` order, `TestMain`, package state), so
selecting individual test functions is not sound. Resolution is pure: `go.mod`, `go.work` and `go/parser` (imports only); the go
command is never run.

## At a glance

| | |
|---|---|
| Granularity | package directory |
| Unit ID format | `go:<dir>`, e.g. `go:internal/store`, `go:.` for a package at the repo root |
| Runners | `go test` |
| Manifests/configs read | `go.mod`, `go.work` (directive-level), `vendor/`, `.golangci.*` (harmless) |
| Lockfiles (and level) | `go.sum`: lines only added widen nothing, any other change is level L0; see [lockfiles](../lockfiles.md) |
| Dynamic layer | none |

## Supported layouts

- **Modules**: every `go.mod` is a module, at any depth, except under `vendor/`, `testdata/`, `node_modules/`, or a directory whose
  name starts with `.` or `_`.
- **Nested modules**: a file belongs to its innermost module. A nested module's files are never files of the parent module's
  packages, and a nested module may import the parent module's packages (and the reverse).
- **`go.work`**: `use` directories are the workspace's modules; `replace` directives with a local target count like those of a
  `go.mod`; the workspace directory's `vendor/` applies to its modules.
- **Local `replace`**: `replace old => ./dir` (in `go.mod` or `go.work`) maps the import-path prefix `old` to that directory.
  Targets that are absolute or leave the repository are ignored.
- **Vendoring**: imports also resolve against the module's `vendor/` (and the `vendor/` of every `go.work` that uses it).
  Vendored packages are graph nodes, never units.
- **Repositories without `go.mod`**: the Go ecosystem is inactive; `.go` files there are left to the general rules (see
  [architecture](../architecture.md)).
- **GOPATH layout beside modules**: a `.go` file under no `go.mod`, in a repository that has at least one `go.mod`, is not analyzed:
  its change widens to `full`.

### File ownership

- A **package directory** is a directory with a `.go` file that belongs to a module and has no path component `testdata`,
  `node_modules`, or starting with `.` or `_`.
- Every other file of a module belongs to the nearest ancestor-or-self package directory of the same module. This includes
  non-package sub-directories: `testdata/`, `fixtures/`, `_tools/`, `.x/`, and `.go` files there (programs a test may build).
- Files of a module that no package owns (for example `config/` under a module without a root package) are **module-level**: every
  package of the module depends on them.
- Not inputs: documentation (`*.md`, `*.markdown`, `*.rst`, `*.adoc`), `.gitignore` and the repo-root licence files
  (`LICENSE*`, `COPYING*`, `NOTICE*`) when they lie directly in a package directory or outside every package (under `testdata/`
  or another non-package sub-directory they are data); `.golangci.{yml,yaml,toml,json}` anywhere; and `go.mod`, `go.sum`,
  `go.work`, `go.work.sum` in a module or `go.work` directory (classified on change, see below).
- **Other ecosystems' sub-trees**: a directory holding another ecosystem's project manifest (`package.json`, `pyproject.toml`,
  `setup.py`, `setup.cfg`, `Cargo.toml`, `pom.xml`, `build.gradle(.kts)`, `settings.gradle(.kts)`, `*.csproj`/`*.fsproj`/`*.vbproj`)
  and no `.go` file anywhere below it is not owned by Go. A frontend change under a root `go.mod` therefore selects no Go unit.
  `//go:embed` patterns and test string literals still reach files there.
- `node_modules/` is ignored.

## Test units

- One unit per package directory with at least one `_test.go` file (whatever its build constraints), not under `vendor/`.
- A package without `_test.go` files is a dependency, not a unit; its importers are still selected.
- Unit ID: `go:<dir>`, the repo-relative package directory (`go:.` at the repo root). The unit's project is its module directory and
  its package is the import path.

## What a unit depends on

Each package has a library node (what an importer compiles) and a unit node (what `go test` compiles: the library plus the test files
and their imports).

The **library** of a package depends on:

- every non-test file directly in its directory, whatever the extension (`.go`, `.s`, `.c`, `.h`, `.syso`, data), except the
  non-inputs listed under [File ownership](#file-ownership);
- the files of its non-package sub-directories (`testdata/`, `fixtures/`, `_x/`, `.x/`), and the in-repo packages that `.go`
  programs there import;
- the packages its non-test `.go` files import;
- its `//go:embed` patterns (a pattern matching a directory embeds it recursively; `all:` is accepted);
- the module-level files of its module;
- for a cgo package (`import "C"`): every C/C++/Objective-C/Fortran/header/assembly/SWIG/`.syso` file of the repository
  (`#include` and `#cgo -I` may reach anywhere).

The **unit** of a package depends on its library and on:

- its `_test.go` files (in-package and external `_test` package), the packages they import, and their `//go:embed` patterns;
- the existing repository paths its tests name in string literals, relative to the package directory (`go test` runs there):
  `"../x/testdata/y.json"`, `filepath.Join("..", "x", "testdata")` (the leading literal arguments of any `X.Join(...)` call); a
  directory means every file under it, including files added later;
- in-repo packages a test may build or run, with their whole import closure:
  - a literal naming a package directory (`"../cmd/x"`, as in `go build ../cmd/x`),
  - `dir/...` patterns (`"../cmd/..."`: every package below),
  - in-repo import paths and `<import path>/...` patterns (`"example.com/m/cmd/x"`),
  - each white-space separated token of a longer literal (`"go run ../cmd/x -v"`),
  - import paths on `go` and `exec` lines of testscript files under the package's `testdata/` (`*.txtar`, and `*.txt` under
    `script/`, `scripts/`, `testscript/`, `testscripts/`).
- programs built from the repository before the tests run (by a Makefile) and run by a relative path that is not in the tree,
  such as `exec.Command("../../../server")`: the main packages whose `go build` output has that name (the directory name, or for
  a module's root package the last element of the module path), with their whole import closure. The literal may sit in a test
  file or in a non-test helper package the test imports (the helper's library then depends on the program, so every test
  importing it does). A bare name (`"git"`) is looked up on `PATH` and adds nothing.

Imports resolve as follows:

- against every in-repo module path and every local `replace` prefix. All matching prefixes are used, not only the longest: an
  import path claimed by two modules depends on both;
- then against the applicable `vendor/` directories;
- paths no module claims are the standard library or external modules (`go.sum` and `go.mod` cover their changes);
- an in-repo path with no package in the tree is unresolved (see below), with an edge to the `.go` files of the directory the
  package would live in, so a package added there later is caught;
- an explicit import of a package under `testdata/` or `_x/` (which `./...` skips) resolves to the package that owns that directory.

Test-only imports stay on the unit: if `a_test.go` imports `c`, changing `c` selects `a`'s tests but not the importers of `a`.

Build constraints (`//go:build`, `+build`) and `_GOOS`/`_GOARCH` file suffixes are ignored: every file counts, so the graph is the
union of every configuration.

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| `go.mod`: only comments (including `// indirect` markers), layout, line order or `retract` | `none` |
| `go.mod`: `module` directive | `module`, `manifest_changed`, scoped to the repo root: every Go unit |
| `go.mod`: any other directive (`go`, `toolchain`, `require`, `replace`, `exclude`, `godebug`, `tool`...) | `module`, `manifest_changed`: the units of the module and of every module that builds with it (modules that `replace` a path with it, transitively, and the other modules of every `go.work` that uses it); not the units of other modules nested in its directory (see [nested modules](#nested-modules)) |
| `go.mod` added or deleted | `module`, `manifest_changed`: the module, the enclosing module (packages move between them), and the modules that build with either, each with every module nested inside it |
| `go.mod` unparseable on one side | `module`, `manifest_changed` (L0): the module and the modules that build with it, each with every module nested inside it |
| `go.mod` that does not parse (either tree) | additionally, any change in the module selects every package of the module and its dependents (`unknown_import`) |
| `go.sum` of a module, lines only added | nothing (`go.sum` only verifies what `go.mod` selects; the `go.mod` change, if any, is classified on its own) |
| `go.sum` of a module, a line removed or changed | `module`, `lock_changed` (L0): the units of that module and of the other modules of every `go.work` that uses it |
| a file under `vendor/` of a module or `go.work` directory | `module`, `lock_changed`: the units of that module, or of the workspace's modules; importers of the vendored package are also selected through the graph |
| `go.work`, `go.work.sum` (any depth) | `full`, `global_input` (global widening table) |
| `.golangci.yml`, `.golangci.yaml`, `.golangci.toml`, `.golangci.json` | `none` |
| `.go` file under no `go.mod` while the repository has a `go.mod` | `full`, `unknown_file` |
| `.go` file in a repository without any `go.mod` | not handled by Go: general rules |

- `go.mod`, `go.sum` or `vendor/` in a directory that is not a module (for example under `testdata/`) are ordinary data files of
  their owning package.
- Every `module` widening carries the ecosystem `go`: it selects Go units only, never another ecosystem's tests under the module
  directory, even for a root `go.mod`.

### Nested modules

A module nested in another module's directory is a separate build: `go test` in it reads its own `go.mod`, `go.sum` and
`vendor/`, not the parent's. A `go.mod` (other than a `module` change), `go.sum` or `vendor/` change of the parent therefore does
not select the nested module's units, unless the nested module builds with the parent:

- it `replace`s a path with the parent's directory (`replace example.com/m => ../`), directly or through other local replaces;
- it is a module of a `go.work` that also uses the parent (any directive change, `go` and `toolchain` included, widens every module
  of the workspace).

A nested module that imports the parent's packages without either builds them from its own requirements; a change to those
packages' files still selects it through the import. Fail-closed cases:

- a nested module whose `go.mod` cannot be read (either tree) is included, with the modules that build with it: whether it
  replaces the parent is unknown;
- when a `go.work` in the module's directory or above cannot be read, workspace membership is unknown: the widening covers every
  module nested inside the directory;
- a `go.work` or `go.work.sum` change is `full` (global table).

## Unresolved and uncertain imports

| Situation | Scope | Fires on |
|---|---|---|
| `go.mod` unreadable (no module path, syntax error, repeated `module`) | `module` (`unknown_import`) | any change in the module: every package of the module and its dependents |
| `.go` file whose package clause or imports do not parse, or larger than 8 MiB | `module` (`uncertain_scan`) | any change in its module: selects its package and every dependent |
| In-repo import path with no package in the tree (generated and ignored by git, or not written yet) | `module` (`unknown_import`) | any change in the importer's module or in the module the path would belong to |
| Same import from a program under `testdata/` or `_x/` | reported only | the pattern edge catches a package added later |
| `//go:embed` directive that does not parse | no widening | the package depends on its whole directory tree |

A `.go` file under `testdata/` that does not parse is test data, not an uncertain file: it selects its owner when it changes. A
change outside the module directories named in the table never fires these (a change in a module nested inside one of them does,
see [Known gaps](#known-gaps-and-over-selection)). See [architecture](../architecture.md) for how unresolved imports fire.

## Running the selection

`bgraph command` emits one `go test` per module, run in the module directory with package paths relative to it (a `go.work` above
it still applies):

```sh
$ bgraph command --base origin/main
# scope: graph
# go: one `go test` per module, run in the module directory with package paths relative to it (a go.work above it still applies); go test has no argument file, so long lists are split into batches
go test ./internal/store ./pkg/api
(cd tools && go test .)
```

- `.` is the module's root package; other packages are `./<rel>`.
- `go test` has no argument file: a command line longer than 8000 characters is split into batches, each its own command
  (with a note that exit codes must be merged).
- A unit whose ID cannot be parsed or lies outside its module directory runs the whole module: `go test ./...`.

See [cli](../cli.md) for the command's options and JSON output.

## Addressing units in bgraph.toml

In a `[[rule]]`, an `affects` entry that is a directory selects every Go unit whose **module directory** or **package directory**
is that directory or lies below it. A unit ID selects exactly that package:

```toml
[[rule]]
paths   = ["proto/**"]
affects = ["services/api"]   # the directory of services/api/go.mod: every package of that module
reason  = "generated code is rebuilt from the .proto files"

[[rule]]
paths   = ["schemas/**"]
affects = ["services/api/internal/store", "go:services/api/internal/codec"]
reason  = "store (and the packages below it) and codec load the schemas at run time"
```

- A package directory (`services/api/internal/store`) selects that package and every package below it; the unit ID
  `go:services/api/internal/store` selects that package only.
- `"."` selects every unit of every ecosystem; `affects = "all"` widens to `full`.
- Globs written for test files (for example `**/*_test.go`) do not match Go units: units are packages, not files.
- A target that matches no unit (a mistyped directory, a package without tests, a glob over `.go` files) is a configuration error:
  the selection is `full` (`user_rule`).

Go has no `[[dynamic]]` or `[[config]]` declarations. See [configuration](../configuration.md).

## Known gaps and over-selection

- **Package granularity**: any change to a package runs all its tests, and every package that imports it (transitively).
- **Union of configurations**: build constraints and GOOS/GOARCH suffixes are ignored, so packages whose files are all excluded on
  the current platform are still units and can be selected.
- **Ambiguous import paths**: a path claimed by several modules or `replace` targets depends on all of them.
- **cgo**: a cgo package depends on every C/C++/header/assembly file of the repository.
- **Nested modules and unresolved imports**: an unresolved import that fires on any change in a module (unreadable `go.mod`,
  uncertain scan, missing in-repo package) also fires on a change in a module nested inside it. A `go.mod` added, deleted or
  unparseable on one side, and a `module` directive change, still widen every module nested inside the scope.
- **Nested modules that import the parent without a local `replace` or a shared `go.work`**: a parent `go.mod` change does not
  select them (they build the parent from their own requirements). A `go.work` that is not committed (created locally or in CI)
  is not seen.
- **`go.sum` beyond added lines is L0**: a removed or changed line widens its module; which packages a checksum affects is not computed.
- **`go.work` and `go.work.sum` are `full`** through the global table, unless a `bgraph.toml` rule says otherwise.
- **Generated packages**: an import of an in-repo package missing from the tree selects the importer on any change in the module.
- **Paths built at run time** are not seen, beyond `X.Join` calls whose leading arguments are literals: a test (or a non-test helper
  package) reading a file through a computed path, including a file in another ecosystem's sub-tree or documentation in a package
  directory, may be missed.
- **Prebuilt binaries under another name**: a program run by a relative path is tied to its sources only when the path's last
  element is the name `go build` gives a main package; a binary renamed by the build (`go build -o bin/srv ./cmd/server`) or found
  through a path built at run time is not seen.
- **GOPATH layout** is not analyzed: such files widen to `full`.

## See also

- [Architecture](../architecture.md): tiers, widening reasons, unresolved imports, the global widening table
- [Lockfiles](../lockfiles.md): levels L0, L1 and L2
- [Configuration](../configuration.md): `bgraph.toml`
- [CLI](../cli.md)
