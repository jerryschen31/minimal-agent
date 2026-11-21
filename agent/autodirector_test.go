package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/agent/director"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/state"
)

// autoProvider sends the director's calls (system prompt director.Prompt) to director and every other call to worker.
type autoProvider struct {
	worker   *modeltest.FakeProvider
	director *modeltest.FakeProvider // nil: every director call answers task_done "checked"
	calls    int                     // director calls
	msgs     [][]model.ChatMessage   // each director call's messages
	toolDefs [][]model.ToolDef       // each director call's tools
}

// Chat routes the call by its system prompt.
func (p *autoProvider) Chat(ctx context.Context, msgs []model.ChatMessage, toolDefs []model.ToolDef) (model.Response, error) {
	if len(msgs) == 0 || msgs[0].Content != director.Prompt {
		return p.worker.Chat(ctx, msgs, toolDefs)
	}
	p.calls++
	p.msgs = append(p.msgs, slices.Clone(msgs))
	p.toolDefs = append(p.toolDefs, slices.Clone(toolDefs))
	if p.director == nil {
		return model.Response{Message: modeltest.AssistantToolCall(fmt.Sprintf("v%d", p.calls), "task_done", `{"reason":"checked"}`)}, nil
	}
	return p.director.Chat(ctx, msgs, toolDefs)
}

// directorOf returns fx's autoProvider (newAutoFixture sets it).
func directorOf(fx *chatSessionFixture) *autoProvider { return fx.Session.Provider.(*autoProvider) }

// scriptDirector makes the director of fx play replies in order.
func scriptDirector(fx *chatSessionFixture, replies ...model.ChatMessage) {
	directorOf(fx).director = &modeltest.FakeProvider{Script: replies}
}

// verdict returns a director reply calling tool ("task_done" or "task_failed") with reason and advice.
func verdict(tool, reason, advice string) model.ChatMessage {
	args, _ := json.Marshal(map[string]string{"reason": reason, "advice": advice})
	return modeltest.AssistantToolCall("v-"+reason, tool, string(args))
}

// contentsTool is a tool named name whose result is "contents of <name>".
type contentsTool struct{ name string }

// GetToolDefinition returns the stub's definition.
func (s contentsTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(s.name, "stub", json.RawMessage(`{"type":"object","properties":{}}`))
}

// CallTool returns "contents of <name>".
func (s contentsTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return "contents of " + s.name, nil
}

// oneTask is a hand-written plan with a single task.
const oneTask = "# Plan: One\n\n## Tasks\n\n### t1: Make a\ndone when: a exists\n\nWrite a.\n"

// newOneTaskFixture writes oneTask to plan.md in a new temp dir (also the cwd) and returns an auto fixture for it.
func newOneTaskFixture(t *testing.T) *chatSessionFixture {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.WriteFile("plan.md", []byte(oneTask), 0o644); err != nil {
		t.Fatal(err)
	}
	fx, _ := newAutoFixture(t, "plan.md")
	return fx
}

//   - Verify a task the director judges not done is tried again with a fresh worker that is told why and the advice,
//     and is done when the director says so; the journal keeps both attempts (attempt 1 and 2, the first with its
//     verdict and advice) and state reads done with 2 attempts
func Test_Unit_AutoRun_RetryThenDone(t *testing.T) {
	fx := newThreeTaskFixture(t)
	scriptDirector(fx, verdict("task_failed", "a is empty", "write abc into a"),
		verdict("task_done", "a holds abc", ""), verdict("task_done", "ok", ""), verdict("task_done", "ok", ""))
	out, err := runAutoYes(t, fx)
	if err != nil {
		t.Fatalf("autoMode: %v\n%s", err, out)
	}
	for _, want := range []string{"not done — a is empty (attempt 1 of 3)", "▶ t1: First (attempt 2 of 3)", "✓ t1 done — a holds abc", "Finished: 3 done"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(fx.Provider.Calls) != 4 {
		t.Fatalf("%d worker calls, want 4", len(fx.Provider.Calls))
	}
	retry := fx.Provider.Calls[1]
	if len(retry) != 2 || !strings.Contains(retry[1].Content, "## Attempt 1 was judged not done") ||
		!strings.Contains(retry[1].Content, "a is empty") || !strings.Contains(retry[1].Content, "Advice: write abc into a") {
		t.Errorf("the retry's message (fresh context: %d messages) lacks the verdict:\n%s", len(retry), retry[len(retry)-1].Content)
	}
	events := withoutToolEvents(journal(t, fx, 1))
	want := []string{"manifest", "task_started:t1", "task_finished:t1", "task_started:t1", "task_finished:t1"}
	if got := eventTypes(events); !slices.Equal(got[:5], want) {
		t.Errorf("journal starts %v, want %v", got[:5], want)
	}
	var started state.TaskStarted
	var first state.TaskFinished
	json.Unmarshal(events[3].Data, &started)
	json.Unmarshal(events[2].Data, &first)
	if started.Attempt != 2 || first.Status != state.TaskFailed || first.Verdict != "a is empty Advice: write abc into a" {
		t.Errorf("attempt 2 started = %+v; attempt 1 finished = %+v", started, first)
	}
	home, _ := state.OpenHome(fx.Session.Config.StateDir)
	ref, _ := state.NewPlanRef("", "plan.md")
	if s, _ := home.Load(ref); s.Tasks["t1"].Status != state.TaskDone || s.Tasks["t1"].Attempts != 2 || s.Tasks["t1"].Verdict != "a holds abc" {
		t.Errorf("t1 = %+v, want done after 2 attempts", s.Tasks["t1"])
	}
}

//   - Verify a task judged not done maxTaskAttempts times fails: the run stops with "task_failed", the error names the
//     task and the last reason, and the later tasks are not started
func Test_Unit_AutoRun_NotDoneEveryAttempt(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Session.Config.MaxTaskAttempts = 2
	scriptDirector(fx, verdict("task_failed", "no a", ""), verdict("task_failed", "still no a", ""))
	out, err := runAutoYes(t, fx)
	if err == nil || !strings.Contains(err.Error(), "t1 not done after 2 attempts: still no a") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out, "✗ t1 failed after 2 attempts") || !strings.Contains(out, "Stopped: task_failed. 0 done, 0 skipped, 1 failed, 2 not started.") {
		t.Errorf("output:\n%s", out)
	}
	if len(fx.Provider.Calls) != 2 {
		t.Errorf("%d worker calls, want 2", len(fx.Provider.Calls))
	}
}

