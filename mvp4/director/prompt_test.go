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
	p := NewPlan(nil, testMaxTasks)
	p.SetTasks([]tasks.Spec{{ID: "t1", Task: "first"}, {ID: "t2", Task: "second", DependsOn: []string{"t1"}}})
	p.StartNext()
	p.TaskDone("found X")
	p.StartNext()
	got := SystemPrompt(PromptInput{Objective: obj, Plan: p, Worker: worker.Info{Tools: []string{"read_file", "write_file"}}, MaxTurnsPerTask: 4, Extra: "Be brief."})
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
	if got := SystemPrompt(PromptInput{Plan: NewPlan(nil, testMaxTasks)}); !strings.Contains(got, "PLAN.") || !strings.Contains(got, "(no tasks yet)") {
		t.Errorf("plan phase prompt:\n%s", got)
	}
	p := NewPlan([]string{"a"}, testMaxTasks)
	p.StartNext()
	p.TaskDone("")
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
	p := NewPlan([]string{"a", "b"}, testMaxTasks)
	p.StartNext()
	p.TaskDone("")
	p.StartNext()
	p.TaskFailed("disk full")
	if got := ReviewPrompt(p, false, 0); !strings.Contains(got, "task t2 failed: disk full") {
		t.Errorf("failed review prompt: %s", got)
	}
	p = NewPlan([]string{"a"}, testMaxTasks)
	p.StartNext()
	p.TaskDone("")
	if got := ReviewPrompt(p, true, 0); !strings.Contains(got, "All 1 tasks are done") || !strings.Contains(got, "Done criteria") {
		t.Errorf("success review prompt: %s", got)
	}
}

// - Verify the review prompt mentions further rounds only when some are left
func Test_ReviewPrompt_RoundsLeft(t *testing.T) {
	p := NewPlan([]string{"a"}, testMaxTasks)
	if got := ReviewPrompt(p, false, 0); strings.Contains(got, "new round") {
		t.Errorf("no rounds left, but:\n%s", got)
	}
	if got := ReviewPrompt(p, false, 2); !strings.Contains(got, "2 more round(s)") || !strings.Contains(got, "no nice-to-haves") {
		t.Errorf("2 rounds left:\n%s", got)
	}
}
