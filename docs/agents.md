# Coding agents

An agent that knows about bgraph runs the tests its change can affect after every edit, instead of the whole suite or no
test at all. Telling it takes two things: a way to ask (the CLI or the MCP server) and a few lines of guidance it reads at
the right moment. This page covers both, and the Go API for tools that drive agents themselves.

## Quick setup

```sh
bgraph agent install            # guidance block in CLAUDE.md / AGENTS.md
bgraph agent install --skill    # also the /bgraph-test skill
claude mcp add bgraph -- bgraph mcp --root .
```

`bgraph agent uninstall` removes everything `install` can write.

## What `bgraph agent install` writes

| Piece | Where | Default |
|---|---|---|
| Guidance block | `CLAUDE.md` and/or `AGENTS.md` (see targets) | always |
| `bgraph-test` skill | `.claude/skills/bgraph-test/SKILL.md`, `.agents/skills/bgraph-test/SKILL.md` | `--skill` |
| PostToolUse hook | `.claude/settings.local.json` (Claude Code), `.codex/hooks.json` (Codex) | `--hook` |
| MCP server entry | `.mcp.json` (`{"command": "bgraph", "args": ["mcp"]}`) | `--mcp` |

**Targets.** `--target claude` writes `CLAUDE.md` (Claude Code), `--target agents` writes `AGENTS.md` (Codex, Cursor,
Antigravity, opencode and other agents that read it). The default, `auto`, updates whichever of the two exist, and both
when neither does. A `CLAUDE.md` that imports `AGENTS.md` (an `@AGENTS.md` line) is skipped when `AGENTS.md` is written,
so the guidance is never read twice.

**The block** sits between `<!-- bgraph:start -->` and `<!-- bgraph:end -->`. A new install replaces it; text outside
the markers is never touched, and the file's line endings are kept. It is a few lines of plain statements: run
`bgraph command` after a change and run what it prints, `scope: full` means the whole suite, `bgraph explain` says why,
CI remains the final check. It names the CLI rather than the MCP tools because subagents do not see MCP server
instructions, and because it then works without the server.

**The skill** runs the affected tests on request (`/bgraph-test`) or before a change is reported as done. A
`SKILL.md` that bgraph did not write is never overwritten or removed.

**The hook** runs `bgraph agent hook` after every shell command (Claude Code's `Bash` and, on Windows, `PowerShell`
tools; Codex's `Bash`). When the command ran a whole test suite (`go test ./...`, `npm test`, `pytest` or `cargo test`
without a filter, `mvn test`, `./gradlew test`, `dotnet test`, `rspec`, `phpunit`, `make test` and similar), it adds
one sentence to the agent's context pointing at `bgraph command`, at most once per session. It never blocks a command
and never fails one. Both agents share the hook protocol, so one command serves both:

- Claude Code: the hook goes to `settings.local.json`, which Claude Code keeps out of git, because it needs `bgraph` on
  `PATH` on every machine that runs it.
- Codex: the hook goes to the project's `.codex/hooks.json`. Codex reads it only in a trusted project, and runs it only
  after the user approves it once (`/hooks` in the Codex TUI; `codex exec --dangerously-bypass-hook-trust` skips the
  approval for one run).

Every step is idempotent: a second `install` reports each file `unchanged`. JSON files keep their content but are
re-indented. `--json` prints the changes as JSON.

### Why this shape

Agents ignore tools they are not told about, and tool descriptions alone rarely change what they do. General code-graph
tools reached the same conclusion and added instruction-file blocks and hooks. Their experience also showed what to
avoid: long imperative blocks (`MUST`, `NEVER`) that are re-read on every turn and get flagged as prompt injection,
hooks that deny file reads (agents work around them, and runs get slower), and guidance written into a repository
without being asked. bgraph's guidance is short, factual, opt-in, and removable.

bgraph also has it easier than a general code graph: an agent already runs tests after an edit, so the guidance only
replaces *which* tests, at a moment the agent has already chosen.

## MCP tools

`bgraph mcp` serves four read-only tools; see [CLI, mcp](cli.md#mcp) for the arguments. The server instructions tell
the agent to call `test_commands` after editing code and run what it returns, to treat `scope: full` as "run the whole
suite", and to read the `widening` section.

| Tool | When an agent uses it |
|---|---|
| `test_commands` | after a change: the shell lines that run the affected tests (never truncated) |
| `affected_tests` | the selection itself: tests with reasons, widenings |
| `dependents_of` | before changing a file: what depends on it |
| `explain_selection` | why a test is, or is not, selected |

## Go API

Everything above is a library, for tools that drive agents themselves (an orchestrator, an IDE, a review bot). Two
packages, so a host that runs its own MCP server does not depend on the MCP SDK:

- [`github.com/qxbao/bgraph/agent`](https://pkg.go.dev/github.com/qxbao/bgraph/agent), no MCP dependency:
  - `NewTools(ix)` returns the tools as typed calls (`Affected`, `TestCommands`, `Dependents`, `Explain`). Each result has
    a `Text()` summary for the agent.
  - `Specs()` returns each tool's name, description and JSON schemas. `Tools.Call(ctx, name, args)` decodes JSON
    arguments and dispatches, so any MCP server can serve the same tools.
  - `Instructions`, `Block()` and `Skill()` are the guidance texts. `UpsertBlock` and `RemoveBlock` manage the marked
    block in any file.
  - `Install` and `Uninstall` do what `bgraph agent` does.
  - `IsFullSuiteCommand` and `Hook` are the hook logic, for a host with its own hook pipeline.
- [`github.com/qxbao/bgraph/mcpserver`](https://pkg.go.dev/github.com/qxbao/bgraph/mcpserver), with the
  [official Go SDK](https://github.com/modelcontextprotocol/go-sdk): `New` / `Run` (what `bgraph mcp` runs) and
  `AddTools(srv, tools)`, which adds the tools to a server of your own.

```go
ix, err := bgraph.Open(ctx, repoDir, bgraph.Options{})
if err != nil {
	return err
}
defer ix.Close()

tools := agent.NewTools(ix)
specs, _ := agent.Specs() // register these on your server...
res, err := tools.Call(ctx, agent.ToolTestCommands, json.RawMessage(`{}`)) // ...and dispatch calls here
// res.Structured is an agent.TestCommandsOutput, res.Text its summary.
```

A host that runs tests itself should take the plan from `TestCommands` (or `bgraph.Commands`) and treat
`Scope == bgraph.TierFull` as "run the whole suite": a full plan has no command.
