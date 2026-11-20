package planner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/tools"
)

// PresentObjective is the present_objective tool: the model calls it when it thinks the draft is a final draft, and
// the user is shown the whole draft and how to save it.
// [agent] Like Claude Code's ExitPlanMode: the model decides when the work is ready to show, and showing it is a tool
// call, so what the user sees is always the real draft, never the model's retelling of it.
type PresentObjective struct {
	draft *objective.Objective // the draft it shows
	show  func()               // prints the draft and the reminder; nil = show nothing
}

// NewPresentObjective returns the present_objective tool for draft.
//
// Notes:
//   - show is called on each successful call; plan mode passes a function that prints the draft and how to save,
//     view or edit it.
func NewPresentObjective(draft *objective.Objective, show func()) *PresentObjective {
	return &PresentObjective{draft: draft, show: show}
}

// GetToolDefinition returns present_objective's name, description and (empty) parameters.
//
// Notes: none.
func (t *PresentObjective) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"present_objective",
		"Shows the user the whole draft objective as the final draft, with how to save it. Call it once the objective, "+
			"the instructions and the context are all settled and nothing is left to ask. Call update_objective first if "+
			"the draft is not up to date. Your turn ends after this call: don't write anything after it.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
	)
}

// CallTool shows the draft and ends the turn.
//
// Notes:
//   - A draft with no objective text is an error (nothing to present yet): the model is told to call update_objective.
//   - On success it returns tools.ErrEndTurn, so the model says nothing after the draft and the reminder.
//   - Arguments are ignored.
func (t *PresentObjective) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	if strings.TrimSpace(t.draft.ObjText) == "" {
		return "", errors.New("the draft has no objective yet: call update_objective first")
	}
	if t.show != nil {
		t.show()
	}
	return "The user was shown the final draft and how to save it. Wait for their answer.", tools.ErrEndTurn
}
