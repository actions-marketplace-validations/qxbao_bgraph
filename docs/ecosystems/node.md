# Node

bgraph selects affected JavaScript and TypeScript tests (CommonJS and ESM) one test file at a time. It reads `package.json`, tsconfig/jsconfig and runner configuration files as text, scans every source file with its own lexer, and resolves imports the way Node, TypeScript and the test runners do, taking the union of every possible target. Nothing is executed: no package manager, no bundler, no runner.

## At a glance

| | |
|---|---|
| Granularity | One test file per unit |
| Unit ID format | Repo-relative path of the test file, e.g. `packages/core/src/parse.test.ts`. A project whose test-file pattern cannot be read gets a "whole project" unit `<all-tests:DIR>`, e.g. `<all-tests:packages/core>` |
| Runners | Vitest, Jest, Playwright, Mocha, AVA, `node --test` (node:test) |
| Manifests/configs read | `package.json` (every one outside `node_modules`), `tsconfig*.json` / `jsconfig*.json` (with `extends`), runner configs (`jest.config.*`, `vitest.config.*`, `vite.config.*`, `playwright.config.*`, `vitest.workspace.*`, `jest.preset.*` / `jest-preset.*`, and named variants such as `vitest.unit.config.ts`), the `jest` key of `package.json` |
| Lockfiles (and level) | `pnpm-lock.yaml` (lockfile 6.x and 9.0) and `package-lock.json` (v2/v3): **L1** by default, **L2** opt-in. `yarn.lock` (classic and yarn 2+) and `bun.lock`: **L1** (L2 falls back to L1: install scripts are not recorded). `npm-shrinkwrap.json`, `bun.lockb`: **L0**. See [lockfiles](../lockfiles.md) |
| Dynamic layer | Yes, record-based narrowing per test file: see [dynamic layer](../dynamic-layer.md) (Node section) |

## Supported layouts

- **Projects.** Every `package.json` outside `node_modules` is a project, including nested ones (for example a sub-directory `package.json` with `{"type": "module"}`). A file belongs to the nearest enclosing project.
- **Workspaces.** Every named package in the repository is a candidate workspace package, whatever the `workspaces` field or `pnpm-workspace.yaml` says (extra candidates are harmless). When two packages share a name, the shallowest directory wins.
- **Files outside every `package.json`** are not part of the Node ecosystem; a change there is handled by the general rules (see [architecture](../architecture.md)).
- **Runner per project.** The runner is detected from `dependencies`/`devDependencies` of the project, then of each ancestor project: the first of `vitest`, `jest`, `@playwright/test`, `mocha`, `ava` that is declared. With none of them, the runner is `node --test`.
- **Runner config files** are found in every directory (for example `tests/unit/jest.config.mjs`) and assigned to the nearest project. A config file named by `projects`/`workspace` in another config is read too. A config in an ancestor project (a root `jest.config.js`) also applies to the files of child packages.
- `node_modules` is never repository code.

## Test units

A JS/TS file (`.ts .tsx .mts .cts .js .jsx .mjs .cjs`, never a `.d.ts`/`.d.mts`/`.d.cts`) is a test when it matches the default patterns **or** a pattern read from the runner config (union, never narrower).

Default patterns, every runner:

- any file under a `__tests__/` directory;
- a name ending in `.test`, `.spec`, `-test`, `_test`, `.e2e` or `.e2e-spec` before the extension (`a.test.ts`, `login.e2e-spec.ts`);
- a file named `test.*` or `spec.*`, or starting with `test-`.

Only for `node --test`, Mocha and AVA projects: every JS/TS file under a `test/` or `tests/` directory (helpers such as `test/utils.js` are included on purpose; running an extra helper is harmless). With Jest, Vitest and Playwright, other files in `test/` are helpers or data, not tests.

Patterns read from the runner config:

| Key | Runner | Notes |
|---|---|---|
| `testMatch` | Jest | Globs; micromatch extglobs `?(…)`, `*(…)`, `@(…)`, `+(…)` are converted; `<rootDir>` is substituted |
| `testRegex` | Jest | String, array, regex literal, or a template literal whose expressions are constant strings or ternaries of constant strings (every combination is used) |
| `include` | Vitest | Globs; `include` inside `coverage`, `optimizeDeps`, `build`, `server`, `preview`, `ssr`, `deps`, `css`, `worker` is ignored |
| `testPathIgnorePatterns` | Jest | Constant elements only; applies only to files matched by a config pattern |
| `rootDir`, `roots` | Jest | A literal `rootDir` is followed; a computed one makes patterns match at any depth |

