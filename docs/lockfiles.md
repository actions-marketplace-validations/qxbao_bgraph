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
| `uv.lock`, version 1 (revisions up to 5) | Python | L1 | no L2 |
| `poetry.lock`, lock-version 1.0, 1.1, 2.0, 2.1 | Python | L1 | no L2; one project per lockfile |
| `yarn.lock`, yarn classic (v1) | Node | L1 | the workspaces and their direct dependencies are read from the `package.json` files |
| `yarn.lock`, yarn 2 and later (`__metadata` version 4 to 6 and 8 to 10) | Node | L1 | other `__metadata` versions stay at L0 |
| `bun.lock` (text lockfile, bun 1.2 and later; `lockfileVersion` 1) | Node | L1 | other versions stay at L0 |
| `npm-shrinkwrap.json`, `bun.lockb` (binary) | Node | L0 | |
| `pdm.lock`, `Pipfile.lock`, `pylock.toml` / `pylock.*.toml`, requirements and constraints files (`requirements*.txt` / `.in`, `constraints*.txt` / `.in`, `*-requirements.txt`, `*_requirements.txt`, any `.txt` / `.in` in a `requirements/` or `reqs/` directory) | Python | L0 | scope: the file's Python project |
| `go.sum` | Go | L1 (lines only added) / L0 | lines only added change no build: nothing widens; a removed or changed line widens the module (and the other members of its `go.work`); `vendor/` changes widen their module too |
| `packages.lock.json` | .NET | L0 | scope: the lockfile's directory |
| `composer.lock` | PHP | L0 | exact Composer project and its in-repo dependents |

Lockfiles not listed here are not claimed by any ecosystem and fall under the general rules. JVM builds have no
lockfile handling: `gradle.lockfile` and `gradle/verification-metadata.xml` are ordinary files of the project that owns
them (see [JVM](ecosystems/jvm.md)).

## Level L1

Both sides of the lockfile are parsed into a dependency graph with one node per installed package and one node per
project (workspace member). The **closure** of a project is everything reachable from its node: direct and transitive
dependencies, through workspace links and path dependencies, including dev, build, optional and target-specific ones
(Cargo records the union of every kind, target and feature; for uv and poetry every extra and dependency group is
included and markers are not evaluated). Two closures are equal when the same packages, compared by their full lockfile
entry (version, integrity, source, resolution...), are reached through the same edges.

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
- **Python, uv.** The projects are the workspace members and path dependencies inside the repository, keyed by
  directory, and the lockfile's directory: the root package, or for a virtual workspace root (no `[project]`) the
  root's dependency groups and requirements recorded in `[manifest]`. An edge keeps its kind (dependency, extra, group)
  and its marker, so moving a dependency between groups or changing its marker is a closure change. A reference that
  matches several packages of the same name is ambiguous (L0). Package content excludes the artifacts' `upload-time`
  and `size` (the hash identifies the artifact) and a local package's `metadata` (specifiers, whose effect is in the
  edges).
- **Python, poetry.** The lockfile resolves one project, its directory, and every locked package is in its closure.
  Dependencies are recorded by name: a name shared by several entries (one per marker range) has an edge to each.
