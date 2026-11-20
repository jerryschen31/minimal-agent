package director

import (
	"errors"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/tools"
)

// - Verify task_done and task_failed set the verdict and end the turn; task_done keeps no advice
// - Verify bad JSON and an empty reason are errors the model sees, and a second verdict is refused (the first stands)
func Test_Unit_VerdictTools(t *testing.T) {
	var v Verdict
	done, failed := NewTaskDone(&v), NewTaskFailed(&v)
	if _, err := failed.CallTool(t.Context(), []byte(`{"reason":"  "}`)); err == nil || errors.Is(err, tools.ErrEndTurn) || v.Given {
		t.Errorf("empty reason: err %v, verdict %+v", err, v)
	}
	if _, err := done.CallTool(t.Context(), []byte(`not json`)); err == nil || v.Given {
		t.Errorf("bad JSON: err %v, verdict %+v", err, v)
	}
	if _, err := failed.CallTool(t.Context(), []byte(`{"reason":" no a ","advice":"write a"}`)); !errors.Is(err, tools.ErrEndTurn) {
		t.Errorf("task_failed: err %v, want ErrEndTurn", err)
	}
	if v != (Verdict{Given: true, Done: false, Reason: "no a", Advice: "write a"}) {
		t.Errorf("verdict = %+v", v)
	}
	if _, err := done.CallTool(t.Context(), []byte(`{"reason":"fine"}`)); err == nil || v.Done {
		t.Errorf("second verdict: err %v, verdict %+v; want refused, the first standing", err, v)
	}

	var v2 Verdict
	if _, err := NewTaskDone(&v2).CallTool(t.Context(), []byte(`{"reason":"a exists","advice":"x"}`)); !errors.Is(err, tools.ErrEndTurn) ||
		v2 != (Verdict{Given: true, Done: true, Reason: "a exists"}) {
		t.Errorf("task_done: %v, %+v", err, v2)
	}
}

// - Verify ToolLog matches results to calls by ID, shows "(no result)" for a call without one, and "(none)" for no calls
// - Verify long arguments and results are cut, and calls past maxLogBytes are left out with a count
func Test_Unit_ToolLog(t *testing.T) {
	call := func(id, name, args string) model.ChatMessage {
		return model.ChatMessage{Role: "assistant", ToolCalls: []model.ToolCall{{ID: id, Function: model.ToolCallFunc{Name: name, Arguments: args}}}}
	}
	result := func(id, content string) model.ChatMessage {
		return model.ChatMessage{Role: "tool", ToolCallID: id, Content: content}
	}

	if got := ToolLog(nil); got != "(none)\n" {
		t.Errorf("no calls: %q", got)
	}
	log := ToolLog([]model.ChatMessage{call("a", "read_file", `{"path":"x"}`), result("a", "hello\nworld"), call("b", "run_command", `{}`)})
	for _, want := range []string{"- read_file {\"path\":\"x\"}\n  → hello\n    world\n", "- run_command {}\n  → (no result)\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	long := strings.Repeat("x", 2000)
	var msgs []model.ChatMessage
	for i := range 30 {
		id := string(rune('a' + i))
		msgs = append(msgs, call(id, "write_file", long), result(id, long))
	}
	log = ToolLog(msgs)
	if len(log) > maxLogBytes+100 || !strings.Contains(log, "x …") || !strings.Contains(log, "more calls not shown") {
		t.Errorf("long log: %d bytes, cut %v, count %v", len(log), strings.Contains(log, "x …"), strings.Contains(log, "more calls not shown"))
	}
}

// - Verify Message holds the task's ID, title, attempt, "done when", body and the worker's summary ("(none)" if empty)
func Test_Unit_Message(t *testing.T) {
	task := plan.Task{ID: "t2", Title: "Test it", DoneWhen: "go test passes", Body: "Run go test."}
	msg := Message(task, 2, 3, "", nil)
	for _, want := range []string{"## Task t2: Test it (attempt 2 of 3)", "Done when: go test passes", "Run go test.",
		"## The worker's summary\n\n(none)", "## The worker's tool calls\n\n(none)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}
