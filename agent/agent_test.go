package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
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

// - Verify the ReAct loop stops after cfg.MaxSteps provider calls and returns ErrMaxSteps when the
// model keeps asking for tools (an unknown tool becomes an error observation, so the loop continues)
func Test_ReActLoop_MaxStepsFromConfig_StopsAtLimit(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.MaxSteps = 2
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("c1", "no_such_tool", "{}"),
		modeltest.AssistantToolCall("c2", "no_such_tool", "{}"),
		modeltest.AssistantToolCall("c3", "no_such_tool", "{}"),
	}

	_, err := reActLoop(context.Background(), fx.Session, nil, modeltest.Msg("user", "u1", "hi"))

	if !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("expected ErrMaxSteps, got %v", err)
	}
	if len(fx.Provider.Calls) != 2 {
		t.Errorf("expected exactly MaxSteps (2) provider calls, got %d", len(fx.Provider.Calls))
	}
}

// - Verify a turn that reaches a final answer within cfg.MaxSteps succeeds
func Test_ReActLoop_MaxStepsFromConfig_FinishesWithinLimit(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.MaxSteps = 2
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("c1", "no_such_tool", "{}"),
		modeltest.AssistantText("done"),
	}

	turn, err := reActLoop(context.Background(), fx.Session, nil, modeltest.Msg("user", "u1", "hi"))

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if last := turn[len(turn)-1]; last.Content != "done" {
		t.Errorf("expected the final message to be %q, got %+v", "done", last)
	}
}