- **Python, effect.** A changed project selects its tests and seeds its files, as for Node; the lockfile's root project
  carries dependency groups uv installs with every member, so its change selects every project under the lockfile's
  directory. The environment is shared by every project of the lockfile, so when packages changed, a changed package
  that may be a pytest plugin (or that such a package depends on) selects every project, and a project with an import
  that no package of its closure (or of the root's) provides by name is selected. See
  [Python](ecosystems/python.md#lockfiles-at-level-l1).

An unchanged closure selects nothing: a lockfile edit that only touches other projects costs nothing.

### How yarn and bun lockfiles are resolved

Each format records resolution differently; bgraph follows the package manager's own rules and stays at L0 when a
dependency does not resolve to exactly one entry.

- **yarn classic (v1).** The lockfile maps `name@range` patterns to entries and records no workspace. The projects are
  the root `package.json` next to the lockfile and the workspaces its `workspaces` globs match; their dependencies
  (`dependencies`, `devDependencies`, `optionalDependencies`, and `peerDependencies` when they resolve) come from their
  manifests, so both sides of the comparison read the manifests of their own tree. As yarn does, a dependency whose name
  is a workspace's and whose range accepts the workspace's version links to that workspace; anything else resolves to the
  entry whose patterns contain `name@range`.
- **yarn 2 and later (Berry).** Workspaces are the `name@workspace:path` entries. A dependency resolves through the
  descriptor index of the lockfile keys, after yarn's own substitutions, which the lockfile does not record: the root
  manifest's `resolutions`, `catalog:` ranges from `.yarnrc.yml`, the dependencies that `packageExtensions` in
  `.yarnrc.yml` and yarn's built-in package extensions add to a package, and ranges bound to their parent (`link:`,
  `portal:`, relative `patch:` paths). A `workspace:` range resolves to the workspace of that name even when it is not
  a key yet. Peer dependencies are not edges: yarn resolves them from the dependent, which is in the same closure. A
  `patch:` entry and the entry it patches are in each other's closure, so a patch
  change (including yarn's built-in compatibility patches for `typescript`, `fsevents` and `resolve`) is a change of
  the patched package. Plug'n'Play does not change how the lockfile is read.
- **bun.** `packages` maps an install path to a package: `a`, or `a/b` for the copy of `b` that `a` resolves instead of
  the hoisted one. A dependency of the package at path `p` resolves to `p/<name>`, then to each shorter parent path, then
  to `<name>`; a workspace's dependencies start at the workspace's package name and include every name its
  `package.json` declares (the lockfile drops some, such as a `devDependency` that is also a peer). A peer dependency also reaches every
  copy of the package whose version the peer range accepts, because bun's isolated linker does not resolve peers
  through the install path. Overrides and catalogs only steer
  resolution, so they are compared through the closures; patches (`patchedDependencies`) are compared like pnpm's.

yarn and bun lockfiles do not record which packages run install scripts, so L2 always falls back to L1 for these
formats.

### Fallbacks to L0

| Situation | Why |
|---|---|
| one side is absent, unreadable or does not parse | nothing to compare |
| the format or the format version changed | closures are not comparable |
| a top-level key outside the package graph changed: pnpm `settings` (`autoInstallPeers`, `excludeLinksFromLockfile`, `injectWorkspacePackages`...), `packageExtensionsChecksum`, `pnpmfileChecksum`, `ignoredOptionalDependencies`, build allow/deny lists, the env lockfile document (the package manager's pinned version); npm keys other than `packages`; Cargo keys other than `package`; uv `conflicts` and unknown keys; poetry top-level `[extras]` and unknown keys | it can change resolution or installation in ways the graph does not show |
| a changed pnpm patch whose selector is a version range, or that selects no package of either side | the patched packages cannot be identified |
| a Cargo package matches several lockfile entries; a uv dependency reference matches several packages | ambiguous |
| uv `revision` or poetry `lock-version` changed, or is newer than bgraph knows | the format version changed |
| a yarn or bun dependency resolves to no entry (or, yarn, to several): a stale lockfile, a range or workspace glob bgraph cannot read, a resolution keyed by a parent's range | the closure would be incomplete |
| yarn Berry `__metadata` (lockfile version or `cacheKey`) changed; bun keys outside `workspaces`, `packages`, `overrides`, catalogs and `patchedDependencies` (`trustedDependencies`, `configVersion`...) | installation may change in ways the graph does not show |
| `bun.lockb` | binary format, not read |
| `[lockfile] level = "L0"` | configured |

The detail of the widening says `lockfile at level L0 (<why>)`; for a changed top-level key it names the keys.

pnpm keys outside the importers and packages that do **not** force L0:

| Key | Handling |
|---|---|
| `catalogs` | a catalog version is the version every importer that uses it references: compared through the closures |
| `overrides` | an override only steers resolution: an overridden version is the version an importer or a package entry references, so the closures show every effect; an override that matches nothing installed changes nothing |
| `patchedDependencies` | a patch whose entry (hash or path) changed, or that was added or removed, marks the packages it selects as changed (`name@version`: that version; `name`: every version), so every project whose closure contains one of them is selected |

uv and poetry keys outside the packages that do **not** force L0:

| Key | Handling |
|---|---|
| uv `[options]` (`exclude-newer`, resolution and prerelease modes), `[manifest]` (members, constraints, overrides) | they only steer resolution or list the members: the resolved graph shows every effect |
| uv `requires-python`, `resolution-markers`, `supported-markers`, `required-markers` | they bound the environments: their effect is in the package versions, per-package resolution markers and edge markers |
| poetry `[metadata]` `content-hash` | a hash of the `pyproject.toml` dependency sections: with the same packages, nothing installed changes |
| poetry `[metadata]` `python-versions` | the project's Python range: its effect is in the locked packages and their markers |

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
  scripts (pnpm 9.0, yarn, bun);
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
