# C and C++

bgraph selects affected tests registered with CTest or Meson. It reads build definitions as text. The selector can
use the build graph when test registrations and their dependencies are statically readable. A project with computed or
unsupported build definitions gets a whole-suite unit.

## At a glance

| | |
|---|---|
| Granularity | registered test, or the whole build project |
| Unit IDs | `cpp:cmake:<dir>::test:<name>`, `cpp:meson:<dir>::test:<name>`, and `::all` for a whole project |
| Runners | `ctest` and `meson test` |
| Build definitions | `CMakeLists.txt`, CMake scripts and presets, `meson.build` and Meson options |
| Package manager inputs | `conan.lock` at L0, Conan definitions and vcpkg manifests |
| Dynamic layer | none |

## Supported layouts

Build projects can live at the repository root or in subdirectories. The analyzer follows supported literal
subdirectory declarations and accounts for changes to inputs that can cross those boundaries. A CMake subdirectory
that declares its own independent setup can also be a separate build root.

## Test units

A named unit identifies one test registered in the build system. For example, `cpp:cmake:library::test:parse`
identifies the CTest test `parse` in the `library` project. The directory is relative to the repository root, with
`.` for a root project. IDs encode literal colons and percent signs in directories and test names.

Computed test discovery can produce a whole-suite unit such as `cpp:cmake:library::all`. A whole-suite unit covers
every test registered by that project's configured build.

## What a unit depends on

For supported declarations, a named test depends on the executable it runs, the sources and headers of that target,
and the in-repository targets linked into it. The selector also accounts for inputs without a clear target owner.

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| Comments or formatting in a supported build script | No widening when the parsed declarations stay the same |
| Build declarations change | `manifest_changed` selects tests of the affected build |
| CMake scripts and presets, Meson options or wrap files | `test_config_changed` selects tests of the affected build |
| Computed build definitions | `config_is_code` or `dynamic_import` selects the whole affected build |
| `conan.lock` | `lock_changed` at L0 selects scoped C/C++ projects |
| `conanfile.txt`, `conanfile.py`, `vcpkg.json` or `vcpkg-configuration.json` | `manifest_changed` selects scoped C/C++ projects |

## Unresolved and uncertain imports

C and C++ headers can be selected by compiler flags, generated on demand, and referenced through macros. When bgraph
cannot establish the dependency from its static inputs, it widens the selection. Literal include names depend on all
matching repository header basenames across search paths. Macro includes, assembly, and recognized native file or
process APIs can make repository-wide changes reach the source and its dependent tests.

## Running the selection

Configure each build directory with the same generator, toolchain and test options as your full suite. bgraph
generates build and test commands from a selection using the configured directories.

- CTest plans run `cmake --build` and then `ctest --output-on-failure`. Named tests are matched by an anchored regex.
  CTest errors when a test command would run no tests. The adapter requires CMake and CTest 3.20 or later.
- Meson plans run `meson test` with the selected names. Meson handles the rebuild. Use Meson 1.2 or later so an
  unmatched test name reports an error.

The default build directory is `build` relative to each project. If both backends are selected at the same root, the
defaults there are `build-cmake` and `build-meson`. Use `bgraph command --build-dir DIR` to point to another directory
and `--build-config NAME` for a CMake multi-config generator. In the MCP `test_commands` tool, use `build_dir` and
`build_config`. Execute commands in the order given and stop if a build fails.

Long filters and names that cannot be passed safely run the whole configured project.

A change to the shared library of each project in a two-project fixture produces this plan.

```sh
# scope: graph
(cd cmake && cmake --build build)
(cd cmake && ctest --test-dir build --output-on-failure --no-tests=error -R '^(core\.check)$')
(cd meson && meson test -C build --print-errorlogs core.check)
```

## Addressing units in bgraph.toml

In a [`[[rule]]`](../configuration.md#rule), use a project directory or the exact ID bgraph prints, such as
`cpp:meson:tools::test:parse`. The files of a named test unit are virtual graph nodes.

## Known gaps and over-selection

- Projects that compute test registration or build inputs can select the entire suite.
- Build options, presets and generated files can change the native test census. Configure the build before running
  the plan, using the same settings as the full suite. A repository can have multiple plausible roots, including
  examples or secondary builds. Each root in a command plan needs a configured build directory.
- The C++ test frameworks used inside a registered executable share the executable's build dependencies.

## See also

- [Architecture](../architecture.md)
- [Evaluation](../evaluation.md)
- [Integration roadmap](../roadmap.md)
- [CTest manual](https://cmake.org/cmake/help/v3.31/manual/ctest.1.html)
- [Meson test command](https://mesonbuild.com/Commands.html#test)
