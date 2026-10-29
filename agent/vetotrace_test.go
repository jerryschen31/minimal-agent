package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/trace"
)

// runVetoed runs deleteCall through runToolCall with a recorderSpy installed and returns the spy's events.
func runVetoed(t *testing.T, fx *chatSessionFixture) []scopedEvent {
	t.Helper()
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	runToolCall(context.Background(), fx.Session, trace.StartTask(spy, "t"), deleteCall())
	return spy.events()
}

// vetoes returns the ToolVetoedData of every tool_vetoed event, in order.
func vetoes(events []scopedEvent) []trace.ToolVetoedData {
	var out []trace.ToolVetoedData
	for _, e := range events {
		if e.typ == trace.ToolVetoed {
			out = append(out, e.data.(trace.ToolVetoedData))
		}
	}
	return out
}

// finished returns the single tool_finished event's data, failing the test if there isn't exactly one.
func finished(t *testing.T, events []scopedEvent) trace.ToolFinishedData {
	t.Helper()
	var out []trace.ToolFinishedData
	for _, e := range events {
		if e.typ == trace.ToolFinished {
			out = append(out, e.data.(trace.ToolFinishedData))
		}
	}
	if len(out) != 1 {
		t.Fatalf("expected one tool_finished, got %d", len(out))
	}
	return out[0]
}

// - Verify a veto with no prompt records one tool_vetoed (decision denied, the hook's reason, no wait),
// placed between tool_requested and tool_finished, all in the task's scope
func Test_RunToolCall_VetoTrace_NoPrompt_Denied(t *testing.T) {
	fx, _ := newHookFixture(t, &fakeHook{err: errDenied})
	fx.Session.PromptAllowDenyCall = nil

	events := runVetoed(t, fx)

	vs := vetoes(events)
	if len(vs) != 1 {
		t.Fatalf("expected one tool_vetoed, got %d: %v", len(vs), vs)
	}
	want := trace.ToolVetoedData{CallID: "call_1", Name: "delete_file", Reason: errDenied.Error(), Decision: trace.VetoDenied}
	if vs[0] != want {
		t.Errorf("tool_vetoed = %+v, want %+v", vs[0], want)
	}
	var types []trace.EventType
	var scopes []string
	for _, e := range events {
		if e.typ == trace.ToolRequested || e.typ == trace.ToolVetoed || e.typ == trace.ToolFinished {
			types = append(types, e.typ)
			scopes = append(scopes, e.scope)
		}
	}
	if wantTypes := []trace.EventType{trace.ToolRequested, trace.ToolVetoed, trace.ToolFinished}; !slices.Equal(types, wantTypes) {
		t.Errorf("event order = %v, want %v", types, wantTypes)
	}
	if len(scopes) == 3 && (scopes[0] != scopes[1] || scopes[1] != scopes[2]) {
		t.Errorf("expected the three events in one scope, got %v", scopes)
	}
	if !finished(t, events).Failed {
		t.Error("expected tool_finished to be failed")
	}
}

// - Verify the user saying no records denied_by_user and the call still fails
func Test_RunToolCall_VetoTrace_UserSaysNo(t *testing.T) {
	fx, _ := newHookFixture(t, &fakeHook{err: errDenied})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, false)

	events := runVetoed(t, fx)

	vs := vetoes(events)
	if len(vs) != 1 || vs[0].Decision != trace.VetoDeniedByUser || vs[0].Reason != errDenied.Error() || vs[0].CallID != "call_1" {
		t.Fatalf("tool_vetoed = %+v, want one denied_by_user for call_1 with reason %q", vs, errDenied)
	}
	if !finished(t, events).Failed {
		t.Error("expected tool_finished to be failed")
	}
}

// - Verify the user saying yes records allowed_by_user, the tool runs, and tool_finished is not failed
func Test_RunToolCall_VetoTrace_UserSaysYes(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true)

	events := runVetoed(t, fx)

	vs := vetoes(events)
	if len(vs) != 1 || vs[0].Decision != trace.VetoAllowedByUser || vs[0].Reason != errDenied.Error() {
		t.Fatalf("tool_vetoed = %+v, want one allowed_by_user with reason %q", vs, errDenied)
	}
	fin := finished(t, events)
	if fin.Failed || fin.Content != "deleted" || tool.calls != 1 {
		t.Errorf("tool_finished = %+v, tool ran %d time(s); want not failed, content \"deleted\", one run", fin, tool.calls)
	}
}

// - Verify a call no hook vetoes records no tool_vetoed (with an allowing hook, and with no hooks)
func Test_RunToolCall_VetoTrace_AllowedRecordsNone(t *testing.T) {
	for name, hs := range map[string][]*fakeHook{"allowing hook": {{}}, "no hooks": nil} {
		t.Run(name, func(t *testing.T) {
			fx, _ := newHookFixture(t)
			for _, h := range hs {
				fx.Session.Hooks = append(fx.Session.Hooks, h)
			}
			var asked []askRecord
			fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true)

			events := runVetoed(t, fx)

			if vs := vetoes(events); len(vs) != 0 {
				t.Errorf("expected no tool_vetoed, got %+v", vs)
			}
			if finished(t, events).Failed {
				t.Error("expected tool_finished not failed")
			}
		})
	}
}

// - Verify two vetoing hooks, first approved and second declined, record two events in hook order
func Test_RunToolCall_VetoTrace_TwoHooks_ApproveThenDeny(t *testing.T) {
	errSecond := errors.New(`denied by rule "*file*"`)
	fx, tool := newHookFixture(t, &fakeHook{name: "first", err: errDenied}, &fakeHook{name: "second", err: errSecond})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true, false)

	events := runVetoed(t, fx)

	vs := vetoes(events)
	if len(vs) != 2 {
		t.Fatalf("expected two tool_vetoed, got %d: %+v", len(vs), vs)
	}
	if vs[0].Decision != trace.VetoAllowedByUser || vs[0].Reason != errDenied.Error() {
		t.Errorf("first tool_vetoed = %+v, want allowed_by_user over %q", vs[0], errDenied)
	}
	if vs[1].Decision != trace.VetoDeniedByUser || vs[1].Reason != errSecond.Error() {
		t.Errorf("second tool_vetoed = %+v, want denied_by_user for %q", vs[1], errSecond)
	}
	if tool.calls != 0 || !finished(t, events).Failed {
		t.Errorf("expected the call to fail without running; ran %d time(s)", tool.calls)
	}
}

// - Verify wait_ms measures the time spent waiting for the user's answer
func Test_RunToolCall_VetoTrace_WaitMS(t *testing.T) {
	fx, _ := newHookFixture(t, &fakeHook{err: errDenied})
	fx.Session.PromptAllowDenyCall = func(context.Context, model.ToolCall, error) bool {
		time.Sleep(30 * time.Millisecond)
		return false
	}

	vs := vetoes(runVetoed(t, fx))

	if len(vs) != 1 || vs[0].WaitMS < 20 {
		t.Errorf("tool_vetoed = %+v, want one event with wait_ms >= 20", vs)
	}
}
