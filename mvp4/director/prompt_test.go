package director

import (
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// - Verify the system prompt carries the instructions, extra text, worker tools, objective sections,
// task statuses with notes, and the current phase
func Test_SystemPrompt_Contents(t *testing.T) {
	obj := Objective{Text: "Ship it.", Sections: []Section{{"Constraints", "No deletes."}}}
	p := NewPlan(nil, testMaxTasks, 1, nil)
	p.SetTasks([]tasks.Spec{{ID: "t1", Task: "first"}, {ID: "t2", Task: "second", DependsOn: []string{"t1"}}})
	p.TaskDone(p.StartNext(anyWorker), "found X")
	run := p.StartNext(anyWorker)
	got := SystemPrompt(PromptInput{Objective: obj, Plan: p, Current: run.Task, Workers: []WorkerGroup{{Type: worker.DefaultType, Count: 1, Info: worker.Info{Tools: []string{"read_file", "write_file"}}}}, MaxTurnsPerTask: 4, Extra: "Be brief."})
	for _, want := range []string{
		Instructions, "Additional instructions:\nBe brief.", `One worker type, "general"`, "read_file, write_file.", "at most 4 turns",
		"## Objective\nShip it.", "## Constraints\nNo deletes.",
		"- t1 [done] first\n   note: found X", "- t2 [running] second (after t1)", "WORK, task t2.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q:\n%s", want, got)
		}
	}
}

// - Verify the phase text for PLAN and REVIEW (with a Done section)
func Test_SystemPrompt_PlanAndReviewPhases(t *testing.T) {
	if got := SystemPrompt(PromptInput{Plan: NewPlan(nil, testMaxTasks, 1, nil)}); !strings.Contains(got, "PLAN.") || !strings.Contains(got, "(no tasks yet)") {
		t.Errorf("plan phase prompt:\n%s", got)
	}
	p := NewPlan([]string{"a"}, testMaxTasks, 1, nil)
	p.TaskDone(p.StartNext(anyWorker), "")
	obj := Objective{Text: "x", Sections: []Section{{"Done when", "- y"}}}
	if got := SystemPrompt(PromptInput{Objective: obj, Plan: p}); !strings.Contains(got, "REVIEW. Check the results against the objective and its Done criteria") {
		t.Errorf("review phase prompt:\n%s", got)
	}
}

// - Verify WrapTurn includes the reply, tools and error, caps long replies, and warns on the last turn
func Test_WrapTurn(t *testing.T) {
	task := &tasks.Task{ID: "t1", Text: "read it"}
	ok := func(reply string, toolsCalled ...string) worker.Result {
		return worker.Result{Status: worker.StatusCompleted, Reply: reply, ToolsCalled: toolsCalled}
	}
	got := WrapTurn(task, WorkerOutcome{Turn: 1, MaxTurns: 5, Result: ok("contents", "read_file")})
	for _, want := range []string{"Task t1: read it", "turn 1 of 5 for this task. Status: completed", "Worker reply:\ncontents", "Tools called: read_file"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "last worker turn") {
		t.Error("last-turn warning shown on turn 1 of 5")
	}

	long := strings.Repeat("é", MaxReplyChars) // 2 bytes each, so the cut lands mid-character
	got = WrapTurn(task, WorkerOutcome{Turn: 5, MaxTurns: 5, Result: ok(long)})
	if !strings.Contains(got, "reply truncated") || !strings.Contains(got, "last worker turn") || strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("long reply not capped cleanly:\n%.200s", got)
	}

	got = WrapTurn(task, WorkerOutcome{Turn: 1, MaxTurns: 2, Result: worker.Result{Status: worker.StatusStepLimit, Error: "max steps"}})
	if !strings.Contains(got, "Status: step_limit") || !strings.Contains(got, "failed with an error: max steps") || !strings.Contains(got, "files it wrote or changed are kept") || strings.Contains(got, "Tools called") {
		t.Errorf("error outcome:\n%s", got)
	}
}

// - Verify the review prompt names a failed task, or says all tasks are done
func Test_ReviewPrompt(t *testing.T) {
	p := NewPlan([]string{"a", "b"}, testMaxTasks, 1, nil)
	p.TaskDone(p.StartNext(anyWorker), "")
	p.TaskFailed(p.StartNext(anyWorker), "disk full")
	if got := ReviewPrompt(p, false, 0); !strings.Contains(got, "task t2 failed: disk full") {
		t.Errorf("failed review prompt: %s", got)
	}
	p = NewPlan([]string{"a"}, testMaxTasks, 1, nil)
	p.TaskDone(p.StartNext(anyWorker), "")
	if got := ReviewPrompt(p, true, 0); !strings.Contains(got, "All 1 tasks are done") || !strings.Contains(got, "Done criteria") {
		t.Errorf("success review prompt: %s", got)
	}
}

// - Verify the worker count and the planning hint appear only when tasks run in parallel, and that given
// tasks are chained then
func Test_Prompts_Parallel(t *testing.T) {
	serial, par := NewPlan(nil, testMaxTasks, 1, nil), NewPlan(nil, testMaxTasks, 3, nil)
	if got := SystemPrompt(PromptInput{Plan: serial}); strings.Contains(got, "workers") || PlanPrompt(1) != PlanPrompt(0) || strings.Contains(PlanPrompt(1), "same time") {
		t.Errorf("serial prompts mention parallel work:\n%s\n%s", got, PlanPrompt(1))
	}
	if got := SystemPrompt(PromptInput{Plan: par}); !strings.Contains(got, "There are 3 workers, so up to 3 tasks run at the same time") {
		t.Errorf("parallel system prompt:\n%s", got)
	}
	if got := PlanPrompt(3); !strings.Contains(got, "Up to 3 tasks can run at the same time") || !strings.Contains(got, "go.mod") {
		t.Errorf("parallel plan prompt: %s", got)
	}
	if got := GapsPlanPrompt(par, 2, 3); !strings.Contains(got, "Up to 3 tasks") {
		t.Errorf("parallel gaps prompt: %s", got)
	}
	if got := SystemPrompt(PromptInput{Plan: NewPlan([]string{"a", "b"}, testMaxTasks, 2, nil)}); !strings.Contains(got, "- t2 [pending] b (after t1)") {
		t.Errorf("given tasks not chained with parallel > 1:\n%s", got)
	}
}

// - Verify the review prompt mentions further rounds only when some are left
func Test_ReviewPrompt_RoundsLeft(t *testing.T) {
	p := NewPlan([]string{"a"}, testMaxTasks, 1, nil)
	if got := ReviewPrompt(p, false, 0); strings.Contains(got, "new round") {
		t.Errorf("no rounds left, but:\n%s", got)
	}
	if got := ReviewPrompt(p, false, 2); !strings.Contains(got, "2 more round(s)") || !strings.Contains(got, "no nice-to-haves") {
		t.Errorf("2 rounds left:\n%s", got)
	}
}
