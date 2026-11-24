# Dart and Flutter

bgraph selects affected `dart test` and `flutter test` tests one test file at a time. Dart imports are static URIs and
every test file is compiled and run as its own entry point, so a test depends on the files its imports reach. bgraph
reads `pubspec.yaml` and `dart_test.yaml` as YAML and scans every `.dart` file with its own lexer; it never runs the Dart
or Flutter toolchain.

## At a glance

| | |
|---|---|
| Granularity | One test file per unit; one whole-package unit when `dart_test.yaml` is not literal |
| Unit IDs | Repo-relative path of the test file, e.g. `packages/core/test/format_test.dart`; `<dart-all-tests:DIR>` for a whole-package unit, e.g. `<dart-all-tests:.>` |
| Runners | `dart test` (package:test); `flutter test` for packages that depend on the Flutter SDK |
| Manifests and configuration | `pubspec.yaml`, `pubspec_overrides.yaml`, `dart_test.yaml`, `build.yaml`, `l10n.yaml`, `melos.yaml`, `analysis_options.yaml` |
| Lockfile | `pubspec.lock` at [L0](../lockfiles.md) |
| Dynamic layer | None |

## Supported layouts

- **Packages.** Every directory with a `pubspec.yaml` is a package, at the repository root or below it. Directories with
  a hidden path segment (`.dart_tool/`, `ios/.symlinks/`, `.plugin_symlinks/`, `.fvm/`) are not searched. A file belongs
  to the nearest package above it, so an `example/` or a fixture package with its own `pubspec.yaml` is a package of
  its own.
- **Package names.** `package:<name>/...` resolves to `<dir>/lib/...` for every directory whose `pubspec.yaml` declares
  that name. Two directories with the same name are both dependencies.
- **Pub workspaces.** A root `pubspec.yaml` with `workspace:` (paths, or globs since Dart 3.11) and members with
  `resolution: workspace` share the root's `pubspec.lock` and one version solve. Nested workspaces are followed.
- **Melos.** `melos.yaml` (or a `melos:` key in the root `pubspec.yaml`) links the repository's packages to each other;
  imports between them resolve through the package names above.
