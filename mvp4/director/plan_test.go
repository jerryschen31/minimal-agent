package director

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/tools"
)

// testMaxTasks is the task limit the tests use (the config default).
const testMaxTasks = 20

// callTool runs a control tool from PhaseTools by name.
func callTool(t *testing.T, p *Plan, name, args string) (string, error) {
	t.Helper()
	for _, tool := range PhaseTools(p, p.Phase) {
		if tool.GetToolDefinition().Function.Name == name {
			return tool.CallTool(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("tool %q is not offered in phase %s", name, p.Phase)
	return "", nil
}

// start starts the next task, failing the test if none can start.
func start(t *testing.T, p *Plan) *tasks.Task {
	t.Helper()
	task, err := p.StartNext()
	if err != nil {
		t.Fatalf("StartNext: %v", err)
	}
	return task
}

// - Verify the happy path through every phase via the tools, that accepted calls end the turn, and that
// depends_on decides the order (b is listed first but needs a)
func Test_Plan_Tools_PlanWorkReview(t *testing.T) {
	p := NewPlan(nil, testMaxTasks)
	if p.Phase != PhasePlan {
		t.Fatalf("NewPlan(nil).Phase = %s, want plan", p.Phase)
	}
	if _, err := callTool(t, p, "set_tasks", `{"tasks":[{"id":"b","task":"do b","depends_on":["a"]},{"id":"a","task":"do a"}]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("set_tasks err = %v, want ErrEndTurn", err)
	}
	if len(p.Tasks.Tasks) != 2 || p.Phase != PhaseWork || p.Tasks.Tasks[1].WorkerType != "general" {
		t.Fatalf("after set_tasks: %+v", p)
	}
	if got := start(t, p); got.ID != "a" || got.Status != tasks.Running {
		t.Fatalf("first task = %+v, want a running", got)
	}
	if out, err := callTool(t, p, "task_done", `{"note":"did a"}`); !errors.Is(err, tools.ErrEndTurn) || out != "Task a marked done." {
		t.Fatalf("task_done = %q, %v", out, err)
	}
	if p.Phase != PhaseWork || p.Current != nil {
		t.Fatalf("after task a: phase %s, current %v", p.Phase, p.Current)
	}
	start(t, p)
	callTool(t, p, "task_done", `{"note":"did b"}`)
	if p.Phase != PhaseReview {
		t.Fatalf("after last task_done: phase %s, want review", p.Phase)
	}
	if _, err := callTool(t, p, "finish", `{"success":true,"summary":"ok","gaps":[]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("finish err = %v", err)
	}
	if p.Phase != PhaseDone || !p.Success || p.Summary != "ok" || p.Tasks.Get("a").Note != "did a" {
		t.Errorf("final plan: %+v", p)
	}
}

// - Verify task_failed is refused once per task while the worker has turns left (also from a later
// director turn, i.e. a fresh PhaseTools call), and the refusal resets for the next task
func Test_Plan_TaskFailed_RefusedOncePerTask(t *testing.T) {
	p := NewPlan([]string{"a", "b"}, testMaxTasks)
	start(t, p)
	p.WorkerTurnsLeft = 2
	_, err := callTool(t, p, "task_failed", `{"reason":"x"}`)
	if err == nil || errors.Is(err, tools.ErrEndTurn) || !strings.Contains(err.Error(), "2 turn(s)") || p.Phase != PhaseWork {
		t.Fatalf("first call: err = %v, phase = %s; want a refusal", err, p.Phase)
	}
	callTool(t, p, "task_done", `{"note":"a done after all"}`)
	start(t, p)
	if _, err := callTool(t, p, "task_failed", `{"reason":"y"}`); err == nil || p.Failed() {
		t.Fatalf("next task, first call: err = %v; want a fresh refusal", err)
	}
	if _, err := callTool(t, p, "task_failed", `{"reason":"y"}`); !errors.Is(err, tools.ErrEndTurn) || !p.Failed() {
		t.Fatalf("second call: err = %v; want it accepted", err)
	}
}

// - Verify the task limit comes from the plan (director.maxTasks), for set_tasks and for a given ## Tasks list
func Test_Plan_MaxTasks_Configurable(t *testing.T) {
	p := NewPlan(nil, 2)
	three := `{"tasks":[{"id":"a","task":"a"},{"id":"b","task":"b"},{"id":"c","task":"c"}]}`
	if _, err := callTool(t, p, "set_tasks", three); err == nil || !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("set_tasks with 3 tasks, limit 2: err = %v", err)
	}
	if desc := PhaseTools(p, p.Phase)[0].GetToolDefinition().Function.Desc; !strings.Contains(desc, "1 to 2 tasks") {
		t.Errorf("set_tasks description doesn't state the limit: %s", desc)
	}
	if !strings.Contains(SystemPrompt(PromptInput{Plan: p}), "1-2 concrete tasks") {
		t.Error("PLAN phase prompt doesn't state the limit")
	}
	if _, err := callTool(t, p, "set_tasks", `{"tasks":[{"id":"a","task":"a"},{"id":"b","task":"b"}]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Errorf("set_tasks with 2 tasks, limit 2: err = %v", err)
	}
	if _, err := ParseObjective([]byte("# Objective\nx\n## Tasks\n- a\n- b\n- c\n"), 2); err == nil || !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("ParseObjective with 3 tasks, limit 2: err = %v", err)
	}
}

// - Verify a failed task moves to review, and finish(success=true) is then refused (not an end-turn)
func Test_Plan_TaskFailed_FinishSuccessRefused(t *testing.T) {
	p := NewPlan([]string{"a", "b"}, testMaxTasks)
	start(t, p)
	callTool(t, p, "task_failed", `{"reason":"no network"}`)
	if p.Phase != PhaseReview || !p.Failed() || p.Tasks.Get("t1").Note != "no network" {
		t.Fatalf("after task_failed: %+v", p)
	}
	_, err := callTool(t, p, "finish", `{"success":true,"summary":"x","gaps":[]}`)
	if err == nil || errors.Is(err, tools.ErrEndTurn) || !strings.Contains(err.Error(), "not done") {
		t.Fatalf("finish(success=true) err = %v, want a refusal", err)
	}
	if _, err := callTool(t, p, "finish", `{"success":false,"summary":"stopped","gaps":["task b"," "]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("finish(success=false) err = %v", err)
	}
	if len(p.Gaps) != 1 || p.Gaps[0] != "task b" {
		t.Errorf("Gaps = %q", p.Gaps)
	}
}

// - Verify bad arguments and wrong-phase calls are ordinary errors that leave the plan unchanged
func Test_Plan_BadCalls_AreErrors(t *testing.T) {
	p := NewPlan(nil, testMaxTasks)
	for _, args := range []string{
		`{"tasks":[]}`, `{"tasks":["a"]}`, `{"tasks":"a"}`, `not json`,
		`{"tasks":[{"id":"a","task":" "}]}`,                                                             // empty text
		`{"tasks":[{"id":"","task":"x"}]}`,                                                              // no id
		`{"tasks":[{"id":"a","task":"x"},{"id":"a","task":"y"}]}`,                                       // duplicate id
		`{"tasks":[{"id":"a","task":"x","depends_on":["zz"]}]}`,                                         // unknown dependency
		`{"tasks":[{"id":"a","task":"x","depends_on":["a"]}]}`,                                          // depends on itself
		`{"tasks":[{"id":"a","task":"x","worker_type":"gpu"}]}`,                                         // unknown worker type
		`{"tasks":[{"id":"a","task":"x","depends_on":["b"]},{"id":"b","task":"y","depends_on":["a"]}]}`, // cycle
	} {
		if _, err := callTool(t, p, "set_tasks", args); err == nil || errors.Is(err, tools.ErrEndTurn) {
			t.Errorf("set_tasks(%s) err = %v, want an error", args, err)
		}
	}
	tooMany := make([]tasks.Spec, testMaxTasks+1)
	for i := range tooMany {
		tooMany[i] = tasks.Spec{ID: fmt.Sprint(i), Task: "t"}
	}
	if err := p.SetTasks(tooMany); err == nil {
		t.Error("SetTasks with too many tasks: want error")
	}
	if p.Phase != PhasePlan {
		t.Fatalf("phase changed to %s after bad calls", p.Phase)
	}
	if err := p.TaskDone("x"); err == nil {
		t.Error("TaskDone in plan phase: want error")
	}
	if err := p.Finish(false, "x", nil); err == nil {
		t.Error("Finish in plan phase: want error")
	}

	p = NewPlan([]string{"a"}, testMaxTasks)
	if err := p.TaskDone("x"); err == nil {
		t.Error("TaskDone before any task started: want error")
	}
	start(t, p)
	if _, err := callTool(t, p, "task_failed", `{"reason":" "}`); err == nil {
		t.Error("task_failed with empty reason: want error")
	}
	p.TaskDone("")
	if _, err := callTool(t, p, "finish", `{"summary":"x","gaps":[]}`); err == nil || !strings.Contains(err.Error(), "success") {
		t.Errorf("finish without success: err = %v", err)
	}
	if _, err := callTool(t, p, "finish", `{"success":true,"summary":" ","gaps":[]}`); err == nil {
		t.Error("finish with empty summary: want error")
	}
}

// - Verify each phase offers only its own control tools
func Test_PhaseTools_PerPhase(t *testing.T) {
	names := func(phase Phase) string {
		var n []string
		for _, tool := range PhaseTools(NewPlan(nil, testMaxTasks), phase) {
			n = append(n, tool.GetToolDefinition().Function.Name)
		}
		return strings.Join(n, ",")
	}
	for phase, want := range map[Phase]string{PhasePlan: "set_tasks", PhaseWork: "task_done,task_failed", PhaseReview: "finish", PhaseDone: ""} {
		if got := names(phase); got != want {
			t.Errorf("PhaseTools(%s) = %q, want %q", phase, got, want)
		}
	}
}
