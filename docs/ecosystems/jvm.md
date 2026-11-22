# JVM (Maven and Gradle)

bgraph selects JVM tests at Maven module / Gradle project granularity. Below the module the class graph is not a sound
dependency relation (reflection, `ServiceLoader`, annotation processors, component scanning, generated sources), so a whole
module is the unit; file-level information only decides which module owns a file. POMs and Gradle scripts are read as text:
Maven and Gradle are never run during analysis.

## At a glance

| | |
|---|---|
| Granularity | One unit per Maven module and per Gradle project (also those without tests) |
| Unit ID format | `maven:<module dir>`: `maven:.` (root module), `maven:core`, `maven:backend/api`. `gradle:<build dir><project path>`: `gradle::` (root project of a build at the repo root), `gradle::core:api`, `gradle:backend:core` (`:core` of the build in `backend/`), `gradle:build-logic:convention` |
| Runners | `maven` (`mvn` / `mvnw`), `gradle` (`gradle` / `gradlew`) |
| Manifests/configs read | `pom.xml` (and local parent POMs of any file name), `.mvn/**`, `mvnw`; `settings.gradle(.kts)`, `build.gradle(.kts)`, scripts applied with `apply from:`, `buildSrc/`, `build-logic/`, included builds, `gradle.properties`, version catalogs (`*.versions.toml`), the Gradle wrapper; string literals of test sources |
| Lockfiles | None. Gradle dependency lockfiles (`gradle.lockfile`) and `gradle/verification-metadata.xml` are ordinary files of the project that owns them (see [lockfiles](../lockfiles.md)) |
| Dynamic layer | Optional per-test-class narrowing inside selected units, off by default (see [Test units](#test-units) and [dynamic layer](../dynamic-layer.md)) |

## Supported layouts

- **Maven reactors.** Every `pom.xml` in the tree is a module, except POMs under `target/` of a module and POMs under `src/` of an
  enclosing module (test resources, invoker ITs, archetype templates), which are files of that module. `<modules>`, Maven 4
  `<subprojects>` and the modules of every profile (union) are followed, including entries that name another `.xml` file.
  A `pom`-packaging POM without `<modules>` still has the POMs of its subdirectories as modules.
- **Several reactors in one repository.** POMs connected by module lists and local parents form a component; a reactor in a
  subdirectory is scoped to its own directory.
- **Gradle builds.** Each `settings.gradle(.kts)` is a build. Projects come from `include(...)` (multi-line lists, with or
  without a leading colon; implied parent projects included), `includeFlat(...)` (sibling of the settings directory), literal
  `project(":x").projectDir = file(...)` / `File(settingsDir, ...)`, and literal renames
  (`project(":x").name = "y"`, `findProject(":x")?.name = "y"`: the path changes, the directory stays). Settings scripts applied
  with a literal `apply from:` are read too (three levels).
- **Standalone Gradle builds.** A `build.gradle(.kts)` outside every project of a settings-based build is a single-project build.
  Scripts under `buildSrc/` and under `src/` of an enclosing project (plugin test fixtures) are not builds.
- **Composite builds.** `includeBuild(...)` in settings: the included build's projects are units of their own (with their own
  build directory in the unit ID). Builds included inside `pluginManagement { }` are plugin builds; the others are library builds.
- **Mixed Maven + Gradle + other ecosystems** in one repository: each unit kind is handled independently; a Node or Python change
  does not select JVM units and vice versa.

**File ownership.** A unit owns every file under its directory except:

- the directories of nested units (a nested module's files belong to it alone);
- build output: Maven `target/`, Gradle `build/` and `.gradle/`;
- `.git/` and `node_modules/`.

Files an **enclosing** unit keeps outside its `src/` (shared configuration, test data, script plugins in `gradle/`) are also
dependencies of every nested unit, because a nested module can read them through `../`. Build scripts of the enclosing unit,
documentation and harmless files (`LICENSE`, `.gitignore`, ...) are not shared. For a Maven module with packaging `pom` (it compiles nothing), its non-code files under `src/`
(checkstyle, license headers, assembly descriptors) are shared too.

## Test units

- One unit per Maven module and per Gradle project. A Gradle project gets a unit when it has a build script or at least one file of
  its own; intermediate projects with no files are not units.
- Units carry `Eco = jvm`, `Runner = maven | gradle`, `Project` = the module directory, `Package` = Maven `groupId:artifactId` or the
  Gradle project path.
- IDs: `maven:<dir>` (`.` for the repo root); `gradle:<build dir><path>` where the build dir is empty for a build at the repo
  root. A build directory never contains `:`, so `gradle:backend:core` is unambiguous.
- Test sources: files under `src/it/` or under a `src/<set>/` whose name contains `test` in any case (`test`, `integrationTest`,
  Kotlin Multiplatform `jvmTest`, `commonTest`, Android `androidTest`), except `src/testFixtures/`; `.java`, `.kt`, `.kts`,
  `.groovy`, `.scala`, `.gradle`, `.clj`. Selection does not depend on this: a unit is selected as a whole.

**Per-class narrowing (dynamic layer).** Optional and off by default (`[dynamic_layer] ecosystems = ["jvm"]` in
[bgraph.toml](../configuration.md), or `--dynamic`). `bgraph record --eco jvm` runs each test class in a fresh JVM with a Java
agent and records the classes and files it loaded; a later selection may then run only the test classes of a selected unit whose
record reaches a changed file (sub-unit IDs `<unit ID>#<class>`, e.g. `maven:core#com.ex.FooTest`). The units are still
built. It applies only when every changed file is a modified `.java`/`.kt` file of a compiled source root (or a documentation
file), no constant changed, and the build does not use Lombok. See [dynamic layer](../dynamic-layer.md).

## What a unit depends on

**Every unit**

- every file it owns (any kind: a resource, SQL file or other non-code file selects the module);
- shared files of enclosing units (see above);
- files and directories named by **string literals of its test sources** (`.java`, `.kt`, `.groovy`, `.scala` under the test source
  sets and `src/testFixtures`) that it does not own, resolved against the module directory (surefire basedir, Gradle's default test
  `workingDir`) and against a Gradle `test { workingDir = rootDir }` or literal directory. Literals in comments are ignored.

**Maven**

- the unit of every reactor module it references by coordinates: `<dependency>` of any scope, type or classifier (`test-jar`),
  BOM imports (`<dependencyManagement>` with `<scope>import</scope>`), `<plugin>` and the plugin's own `<dependencies>`, build
  `<extension>`, any other element with `groupId`/`artifactId` children (`annotationProcessorPaths`, dependency-plugin
  `<artifactItem>`, ...) and `groupId:artifactId[:...]` texts in plugin configuration;
- its **local parent chain**: the parent POM files, and the references they declare (`<dependencies>`, `<build><plugins>`,
  `<extensions>`; not `<pluginManagement>`), interpolated in the child as in the effective POM. The parent is found through
  `relativePath` (any file name; default `../pom.xml`) when its coordinates match, otherwise as the reactor module with the parent's
  coordinates;
- **property interpolation**: `${project.*}`, `${pom.*}`, `${project.parent.*}`, `${revision}` and any `<properties>` of the local
  parent chain; `${project.basedir}`, `${project.parent.basedir}`; `${maven.multiModuleProjectDirectory}`,
  `${session.executionRootDirectory}`, `${project.rootDirectory}` and `${main.basedir}` (directory-maven-plugin) are the reactor
  root;
- files and directories named by paths in its POM chain that it does not own: `<sourceDirectory>`, `<testSourceDirectory>`,
  `<resource>`/`<testResource>` directories, build-helper `<sources>` (dependencies even when absent), and any other element text
  or attribute that looks like a relative path and names an existing file or directory (surefire `suiteXmlFile`, checkstyle
  `configLocation`, antrun `<fileset dir="...">`, assembly descriptors). Inherited paths are resolved against the child. A path
  into an ancestor directory is not an edge (its shared files already reach the module).

**Gradle**

- the unit of every project a script references, whatever the configuration: `project(":x")`, `project(path: ":x", ...)`,
  `findProject`, `evaluationDependsOn`, `rootProject` used as a value, type-safe accessors `projects.x.y` (kebab/snake names
  camel-cased), `testFixtures(...)`, `platform(...)`, `enforcedPlatform(...)`, string task paths such as `":x:jar"`;
- references inside `allprojects { }` / `subprojects { }` / `project(":x") { }` / `configure(...) { }` apply to the projects that
  block configures, and those projects also depend on the configuring script;
- references stored in extra properties (`ext { }`, `extra`) are dependencies of every project of the build;
- references in the project-configuring blocks of a settings script (`gradle.allprojects { }`, ...);
- references in convention plugins of `buildSrc/`, `build-logic/` and included builds are dependencies of every project of the build;
- scripts applied with `apply from:` (relative to the project or root, three levels) and the references inside them;
- **composite builds**: every project of a build depends on every project of the builds it includes; a library build depends on
  every project of the including build and of its sibling library builds (dependency substitution by coordinates is invisible to a
  text read);
- files named by strings starting with `../`, `$rootDir/`, `${rootProject.projectDir}/` and similar, and strings after
  `rootProject` / `rootDir` / `settingsDir` in a call chain (`rootProject.file("data/x")`) that name an existing file or directory.

## Changes to manifests, configuration and lockfiles

"Build scope" below is every JVM unit of the affected reactor/build (module tier, eco `jvm`); when that build is rooted at the
repository root it is `full`.

| Change | Effect |
|---|---|
| A file under any `.mvn/` directory | `global_input`, build scope of the reactor whose POM sits next to `.mvn/` |
| `mvnw`, `mvnw.cmd` | `global_input`, build scope of that reactor |
| `pom.xml` of a leaf module (no modules, no local children) | No widening: the module and its dependents through the graph (edges of both base and head count) |
| Root/shared POM (lists modules or is a local parent): only `<version>` / `<parent><version>` changed, or only comments/formatting | No widening: inheriting modules are reached through their parent edge |
| Root/shared POM: any other change, added or removed, or unreadable on one side | `manifest_changed`; build scope for the top POM of a reactor, the POM's own directory (module tier) for a mid-level parent/aggregator |
| Any POM whose profile declares `<modules>` | `manifest_changed`, scoped as above, for any change to it |
| Remote (non-local) parent coordinates changed, any POM | `manifest_changed`, scoped as above |
| `pom.xml` under `src/` or `target/` (not a module) | Ordinary file of the owning module |
| Settings script of a build, or a script it applies that sits in the settings directory | `manifest_changed`, build scope |
| Script applied by the settings script from a subdirectory (`gradle/includes.gradle`) | No widening: graph node of the project that owns it (shared with nested projects) |
| Build script of the root project | `manifest_changed`, build scope |
| Project build script using `beforeProject`, `afterProject`, `projectsEvaluated`, `projectsLoaded`, or unreadable / not tokenizable | `manifest_changed`, build scope |
| Any other project build script (including one with `allprojects { }` / `subprojects { }`) | No widening: the project, the projects it configures and their dependents through the graph |
| Anything under `buildSrc/` or `build-logic/` | `global_input`, build scope |
| `gradlew`, `gradlew.bat`, `gradle/wrapper/**` | `global_input`, build scope |
| `gradle.properties` (any directory of a build) | `global_input`, build scope |
| Version catalog: `*.versions.toml`, `gradle/*.toml` | `global_input`, build scope |
| Other `*.gradle(.kts)` files (script plugins) | Graph node: the projects that own or apply it |
| `gradle.lockfile`, verification metadata | Graph node: the owning project (a root-level file reaches every nested project) |
| `.java-version`, `.sdkmanrc`, `.tool-versions`, CI files and other entries of the global table | `full`, `global_input` (see [architecture](../architecture.md)) |

The build scope of a Gradle build is the directory containing the settings directory and every project directory (an
`includeFlat` or `projectDir` project may lie outside it). A change that widens an included build also reaches the including
build's projects, which depend on every project of the included build.

## Unresolved and uncertain constructs

| Construct | Effect |
|---|---|
| Gradle settings that hide the project list or layout: `include` / `includeFlat` / `includeBuild` with a non-literal argument (a loop, a variable), `projectDir` assigned without a literal receiver (inside `apply { }`, `with { }`), a rename to a computed name, `setProjectDir` / `setName`, a customized `buildFileName`, `apply from:` a non-literal script, an unreadable settings script | Unresolved (`dynamic_import`) at `full` tier: any change inside the build's scope selects the full suite |
| Two module POMs in one directory | Unresolved (`unknown_import`) at `full`: any change selects the full suite |
| Maven dependency coordinate with a property no POM defines (remote parent, `-D`) | Edges to every module it could name; on any change within the reactor, `unknown_import` is reported and the POM is seeded: the module and its dependents are selected |
| Maven module whose own `groupId`/`artifactId` keeps an undefined property | Any coordinate reference that matches no module may name it: an edge is added |
| Source/resource directory with an undefined property | On any change within the reactor, `unknown_import`, seeded at the POM: the module and its dependents |
| Plugin path with a property no POM defines (`${x}/checkstyle.xml`, `${x}-suppressions.xml`) | Matched anywhere in the repository (`**/checkstyle.xml`, `**/*-suppressions.xml`); external properties (`settings.*`, `env.*`, `user.*`, `java.*`, `os.*`, `maven.*`, `session.*`, build output) are ignored |
| Unreadable POM | The module depends on every other module |
| Unreadable file at a parent's `relativePath` | The module depends on that file |
| Gradle reference that cannot be placed: `project(":$name")`, `apply from:` a non-literal script, `allprojects` / `subprojects` / `childProjects` navigated as a collection, an accessor or literal `project(..)` that names no known project | The project depends on every project of its build (no widening) |
| Gradle script that cannot be read or tokenized | Its project depends on every project of the build; a change to the script widens to the build scope |
| File outside every module and project | General rule: `unknown_file`, `full` (see [architecture](../architecture.md)) |
| File under a project's `build/` output | Not owned: `unknown_file` scoped to the enclosing project |

## Running the selection

`bgraph command` groups the selected units by build (reactor root POM or settings script, wrapper, runner) and runs each command
in that build's directory. The wrapper nearest to the build directory (in it or an ancestor) is used when the repository has one
(`./mvnw`, `../gradlew`; `mvnw.cmd` / `gradlew.bat` with backslashes on Windows), otherwise `mvn` / `gradle` from `PATH`.

**Maven** (two steps):

```sh
# scope: graph
# maven: step 1 installs the upstream modules (-am) into the local repository ~/.m2 so that step 2 tests only the selected modules; skip step 1 when CI already installed every module. ...
./mvnw -pl app,core -am -DskipTests install
./mvnw -pl app,core verify
```

- `-pl` entries are module directories relative to the reactor root (`.` for the root module; an entry starting with `-`, `!`, `?`
  or `:` gets a `./` prefix).
- `verify` keeps Failsafe integration tests.
- If the module list does not fit the command line (8000 characters) or a directory contains `,` or a newline, a single
  `mvn verify` of the whole reactor is emitted instead.

**Gradle** (one task per project, root project as `:test`):

```sh
# gradle: a remote build cache already avoids re-running unchanged tests; ... A project without a `test` task (no java plugin) makes `:<project>:test` fail
./gradlew :app:test :core:test
cd backend && ../gradlew :core:test
```

Long task lists are split into batches. Scope `none` emits no command; `full` emits no selective command.

**With the dynamic layer** a narrowed Maven module runs in its own step-2 invocation with the class list in the environment, and a
narrowed Gradle project gets an init script written to `--list-dir`:

```sh
MAVEN_ARGS='-Dtest=com.ex.FooTest,com.ex.FooTest$*' ./mvnw -pl core -Dsurefire.failIfNoSpecifiedTests=false verify
./gradlew -I lists/gradle-classes-_.gradle :app:testClasses :core:test
```

A unit whose kept class list is empty is only compiled (Maven step 1; Gradle `:p:testClasses`). Without `--list-dir`, narrowed
Gradle projects run whole (noted). `MAVEN_ARGS` needs Maven 3.9 or newer; an older Maven ignores it and runs every class.

## Known gaps and over-selection

- **Paths built at run time** in tests (`"../" + name`, `System.getProperty`, `Paths.get` with variables) and other test working
  directories are not seen; only literal paths are.
- A plugin parameter whose whole value is one undefined property (`<configLocation>${x}</configLocation>`) may point anywhere and
  is ignored.
- Gradle `workingDir` set by a convention plugin (`buildSrc/`, `build-logic/`) is not modelled; only scripts of the projects are read.
- Gradle projects declared by a settings plugin (automatic include) rather than `include(...)` are not seen: they are not units,
  and their files belong to the enclosing project.
- Maven profile activation is not evaluated: modules and references of every profile count (union).
- Granularity is the module: any change in a module selects all of its tests and every downstream module; a widely used module
  (`*-common`) selects nearly everything.
- Every file under a module directory is owned, including documentation: a `README.md` inside a module selects that module.
- Convention plugins in `buildSrc/` / `build-logic/` and included builds connect every project of the build to every project they
  name, because which project applies which plugin is not modelled.
- A library build of a composite build depends on every project of the including build and of its sibling library builds.
- Every Gradle project with a build script or a file is a unit; `:<project>:test` fails for a project without a `test` task.
- Maven step 1 builds the upstream modules (`-am`) into `~/.m2`; without it, step 2 would use stale artifacts.
- Maven aggregator (`pom` packaging) modules are units and appear in `-pl` lists (they run no tests).

## See also

- [Architecture](../architecture.md): tiers, widening reasons, unresolved imports, the global table
- [Configuration](../configuration.md): `bgraph.toml` rules and `[dynamic_layer]`
- [Lockfiles](../lockfiles.md)
- [CLI](../cli.md): `bgraph affected`, `explain`, `command`, `record`
- [Dynamic layer](../dynamic-layer.md)
- [.NET](dotnet.md)
