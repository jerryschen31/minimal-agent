package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
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

			got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), call)

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

	_, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

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

	turn, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if last := turn[len(turn)-1]; last.Content != "done" {
		t.Errorf("expected the final message to be %q, got %+v", "done", last)
	}
}

// stubTool is a Tool that returns a fixed result or error.
type stubTool struct {
	result string
	err    error
}

func (stubTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("stub", "returns a fixed result", nil)
}
func (s stubTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return s.result, s.err
}

// - Verify every tool call, however it ends, records tool_requested then tool_finished under the task's scope, with the same
// call_id, and that tool_finished holds exactly the content the model is sent back.
func Test_RunToolCall_RecordsRequestedAndFinished(t *testing.T) {
	cases := []struct {
		name       string
		tool       tools.Tool
		toolName   string
		args       string
		wantFailed bool
	}{
		{"success", stubTool{result: "42"}, "stub", `{"a":1}`, false},
		{"success, no arguments", stubTool{result: "42"}, "stub", ``, false},
		{"tool error", stubTool{err: errors.New("disk on fire")}, "stub", `{}`, true},
		{"tool panic", panicTool{boom: func() { panic("kaboom") }}, "boom", `{}`, true},
		{"unknown tool", stubTool{}, "no_such_tool", `{}`, true},
		{"invalid JSON", stubTool{}, "stub", `{not json`, true},
		{"arguments not an object", stubTool{}, "stub", `null`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newChatSessionFixture(t, 10, 100)
			spy := &recorderSpy{}
			fx.Session.Recorder = spy
			if err := fx.Tools.Register(tc.tool); err != nil {
				t.Fatalf("Register: %v", err)
			}
			scope := trace.StartTask(spy, "t")
			call := model.ToolCall{ID: "call_1", Function: model.ToolCallFunc{Name: tc.toolName, Arguments: tc.args}}

			msg := runToolCall(context.Background(), fx.Session, scope, call)

			var evs []scopedEvent
			for _, e := range spy.events() {
				if e.typ == trace.ToolRequested || e.typ == trace.ToolFinished {
					evs = append(evs, e)
				}
			}
			if len(evs) != 2 || evs[0].typ != trace.ToolRequested || evs[1].typ != trace.ToolFinished {
				t.Fatalf("expected tool_requested then tool_finished, got %v", evs)
			}
			if evs[0].scope != scope.ID() || evs[1].scope != scope.ID() {
				t.Errorf("tool events scope = %q, %q; want %q", evs[0].scope, evs[1].scope, scope.ID())
			}
			req := evs[0].data.(trace.ToolRequestedData)
			if req.CallID != "call_1" || req.Name != tc.toolName || req.Arguments != tc.args {
				t.Errorf("tool_requested = %+v", req)
			}
			fin := evs[1].data.(trace.ToolFinishedData)
			if fin.CallID != "call_1" || fin.Content != msg.Content || fin.Failed != tc.wantFailed {
				t.Errorf("tool_finished = %+v, message content %q", fin, msg.Content)
			}
		})
	}
}
