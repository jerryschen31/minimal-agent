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
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// testMaxTasks is the task limit the tests use (the config default).
const testMaxTasks = 20

// callTool runs a control tool from PhaseTools by name; run is the task the task tools act on.
func callTool(t *testing.T, p *Plan, run *TaskRun, name, args string) (string, error) {
	t.Helper()
	for _, tool := range PhaseTools(p, p.Phase, run) {
		if tool.GetToolDefinition().Function.Name == name {
			return tool.CallTool(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("tool %q is not offered in phase %s", name, p.Phase)
	return "", nil
}

// anyWorker is StartNext's "a worker is free" for tests: always.
func anyWorker(string) bool { return true }

// start starts the next task, failing the test if none can start.
func start(t *testing.T, p *Plan) *TaskRun {
	t.Helper()
	run := p.StartNext(anyWorker)
	if run == nil {
		t.Fatal("StartNext: no task started")
	}
	return run
}

// - Verify the happy path through every phase via the tools, that accepted calls end the turn, and that
// depends_on decides the order (b is listed first but needs a)
func Test_Plan_Tools_PlanWorkReview(t *testing.T) {
	p := NewPlan(nil, testMaxTasks, 1, nil)
	if p.Phase != PhasePlan {
		t.Fatalf("NewPlan(nil).Phase = %s, want plan", p.Phase)
	}
	if _, err := callTool(t, p, nil, "set_tasks", `{"tasks":[{"id":"b","task":"do b","depends_on":["a"]},{"id":"a","task":"do a"}]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("set_tasks err = %v, want ErrEndTurn", err)
	}
	if len(p.Tasks.Tasks) != 2 || p.Phase != PhaseWork || p.Tasks.Tasks[1].WorkerType != "general" {
		t.Fatalf("after set_tasks: %+v", p)
	}
	run := start(t, p)
	if run.Task.ID != "a" || run.Task.Status != tasks.Running || p.StartNext(anyWorker) != nil {
		t.Fatalf("first task = %+v, want a running and b waiting for it", run.Task)
	}
	if out, err := callTool(t, p, run, "task_done", `{"note":"did a"}`); !errors.Is(err, tools.ErrEndTurn) || out != "Task a marked done." {
		t.Fatalf("task_done = %q, %v", out, err)
	}
	if p.Phase != PhaseWork {
		t.Fatalf("after task a: phase %s", p.Phase)
	}
	if _, err := callTool(t, p, run, "task_done", `{"note":"again"}`); err == nil {
		t.Error("task_done on a task that already ended: want error")
	}
	run = start(t, p)
	callTool(t, p, run, "task_done", `{"note":"did b"}`)
	if p.Phase != PhaseReview {
		t.Fatalf("after last task_done: phase %s, want review", p.Phase)
	}
	if _, err := callTool(t, p, nil, "finish", `{"success":true,"summary":"ok","gaps":[]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("finish err = %v", err)
	}
	if p.Phase != PhaseDone || !p.Success || p.Summary != "ok" || p.Tasks.Get("a").Note != "did a" {
		t.Errorf("final plan: %+v", p)
	}
}

// - Verify task_failed is refused once per task while the worker has turns left (also from a later
// director turn, i.e. a fresh PhaseTools call), and the refusal resets for the next task
func Test_Plan_TaskFailed_RefusedOncePerTask(t *testing.T) {
	p := NewPlan([]string{"a", "b"}, testMaxTasks, 1, nil)
	run := start(t, p)
	run.WorkerTurnsLeft = 2
	_, err := callTool(t, p, run, "task_failed", `{"reason":"x"}`)
	if err == nil || errors.Is(err, tools.ErrEndTurn) || !strings.Contains(err.Error(), "2 turn(s)") || p.Phase != PhaseWork {
		t.Fatalf("first call: err = %v, phase = %s; want a refusal", err, p.Phase)
	}
	callTool(t, p, run, "task_done", `{"note":"a done after all"}`)
	run = start(t, p)
	run.WorkerTurnsLeft = 1
	if _, err := callTool(t, p, run, "task_failed", `{"reason":"y"}`); err == nil || p.Failed() {
		t.Fatalf("next task, first call: err = %v; want a fresh refusal", err)
	}
	if _, err := callTool(t, p, run, "task_failed", `{"reason":"y"}`); !errors.Is(err, tools.ErrEndTurn) || !p.Failed() {
		t.Fatalf("second call: err = %v; want it accepted", err)
	}
}

// - Verify the task limit comes from the plan (director.maxTasks), for set_tasks and for a given ## Tasks list
func Test_Plan_MaxTasks_Configurable(t *testing.T) {
	p := NewPlan(nil, 2, 1, nil)
	three := `{"tasks":[{"id":"a","task":"a"},{"id":"b","task":"b"},{"id":"c","task":"c"}]}`
	if _, err := callTool(t, p, nil, "set_tasks", three); err == nil || !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("set_tasks with 3 tasks, limit 2: err = %v", err)
	}
	if desc := PhaseTools(p, p.Phase, nil)[0].GetToolDefinition().Function.Desc; !strings.Contains(desc, "1 to 2 tasks") {
		t.Errorf("set_tasks description doesn't state the limit: %s", desc)
	}
	if !strings.Contains(SystemPrompt(PromptInput{Plan: p}), "1-2 concrete tasks") {
		t.Error("PLAN phase prompt doesn't state the limit")
	}
	if _, err := callTool(t, p, nil, "set_tasks", `{"tasks":[{"id":"a","task":"a"},{"id":"b","task":"b"}]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Errorf("set_tasks with 2 tasks, limit 2: err = %v", err)
	}
	if _, err := ParseObjective([]byte("# Objective\nx\n## Tasks\n- a\n- b\n- c\n"), 2); err == nil || !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("ParseObjective with 3 tasks, limit 2: err = %v", err)
	}
}

// - Verify a failed task moves to review, and finish(success=true) is then refused (not an end-turn)
func Test_Plan_TaskFailed_FinishSuccessRefused(t *testing.T) {
	p := NewPlan([]string{"a", "b"}, testMaxTasks, 1, nil)
	callTool(t, p, start(t, p), "task_failed", `{"reason":"no network"}`)
	if p.Phase != PhaseReview || !p.Failed() || p.Tasks.Get("t1").Note != "no network" {
		t.Fatalf("after task_failed: %+v", p)
	}
	_, err := callTool(t, p, nil, "finish", `{"success":true,"summary":"x","gaps":[]}`)
	if err == nil || errors.Is(err, tools.ErrEndTurn) || !strings.Contains(err.Error(), "not done") {
		t.Fatalf("finish(success=true) err = %v, want a refusal", err)
	}
	if _, err := callTool(t, p, nil, "finish", `{"success":false,"summary":"stopped","gaps":["task b"," "]}`); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("finish(success=false) err = %v", err)
	}
	if len(p.Gaps) != 1 || p.Gaps[0] != "task b" {
		t.Errorf("Gaps = %q", p.Gaps)
	}
}

// - Verify bad arguments and wrong-phase calls are ordinary errors that leave the plan unchanged
func Test_Plan_BadCalls_AreErrors(t *testing.T) {
	p := NewPlan(nil, testMaxTasks, 1, nil)
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
		if _, err := callTool(t, p, nil, "set_tasks", args); err == nil || errors.Is(err, tools.ErrEndTurn) {
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
	if err := p.TaskDone(nil, "x"); err == nil {
		t.Error("TaskDone in plan phase: want error")
	}
	if err := p.Finish(false, "x", nil); err == nil {
		t.Error("Finish in plan phase: want error")
	}

	p = NewPlan([]string{"a"}, testMaxTasks, 1, nil)
	if err := p.TaskDone(&TaskRun{Task: p.Tasks.Get("t1")}, "x"); err == nil {
		t.Error("TaskDone before the task started: want error")
	}
	run := start(t, p)
	if _, err := callTool(t, p, run, "task_failed", `{"reason":" "}`); err == nil {
		t.Error("task_failed with empty reason: want error")
	}
	p.TaskDone(run, "")
	if _, err := callTool(t, p, nil, "finish", `{"summary":"x","gaps":[]}`); err == nil || !strings.Contains(err.Error(), "success") {
		t.Errorf("finish without success: err = %v", err)
	}
	if _, err := callTool(t, p, nil, "finish", `{"success":true,"summary":" ","gaps":[]}`); err == nil {
		t.Error("finish with empty summary: want error")
	}
}

// - Verify each phase offers only its own control tools
func Test_PhaseTools_PerPhase(t *testing.T) {
	names := func(phase Phase) string {
		var n []string
		for _, tool := range PhaseTools(NewPlan(nil, testMaxTasks, 1, nil), phase, nil) {
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

// - Verify parallel bookkeeping: two independent tasks run at once; after one fails no new task starts,
// and the run moves to REVIEW only when the other has ended too (the drain)
func Test_Plan_Parallel_FailureDrains(t *testing.T) {
	p := NewPlan(nil, testMaxTasks, 2, nil)
	err := p.SetTasks([]tasks.Spec{{ID: "a", Task: "a"}, {ID: "b", Task: "b"}, {ID: "c", Task: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := start(t, p), start(t, p)
	if a.Task.ID != "a" || b.Task.ID != "b" {
		t.Fatalf("started %s and %s, want a and b", a.Task.ID, b.Task.ID)
	}
	callTool(t, p, a, "task_failed", `{"reason":"broken"}`)
	if p.Phase != PhaseWork || p.StartNext(anyWorker) != nil {
		t.Fatalf("after a failed: phase %s, or a new task started; want work, and no new task", p.Phase)
	}
	callTool(t, p, b, "task_done", `{"note":"b ok"}`)
	if p.Phase != PhaseReview || p.Tasks.Get("c").Status != tasks.Pending {
		t.Errorf("after the drain: phase %s, c %s; want review, c pending", p.Phase, p.Tasks.Get("c").Status)
	}
}

// - Verify worker types: set_tasks accepts only configured types (empty = the first), StartNext skips a
// ready task whose type has no free worker, and the prompt lists each type with its description
func Test_Plan_WorkerTypes(t *testing.T) {
	p := NewPlan(nil, testMaxTasks, 3, []string{"engineer", "tester"})
	if _, err := callTool(t, p, nil, "set_tasks", `{"tasks":[{"id":"a","task":"x","worker_type":"general"}]}`); err == nil || !strings.Contains(err.Error(), `unknown worker_type "general"`) {
		t.Errorf("unknown type: err = %v", err)
	}
	callTool(t, p, nil, "set_tasks", `{"tasks":[{"id":"a","task":"code a"},{"id":"b","task":"code b"},{"id":"t","task":"test","worker_type":"tester"}]}`)
	if got := p.Tasks.Get("a").WorkerType; got != "engineer" {
		t.Errorf("default type = %q, want engineer", got)
	}
	run := p.StartNext(func(wt string) bool { return wt == "tester" }) // every engineer is busy
	if run == nil || run.Task.ID != "t" {
		t.Fatalf("with only a tester free, started %+v; want task t", run)
	}
	if p.StartNext(func(string) bool { return false }) != nil {
		t.Error("started a task with no free worker")
	}

	got := SystemPrompt(PromptInput{Plan: p, Workers: []WorkerGroup{
		{Type: "engineer", Count: 2, Description: "Writes code.", Info: worker.Info{Model: "m1", Tools: []string{"write_file"}}},
		{Type: "tester", Count: 1, Info: worker.Info{Model: "m2"}},
	}})
	for _, want := range []string{`the default is "engineer"`, "- t [running] test (worker_type: tester)", "- engineer (2 worker(s), model m1): Writes code. Tools: write_file.", "- tester (1 worker(s), model m2) Tools: none"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}
