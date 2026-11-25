# Rust

bgraph selects Rust tests at Cargo-target granularity: every target that `cargo test` runs (library, doc-tests, binaries,
integration tests, benches, examples) is a test unit. Inside a crate the `mod`/`use` graph is not a sound dependency relation
(trait impls are crate-global, `macro_rules!` is textually scoped, `inventory`/`linkme` register at link time, `#[cfg]` changes the
module tree), so the module tree is only used to decide which target owns a file. Cargo is never run: manifests and sources are read
as text.

## At a glance

| | |
|---|---|
| Granularity | Cargo target |
| Unit ID format | `<pkgdir>::<kind>[:<name>]`, e.g. `crates/core::lib`, `crates/core::test:api`, `.::bin:mytool` |
| Runners | `cargo test` |
| Manifests/configs read | `Cargo.toml` (packages, workspaces, target tables, dependencies, `[patch]`, `[replace]`), `build.rs`; `.cargo/config(.toml)` is classified on change but not parsed |
| Lockfiles (and level) | `Cargo.lock` v3/v4, level L1 by default (L2 behaves as L1); see [lockfiles](../lockfiles.md) |
| Dynamic layer | none |

## Supported layouts

- **Workspaces**: a `Cargo.toml` with a `[workspace]` table. A package belongs to the workspace rooted in its own directory, else
  to the one its `package.workspace` key names, else to the nearest ancestor workspace; when that workspace's `exclude` globs
  match the package, the package is standalone. `members` is not read: membership is decided by directory nesting and `exclude`.
  Workspace membership only decides where `{ workspace = true }` dependencies are looked up.
- **Standalone crates**: a `Cargo.toml` with a `[package]` table and no enclosing workspace, including one at the repo root
  (package directory `.`).
- **Virtual manifests**: a workspace root without `[package]` is not a package and has no units.
- **Non-member crates**: every `Cargo.toml` with `[package]` (or the legacy `[project]`) is a package, wherever it is (`fuzz/`,
  example crates, test fixtures). Such crates produce extra units; they never cause a missed test.
- **Nested packages**: a package nested in another package's directory is a package of its own; the outer package does not own its
  files.
- **Ignored directories**: `target/` next to a `Cargo.toml` (build output), `node_modules/` and `.git/`. A `Cargo.toml` or
  `Cargo.lock` under such a `target/` is not treated as a manifest.
- **Package name**: `package.name`; when absent, the directory name (`root` for the repo root).

### File ownership

Every file belongs to its nearest enclosing package. Within the package:

- a file reached from a target's root file through its module tree, `#[path]`, a literal `include!`/`include_str!`/`include_bytes!`
  or `automod::dir!` is **owned** by that target;
- `build.rs` and the files it reads are **package-level**: every target of the package depends on them;
- any other file of the package is **package-level**, except unowned `.rs` files under `tests/` (`benches/`, `examples/`), which only
  the integration tests (benches, examples) depend on when the package has such targets. Data files under `tests/` stay
  package-level because unit tests in `src/` may read them at run time;
- documentation (`*.md`, `*.markdown`, `*.rst`, `*.adoc`), licence files at the repo root (`LICENSE*`, `COPYING*`, `NOTICE*`;
  a licence file inside a package directory is an ordinary package-level file), `.gitignore`, `clippy.toml`, `.clippy.toml`,
  `rustfmt.toml`, `.rustfmt.toml`, `deny.toml` and files under `.cargo/` are never package-level inputs (a README included with
  `#![doc = include_str!("../README.md")]` is owned by the library and does select the doc-tests).

A file outside every package (for example a `.rs` file under the root of a virtual workspace) is left to the general rules
(`unknown_file`, see [architecture](../architecture.md)).

## Test units

One unit per Cargo target, discovered the way Cargo does:

| Kind | Unit ID | Discovered from |
|---|---|---|
| `lib` | `<pkgdir>::lib` | `[lib]` table or `src/lib.rs` (`[lib] path` honoured); no unit when `[lib] test = false` |
| `doc` | `<pkgdir>::doc` | the library's doc-tests; no unit when `[lib] doctest = false` |
| `bin` | `<pkgdir>::bin:<name>` | `[[bin]]`, `src/main.rs` (named after the package), `src/bin/*.rs`, `src/bin/*/main.rs`; no unit when `test = false` |
| `test` | `<pkgdir>::test:<name>` | `[[test]]`, `tests/*.rs`, `tests/*/main.rs`; no unit when `test = false` |
| `bench` | `<pkgdir>::bench:<name>` | `[[bench]]`, `benches/*.rs`, `benches/*/main.rs` (run once in test mode) |
| `example` | `<pkgdir>::example:<name>` | `[[example]]`, `examples/*.rs`, `examples/*/main.rs` |

- `<pkgdir>` is the repo-relative package directory, `.` for the repo root.
- `autobins`, `autotests`, `autobenches`, `autoexamples = false` disable auto-discovery; explicit targets without `path` use Cargo's
  default locations.
