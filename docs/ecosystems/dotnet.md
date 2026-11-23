# .NET

bgraph selects .NET tests at MSBuild-project granularity: every `.csproj`, `.fsproj` and `.vbproj` is a unit, run with
`dotnet test`. Inside a project the file and namespace graph is not a sound dependency relation (partial classes, source
generators, globbed items, reflection), so file-level information only decides which projects own a file. Project files and
the props/targets that apply to them are read and evaluated as text; MSBuild is never run during analysis.

## At a glance

| | |
|---|---|
| Granularity | One unit per MSBuild project (`.csproj`, `.fsproj`, `.vbproj`), test project or not |
| Unit ID format | `dotnet:<project file path>`: `dotnet:src/App/App.csproj`, `dotnet:tests/Core.Tests/Core.Tests.fsproj` |
| Runners | `dotnet` (`dotnet test`, VSTest or Microsoft.Testing.Platform) |
| Manifests/configs read | Project files, `Directory.Build.props/targets`, `Directory.Packages.props`, every `<Import>`ed file (shared-project `.projitems` included), `.editorconfig`, `.globalconfig`, `Directory.Build.rsp`, `MSBuild.rsp`, `global.json` (runner and Arcade SDK detection), `.sln`/`.slnx` locations (for `$(SolutionDir)`) |
| Lockfiles | `packages.lock.json` at level L0 (closure not compared, see [lockfiles](../lockfiles.md)) |
| Dynamic layer | Per-test-class narrowing of VSTest test projects from code coverage, off by default ([dynamic layer §9.11](../dynamic-layer.md#911-net)) |

## Supported layouts

- **Any directory layout.** Every `.csproj`/`.fsproj`/`.vbproj` in the tree is a unit, except project files under an `obj/` or
  `node_modules/` directory. Solutions are not needed and not read for membership.
- **Nested projects.** A file belongs to **every** ancestor project whose directory contains it (the SDK default globs include
  subdirectories), so a change below a nested project selects the nested project and its enclosing projects; a file owned only
  by the outer project does not select the nested one.
- **Several projects in one directory** each own the directory's files.
- **Shared projects** (`.shproj` + `.projitems`): the importing project depends on the `.projitems` and on the files it lists,
  wherever they live.
- **Arcade SDK repositories**: when the nearest `global.json` names `Microsoft.DotNet.Arcade.Sdk` in `msbuild-sdks`, `$(RepoRoot)`
  is the `global.json` directory and `$(RepositoryEngineeringDir)` / `$(RepositoryEngineeringCommonDir)` its `eng/` and
  `eng/common/`; Arcade output directories (`$(ArtifactsDir)` and the like) are build output.
- **Mixed repositories**: a .NET change selects no Node/Python/JVM test, and the other way round.

**File ownership.** A project owns every file under its directory except: `bin/` and `obj/` at the top of the project directory,
other project files (`.csproj`, `.fsproj`, `.vbproj`, `.vcxproj`), `.sln`/`.slnx`, `*.user`, `*.vssscc`, documentation (`*.md`,
`*.rst`, ...) and harmless files (`LICENSE`, `.gitignore`, ...).

## Test units

- One unit per project file. Test projects are **not** told apart from libraries: the test framework reference can come from a
  props file, and `dotnet test` skips a project that is not a test project.
- Units carry `Eco = dotnet`, `Runner = dotnet`, `Project` = the project directory, `Package` = the project file path, and
  `Config` = the `global.json` that selects the Microsoft.Testing.Platform runner (empty for VSTest).
- ID: `dotnet:<project file path>`, repo-relative with forward slashes. The unit's graph node is `<dotnet:<project file path>>`;
  a `ProjectReference` points at that same node, so selection is reachability of the node.

## What a unit depends on

- **Its project file and every file it owns** (see above).
- **Items**: the file named by the `Include` of every item, wherever it lives: always for file item types (`Compile`, `Compile Link`,
  `None`, `Content`, `EmbeddedResource`, `AdditionalFiles`, `Analyzer`, `Reference`, `Page`, `Resource`, `EditorConfigFiles`, ...,
  even when the file does not exist yet), and for any other item type when the value contains a path separator or a wildcard or
  names an existing file; plus `HintPath` files. A wildcard is a pattern edge, so a file added later under the glob selects the project. Item types
  whose `Include` is a name (`PackageReference`, `PackageVersion`, `FrameworkReference`, `Using`, `InternalsVisibleTo`,
  `ProjectCapability`, `AssemblyAttribute`, ...) are skipped, and so are `Include`s that reference other items (`@(...)`,
  `%(...)`). An item inside a `<Target>` that cannot be evaluated is computed at build time and ignored.
- **`ProjectReference`**: the referenced project's node, including `OutputItemType="Analyzer"` and
  `ReferenceOutputAssembly="false"`; a wildcard reference links every matching project. A reference to a project type bgraph does
  not model (`.vcxproj`, `.wapproj`, `.proj`, ...) depends on that file and on everything below its directory. A reference to a
  file that does not exist depends on that path (picked up when it is added).
- **Implicit imports**: the nearest `Directory.Build.props`, `Directory.Build.targets` and `Directory.Packages.props` at or above
  the project directory. The nearest wins; the chain continues upward only when the file imports its parent
  (`$([MSBuild]::GetPathOfFileAbove(...))`), which is an ordinary import. A change to such a file therefore selects exactly the
  projects below it.
- **Explicit imports**: every file named by `<Import Project="...">`, transitively, including wildcard imports. A conditional
  import of a file that does not exist yet is still an edge. `Sdk="..."` imports are SDK files, not repository files.
- **Analyzer configuration**: every `.editorconfig`, `.globalconfig`, `Directory.Build.rsp` and `MSBuild.rsp` in the project
  directory or above (they can turn diagnostics into build errors).
- **Strings read by tasks**: a task can read a file no item declares (`Copy SourceFiles`, `Exec Command`,
  `ReadLinesFromFile File`). Every attribute value, element text and property value of the project and of the files it imports
  (inside `<Target>` too) is evaluated and split into words (on `;` and whitespace, quotes stripped; for a word with `=` such as
  `-File=x`, the text after the last `=` is tried too), resolved against the project directory and the defining file's directory; a word
  that names an existing file is a dependency, an existing directory named with a separator becomes `dir/**`, and a wildcard path a
  pattern edge.

**Property evaluation.** Properties are evaluated per project over the union of every definition in the project and its import
closure (every value, since conditions are not evaluated; at most 64 combinations):

- `$(MSBuildThisFileDirectory)`, `$(MSBuildThisFile*)`, `$(MSBuildProjectDirectory)`, `$(MSBuildProject*)`;
- `$(SolutionDir)` when a `.sln`/`.slnx` sits in the project directory or an ancestor;
- property functions `[MSBuild]::GetPathOfFileAbove`, `GetDirectoryNameOfFileAbove`, `NormalizePath`, `NormalizeDirectory`,
  `EnsureTrailingSlash`, `ValueOrDefault`, `GetTargetFramework*`, `[System.IO.Path]::Combine`, `GetFullPath`, `GetDirectoryName`;
- when the repository does not define them: `$(Configuration)`, `$(Platform)`, `$(TargetFramework)`, `$(RuntimeIdentifier)` and
  similar become `*` (every value); build-output properties (`$(OutputPath)`, `$(IntermediateOutputPath)`, `$(ArtifactsPath)`, ...)
  drop the item; toolchain locations (`$(MSBuildExtensionsPath)`, `$(MSBuildSDKsPath)`, `$(VSToolsPath)`, ...) are ignored. A
  definition found in the project or its imports is used instead.

**Conditions are never evaluated.** Every conditional `ProjectReference`, `Import` and item counts as present (the union of all
branches, e.g. `TargetFramework`-specific references). Under a `Condition`, a property defined nowhere in the repository (SDK,
command line, environment) is lenient: preceded by text of its own path segment (`eng/Build.$(Kind).targets`) it is `*`; starting a
segment and glued to a name after it, it is any directories (`**/*`): `eng/$(ProjectType).targets` matches `eng/**/*.targets`, and
`$(EngDir)contract.targets` at the start of the path matches `*contract.targets` in any directory.

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| `global.json` (any directory) | `full`, `global_input` (global table, see [architecture](../architecture.md)) |
| `nuget.config` (any case) | `global_input`, module tier, every .NET unit (no other ecosystem's) |
| `.config/dotnet-tools.json` | `global_input`, every .NET unit |
| `Directory.Solution.props`, `Directory.Solution.targets` | `global_input`, every .NET unit |
| `*.runsettings` | `test_config_changed`, every .NET unit (a props file anywhere can select it) |
| `packages.lock.json` | `lock_changed`, module tier, the .NET units under its directory |
| `.sln`, `.slnx`, `.slnf` | Nothing: the projects they list bring their own files |
| `.shproj` | Nothing (the `.projitems` carries the content) |
| `.csproj`, `.fsproj`, `.vbproj` | No widening: graph node, the project and its dependents. An added project is selected; a deleted one selects the projects that referenced it |
| `Directory.Build.props`, `Directory.Build.targets`, `Directory.Packages.props` | No widening: the projects below it (and the projects below a child file that imports it) |
| Imported `.props`/`.targets`/`.projitems`, `.editorconfig`, `.globalconfig`, `Directory.Build.rsp`, `MSBuild.rsp`, files named by items or task strings | No widening: graph node, the projects that depend on it |
| A file under a project directory with no other role (data, scripts) | The owning projects |
| A file outside every project that nothing references | General rule: `unknown_file`, `full` |
| `obj/` / `bin/` content of a project | Not owned: `unknown_file` scoped to the enclosing project |

## Unresolved and uncertain constructs

Each of the following is recorded as an unresolved construct (`unknown_import`) **seeded at the project file** that contains it,
or that imports the props/targets file containing it. It fires on any change in the repository: the project and its dependents
are selected, and the widening is reported. Projects without the construct are not affected.

- an `<Import>` expression that cannot be evaluated (an undefined property — under a `Condition` only one that is a whole path
  segment —, an unsupported property function, an item list `@(...)` or metadata `%(...)` reference), or that names a file
  outside the repository; without a `Condition`, one that evaluates to an empty path;
- an `<Import>` or `ProjectReference` without a `Condition` that names a file that does not exist;
- an item `Include` (outside a `<Target>`) that cannot be evaluated under the same rules, or names a path outside the repository
  (`HintPath` excepted);
- a `ProjectReference` whose whole path segment is an undefined property (`$(SomeProject)`), even under a `Condition`: it may name
  any project;
- an MSBuild file that is not well-formed XML (seeded at that file: every project that imports it).

Strings read by tasks that cannot be evaluated are **ignored**, not unresolved (see below).

## Running the selection

`bgraph command` emits one `dotnet test` per selected project, from the repository root, sorted by unit ID (`dotnet test` accepts a
single project). When the nearest `global.json` at or above the project names `"runner": "Microsoft.Testing.Platform"`, the
project is passed with `--project` (the positional form is VSTest-only in that mode).

```sh
# scope: graph
# dotnet: units are MSBuild projects, one `dotnet test` per project (it accepts a single project); a selected project that is not a test project is skipped by dotnet test
dotnet test src/App/App.csproj
dotnet test tests/App.Tests/App.Tests.csproj
```

With Microsoft.Testing.Platform:

```sh
# dotnet: global.json selects Microsoft.Testing.Platform (.NET 10 SDK), which takes `--project <file>` once per invocation
dotnet test --project tests/App.Tests/App.Tests.csproj
```

No list file, environment variable or batching is used. Scope `none` emits no command; `full` emits no selective command.

## Known gaps and over-selection

- Strings read by tasks that cannot be evaluated (undefined property, item list or metadata reference, unsupported property
  function) are ignored; a path assembled at build time by an inline task or a script, and test data reached through absolute or
  environment-derived paths, are not seen.
- Item `Include`s built from other items (`@(...)`, `%(...)`) are skipped.
- Conditions are not evaluated: references and imports of every configuration, platform and target framework count (over-selection).
- A property supplied only on the command line or by the environment and used in a `ProjectReference` or `Import` (outside the
  lenient `Condition` cases above) makes its project (and its dependents) selected on every change; repositories with many such constructs stay broadly selected.
- Granularity is the project, and a file belongs to every enclosing project: a change below a nested project also selects the
  outer ones.
- Every project is a unit, including libraries; `dotnet test` skips those that are not test projects.
- `packages.lock.json` is L0: any change selects every .NET project under its directory; the resolved closure is not compared.
- A `Directory.Packages.props` change selects every project below it, whichever package version changed.
- Project types other than `.csproj`/`.fsproj`/`.vbproj` (`.vcxproj`, `.wapproj`, traversal `.proj`) are not units.
- One `dotnet test` invocation per project: no shared build across projects.
- Without the dynamic layer (off by default) selected projects run all their tests; with it, only VSTest projects are narrowed
  to classes, and only for changes inside method bodies of C# files ([§9.11](../dynamic-layer.md#911-net)).

## See also

- [Architecture](../architecture.md): tiers, widening reasons, unresolved imports, the global table
- [Configuration](../configuration.md): `bgraph.toml` rules
- [Lockfiles](../lockfiles.md)
- [CLI](../cli.md): `bgraph affected`, `explain`, `command`
- [Dynamic layer](../dynamic-layer.md)
- [JVM (Maven and Gradle)](jvm.md)