//   - Verify the director's first call: its own system prompt, a message with the task's "done when", the worker's
//     summary and its tool calls with their results; its tools are the verdict tools and the session's read tools,
//     never a write tool (a director call to write_file is refused as an unknown tool)
func Test_Unit_AutoRun_DirectorSeesTheWork(t *testing.T) {
	fx := newOneTaskFixture(t)
	for _, name := range []string{"read_file", "write_file"} {
		if err := fx.Session.Tools.Register(contentsTool{name}); err != nil {
			t.Fatal(err)
		}
	}
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("w1", "read_file", `{"path":"a"}`), modeltest.AssistantText("made a"),
	}
	scriptDirector(fx, modeltest.AssistantToolCall("d1", "write_file", `{}`), verdict("task_done", "a exists", ""))
	if out, err := runAutoYes(t, fx); err != nil {
		t.Fatalf("autoMode: %v\n%s", err, out)
	}
	d := directorOf(fx)
	if d.calls != 2 {
		t.Fatalf("%d director calls, want 2", d.calls)
	}
	msg := d.msgs[0][1].Content
	for _, want := range []string{"Done when: a exists", "## The worker's summary\n\nmade a", `- read_file {"path":"a"}`, "→ contents of read_file"} {
		if !strings.Contains(msg, want) {
			t.Errorf("director's message lacks %q:\n%s", want, msg)
		}
	}
	var names []string
	for _, def := range d.toolDefs[0] {
		names = append(names, def.Function.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"read_file", "task_done", "task_failed"}) {
		t.Errorf("director's tools = %v", names)
	}
	last := d.msgs[1][len(d.msgs[1])-1]
	if last.Role != "tool" || !strings.Contains(last.Content, `unknown tool "write_file"`) {
		t.Errorf("director's write_file call got %q, want an unknown-tool error", last.Content)
	}
}

//   - Verify a director that replies without a verdict is reminded once (director.Reminder) and then the attempt counts
//     as not done ("the director gave no verdict")
func Test_Unit_AutoRun_DirectorWithoutVerdict(t *testing.T) {
	fx := newOneTaskFixture(t)
	fx.Session.Config.MaxTaskAttempts = 1
	scriptDirector(fx, modeltest.AssistantText("looks fine"), modeltest.AssistantText("really fine"))
	out, err := runAutoYes(t, fx)
	if err == nil || !strings.Contains(out, "not done — the director gave no verdict") {
		t.Errorf("err = %v:\n%s", err, out)
	}
	d := directorOf(fx)
	if d.calls != 2 || d.msgs[1][len(d.msgs[1])-1].Content != director.Reminder {
		t.Errorf("%d director calls; the second's last message should be the reminder", d.calls)
	}
}

//   - Verify a director whose model call fails stops the run with "error" and no retry, and the journal records the
//     task as failed with the worker's summary
func Test_Unit_AutoRun_DirectorError(t *testing.T) {
	fx := newOneTaskFixture(t)
	directorOf(fx).director = &modeltest.FakeProvider{Err: errors.New("boom")}
	fx.Provider.Reply = "made a"
	out, err := runAutoYes(t, fx)
	if err == nil || !strings.Contains(err.Error(), "the director failed") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	if len(fx.Provider.Calls) != 1 || !strings.Contains(out, "Stopped: error.") {
		t.Errorf("%d worker calls, want 1 (no retry):\n%s", len(fx.Provider.Calls), out)
	}
	events := journal(t, fx, 1)
	var fin state.TaskFinished
	if err := json.Unmarshal(events[2].Data, &fin); err != nil || fin.Status != state.TaskFailed || fin.Summary != "made a" {
		t.Errorf("task_finished = %+v (%s)", fin, events[2].Type)
	}
}