**Whole-project unit.** When a test-file pattern is computed by code (a function call, a variable, a negated extglob `!(…)`) or a `testRegex` is not a valid regular expression for bgraph, the set of tests is unknown. The project then gets the unit `<all-tests:DIR>`, which is selected whenever anything at all is selected or widened, and runs the whole project. A [`[[config]]`](../configuration.md) declaration whose `replaces` names every computed test-pattern key (`include`, `testMatch`, `testRegex`) of that file removes it; its optional `tests = [...]` globs are added to the test patterns.

**Snapshots.** `__snapshots__/a.test.ts.snap` belongs to `a.test.ts` only: changing it selects that test.

## What a unit depends on

A test depends on everything it reaches through the edges below (transitively).

**Scanned references** (JS/TS files, and the `<script>`/`<style>` blocks of `.vue`, `.svelte`, `.astro` and the ESM blocks of `.mdx`):

- `import`/`export … from`, `export *`, `import type` (type-only edges count), `require()`, `import x = require()`, `import()` with a constant, `require.resolve()`;
- `jest.mock`/`unmock`/`doMock`/`requireActual`/`requireMock`/… and `vi.mock`/`doMock`/`importActual`/`importMock`/…;
- `import.meta.glob(...)` / `globEager` and webpack `require.context(dir, …)`: a glob edge, so a file added later is caught;
- `import()` of a template or concatenation with a static prefix (`` import(`./locales/${l}.json`) ``): every existing match plus a glob on the prefix directory;
- `new URL('./w.wasm', import.meta.url)` is a file reference; `new URL('.', import.meta.url)` or `'./dir/'` depends on every file under that directory;
- assets (`.css`, `.json`, `.svg`, `?raw` queries…) are edges to the file; in SFC files `src="x.ts"` and `@import "a.css"` are tried as relative paths first.

**Resolution** (union of every plausible target):

- relative and absolute paths with extension probing (`.ts .tsx .d.ts .mts .d.mts .cts .d.cts .js .jsx .mjs .cjs .json .node .vue .svelte`), directory `index.*`, a directory's `package.json` (`main`, `module`, `types`, `typings`, `browser`, `source`, `exports["."]`);
- TypeScript extension substitution: `./util.js` → `util.ts`/`.tsx`/`.d.ts`, `.jsx` → `.tsx`, `.mjs` → `.mts`/`.d.mts`, `.cjs` → `.cts`/`.d.cts`;
- tsconfig/jsconfig `paths` (exact key, then longest `*` prefix; every fallback target) and `baseUrl`, from every config in the importing file's directory and its ancestors, with `extends` followed;
- `package.json` `exports` (every condition and every array element, subpath patterns `./features/*`, `null` exclusions) and `imports` (`#internal/*`), self-reference by package name, workspace packages by name;
- build outputs absent from the tree (`dist/`, `build/`, `lib/`, `out/`, format variants such as `dist/cjs`, `dist/types`) are mapped back to sources through tsconfig `outDir`/`rootDir` or a `src/` directory;
- aliases: Jest `moduleNameMapper` (regex, `$1` substitution, `<rootDir>`) from a config file or the `jest` key, Vite/Vitest `resolve.alias` (object or `[{find, replacement}]`, literal targets, `path.resolve(__dirname, '…')`, `fileURLToPath(new URL('…', import.meta.url))`), and `alias`/`alias_regex` of a `[[config]]` declaration;
- Node built-ins (`fs`, `node:test`…) and declared external packages create no edge (except at lockfile level L2, see below);
- an import that matches a file only when ignoring case still creates the edge, with a `case_mismatch` warning.

**Implicit inputs of every test of a project:**

- the project's runner config files themselves;
- every constant string in a runner config that names an existing file or directory (`setupFiles`, `globalSetup`, `preset`, `transform`…); a directory means every file under it. Values of `exclude`, `coverage`, `include`, `testMatch`, `testRegex`, `roots`, `collectCoverageFrom`, `testPathIgnorePatterns` and similar keys are not inputs;
- `importSource`/`jsxImportSource` in a runner config: the `jsx-runtime` and `jsx-dev-runtime` modules of that path (the tsconfig `jsxImportSource` is not read);
- every file under a `__mocks__/` directory of the project. For Jest projects, `<project or roots>/__mocks__/<name>` also replaces a bare import of `<name>`: the edge goes to the mock.

**Lockfile level L2** (opt-in): an import that resolves to an external package is an edge to a virtual node of that package, so a lockfile change can be narrowed to its importers.

## Changes to manifests, configuration and lockfiles

