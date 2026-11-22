# Lockfiles

A lockfile change can affect any test whose code loads a package whose resolved version changed. bgraph classifies
lockfile changes at one of three levels, chosen with [`[lockfile] level`](configuration.md#lockfile) in `bgraph.toml`.

## Levels

| Level | Selects | Kind |
|---|---|---|
| `L0` | every project under the lockfile's directory (`module` scope, `lock_changed`) | safe default for unsupported formats |
| `L1` (default) | the projects whose **resolved dependency closure** changed, and the tests that import their files | sound narrowing, verified against the package managers |
| `L2` (opt-in) | inside such a project, only the tests that import a **direct dependency** whose closure changed | trusted narrowing with guards |

The widening always carries the ecosystem (`eco`), so a lockfile at the repository root never selects another
ecosystem's tests.

## Supported formats

| Lockfile | Ecosystem | Highest level | Notes |
|---|---|---|---|
| `pnpm-lock.yaml`, lockfile 6.x and 9.0 | Node | L2 (6.x) / L1 (9.0) | 9.0 does not record install scripts, so L2 always falls back to L1 |
| `package-lock.json`, `lockfileVersion` 2 and 3 | Node | L2 | the `packages` section is read; version 1 stays at L0 |
| `Cargo.lock`, version 3 and 4 | Rust | L1 | no L2: units are whole Cargo targets |
| `npm-shrinkwrap.json`, `yarn.lock`, `bun.lock`, `bun.lockb` | Node | L0 | |
| `poetry.lock`, `uv.lock`, `pdm.lock`, `Pipfile.lock`, `pylock.toml` / `pylock.*.toml`, requirements and constraints files (`requirements*.txt` / `.in`, `constraints*.txt` / `.in`, `*-requirements.txt`, `*_requirements.txt`, any `.txt` / `.in` in a `requirements/` or `reqs/` directory) | Python | L0 | scope: the file's Python project |
| `go.sum` | Go | L0 | scope: the module; `vendor/` changes widen their module too |
| `packages.lock.json` | .NET | L0 | scope: the lockfile's directory |

Lockfiles not listed here are not claimed by any ecosystem and fall under the general rules. JVM builds have no
lockfile handling: `gradle.lockfile` and `gradle/verification-metadata.xml` are ordinary files of the project that owns
them (see [JVM](ecosystems/jvm.md)).

## Level L1

Both sides of the lockfile are parsed into a dependency graph with one node per installed package and one node per
project (workspace member). The **closure** of a project is everything reachable from its node: direct and transitive
dependencies, through workspace links and path dependencies, including dev, build, optional and target-specific ones
(Cargo records the union of every kind, target and feature). Two closures are equal when the same packages, compared by
their full lockfile entry (version, integrity, source, resolution...), are reached through the same edges.

Effect of a changed closure:

- **Node.** The project's tests are selected (`module` widening), and every file of the project counts as changed, so
  the tests of other projects that import its files are selected through the graph. A project that is not itself in the
  lockfile belongs to the nearest lockfile project containing it.
- **Node, root project.** The lockfile's root project holds the workspace tooling (runner, transpiler, plugins): when
  its closure changes, every project under the lockfile's directory is selected.
- **Node, undeclared imports.** An import of a package that the importing project does not declare resolves to whatever
  is hoisted: if a package with that name changed, the importing project is selected.
- **Rust.** Each package governed by the `Cargo.lock` (the nearest one in its directory or above) is compared on its own
  closure. A workspace member's closure contains the members it depends on, so a dependency change of one member also
  selects the members that depend on it. Build dependencies and proc-macros need no special rule: they are in the
  closure of every member that uses them. A package at the repository root is scoped to the root, which selects every
  Rust unit.

An unchanged closure selects nothing: a lockfile edit that only touches other projects costs nothing.

### Fallbacks to L0

| Situation | Why |
|---|---|
| one side is absent, unreadable or does not parse | nothing to compare |
| the format or the format version changed | closures are not comparable |
| a setting outside the package graph changed (pnpm overrides, patches, resolution settings; Cargo top-level keys other than packages) | it can change resolution in ways the graph does not show |
| a Cargo package matches several lockfile entries | ambiguous |
| `[lockfile] level = "L0"` | configured |

The detail of the widening says `lockfile at level L0 (<why>)`.

## Level L2 (opt-in)

At L2, a Node import of an external package becomes an edge to a virtual node `<npm:package>`. For a project whose
closure changed, only the tests that (transitively) import a **direct dependency whose own closure changed** are
selected; a transitive change under an imported direct dependency narrows to that dependency's importers. Such a
narrowed `lock_changed` widening has tier `graph`. Jest root mocks keep the edge to the real package
(`jest.requireActual`).

L2 falls back to L1 for the project when any of these holds:

- the project was added to or removed from the lockfile, or no direct dependency carries the change;
- a changed direct dependency is a local or workspace package, or a direct dependency was added or removed;
- a package that changed anywhere in the project's closure runs install scripts, or the format does not record install
  scripts (pnpm 9.0);
- a package that changed anywhere in the project's closure has peer dependencies;
- a changed package installs executables named in a `package.json` script or a runner config, or whose names the
  lockfile does not record;
- the dependency is a runner, transpiler, bundler, plugin, preset or type package (`@types/*`, `jest`, `vitest`,
  `@babel/*`, `typescript`, `*-plugin-*`, `*-loader`...), which is loaded without an import;
- the package is named as a string in a runner config or in a non-dependency key of `package.json` (presets, plugins,
  environments, setup files, CLI scripts);
- no file of the project imports the dependency (transitive, plugin or tool use);
- the project has an undeclared import of a changed package.

The widening detail says `L2 not applicable: <why>` when it falls back.

## How the levels were validated

L1 is enabled for a format only after a differential check against the package manager itself: for every sampled
lockfile commit of real repositories, the closure bgraph computes for each project is compared with the one the package
manager reports. See [Evaluation](evaluation.md#lockfile-differential-check) for the numbers.
