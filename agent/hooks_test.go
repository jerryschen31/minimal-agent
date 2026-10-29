package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
)

// fakeHook is a hooks.Hook that records every call it sees and returns err (nil = allow).
// order, if set, is shared between hooks so a test can check the order they were called in.
type fakeHook struct {
	name  string
	err   error
	seen  []model.ToolCall
	order *[]string
}

func (h *fakeHook) BeforeTool(ctx context.Context, tc model.ToolCall) error {
	h.seen = append(h.seen, tc)
	if h.order != nil {
		*h.order = append(*h.order, h.name)
	}
	return h.err
}

// countingTool is a Tool named "delete_file" that counts how often CallTool runs.
type countingTool struct{ calls int }

func (*countingTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("delete_file", "pretends to delete a file", nil)
}
func (c *countingTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	c.calls++
	return "deleted", nil
}

var errDenied = errors.New(`denied by rule "*delete*"`)

// newHookFixture returns a session with countingTool registered and the given hooks installed.
func newHookFixture(t *testing.T, hs ...hooks.Hook) (*chatSessionFixture, *countingTool) {
	t.Helper()
	fx := newChatSessionFixture(t, 10, 100)
	tool := &countingTool{}
	if err := fx.Tools.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	fx.Session.Hooks = hs
	return fx, tool
}

func deleteCall() model.ToolCall {
	return model.ToolCall{ID: "call_1", Function: model.ToolCallFunc{Name: "delete_file", Arguments: `{"path":"a.txt"}`}}
}

// - Verify a vetoing hook stops the tool from running, its error becomes the tool observation,
// and tool_finished records the call as failed
func Test_RunToolCall_HookVetoes_ToolNotRunAndErrorObserved(t *testing.T) {
	hook := &fakeHook{err: errDenied}
	fx, tool := newHookFixture(t, hook)
	spy := &recorderSpy{}
	fx.Session.Recorder = spy

	got := runToolCall(context.Background(), fx.Session, trace.StartTask(spy, "t"), deleteCall())

	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, but CallTool ran %d time(s)", tool.calls)
	}
	if want := `error: denied by rule "*delete*"`; got.Content != want {
		t.Errorf("expected content %q, got %q", want, got.Content)
	}
	if got.ToolCallID != "call_1" {
		t.Errorf("expected ToolCallID %q, got %q", "call_1", got.ToolCallID)
	}
	var fin *trace.ToolFinishedData
	for _, e := range spy.events() {
		if e.typ == trace.ToolFinished {
			d := e.data.(trace.ToolFinishedData)
			fin = &d
		}
	}
	if fin == nil {
		t.Fatal("expected a tool_finished event, got none")
	}
	if fin.CallID != "call_1" || !fin.Failed || fin.Content != got.Content {
		t.Errorf("tool_finished = %+v; want CallID call_1, Failed true, Content %q", *fin, got.Content)
	}
}

// - Verify an allowing hook sees the call (name and arguments) and the tool then runs normally
func Test_RunToolCall_HookAllows_ToolRuns(t *testing.T) {
	hook := &fakeHook{}
	fx, tool := newHookFixture(t, hook)

	got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

	if tool.calls != 1 || got.Content != "deleted" {
		t.Errorf("expected the tool to run once and return %q; ran %d time(s), content %q", "deleted", tool.calls, got.Content)
	}
	if len(hook.seen) != 1 {
		t.Fatalf("expected the hook to be called once, got %d", len(hook.seen))
	}
	if seen := hook.seen[0].Function; seen.Name != "delete_file" || seen.Arguments != `{"path":"a.txt"}` {
		t.Errorf("hook saw %+v; want name delete_file, arguments {\"path\":\"a.txt\"}", seen)
	}
}

// - Verify hooks run in order and the first veto wins: later hooks are not asked
func Test_RunToolCall_MultipleHooks(t *testing.T) {
	t.Run("first vetoes", func(t *testing.T) {
		var order []string
		first := &fakeHook{name: "first", err: errDenied, order: &order}
		second := &fakeHook{name: "second", order: &order}
		fx, tool := newHookFixture(t, first, second)

		got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

		if !slices.Equal(order, []string{"first"}) {
			t.Errorf("expected only the first hook to be called, got %v", order)
		}
		if tool.calls != 0 || !strings.HasPrefix(got.Content, "error:") {
			t.Errorf("expected a denied call; tool ran %d time(s), content %q", tool.calls, got.Content)
		}
	})
	t.Run("both allow", func(t *testing.T) {
		var order []string
		first := &fakeHook{name: "first", order: &order}
		second := &fakeHook{name: "second", order: &order}
		fx, tool := newHookFixture(t, first, second)

		got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

		if !slices.Equal(order, []string{"first", "second"}) {
			t.Errorf("expected both hooks in order, got %v", order)
		}
		if tool.calls != 1 || got.Content != "deleted" {
			t.Errorf("expected the tool to run; ran %d time(s), content %q", tool.calls, got.Content)
		}
	})
}

// - Verify a session with no hooks (nil Hooks) runs tools as before
func Test_RunToolCall_NoHooks_ToolRuns(t *testing.T) {
	fx, tool := newHookFixture(t) // fx.Session.Hooks is nil

	got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

	if tool.calls != 1 || got.Content != "deleted" {
		t.Errorf("expected the tool to run once and return %q; ran %d time(s), content %q", "deleted", tool.calls, got.Content)
	}
}

// - Verify hooks are not asked about calls that already failed: an unknown tool or bad arguments
func Test_RunToolCall_HooksSkippedForInvalidCalls(t *testing.T) {
	cases := map[string]model.ToolCallFunc{
		"unknown tool": {Name: "no_such_tool", Arguments: `{}`},
		"invalid JSON": {Name: "delete_file", Arguments: `{not json`},
		"not object":   {Name: "delete_file", Arguments: `null`},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			hook := &fakeHook{err: errDenied}
			fx, tool := newHookFixture(t, hook)
			call := model.ToolCall{ID: "call_1", Function: fn}

			got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), call)

			if len(hook.seen) != 0 {
				t.Errorf("expected the hook not to be called, got %d call(s)", len(hook.seen))
			}
			if tool.calls != 0 || !strings.HasPrefix(got.Content, "error:") || strings.Contains(got.Content, "denied") {
				t.Errorf("expected the earlier validation error; tool ran %d time(s), content %q", tool.calls, got.Content)
			}
		})
	}
}

// - Verify, through the whole ReAct loop, that a denied call's error reaches the model's next
// request (so it can recover) and the turn still finishes with the model's final answer
func Test_ReActLoop_HookVeto_ModelSeesErrorAndFinishes(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "delete_file", `{"path":"a.txt"}`),
		modeltest.AssistantText("I was not allowed to delete it."),
	}

	turn, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "delete a.txt"))

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
	if len(fx.Provider.Calls) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(fx.Provider.Calls))
	}
	second := fx.Provider.Calls[1]
	last := second[len(second)-1]
	if last.Role != "tool" || last.ToolCallID != "call_1" || last.Content != `error: denied by rule "*delete*"` {
		t.Errorf("expected the second request to end with the denial observation, got %+v", last)
	}
	if final := turn[len(turn)-1]; final.Content != "I was not allowed to delete it." {
		t.Errorf("expected the final message to be the model's answer, got %+v", final)
	}
}
