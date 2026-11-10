package planner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/tools"
)

// - Verify present_objective's definition has its name and valid JSON parameters
func Test_Unit_PresentObjective_Definition(t *testing.T) {
	def := NewPresentObjective(&objective.Objective{}, nil).GetToolDefinition()
	if def.Function.Name != "present_objective" {
		t.Errorf("name = %q, want present_objective", def.Function.Name)
	}
	if !json.Valid(def.Function.Params) {
		t.Errorf("Params is not valid JSON: %s", def.Function.Params)
	}
}

// - Verify a draft with an objective is shown once and the turn ends (tools.ErrEndTurn), with the draft unchanged
func Test_Unit_PresentObjective_ShowsAndEndsTurn(t *testing.T) {
	draft := objective.Objective{ObjText: "Count words.", Context: "A CLI."}
	before := draft
	shown := 0
	got, err := NewPresentObjective(&draft, func() { shown++ }).CallTool(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, tools.ErrEndTurn) {
		t.Errorf("err = %v, want tools.ErrEndTurn", err)
	}
	if got == "" || shown != 1 {
		t.Errorf("result = %q, shown %d times; want a result and 1", got, shown)
	}
	if draft != before {
		t.Errorf("draft changed to %+v", draft)
	}
}

// - Verify a draft with no objective text is an error pointing at update_objective, and nothing is shown
func Test_Unit_PresentObjective_EmptyDraftRefused(t *testing.T) {
	shown := 0
	_, err := NewPresentObjective(&objective.Objective{Context: "only context"}, func() { shown++ }).
		CallTool(context.Background(), json.RawMessage(`{}`))
	if err == nil || errors.Is(err, tools.ErrEndTurn) || !strings.Contains(err.Error(), "update_objective") {
		t.Errorf("err = %v, want an error pointing at update_objective", err)
	}
	if shown != 0 {
		t.Errorf("show called %d times on a refused call", shown)
	}
}
