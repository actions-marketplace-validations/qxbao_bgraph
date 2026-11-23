# PHP

bgraph selects each PHP project as one unit. PHPUnit bootstraps, Composer autoloading and Pest hooks can share code
and data across test files. Each selected project runs its whole PHPUnit or Pest suite. Indexing and selection use Go
and Git and can run before dependencies are installed.

## At a glance

| | |
|---|---|
| Granularity | project directory |
| Unit ID format | `<dir>::php`, for example `services/api::php` or `.::php` at the root |
| Runners | PHPUnit, Pest |
| Manifests and configuration | `composer.json`, `phpunit.xml`, `phpunit.xml.dist`, `phpunit.dist.xml`, `Pest.php` |
| Lockfile | `composer.lock` at [level L0](../lockfiles.md) |
| Dynamic layer | none |

## Supported layouts

- **Composer packages.** Every `composer.json` outside dependency directories defines a project. Nested projects own
  their own files. A directory with a standard PHPUnit XML file can also define a project.
- **PHPUnit.** A project with a standard PHPUnit XML file runs that configuration. Without XML, bgraph recognizes
  conventional `*Test.php` and `.phpt` files and uses the conventional `tests/` and `test/` directories where possible.
- **Pest.** A `pestphp/pest` dependency or a conventional `Pest.php` marker identifies the runner. Pest uses the
  project's PHPUnit XML configuration when present.
- **Mixed repositories.** A separate project of another ecosystem without PHP sources is excluded from PHP ownership.
  An explicit dependency can still connect files across project and ecosystem boundaries.

## Test units

A test unit is a project with its own test suite. The unit's ID uses the repo-relative project directory. The graph
stores a virtual file such as `<php:services/api::php>` for the unit. Composer libraries without suites can still
contribute dependencies to runnable projects.

## What a unit depends on

- Owned project files, including PHP source, helpers, templates, fixtures and other data.
- Composer `autoload` and `autoload-dev` roots for PSR-4 and PSR-0, `classmap`, `files`, and `include-path`.
- Local Composer repositories and path dependencies recorded in `composer.lock`, and files reached through them.
- PHPUnit XML references such as bootstrap files, suite directories and explicit test files.
- Literal `include`, `include_once`, `require` and `require_once` targets, including concatenation with `__DIR__`.
  The graph follows those edges through shared PHP code.
- PHP data paths in common file and directory functions such as `file_get_contents`, `fopen`, and `scandir`.

Composer autoload directories contribute to project ownership. The selector keeps the project whole because tests can
load classes and data through runtime mechanisms.

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| Only Composer metadata such as description, keywords, homepage, authors or license | no manifest widening |
| Other Composer manifest changes, including dependency or autoload changes | `manifest_changed`, the owning PHP project and its graph dependents |
| PHPUnit XML or `Pest.php` changes | `test_config_changed`, the owning PHP project and its graph dependents |
| Composer lockfile or installed dependencies change | `lock_changed`, the owning PHP project and its graph dependents |

Widenings are PHP-specific. A nested project can still be selected through a dependency.

## Unresolved and uncertain imports

Computed includes and data reads report `dynamic_import`. A missing literal include reports `unknown_import` when a
changed path could provide it. Invalid manifests, unsupported configuration paths and incomplete scans report
`uncertain_scan`. These conditions seed the affected project or file so graph dependents also run. PHP source outside
any recognized project can widen to the full suite when the graph has no owner for it.

## Running the selection

Install the project's test dependencies before executing the run plan. For a Composer project, bgraph invokes the
runner in `vendor/bin` or the configured bin directory. On Windows it passes the Composer proxy to `php` directly.
XML-only projects use the runner on `PATH`. The command runs from the project directory and passes the selected XML
file when available.

```console
$ bgraph command --base HEAD
# scope: graph
vendor/bin/phpunit --configuration phpunit.xml.dist
```

## Addressing units in `bgraph.toml`

Use a project directory to select units under it or an exact ID to select a single project.

```toml
[[rule]]
paths = ["contracts/api/**/*.json"]
affects = ["services/api::php"]
reason = "the PHP API suite loads these contracts at runtime"
```

An `affects` glob matches unit files, which are virtual nodes for PHP. Use an ID or directory for project selection.

## Known gaps and over-selection

- Changes to a single input generally select the entire suite of a single-project repository.
- Composer scripts and repository-specific runner flags may need to run through the caller's own CI command. A
  detected PHPUnit or Pest script with an unknown suite layout asks the caller to run its full suite.
- Composer lockfile changes select the project at L0. Dynamic test-level narrowing is unavailable.
- The shadow audit currently maps result files to file-based test units. PHP project units are evaluated through the PHP
  T1 and T2 oracles.

## See also

- [Architecture](../architecture.md)
- [Configuration](../configuration.md)
- [Evaluation](../evaluation.md)
- [Composer schema](https://getcomposer.org/doc/04-schema.md)
- [Composer binary configuration](https://getcomposer.org/doc/06-config.md#bin-compat)
- [Organizing tests in PHPUnit](https://docs.phpunit.de/en/12.5/organizing-tests.html)
- [Configuring tests in Pest](https://pestphp.com/docs/configuring-tests)
