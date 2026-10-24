package mcpconnect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// - Verify hintsFrom maps MCP annotations to ToolHints, including the MCP default that a tool which is not
// read-only and doesn't set destructiveHint counts as destructive
func Test_Unit_HintsFrom(t *testing.T) {
	no := false
	yes := true
	tests := []struct {
		name string
		in   *mcp.ToolAnnotations
		want ToolHints
	}{
		{"nil annotations", nil, ToolHints{}},
		{"read-only", &mcp.ToolAnnotations{ReadOnlyHint: true}, ToolHints{Known: true, ReadOnly: true}},
		{"destructive by default", &mcp.ToolAnnotations{}, ToolHints{Known: true, Destructive: true}},
		{"destructive explicitly false", &mcp.ToolAnnotations{DestructiveHint: &no}, ToolHints{Known: true}},
		{"destructive explicitly true", &mcp.ToolAnnotations{DestructiveHint: &yes}, ToolHints{Known: true, Destructive: true}},
		{"idempotent, not destructive", &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true}, ToolHints{Known: true, Idempotent: true}},
		{"read-only ignores destructive", &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, ToolHints{Known: true, ReadOnly: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hintsFrom(tt.in); got != tt.want {
				t.Errorf("hintsFrom(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

// notMCPTool is a tools.Tool that is not an MCP tool.
type notMCPTool struct{}

func (notMCPTool) GetToolDefinition() model.ToolDef { return model.ToolDef{} }
func (notMCPTool) CallTool(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

// - Verify HintsOf returns the stored hints for an MCP tool and ok=false for any other tool
func Test_Unit_HintsOf(t *testing.T) {
	if _, ok := HintsOf(notMCPTool{}); ok {
		t.Errorf("expected ok=false for a non-MCP tool")
	}
	mt, err := newMCPTool(&McpConnection{Name: "s"}, &mcp.Tool{Name: "t", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, testMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := HintsOf(mt); !ok || got != (ToolHints{Known: true, ReadOnly: true}) {
		t.Errorf("HintsOf(mcp tool) = (%+v, %v), want read-only hints and ok=true", got, ok)
	}
}
