package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
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

// - Verify run_end is recorded by ShutdownAgent after background work has drained: a compaction still running when
// RunAgent returns must land its event before run_end, and RunAgent alone must not end the run
func Test_Shutdown_RecordsRunEndAfterBackgroundCompaction(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	a := &Agent{session: fx.Session, agentMode: "chat"}

	if err := a.RunAgent(context.Background(), nil); err != nil { // empty input: the loop ends at EOF
		t.Fatal(err)
	}
	if slices.Contains(spy.eventTypes(), trace.RunEnd) {
		t.Fatalf("RunAgent recorded run_end before shutdown: %v", spy.eventTypes())
	}

	release := make(chan struct{})
	fx.Context.CompactWG.Add(1)
	go func() { // stands in for an auto-compaction that is still waiting on the model
		defer fx.Context.CompactWG.Done()
		<-release
		spy.Record(spy.RunID(), trace.Compaction, trace.CompactionData{Trigger: "auto"})
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.ShutdownAgent(context.Background())
	}()
	close(release)
	<-done

	types := spy.eventTypes()
	want := []trace.EventType{trace.RunStart, trace.Compaction, trace.RunEnd}
	if !slices.Equal(types, want) {
		t.Errorf("events = %v, want %v", types, want)
	}
	a.ShutdownAgent(context.Background()) // a second shutdown writes nothing more
	if len(spy.eventTypes()) != len(want) {
		t.Errorf("second ShutdownAgent recorded more events: %v", spy.eventTypes())
	}
}

// - Verify a non-positive mcpResultMaxBytes fails setup instead of silently dropping every MCP tool
func Test_SetupAgent_RejectsNonPositiveMCPResultCap(t *testing.T) {
	for _, n := range []int{0, -1} {
		var a Agent
		err := a.SetupAgent(context.Background(), config.Config{MaxSteps: 10, MCPResultMaxBytes: n})
		if err == nil || !strings.Contains(err.Error(), "mcpResultMaxBytes") {
			t.Errorf("MCPResultMaxBytes %d: err = %v, want an mcpResultMaxBytes error", n, err)
		}
	}
}
