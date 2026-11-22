package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/internal/mcpserver"
)

// runMCP serves the MCP protocol on stdio (stdout carries protocol messages only; diagnostics go to stderr).
func runMCP(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("bgraph mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "directory inside the repo")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "mcp: extra arguments: %v\n", fs.Args())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ix, err := bgraph.Open(ctx, *root, bgraph.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer ix.Close()
	if err := mcpserver.Run(ctx, ix, buildVersion()); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
