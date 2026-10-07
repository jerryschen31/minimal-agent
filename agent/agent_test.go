package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

// panicTool is a Tool whose CallTool always panics with the given behavior.
type panicTool struct{ boom func() }

func (panicTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("boom", "always panics", nil)
}

func (p panicTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	p.boom()
	return "unreachable", nil
}

// - Verify that a tool that panics (explicitly, or via a runtime error) becomes an error
// observation for the model instead of crashing the agent
func Test_RunToolCall_ToolPanics_RecoveredAsErrorMessage(t *testing.T) {
	cases := map[string]struct {
		boom    func()
		wantErr string // substring the message content must contain
	}{
		"explicit panic": {func() { panic("kaboom") }, "kaboom"},
		"runtime error": {func() {
			var m map[string]int
			m["a"] = 1 // write to a nil map panics
		}, "nil map"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newChatSessionFixture(t, 10, 9)
			if err := fx.Tools.Register(panicTool{boom: tc.boom}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			call := model.ToolCall{ID: "call_1", Function: model.ToolCallFunc{Name: "boom", Arguments: "{}"}}

			got := runToolCall(context.Background(), fx.Session, call)

			if got.Role != "tool" {
				t.Errorf("expected role %q, got %q", "tool", got.Role)
			}
			if got.ToolCallID != "call_1" {
				t.Errorf("expected ToolCallID %q, got %q", "call_1", got.ToolCallID)
			}
			if !strings.HasPrefix(got.Content, "error:") || !strings.Contains(got.Content, "tool panicked") ||
				!strings.Contains(got.Content, tc.wantErr) {
				t.Errorf("expected content like %q mentioning %q, got %q", "error: tool panicked: …", tc.wantErr, got.Content)
			}
		})
	}
}
