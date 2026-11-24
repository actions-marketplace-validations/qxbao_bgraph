package agent

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// ToolSpec describes one tool for a host that serves it from its own MCP server: the name, the description shown to the
// agent, the JSON schemas of the arguments and of the structured result, and whether it only reads.
type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	// ReadOnly: the tool changes nothing (MCP annotations readOnlyHint and idempotentHint, openWorldHint false).
	ReadOnly bool `json:"readOnly"`
}

// Tool descriptions, as the agent reads them.
const (
	descAffected = "Select the tests affected by the change between base and head (default: merge-base with the default branch → working tree). " +
		"Returns scope (none|graph|package|module|full), the tests with reasons, and the widening/warnings sections that explain why " +
		"the scope was widened. scope=full means run everything. To get the commands that run the selection, use test_commands."
	descTestCommands = "After editing code: the commands that run only the tests affected by the change (default: merge-base with the " +
		"default branch → working tree), one shell line each, to run from the repository root instead of the whole suite. " +
		"scope=full returns no command and means run the whole test suite; scope=none means no test is affected. " +
		"The widening section says why the scope was widened."
	descDependents = "List the files that depend (transitively) on the given files, tests first, each with the dependency chain, plus the test units " +
		"reached. Use it before changing a file to see what the change can break. `unresolved` lists imports that could not be resolved " +
		"and whose scope contains a queried file: they MAY also depend on it."
	descExplain = "Explain why a test unit is selected, or where a changed file leads, for the change between base and head. " +
		"Also returns the selection's scope and widening/warnings."
)

// Specs returns the tool specs, in the order a server should list them.
func Specs() ([]ToolSpec, error) {
	var specs []ToolSpec
	add := func(name, desc string, in, out *jsonschema.Schema, err error) error {
		if err != nil {
			return fmt.Errorf("%s schema: %w", name, err)
		}
		ib, err := json.Marshal(in)
		if err != nil {
			return err
		}
		ob, err := json.Marshal(out)
		if err != nil {
			return err
		}
		specs = append(specs, ToolSpec{Name: name, Description: desc, InputSchema: ib, OutputSchema: ob, ReadOnly: true})
		return nil
	}
	for _, f := range []func() error{
		func() error {
			in, out, err := schemas[AffectedInput, AffectedOutput]()
			return add(ToolAffected, descAffected, in, out, err)
		},
		func() error {
			in, out, err := schemas[TestCommandsInput, TestCommandsOutput]()
			return add(ToolTestCommands, descTestCommands, in, out, err)
		},
		func() error {
			in, out, err := schemas[DependentsInput, DependentsOutput]()
			return add(ToolDependents, descDependents, in, out, err)
		},
		func() error {
			in, out, err := schemas[ExplainInput, ExplainOutput]()
			return add(ToolExplain, descExplain, in, out, err)
		},
	} {
		if err := f(); err != nil {
			return nil, err
		}
	}
	return specs, nil
}

func schemas[In, Out any]() (*jsonschema.Schema, *jsonschema.Schema, error) {
	in, err := jsonschema.For[In](nil)
	if err != nil {
		return nil, nil, err
	}
	out, err := jsonschema.For[Out](nil)
	return in, out, err
}