- **Flutter.** A package that depends on an SDK package of the Flutter SDK (`sdk: flutter` under `dependencies`,
  `dev_dependencies` or `dependency_overrides`: `flutter`, `flutter_test`, `integration_test`...) runs with `flutter test`.
  Its platform directories (`android/`, `ios/`, `macos/`, `linux/`, `windows/`, `web/`) are not Dart code: their files
  follow the general rules (another ecosystem's project, such as a Gradle build under `android/`, or the package).
- **Mixed repositories.** A Dart package next to (or at the same root as) a Node, Python or other project is supported.
  Dart manifest, configuration and lockfile widenings select Dart units only.
- A repository with no `pubspec.yaml` costs nothing: its `.dart` files, if any, follow the general rules.

## Test units

A `.dart` file of a package is a test unit when it is

1. under `test/` with a name matching `*_test.dart` (what `dart test` and `flutter test` run with no argument), or
2. under one of the top-level `paths:` of the package's `dart_test.yaml` with a name matching its top-level
   `filename:` (the union of both rules is used, because `flutter test` does not read `paths`), or
3. in a Flutter package, under `integration_test/` with a name matching `*_test.dart`.

**Whole-package unit.** When `dart_test.yaml` does not parse, includes another file (`include:`), or sets `paths` or
`filename` anywhere below the top level (in `presets`, `tags`, `on_os`, `on_platform`...), the files of rules 1 and 2
are not units: the package gets one unit `<dart-all-tests:DIR>` that runs its whole suite. It is selected whenever
anything is selected or widened. Integration tests stay file units.

## What a unit depends on

- **Directives.** `import`, `export`, `part` and `import augment` of a test file and, transitively, of every file they
  reach. Every URI of a conditional directive counts (`import 'stub.dart' if (dart.library.io) 'io.dart'` depends on
  both), and a deferred import is an ordinary import. `dart:` URIs and `package:` URIs of a name that no package in the
  repository declares are external (the lockfile and manifests cover them). `part of` adds nothing: the library already
  depends on its part.
- **`flutter_test_config.dart`** in the test file's directory and in every ancestor directory up to the package root.
- **Native-assets hooks.** Every `.dart` file of a package with `hook/build.dart` or `hook/link.dart` depends on them, so
  a hook change reaches the package's tests and every test that imports the package.
- **String literals that name files.** In every file (tests and the helpers they import), a literal that names an
  existing file or directory relative to the file's directory, the package root (the working directory of `dart test`)
  or the repository root is a dependency; a directory stands for every file below it, including files added later.
  This covers golden files (`matchesGoldenFile('goldens/button.png')`), fixtures (`File('test/fixtures/user.json')`),
  programs a test runs (`Process.run('dart', ['bin/tool.dart'])`, which then also depends on what the program imports),
  and `package:` URIs written in strings. Adjacent literals are joined, and the literal leading arguments of a `join(...)`
  call (`p.join('test', 'data', name)`) count as one path. An interpolated literal depends on its static directory
  prefix (`'goldens/$name.png'` → `goldens/*.png`) or, when only its tail holds a directory, on any path with that tail
  (`'$root/fixtures/a.json'` → `**/fixtures/a.json`). A bare word, `./name` and paths that clean to `.` or `..` are not
  dependencies.
- **Process launches.** A file that calls `Process.run`, `Process.runSync`, `Process.start` or `TestProcess.start`
  depends on its package's `bin/`.
- **Isolates.** `Isolate.spawnUri` (and `loadUri` of `dart:mirrors`) with a literal URI depends on the spawned file.

## Changes to manifests, configuration and lockfiles

The comparison is structural: both versions are parsed, so comments and formatting never widen.

| Change | Effect |
|---|---|
| `pubspec.yaml`, only `description`, `homepage`, `repository`, `issue_tracker`, `documentation`, `topics`, `funding`, `screenshots`, `false_secrets`, `platforms` or `publish_to` | none (a test that reads `pubspec.yaml` by a literal still depends on it) |
| `pubspec.yaml`, any other key (`dependencies`, `dev_dependencies`, `dependency_overrides`, `environment`, `resolution`, `workspace`, `executables`, `flutter:`...), a package added or removed, or a file that does not parse | `module`, `manifest_changed`: the package, the packages that share its resolution (a workspace root and all its members), and every in-repository package that depends on it by path or by name, transitively |
| `pubspec_overrides.yaml` | `module`, `manifest_changed`: as above |
| `pubspec.lock` | `module`, `lock_changed` at L0: the package and, at a workspace root, every member |
| `dart_test.yaml`, `l10n.yaml` at a package root | `module`, `test_config_changed`: the package (units are derived again) |
| `build.yaml` or `build.<name>.yaml` at a package root | `module`, `test_config_changed`: the package and its in-repository dependents (builders can apply to dependents) |
| `melos.yaml` | `module`, `test_config_changed`: every Dart package below it |
| `analysis_options.yaml`, at any depth | none: only the analyzer reads it, never the compiler or the test runners |
| `.fvmrc`, `.fvm/fvm_config.json` | `full`, `global_input` (`toolchain_pin`, see [Architecture](../architecture.md#global-inputs-and-harmless-files)) |

Every Dart widening applies to Dart units only. It names packages exactly (a package nested in a widened one is widened
only when it is named itself), except for `melos.yaml` and for a `pubspec.yaml` that does not parse on either side of
the change, which widen every package below their directory.

## Unresolved and uncertain imports

| Situation | Effect |
|---|---|
| A directive or `package:` target that is not in the tree, but whose package is | edge to the missing path (adding the file later is caught) and `unknown_import` at module tier: any change in the importer's package or the target's package selects the importer's dependents |
| The missing file looks generated (`*.g.dart`, `*.freezed.dart`, `*.mocks.dart`, `*.gr.dart`, `*.config.dart`, `*.pb*.dart`...) or is in `package:flutter_gen/` | the same, with detail `generated_output`; a [`[[generated]]`](../configuration.md#generated) entry whose `outputs` match it replaces the widening by its `sources` |
| A URI that leaves the repository, is absolute, or has another scheme | `unknown_import` at module tier for the importer's package |
| `Isolate.spawnUri` / `loadUri` with a computed URI, or a tear-off | `dynamic_import` at module tier: a changed `.dart` file of the package selects the file's dependents (a test file no reference reaches does not count) |
| An unterminated string, comment or interpolation, or a directive with an interpolated URI | `uncertain_scan`: any change in the repository selects the file's dependents |
| An integration test of a Flutter app with platform directories | a change under `android/`, `ios/`, `macos/`, `linux/`, `windows/` or `web/` also selects it (`unknown_import` detail: it runs the host app) |

`bgraph unresolved` lists them.

## Running the selection

`bgraph command` emits one command per package, run from the package directory with paths relative to it: `dart test
<files>` or `flutter test <files>`. Integration tests get a separate `flutter test <files>` command, which needs a device
or an emulator. A whole-package unit, a test file outside its package, or a file list longer than the command-line limit
(8,000 characters by default, below the Windows limit) runs the package's whole suite (`dart test`, `flutter test`, or
`flutter test integration_test`).

For a change to `packages/core/lib/src/format.dart`, imported by a Flutter app's library and by the core package's own
test, bgraph prints:

```sh
# scope: graph
cd app && flutter test test/home_test.dart
cd app && flutter test integration_test/app_test.dart
cd packages/core && dart test test/format_test.dart
```

## Addressing units in bgraph.toml

In a [`[[rule]]`](../configuration.md#rule), use a test file path (the unit ID), a glob over test files
(`packages/core/test/*_test.dart`), a package directory (every unit of that package and of the packages below it), or a
whole-package unit ID such as `<dart-all-tests:packages/core>`. A target that matches no unit makes the selection full.

## Known gaps and over-selection

- `package:` resolution is the union over every package of that name in the repository: a package that depends on a
  published version rather than the local copy still depends on the local copy.
- Every in-repository package that names another package as a dependency, in any section and in any form (path,
  hosted, git), is a dependent for manifest and `build.yaml` widenings. A member's dependency change widens its whole
  workspace, because the workspace shares one resolution.
- Library code that names its own configuration by a literal (a tool that reads `pubspec.yaml` or `dart_test.yaml` from
  the working directory) links that file to every test that imports the tool.
- A file that nothing references by an import or a literal (an asset, an ARB file, a font, a native source) widens to
  its package through the general `unreferenced_file` and `unknown_file` rules. Changes under a Flutter app's platform
  directories follow the general rules too.
- A missing generated file widens its importer to every change of the package until a `[[generated]]` entry describes
  it.
- Paths built at run time by other means than a literal head, a literal tail or a `join(...)` call are not followed; a
  file read that way usually widens through the `unreferenced_file` rule. A process launch is assumed to run its own
  package's `bin/`; a program of another package started by name (`dart run other:tool`) needs a `[[rule]]`.
- A literal is resolved against its own file, package and the repository root only. An asset of another package loaded
  by its key (`AssetImage('assets/x.png', package: 'other')`) is not linked to the test that loads it; a change to it
  widens to its own package only, so such assets need a `[[rule]]`.
- `flutter test` applies only the nearest `flutter_test_config.dart`; bgraph links every ancestor one.
- Flutter `integration_test` commands need a device; their selection is still reported.

## See also

- [Architecture](../architecture.md)
- [Configuration](../configuration.md)
- [Lockfiles](../lockfiles.md)
- [Evaluation](../evaluation.md)
- [package:test configuration](https://github.com/dart-lang/test/blob/master/pkgs/test/doc/configuration.md)
- [Pub workspaces](https://dart.dev/tools/pub/workspaces)