| Change | Effect |
|---|---|
| `package.json`, only whitespace/key order | Nothing |
| `package.json`, only `version`, `description`, `keywords`, `author`, `contributors`, `homepage`, `repository`, `bugs`, `license`, `funding`, `maintainers`, `publishConfig` | Nothing |
| `package.json`, any other top-level key (`dependencies`, `scripts`, `exports`, `main`, `type`, `jest`…), or file added, deleted or unparsable | `module`, `manifest_changed`, scope = directory of the `package.json` |
| `tsconfig*.json` / `jsconfig*.json` | `module`, `test_config_changed`, scope = its directory and the directory of every tsconfig that `extends` it (transitively, base or head) |
| `jest`, `vitest`, `vite`, `playwright`, `babel`, `webpack`, `rollup`, `esbuild`, `tsup`, `rspack`, `karma`, `ava`, `cypress`, `next`, `nuxt`, `svelte`, `astro` `.config.*`; `.babelrc` (and `.json`, `.js`, `.cjs`), `.swcrc`, `.mocharc` (and `.json`, `.yml`, `.yaml`, `.js`, `.cjs`), `vitest.workspace.ts`/`.js`/`.json`, `.c8rc`, `.c8rc.json`, `.nycrc`, `.nycrc.json`, `jest.preset.js`, `jest-preset.js`, `node.config.json`, `.browserslistrc`, `browserslist` | `module`, `test_config_changed`, scope = nearest Node project (the file's directory when outside every project) |
| Other runner config files (named variants such as `vitest.unit.config.ts`, files listed by `projects`) | No widening; they are inputs of every test of their project, so the graph selects those tests |
| Lockfile, level **L0** (configured, unsupported format, unreadable side, format change, change outside the package graph) | `module`, `lock_changed`, scope = lockfile directory, Node units only |
| Lockfile, level **L1** (default) | `module`, `lock_changed` per project whose resolved closure changed; every file of the project counts as changed, so importers in other projects are selected too. A change in the root project's closure (workspace tooling) selects every project under the lockfile. An undeclared import of a changed package widens the importing project |
| Lockfile, level **L2** (`[lockfile] level = "L2"`) | `graph`, `lock_changed`: only the importers of the changed direct dependencies; falls back to L1 (see below) |
| `.nvmrc`, `.node-version`, `.npmrc`, `.yarnrc`, `.yarnrc.yml`, `.yarn/**`, `.pnpmfile.cjs`, `bunfig.toml`, `pnpm-workspace.yaml`, `lerna.json`, `nx.json`, `turbo.json`, `.env`, `.env.*`, `.envrc` | `full`, `global_input` (global table, see [architecture](../architecture.md)) |

L2 falls back to L1 for a project when the project is added to or removed from the lockfile, no direct dependency carries the change, the change involves a local or workspace dependency, a direct dependency added or removed, a package with install scripts (pnpm 9.0 does not record them, so L2 always falls back there), peer dependencies, an executable of a changed package named in a config or `package.json` script (pnpm does not record executable names, so a changed package with executables always falls back there), a runner/toolchain/plugin/preset/`@types` package, a dependency named in a runner config or a non-dependency key of `package.json`, a dependency no file of the project imports, or an undeclared import of a changed package. Details: [lockfiles](../lockfiles.md).

A source or data file that nothing in the graph depends on widens its project (`unreferenced_file`); an unknown file type nobody references widens its project (`unknown_file`). See [architecture](../architecture.md).

## Unresolved and uncertain imports

An unresolved import does not widen a scope of tests directly: when a file changes inside the import's scope, the importing file counts as changed and its dependents are selected. Inspect them with `bgraph unresolved [--class unknown|dynamic|ambiguous|uncertain_scan]`.

| Case | Class | Fires on a change in |
|---|---|---|
| Relative import of a missing file | `unknown` | the target's directory (adding the file is caught) |
| Bare import that cannot be resolved (subpath not exported, invalid `exports`, broken config) | `unknown` | the importer's project |
| Undeclared bare package (`import 'ghost'`) | report only | tracked through the globs `**/ghost`, `**/ghost.*`, `**/ghost/**` |
| Import of a package's build output that is not in the tree and cannot be mapped to sources | `unknown` (`generated_output` in the detail) | the producing package |
| `require(x)` / `import(x)` with no static prefix, a non-relative glob, a prefix that does not resolve | `dynamic` | anywhere in the repository |
| File the lexer cannot read with certainty (`const r = require; r('x')`, unclosed SFC block, files over 4 MiB) | `uncertain_scan` | anywhere in the repository |
| Runner config key computed by code (`moduleNameMapper: buildMapper()`, alias from a helper, `roots`, `projects`/`workspace` that are not literal), any non-null `modulePaths`, `moduleDirectories`, `resolver` or `haste`, or an unreadable runner config file | `unknown` (`config_is_code` in the detail), reported as `unknown_import` | the project: every test of the project is selected |

Describe these in [`bgraph.toml`](../configuration.md):

- `[[generated]]` (`outputs`, `sources`, `owner`): an ignored build output and its sources become glob edges instead of a package-wide unresolved import.
- `[[config]]` (`file`, `replaces`, `alias`, `alias_regex`, `tests`, `ignore_paths`, `sources`, `reason`): replaces computed `alias`, `moduleNameMapper`, `include`, `testMatch`, `testRegex`, `projects` or `workspace` keys of one config file. A declaration naming a missing file changes nothing.

```toml
[[config]]
file = "vitest.config.ts"
replaces = ["alias"]
alias = { "@x/shared" = "packages/shared/src/index.ts" }
sources = ["scripts/aliases.js"]
reason = "scripts/aliases.js maps @x/<pkg> to packages/<pkg>/src/index.ts"
```

`[[dynamic]]` declarations are not used by the Node ecosystem.

## Running the selection

`bgraph command` groups the selected units by (project, runner) and runs each command from the project directory (the nearest `package.json`). Paths are relative to that directory.

| Runner | Command | Notes |
|---|---|---|
| Vitest | `npx vitest run <files>` | The argument is a path substring: `a.test.ts` also runs `aa.test.ts` |
| Jest | `npx jest --runTestsByPath <files>` | Exact paths |
| Playwright | `npx playwright test <regex>…` | Each path is escaped and `/` becomes `[\\/]`; unanchored, may over-select. With `--list-dir` and a list over the length limit: `npx playwright test --test-list <file>` (Playwright 1.56 or later) |
| Mocha | `npx mocha <files>` | |
| AVA | `npx ava <files>` | |
| node:test | `node --test <files>` | Positional arguments are globs (Node 21 or later) |

- A whole-project unit, a file outside the project, or (Mocha, AVA, node:test) a file name with glob characters falls back to the whole-project command (`npx vitest run`, `npx jest`, `npx playwright test`, `npx mocha`, `npx ava`, `node --test`).
- Long lists are split into batches of at most 8000 characters per command line; merge the exit codes.
- Scope `none` emits no command; scope `full` emits none either and asks for the full suite.

```console
$ bgraph command --base origin/main
# scope: graph
# vitest: the argument is a substring of the path, so a.test.ts also matches aa.test.ts (over-selection, safe)
(cd packages/core && npx vitest run src/parse.test.ts test/format.test.ts)
(cd packages/web && npx jest --runTestsByPath src/app.test.tsx)
```

See [CLI](../cli.md) for `--json` and `--list-dir`.

## Known gaps and over-selection

- **Dynamic and uncertain sites fire on any change in the repository**, including documentation and data files: the file holding a non-constant `require`/`import()` or an uncertain scan is selected (with its dependents) on almost every change.
- **Computed config keys** (`config_is_code`) select every test of the project on any change inside the project, documentation included, until a `[[config]]` declaration describes them.
- **Whole-project units** are selected whenever anything is selected or widened anywhere in the repository.
- **One runner per project.** A project with both Vitest and Playwright (or Jest) gets the first detected runner for all its test files, and commands run from the nearest `package.json`; a runner configured only at a monorepo root must be able to run from there.
- **Barrels and package entry points** create real but wide fan-in: changing a file re-exported by an `index.ts` selects every test that imports the barrel.
- **Manifest and config widenings are not limited to Node units**: a `package.json`, tsconfig or runner config change selects every test unit under its scope directory, including units of other ecosystems there.
- **Package-level edges at L1**: a lockfile change selects every test of each project whose closure changed; L2 narrows only npm and pnpm 6.x lockfiles (pnpm 9.0, yarn and bun lockfiles do not record install scripts).
- **Not seen by the scanner:** files read through `fs` at run time, scripts run through `child_process`, stylesheet-to-stylesheet `@import` outside SFC files, imports inside HTML or other template formats. Such files usually have no dependents and widen their project (`unreferenced_file`, `unknown_file`) instead of being missed.
- **Tool configs the ecosystem does not know** (ESLint, Prettier…) widen their project as unreferenced or unknown files; declare them with a `[[rule]]` `affects = "none"`.
- Module resolution roots that are not modeled (`NODE_PATH`, Babel module-resolver) are approximated by the suffix globs of undeclared bare imports. (A Jest `moduleDirectories` or `modulePaths` key is `config_is_code`, see above.)

## See also

- [Architecture](../architecture.md): tiers, widening reasons, unresolved imports, global table
- [Configuration](../configuration.md): `bgraph.toml`
- [Lockfiles](../lockfiles.md): levels L0, L1, L2
- [CLI](../cli.md)
- [Dynamic layer](../dynamic-layer.md)
