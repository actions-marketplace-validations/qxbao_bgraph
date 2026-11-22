# Python

bgraph selects affected pytest tests (and tests of projects driven by Django's own runner) one test file at a time. It reads the pytest and packaging configuration as text, scans every `.py` file with its own lexer, and resolves imports against the union of every directory that could be on `sys.path`. The static analysis never runs Python; only the opt-in [dynamic layer](../dynamic-layer.md) starts an interpreter (to fingerprint the environment).

## At a glance

| | |
|---|---|
| Granularity | One test file per unit; a directory unit for collection-hook scopes; a whole-project unit when collection settings are computed |
| Unit ID format | Repo-relative path of the test file, e.g. `tests/test_api.py`; a directory for a collection-hook scope, e.g. `tests/examplefiles`; `<py-all-tests:DIR>` for a whole-project unit, e.g. `<py-all-tests:.>` |
| Runners | pytest (`python -m pytest`); Django's runner (`tests/runtests.py`, `runtests.py`, `manage.py test`) |
| Manifests/configs read | `pyproject.toml`, `setup.cfg`, `setup.py` (text only), `pytest.ini`, `.pytest.ini`, `pytest.toml`, `.pytest.toml`, `tox.ini`, `Pipfile`, requirements files, `conftest.py` |
| Lockfiles (and level) | `poetry.lock`, `uv.lock`, `pdm.lock`, `Pipfile.lock`, `pylock.toml` / `pylock.*.toml`, requirements and constraints files: **L0** only. See [lockfiles](../lockfiles.md) |
| Dynamic layer | Yes, record-based narrowing per test file: see [dynamic layer](../dynamic-layer.md) (Python collector and environment fingerprint sections) |

## Supported layouts

- **Projects.** A directory with `pyproject.toml`, `setup.py`, `setup.cfg`, `pytest.ini`, `.pytest.ini`, `pytest.toml` or `.pytest.toml`, or a `tox.ini` with a `[pytest]` section, is a project. When some `.py` file lies outside every such directory, the repository root is a project too. A file belongs to the nearest project.
- **Nested build configurations are not projects.** A `setup.py`/`setup.cfg`/`pyproject.toml` inside a package (a directory with `__init__.py`) of an enclosing project belongs to that project (numpy.distutils-style per-subpackage `setup.py`), unless the directory holds a pytest configuration.
- **Layouts.** Flat layout, `src/` layout, namespace packages (PEP 420), several projects in one repository, and mixed repositories (for example a Node project next to a Python one: a change in one project's sources selects no test of the other, and Python configuration widenings select Python units only).
- **Never repository code:** paths through `node_modules`, `site-packages` or `__pycache__`. No test is collected below a directory holding a `pyvenv.cfg`.

## Test units

A file is a pytest test unit when:

1. its name matches `python_files` (default `test_*.py`, `*_test.py`; a pattern with `/` matches the path, as in pytest);
2. it lies under the pytest rootdir and under `testpaths` when set;
3. no directory on its way matches `norecursedirs` (default `*.egg`, `.*`, `_darcs`, `build`, `CVS`, `dist`, `node_modules`, `venv`, `{arch}`), `--ignore` or `--ignore-glob`.

`conftest.py` is never a unit.

**Configuration.** The configuration of a file is the nearest ancestor directory that has one, searched in pytest's order: `pytest.toml`, `.pytest.toml`, `pytest.ini`, `.pytest.ini`, `pyproject.toml` (`[tool.pytest.ini_options]` or `[tool.pytest]`), `tox.ini` (`[pytest]`), `setup.cfg` (`[tool:pytest]`, or the old `[pytest]`). The keys read are `python_files`, `testpaths`, `norecursedirs`, `pythonpath` and `addopts`; in `addopts`: `--doctest-modules`, `--doctest-glob`, `-p NAME`, `--ignore`, `--ignore-glob`, and `-o`/`--override-ini` for `python_files`, `testpaths`, `norecursedirs` and `pythonpath`.

**Other unit kinds:**

| Unit | When | ID |
|---|---|---|
| Doctest module | `--doctest-modules` in `addopts`: every collectable module (except `setup.py`, `__main__.py`) is a unit | file path |
| Directory unit | A `conftest.py` that implements `pytest_collect_file`, `pytest_collect_directory`, `pytest_pycollect_makemodule` or `pytest_ignore_collect` (itself or through a repo plugin it loads with `pytest_plugins`), a `-p` plugin or `pytest11` entry point of the repo that does, or `--doctest-glob` in `addopts`: any file below may be a test item | the directory |
| Whole-project unit | `python_files`, `testpaths`, `norecursedirs`, `pythonpath` or `addopts` is computed (a `{…}` substitution in `tox.ini`, `${…}` in a key other than `addopts`, a TOML value that is not a string or a list of strings) or the config file is unreadable or not valid TOML | `<py-all-tests:DIR>` (DIR = the configuration's directory) |
| Django test module | No pytest configuration applies and the project has `tests/runtests.py`, `runtests.py` or `manage.py`: `test*.py` files below the label root, through a chain of packages (`__init__.py` in every directory) | file path |

A whole-project unit is selected whenever anything is selected or widened, and runs the project's whole suite.

## What a unit depends on

**Imports** recognized anywhere in a file (function bodies, `try`/`except`, `if TYPE_CHECKING:`; strings and comments are ignored):

- `import a.b.c`, `from a.b import c`, `from . import x`, `from ..a import b`, `from x import *`;
- `importlib.import_module("m")`, `__import__("m")`, `runpy.run_module("m")`, `pytest.importorskip("m")` with a constant (also `for name in ("a", "b"): import_module(name)` over literal strings);
- `pytest_plugins = ["a.b"]` (or a plain string), usually in a `conftest.py` (read in any file);
- heuristic edges (only added when the target exists): `mock.patch("a.b.c")`, `patch.object`/`multiple`/`dict`, `monkeypatch.setattr("a.b.c", …)`, Django settings `INSTALLED_APPS`/`MIDDLEWARE`/`ROOT_URLCONF`/`AUTHENTICATION_BACKENDS` strings, and `>>> import x` lines in doctests.

**Import roots** (an absolute import is resolved under every root and the matches are united):

- the repository root and every project directory, plus its `src/`, `lib/` and `python/` when they hold Python files;
- package locations from `pyproject.toml` (setuptools `package-dir` and `packages.find.where`, hatch build `sources`/`packages`/`include`, poetry `packages` `from`/`include`, flit, pdm `package-dir`/`includes`, maturin `python-source`, scikit-build `wheel.packages`), `setup.cfg` (`package_dir`, `packages.find` `where`) and `setup.py` (`package_dir`, `where=`, `find_packages("…")`);
- pytest `pythonpath`;
- the rootdir-insertion directory of every test and `conftest.py` (the first ancestor without `__init__.py`), as pytest's prepend/append import modes do;
- literal `sys.path.insert/append/extend` and `site.addsitedir` targets, including `os.path`/`pathlib` expressions built from `__file__`.

**Resolution rules:**

- a module resolves to `x.py`, its `.pyi` stub, `x/__init__.py(i)`, or a namespace directory; `from pkg import mod` also depends on `pkg/mod.py`;
- importing `a.b.c` also depends on `a/__init__.py` and `a/b/__init__.py`; every Python file depends on the `__init__.py` of each of its ancestor directories;
- `importlib.import_module("pkg.plugins." + name)` (a static prefix) depends on everything under `pkg/plugins/`, with no widening;
- an absolute import no root resolves, but whose path suffix exists somewhere in the repo, is tracked by glob edges (`**/a/b/c.py`, `**/a/b/c/__init__.py`, ancestor `__init__.py`), so a file added later is caught;
- standard-library modules and distributions declared anywhere in the repo (`[project]` dependencies and optional dependencies, `dependency-groups`, `build-system.requires`, poetry, pdm, uv, hatch environments, `setup.cfg` requires, `setup.py` strings, tox `deps`, `Pipfile`, requirements files) create no edge when no root resolves them and no module with that path suffix exists in the repo; common import-name/distribution differences (`yaml`/`pyyaml`, `PIL`/`pillow`…) and namespace distributions are known.

**Implicit inputs of a test:**

- every `conftest.py` in its directory and every ancestor directory;
- the modules named by `-p` in the `addopts` of its configuration;
- the modules of every `pytest11` entry point declared in the repo (`[project.entry-points.pytest11]`, `[tool.poetry.plugins.pytest11]`, `setup.cfg` `[options.entry_points]`, `setup.py`);
- for a directory unit: every file below the directory, its conftests, `-p` plugins and entry points.

## Changes to manifests, configuration and lockfiles

Widenings below, except the global `.python-version` row, are `module` tier and tagged `python`: they select Python units only. They apply only when the repository has a Python project.

| Change | Effect |
|---|---|
| `pyproject.toml`: `[tool.pytest…]` or `[tool.tox…]` | `test_config_changed`, scope = its directory |
| `pyproject.toml`: only `[project]` `description`, `authors`, `maintainers`, `urls`, `classifiers`, `readme`, `keywords`, `license`, `license-files`, or `[tool.X]` of a linter/formatter/release tool (black, ruff, isort, mypy, coverage, flake8, pylint, pyright, towncrier, bumpversion, sphinx…) | Nothing |
| `pyproject.toml`: anything else (`version`, dependencies, `build-system`, other tools), or added, deleted, unparsable | `manifest_changed`, scope = its directory |
| `setup.cfg`: `[tool:pytest]` / `[pytest]` | `test_config_changed`, scope = its directory |
| `setup.cfg`: only tool sections (`flake8`, `pycodestyle`, `pep8`, `pylint`, `pylint.*`, `isort`, `tool:isort`, `mypy`, `mypy-*`, `coverage:*`, `bdist_wheel`, `sdist`, `bumpversion`, `bumpversion:*`, `tool:black`, `pydocstyle`, `yapf`) or harmless `[metadata]` keys (`description`, `long_description*`, `author*`, `maintainer*`, `url`, `project_urls`, `classifiers`, `keywords`, `license*`, `platforms`, `download_url`) | Nothing |
| `setup.cfg`: other sections or `[metadata]` keys, or added, deleted | `manifest_changed`, scope = its directory |
| `setup.py` | `manifest_changed`, scope = its directory (code that runs at build time) |
| `Pipfile` | `manifest_changed`, scope = its directory |
| `pytest.ini`, `.pytest.ini`, `pytest.toml`, `.pytest.toml` | `test_config_changed`, scope = its directory |
| `tox.ini`, `noxfile.py` | `test_config_changed`, scope = its directory |
| `poetry.lock`, `uv.lock`, `pdm.lock`, `Pipfile.lock`, `pylock.toml`, `pylock.*.toml`; requirements files: a `.txt` or `.in` whose name starts with `requirements`, `constraints` or `requirement-`, or ends with `-requirements`/`_requirements` (case-insensitive), or any `.txt`/`.in` in a directory named `requirements`, `reqs` or `requirement` | `lock_changed` at L0, scope = the deepest enclosing Python project (the repository root when none) |
| `.flake8`, `.pylintrc`, `.isort.cfg`, `mypy.ini`, `ruff.toml`, `.coveragerc`, `.pre-commit-config.yaml`, `.editorconfig`, `MANIFEST.in`, `codecov.yml`, `.readthedocs.yaml`, `pyrightconfig.json`… and `docs/conf.py` (in `docs/`, `doc/`, `source/`) | Nothing |
| Native sources (`.pyx`, `.pxd`, `.pxi`, `.c`, `.h`, `.cpp`, `.f90`, `.rs`, `.cu`…, `meson.build`, `CMakeLists.txt`) in a project that builds extensions | `unknown_file`, scope = the project |
| `.python-version` | `full`, `global_input` (global table, see [architecture](../architecture.md)) |

A Python file that nothing imports (a script a test may run) widens its project (`unreferenced_file`); a data or native file nobody references widens its project (`unknown_file`). See [architecture](../architecture.md).

## Unresolved and uncertain imports

When a change fires an unresolved import, the importing file counts as changed and its dependents are selected. Inspect them with `bgraph unresolved`.

| Case | Class | Fires on |
|---|---|---|
| Undeclared absolute import found nowhere in the repo | `unknown`, report only | tracked by glob edges; a module added later is caught |
| Relative import of a missing file | `unknown`, report only | exact-path edges to `x.py`, `x.pyi`, `x/__init__.py(i)`; adding the file is caught |
| Relative import that climbs above the repository root | `unknown` | any change in the importer's project |
| `import_module(name)`, `__import__(name)`, `runpy.run_module(name)`, `pytest.importorskip(name)`, `pytest_plugins` with a non-constant element, `pkgutil.walk_packages`/`iter_modules`, `importlib.util.spec_from_file_location`, a static prefix that is not a repo package | `dynamic` | a loadable file (`.py`, `.pyi`, `.pyc`, `.so`, `.so.*`, `.pyd`) added, changed or deleted **anywhere in the repository** |
| `sys.path` edit bgraph cannot evaluate, or any assignment to `sys.path` | `dynamic` | same as above |
| File the lexer cannot read with certainty (unterminated string, unbalanced brackets) | `uncertain_scan` | same as above |
| Computed pytest collection settings | `unknown`, report only | the project gets a whole-project unit |

Documentation, data and tool-config changes never fire a dynamic site. Describe a site with `[[dynamic]]` in [`bgraph.toml`](../configuration.md): it then depends only on the declared globs.

```toml
[[dynamic]]
file = "pkg/loader.py"
lines = [3]                 # optional: only these sites; default every site of the file
loads = ["plugins/**/*.py"] # or: none = true
reason = "load() only imports the plugins/ modules"
```

`reason` is required; `loads`, `none = true` or `sys_path_only = true` is required. `sys_path_only = true` covers only the `sys.path` sites of the file and cannot be combined with `lines`. A declaration that matches no dynamic site (wrong `lines`, or a file without such sites) is reported by `bgraph unresolved` as stale and the widening stays; one naming a file that is not a Python file of the repo is reported as inert.

## Running the selection

`bgraph command` emits one command per (project, runner, configuration), run from the project directory. Paths are relative to it; a path starting with `-` or `@` is written `./…`.

| Runner | Command |
|---|---|
| pytest | `python -m pytest --rootdir R -c CFG <files and directories>` (R = the configuration's directory, CFG = the configuration file, both relative to the project directory, e.g. `--rootdir .. -c ../pyproject.toml`; `--rootdir .` and no `-c` without a configuration) |
| pytest, whole-project unit | `python -m pytest --rootdir R -c CFG` (no paths) |
| Django, `tests/runtests.py` | `python tests/runtests.py <labels>` (labels relative to `tests/`) |
| Django, `manage.py` | `python manage.py test <labels>` |

- Django labels are dotted module paths (`admin_views.test_forms`), relative to the driver's directory for `runtests.py` and to the project directory for `manage.py`; a file outside the label root runs the whole suite (the driver with no labels).
- A long pytest list is written to a file when `--list-dir` is given and passed as `@<file>` (needs pytest 8.2 or later); otherwise long lists are split into batches of at most 8000 characters per command line (session fixtures run once per batch; merge the exit codes).

```console
$ bgraph command --base origin/main
# scope: graph
python -m pytest --rootdir . -c pyproject.toml tests/test_api.py tests/unit/test_models.py
cd services/billing && python -m pytest --rootdir . -c pytest.ini tests/test_invoice.py
```

See [CLI](../cli.md) for `--json` and `--list-dir`.

## Known gaps and over-selection

- **Dynamic sites dominate selectivity.** Name-based imports (`import_module(name)`, `__import__`, plugin discovery, settings-driven loading) are common in frameworks; each one makes its file and every test that reaches it selected on almost any `.py` change in the repository. `[[dynamic]]` narrows the sites whose targets can be bounded; the [dynamic layer](../dynamic-layer.md) narrows by recorded loads.
- **Package `__init__.py` fan-in.** Importing any submodule depends on every ancestor `__init__.py`; a package whose `__init__.py` imports most of the package makes most tests depend on most files.
- **Heuristic edges add, never remove**: string targets (`mock.patch`, Django settings, doctests) can only add dependencies.
- **Whole-project units** are selected whenever anything is selected or widened anywhere in the repository.
- **`pytest11` entry points** declared anywhere in the repo are inputs of every test of the repo.
- **One declared-dependency set for the repository**: a distribution declared by one project counts as declared for imports in every project.
- **Manifest changes** (`version` included) widen to the whole project, since installed metadata is visible at run time.
- **Lockfiles and requirements files** stay at L0: any change selects every Python test of the project.
- **Not seen by the scanner:** files read with `open()` at run time, subprocesses, and plugins enabled outside the repository configuration (environment variables, command-line `-p`). Unreferenced files widen their project instead of being missed.
- Only `tox.ini`'s `[pytest]` section is read as configuration; tox environments themselves are not modeled (a `tox.ini` change widens the project).

## See also

- [Architecture](../architecture.md): tiers, widening reasons, unresolved imports, global table
- [Configuration](../configuration.md): `bgraph.toml`
- [Lockfiles](../lockfiles.md): levels L0, L1, L2
- [CLI](../cli.md)
- [Dynamic layer](../dynamic-layer.md)
