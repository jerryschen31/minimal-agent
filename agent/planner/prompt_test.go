package planner

import (
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
)

// - Verify PlanMessage holds the objective's path and its standard form, and asks for propose_plan
func Test_Unit_PlanMessage_HoldsPathAndObjective(t *testing.T) {
	o := objective.Objective{ObjText: "Count words.", Instructions: "Use Go.", Context: "A CLI."}
	got := PlanMessage("dir/objective.md", o)
	for _, want := range []string{"dir/objective.md", o.Markdown(), "propose_plan"} {
		if !strings.Contains(got, want) {
			t.Errorf("PlanMessage() lacks %q:\n%s", want, got)
		}
	}
}

// - Verify RevisionNote holds the plan's Markdown without its final newline, then "What I want to change: <change>"
// - Verify it says changes go into the objective through update_objective, after the change
func Test_Unit_RevisionNote_HoldsPlanChangeAndPointsAtUpdateObjective(t *testing.T) {
	p := plan.Plan{Format: plan.FormatV1, ObjectivePath: "objective.md", ObjectiveHash: "sha256:x", Version: 1, Title: "Words",
		Tasks: []plan.Task{{ID: "t1", Title: "Write it"}}}
	got := RevisionNote(p, "Add a README task.")
	md := strings.TrimSuffix(p.Markdown(), "\n")
	if !strings.Contains(got, md) {
		t.Errorf("RevisionNote() lacks the plan:\n%s", got)
	}
	if strings.Contains(got, md+"\n\n\n") {
		t.Errorf("RevisionNote() kept the plan's final newline:\n%q", got)
	}
	change := strings.Index(got, "What I want to change: Add a README task.")
	if change < 0 {
		t.Fatalf("RevisionNote() lacks the change:\n%s", got)
	}
	if strings.Index(got, md) > change {
		t.Errorf("RevisionNote() puts the change before the plan:\n%s", got)
	}
	if note := strings.Index(got, "update_objective"); note < change {
		t.Errorf("RevisionNote() does not point at update_objective after the change:\n%s", got)
	}
}
