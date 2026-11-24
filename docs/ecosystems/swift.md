# Swift

bgraph selects SwiftPM tests by test target. It reads `Package.swift` as text and follows dependencies between targets and
local packages. A package whose manifest cannot be read statically gets one unit that runs its whole suite.

## At a glance

| | |
|---|---|
| Granularity | SwiftPM test target, or the package when the manifest is opaque |
| Unit IDs | `swift:<package-dir>::test:<target>` and `swift:<package-dir>::all` |
| Runner | `swift test` (XCTest and Swift Testing) |
| Manifests | `Package.swift` and toolchain-specific `Package@swift-*.swift` variants |
| Lockfile | `Package.resolved` at [L0](../lockfiles.md) |
| Dynamic layer | None |

## Supported layouts

- SwiftPM packages can live at the repository root or in subdirectories. Nested packages are separate projects and can
  depend on one another through literal `.package(path: ...)` declarations. If the source roots of two packages
  overlap, tests can depend on the same source through both targets.
- Target sources can use SwiftPM's standard directory layout or a literal `path` and `sources` list. The selector also
  recognizes flat source directories for single targets, according to the declared Swift tools version.
- Repositories with no SwiftPM manifest use the usual [file rules](../architecture.md#selection-details).

## Test units

A supported `.testTarget(name: ...)` declaration produces one unit. For example, `swift:packages/kit::test:KitTests`
identifies `KitTests` in `packages/kit`. Use `.` for a package at the repository root. The ID encodes `:` and `%` when
they occur inside a directory or target name. A unit covers the whole test target, including test helpers and both test
frameworks.

When the manifest's target declarations require Swift evaluation, `swift:<package-dir>::all` represents the whole package.

## What a unit depends on

- A target depends on its sources, declared target dependencies and local modules its Swift sources import or reference
  with `canImport`. Conditional target dependencies count on every platform.
- Local package dependencies contribute their build closure to the packages that use them.
- Declared resources, implicit localized and Apple-platform resources, and data without a clear owner are shared
  package inputs. Literal runtime paths can point into another package or ecosystem. The scanner also accounts for
  subprocesses that may run the repository's Swift programs. [Swift package resource rules](https://developer.apple.com/documentation/xcode/bundling-resources-with-a-swift-package)

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| Comments or formatting in a supported manifest | No widening when the parsed definition and tools version stay the same |
| Build-relevant manifest change | `module`, `manifest_changed` for affected Swift packages |
| Only test target declarations change | `module`, `test_config_changed` |
| A computed or unsupported manifest changes | `module`, `config_is_code` |
| `.swiftpm/` configuration or an `.xctestplan` in a package | `module`, `test_config_changed` |
| `Package.resolved` at the package root or under `.swiftpm/` | `module`, `lock_changed` at L0 |

These widenings are scoped to Swift packages. They include in-repository consumers where a dependency can propagate
the change.

## Unresolved and uncertain imports

Missing local dependencies report `unknown_import`. Computed manifests and recognized computed runtime paths report
`dynamic_import`. If lexical analysis stops early, `uncertain_scan` makes changes elsewhere in the repository reach the
affected unit and its dependents. When SwiftPM is active, a changed Swift file outside its packages can require a full
selection.

## Running the selection

`bgraph command` emits one `swift test` invocation per selected package, run from that package's directory. It uses
`--skip` to exclude known unselected test modules. Tests from linked libraries and XCTest cases with unqualified
Objective-C names still run. Opaque units, custom test entry points, and commands that would exceed the argument limit
run the whole package.

For a change that reaches `AlphaTests` in a package with two test targets, bgraph prints the following command.

```sh
# scope: graph
swift test --skip '^(?:BetaTests)\.'
```

## Addressing units in bgraph.toml

In a [`[[rule]]`](../configuration.md#rule), use a package directory, a test target's directory, or an exact unit ID such
as `swift:packages/kit::test:KitTests`. The unit's graph file is virtual, so a glob over `.swift` files cannot address a
test target.

## Known gaps and over-selection

- Packages that construct their target arrays with Swift code or supply toolchain-specific manifests can run their
  entire suites.
- Runtime reads through unrecognized application-specific wrappers may need a `[[rule]]` to describe the dependency.
- The runner can build more targets than it runs, since `swift test --skip` filters test execution.
- Xcode project and workspace schemes are outside the SwiftPM integration.

## See also

- [Architecture](../architecture.md)
- [Evaluation](../evaluation.md)
- [Integration roadmap](../roadmap.md)
- [SwiftPM `swift test` documentation](https://github.com/swiftlang/swift-package-manager/blob/main/Sources/PackageManagerDocs/Documentation.docc/SwiftTest.md)
