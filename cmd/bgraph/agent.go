package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/qxbao/bgraph/agent"
	"github.com/qxbao/bgraph/internal/gitx"
)

const agentUsage = `Usage:
  bgraph agent install   [--root DIR] [--target auto|claude|agents|claude,agents] [--skill] [--hook] [--mcp] [--json]
  bgraph agent uninstall [--root DIR] [--json]
  bgraph agent hook      (PostToolUse hook of Claude Code and Codex: reads the hook input on stdin)
`

// runAgent installs or removes the agent guidance, or runs the hook.
func runAgent(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, agentUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "hook":
		return runAgentHook(rest, stdin, stdout, stderr)
	case "install", "uninstall":
	case "help", "-h", "--help":
		fmt.Fprint(stdout, agentUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "agent: invalid subcommand %q\n\n%s", sub, agentUsage)
		return 2
	}
	fs := flag.NewFlagSet("bgraph agent "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	asJSON := fs.Bool("json", false, "print the changes as JSON")
	var target *string
	var skill, hook, mcp *bool
	if sub == "install" {
		target = fs.String("target", "auto", "instruction files: claude (CLAUDE.md), agents (AGENTS.md), both, or auto (the existing ones, both when none)")
		skill = fs.Bool("skill", false, "also write the bgraph-test skill")
		hook = fs.Bool("hook", false, "also add the PostToolUse hook (Claude Code: .claude/settings.local.json, Codex: .codex/hooks.json)")
		mcp = fs.Bool("mcp", false, "also add the bgraph MCP server to .mcp.json")
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent %s: extra arguments: %v\n", sub, fs.Args())
		return 2
	}
	repo, err := gitx.Open(context.Background(), *root)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	repoRoot := repo.Root
	repo.Close()

	var changes []agent.Change
	if sub == "install" {
		ts, err := agent.ParseTargets(*target)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 2
		}
		changes, err = agent.Install(repoRoot, agent.InstallOptions{Targets: ts, Skill: *skill, Hook: *hook, MCP: *mcp})
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		changes, err = agent.Uninstall(repoRoot)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}
	if changes == nil {
		changes = []agent.Change{}
	}
	if *asJSON {
		return printJSON(stdout, stderr, changes)
	}
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "nothing to remove")
	}
	for _, c := range changes {
		if c.Note != "" {
			fmt.Fprintf(stdout, "%-9s %s (%s)\n", c.Action, c.Path, c.Note)
		} else {
			fmt.Fprintf(stdout, "%-9s %s\n", c.Action, c.Path)
		}
	}
	return 0
}

// runAgentHook never fails the agent's tool call: any problem is reported on stderr with exit code 0.
func runAgentHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "agent hook: extra arguments: %v\n", args)
		return 0
	}
	in, err := io.ReadAll(io.LimitReader(stdin, 16<<20))
	if err != nil {
		fmt.Fprintln(stderr, "bgraph agent hook:", err)
		return 0
	}
	out, err := agent.Hook{}.Handle(in)
	if err != nil {
		fmt.Fprintln(stderr, "bgraph agent hook:", err)
		return 0
	}
	if out != nil {
		fmt.Fprintln(stdout, string(out))
	}
	return 0
}
