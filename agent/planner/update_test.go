package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/objective"
)

// - Verify update_objective's definition has its name and valid JSON parameters
func Test_Unit_UpdateObjective_Definition(t *testing.T) {
	def := NewUpdateObjective(&objective.Objective{}).GetToolDefinition()
	if def.Function.Name != "update_objective" {
		t.Errorf("name = %q, want update_objective", def.Function.Name)
	}
	if !json.Valid(def.Function.Params) {
		t.Errorf("Params is not valid JSON: %s", def.Function.Params)
	}
}

// - Verify each call changes only the sections it gives, trims text, and says the user has not seen the draft
// - Verify "" clears instructions and context
// - Verify a "## Sub" heading inside a section is accepted
func Test_Unit_UpdateObjective_ChangesSections(t *testing.T) {
	draft := &objective.Objective{}
	tool := NewUpdateObjective(draft)

	steps := []struct {
		name string
		args string
		want objective.Objective
	}{
		{"objective only, trimmed", `{"objective":"  Build a word counter.  "}`,
			objective.Objective{ObjText: "Build a word counter."}},
		{"instructions and context added", `{"instructions":"## Steps\n- Use Go","context":"\nA CLI.\n"}`,
			objective.Objective{ObjText: "Build a word counter.", Instructions: "## Steps\n- Use Go", Context: "A CLI."}},
		{"objective changed, others kept", `{"objective":"Build a line counter."}`,
			objective.Objective{ObjText: "Build a line counter.", Instructions: "## Steps\n- Use Go", Context: "A CLI."}},
		{"empty strings clear instructions and context", `{"instructions":"","context":""}`,
			objective.Objective{ObjText: "Build a line counter."}},
	}
	for _, s := range steps {
		got, err := tool.CallTool(context.Background(), json.RawMessage(s.args))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", s.name, err)
		}
		if got != "Draft updated. The user has not seen it." {
			t.Errorf("%s: result = %q", s.name, got)
		}
		if *draft != s.want {
			t.Errorf("%s: draft = %+v, want %+v", s.name, *draft, s.want)
		}
	}
}

// - Verify bad JSON, no field, an empty objective and text that would not read back are errors
// - Verify a refused call leaves the draft unchanged
func Test_Unit_UpdateObjective_Refuses(t *testing.T) {
	start := objective.Objective{ObjText: "Keep me.", Instructions: "Use Go.", Context: "A CLI."}
	tests := []struct {
		name, args, wantErr string
	}{
		{"bad JSON", `{"objective":`, "invalid arguments"},
		{"no field", `{}`, "at least one"},
		{"unknown field only", `{"notes":"x"}`, "at least one"},
		{"empty objective", `{"objective":"   "}`, "can't be empty"},
		{"instructions heading inside instructions", `{"instructions":"# Instructions\nx"}`, "read back"},
		{"top-level heading inside objective", `{"objective":"Build it.\n# Notes\ny"}`, "read back"},
		{"context heading inside context", `{"context":"a\n## Context\nb"}`, "read back"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			draft := start
			tool := NewUpdateObjective(&draft)
			_, err := tool.CallTool(context.Background(), json.RawMessage(tt.args))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, tt.wantErr)
			}
			if draft != start {
				t.Errorf("draft changed to %+v", draft)
			}
		})
	}
}
