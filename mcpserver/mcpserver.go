// Package mcpserver serves bgraph's agent tools (package agent: affected_tests, test_commands, dependents_of and
// explain_selection) over the Model Context Protocol with the official Go SDK (docs/cli.md, mcp). The server keeps one
// *bgraph.Index open for the whole session, so the scan cache and the analysis memo stay warm.
//
// A host that already runs its own MCP server can register the same tools with AddTools, or serve agent.Specs and
// agent.Tools.Call with any other implementation.
package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qxbao/bgraph"
	"github.com/qxbao/bgraph/agent"
)

// New builds the MCP server for ix, with agent.Instructions as its instructions. It does not take ownership of ix: the caller
// closes it.
func New(ix *bgraph.Index, version string) (*mcp.Server, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "bgraph", Version: version}, &mcp.ServerOptions{Instructions: agent.Instructions})
	if err := AddTools(srv, agent.NewTools(ix)); err != nil {
		return nil, err
	}
	return srv, nil
}

// AddTools registers every agent tool of t on srv. A tool error (bad arguments, a git failure) is returned as a tool result
// with IsError set, not as a protocol error.
func AddTools(srv *mcp.Server, t *agent.Tools) error {
	specs, err := agent.Specs()
	if err != nil {
		return err
	}
	for _, s := range specs {
		tool := &mcp.Tool{Name: s.Name, Description: s.Description, InputSchema: s.InputSchema, OutputSchema: s.OutputSchema}
		if s.ReadOnly {
			f := false
			tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &f}
		}
		name := s.Name
		srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args json.RawMessage
			if req.Params != nil {
				args = req.Params.Arguments
			}
			res, err := t.Call(ctx, name, args)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text}}, StructuredContent: res.Structured}, nil
		})
	}
	return nil
}

// Run serves ix on stdio until the client disconnects or ctx ends.
func Run(ctx context.Context, ix *bgraph.Index, version string) error {
	srv, err := New(ix, version)
	if err != nil {
		return err
	}
	return srv.Run(ctx, &mcp.StdioTransport{})
}
