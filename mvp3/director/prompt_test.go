package director

import (
	"errors"
	"strings"
	"testing"
)

// - Verify the system prompt carries the instructions, extra text, worker tools, objective sections,
// task statuses with notes, and the current phase
func Test_SystemPrompt_Contents(t *testing.T) {
	obj := Objective{Text: "Ship it.", Sections: []Section{{"Constraints", "No deletes."}}}
	p := NewPlan([]string{"first", "second"}, testMaxTasks)
	p.TaskDone("found X")
	got := SystemPrompt(PromptInput{Objective: obj, Plan: p, WorkerTools: []string{"read_file", "write_file"}, MaxTurnsPerTask: 4, Extra: "Be brief."})
	for _, want := range []string{
		Instructions, "Additional instructions:\nBe brief.", "read_file, write_file", "at most 4 turns",
		"## Objective\nShip it.", "## Constraints\nNo deletes.",
		"1. [done] first\n   note: found X", "2. [current] second", "WORK, task 2 of 2",
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
	p.TaskDone("")
	obj := Objective{Text: "x", Sections: []Section{{"Done when", "- y"}}}
	if got := SystemPrompt(PromptInput{Objective: obj, Plan: p}); !strings.Contains(got, "REVIEW. Check the results against the objective and its Done criteria") {
		t.Errorf("review phase prompt:\n%s", got)
	}
}

// - Verify WrapTurn includes the reply, tools and error, caps long replies, and warns on the last turn
func Test_WrapTurn(t *testing.T) {
	got := WrapTurn(0, 3, "read it", WorkerOutcome{Turn: 1, MaxTurns: 5, Reply: "contents", ToolsCalled: []string{"read_file"}})
	for _, want := range []string{"Task 1 of 3: read it", "turn 1 of 5", "Worker reply:\ncontents", "Tools called: read_file"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "last worker turn") {
		t.Error("last-turn warning shown on turn 1 of 5")
	}

	long := strings.Repeat("é", MaxReplyChars) // 2 bytes each, so the cut lands mid-character
	got = WrapTurn(0, 1, "t", WorkerOutcome{Turn: 5, MaxTurns: 5, Reply: long, Err: nil})
	if !strings.Contains(got, "reply truncated") || !strings.Contains(got, "last worker turn") || strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("long reply not capped cleanly:\n%.200s", got)
	}

	got = WrapTurn(0, 1, "t", WorkerOutcome{Turn: 1, MaxTurns: 2, Err: errors.New("max steps")})
	if !strings.Contains(got, "failed with an error: max steps") || !strings.Contains(got, "files it wrote or changed are kept") || strings.Contains(got, "Tools called") {
		t.Errorf("error outcome:\n%s", got)
	}
}

// - Verify the review prompt names a failed task, or says all tasks are done
func Test_ReviewPrompt(t *testing.T) {
	p := NewPlan([]string{"a", "b"}, testMaxTasks)
	p.TaskDone("")
	p.TaskFailed("disk full")
	if got := ReviewPrompt(p, false); !strings.Contains(got, "task 2 failed: disk full") {
		t.Errorf("failed review prompt: %s", got)
	}
	p = NewPlan([]string{"a"}, testMaxTasks)
	p.TaskDone("")
	if got := ReviewPrompt(p, true); !strings.Contains(got, "All 1 tasks are done") || !strings.Contains(got, "Done criteria") {
		t.Errorf("success review prompt: %s", got)
	}
}