- A target with `test = false` is still a graph node: a `[[bin]] test = false` is built for the integration tests, which depend on
  it.

## What a unit depends on

A unit depends on (directly or transitively):

- every file its target owns: the module tree from the root file (`mod x;` resolves to `x.rs` or `x/mod.rs`, inline modules and
  `#[path]` included; a `mod` whose file does not exist, such as one `#[cfg]`'d for another OS, owns nothing);
- files named by literal `include!`, `include_str!`, `include_bytes!` (relative to the including file), and by
  `concat!(env!("CARGO_MANIFEST_DIR"), "...")` (relative to the package); a missing target is still an edge, so a file added later is
  caught; `include!` of a `.rs` file is followed as module content;
- every file under a directory named by `automod::dir!("...")`;
- the package-level files of its package (see [File ownership](#file-ownership));
- bins, tests, benches, examples and the doc unit: the library of their package;
- tests, benches and examples: every binary of the package (Cargo builds them, `CARGO_BIN_EXE_*`);
- the library of every in-repo package it depends on, through normal, dev and build dependencies alike (target-specific
  `[target.*.dependencies]` included). A dependency resolves to a package directory through:
  - `path = "..."` (relative to the package),
  - `{ workspace = true }` and the `path` of the matching `[workspace.dependencies]` entry,
  - a `[patch.*]` or `[replace]` entry with a `path`, matched by the real crate name (`package = "..."` renames are followed);
  - a version-only dependency whose name happens to match a workspace member is **not** an edge;
  - a dependency on a package without a library depends on all of its targets;
- the build script (`build.rs`, or `package.build = "..."`; none when `build = false`) and what it reads:
  - its own module tree and includes,
  - every string literal in it that names an existing file or directory relative to the package directory (a
    `cargo:rerun-if-changed=` / `cargo::rerun-if-changed=` prefix is stripped; a directory means every file under it), including
    files outside the package,
  - a `../` literal that stays inside the repository but does not exist yet (a file added there later is an input).

`include!(concat!(env!("OUT_DIR"), ...))` needs no edge: build output is covered by the build script being a package-level input.

Dependency cycles (for example through a dev-dependency) terminate and select every package on the cycle. Proc-macro and
build-dependency crates are library nodes like any other: changing one selects its users.

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| `Cargo.toml`: only harmless keys (`package.description`, `keywords`, `categories`, `homepage`, `repository`, `documentation`, `authors`, `license`, `license-file`, `readme`, `publish`, `exclude`, `include`, `metadata`, `default-run`; `[badges]`; `workspace.metadata`, `workspace.default-members`; harmless keys of `[workspace.package]`) | `none` |
| `Cargo.toml`: package-level keys (any other `[package]` key such as `name`, `version`, `edition`, `build`, `links`, `auto*`, `rust-version`, `workspace`; `[lib]`, `[[bin]]`, `[[test]]`, `[[bench]]`, `[[example]]`, `[lints]`) | `module`, `manifest_changed`, scoped to the package and to each package that depends on it transitively |
| `Cargo.toml`: workspace-wide keys (`[dependencies]`, `[dev-dependencies]`, `[build-dependencies]`, `[target.*]`, `[features]`, `[patch]`, `[replace]`, `[profile]`, `cargo-features`, `package.resolver`, any other `[workspace]` key, unknown keys) | `module`, `manifest_changed`, scoped to the workspace root (nearest ancestor-or-self `Cargo.toml` with `[workspace]`; the manifest's own directory when there is none) |
| `Cargo.toml` of a virtual workspace: package-level keys | treated as workspace-wide |
| `Cargo.toml` added, deleted, or unparseable on one side | `module`, `manifest_changed`, scoped to the workspace root |
| `Cargo.toml` that does not parse (either tree) | additionally `full` (`unknown_import`) on any change under its directory |
| `Cargo.lock` | `module`, `lock_changed`, per package whose resolved closure changed (L1); see below |
| `.cargo/config` or `.cargo/config.toml` at the repo root | `full`, `global_input` |
| `.cargo/config(.toml)` below the root | `module`, `test_config_changed`, scoped to the directory that holds `.cargo/` |
| `clippy.toml`, `.clippy.toml`, `rustfmt.toml`, `.rustfmt.toml`, `deny.toml` | `none` |
| `rust-toolchain`, `rust-toolchain.toml` (any depth) | `full`, `global_input` (global widening table) |

Every `module` widening above carries the ecosystem `rust`: it selects Rust units only, never another ecosystem's tests under the
same directory, even when the scope is the repo root.

### `Cargo.lock`

At level L1 (the default) both sides are parsed and, for every package governed by the lockfile (the nearest `Cargo.lock` in its
directory or an ancestor), its resolved dependency closure is compared. `Cargo.lock` records the union of every dependency kind,
target and feature, and a member's closure contains the members it depends on, so reverse dependents follow without a rule of
their own.

- Closure changed, or package added to / removed from the lockfile, or not in it: that package is widened (`module`,
  `lock_changed`).
- Fallback to L0 (every Rust unit under the lockfile's directory): `[lockfile] level = "L0"`, a side that is absent or does not
  parse, a lockfile `version` other than 3 or 4, a version change, a change outside `[[package]]`, or a package name that matches
  several source-less entries.
- `level = "L2"` is accepted and behaves as L1: units are whole targets, and which targets use a crate is not known without name
  resolution.

## Unresolved and uncertain imports

| Situation | Effect |
|---|---|
| Module tree cannot be read lexically: `mod` declared inside a macro or a block, macro-generated module name (`$name`), `#[cfg_attr(.., path = ..)]`, non-literal `#[path]`, a `#[path]` on an enclosing inline module, a non-literal `include!` argument, a file that does not lex (unterminated comment or string, unbalanced delimiters) or is larger than 8 MiB | the target depends on every file of its package (reported as `dynamic`, no widening by itself) |
| Same for the build script | every target of the package depends on every file of the package |
| `include!`/`include_str!`/`include_bytes!` of a path outside the repository | `full` (`unknown_import`) on any change |
| Build-script literal starting with `..` that resolves outside the repository | `full` (`unknown_import`) on any change |
| `Cargo.toml` that does not parse | `full` (`unknown_import`) on any change under its directory |

Absolute paths in build-script literals are ignored (they cannot name a repository file portably). See
[architecture](../architecture.md) for how unresolved imports fire.

## Running the selection

`bgraph command` emits one `cargo test` per package, run in the package directory and addressed with `-p <package>`, plus a separate
`--doc` command when doc-tests are selected (Cargo cannot combine `--doc` with other target selectors):

```sh
$ bgraph command --base origin/main
# scope: graph
# cargo: running a subset with -p changes feature unification compared to a full workspace run
(cd crates/core && cargo test -p core --lib --test api --test cli)
(cd crates/core && cargo test -p core --doc)
(cd crates/server && cargo test -p server --lib --bin server)
```

- Target selectors: `--lib`, `--bin <name>`, `--test <name>`, `--bench <name>`, `--example <name>`, sorted (lib, bin, test, bench,
  example, then by name) and deduplicated.
- A unit without a package name is addressed with `--manifest-path <pkgdir>/Cargo.toml` instead of `-p`.
- A unit ID that cannot be parsed runs the whole package: `--all-targets` plus the `--doc` command.
- Cargo commands are not split into batches.
- Running a subset with `-p` can unify features differently from a full workspace run; `bgraph command` prints that note
  whenever the plan contains Cargo commands.

See [cli](../cli.md) for the command's options and JSON output.

## Addressing units in bgraph.toml

In a `[[rule]]`, an `affects` entry that is a directory selects every Rust unit whose package directory is that directory or lies
below it:

```toml
[[rule]]
paths   = ["schemas/**"]
affects = ["crates/codec"]    # every target of the package in crates/codec (and of packages nested below it)
reason  = "codec tests load the schemas at run time"
```

- Use the package directory (`.` for a root package), not a target or a file.
- Globs written for test files (for example `crates/*/tests/*.rs`) do not match Rust units: units are targets, not files.
- `affects = "all"` and `affects = "none"` behave as for every ecosystem.

Rust has no `[[dynamic]]` or `[[config]]` declarations. See [configuration](../configuration.md).

## Known gaps and over-selection

- **Package-level granularity for unowned files**: a file no target's module tree reaches (data, snapshots, `tests/` data) selects
  every target of its package and, through the library, every dependent package.
- **Unreadable module trees**: one macro-generated or `cfg_attr`-pathed module makes its target depend on every file of the package.
- **Feature unification**: a dependency or feature change anywhere in a workspace widens the whole workspace.
- **Nested scopes**: a `module` widening scoped to a package directory also selects packages nested below it; scoped to a root
  package, it selects every Rust unit of the repository.
- **Non-member crates** are indexed and can be selected (extra units).
- **Paths built at run time**: a test that reads a file through a path computed at run time (not a literal `include*!`) depends on it
  only when the file is package-level. Documentation files (`*.md`, `*.markdown`, `*.rst`, `*.adoc`) that no `include*!` or build-script literal
  names are never inputs, so a test reading such a file at run time is not seen.
- **Build scripts** are scanned for string literals only: a path assembled at run time (`format!`, `Path::join` of variables) is
  not seen, beyond the package-level dependency every target already has on the build script.
- **`Cargo.lock` L2** is not available.

## See also

- [Architecture](../architecture.md): tiers, widening reasons, unresolved imports, the global widening table
- [Lockfiles](../lockfiles.md): levels L0, L1 and L2
- [Configuration](../configuration.md): `bgraph.toml`
- [CLI](../cli.md)
