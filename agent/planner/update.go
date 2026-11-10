package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/objective"
)

// UpdateObjective is the update_objective tool: it changes plan mode's in-memory draft, one section at a time.
// [agent] It is silent: the user doesn't see the draft after each change (that was distracting). The model summarizes
// its understanding in its replies instead, and shows the final draft with present_objective; the user can /view it.
type UpdateObjective struct {
	draft *objective.Objective // the draft it changes; plan mode's /save writes it
}

// NewUpdateObjective returns the update_objective tool for draft.
//
// Notes: none.
func NewUpdateObjective(draft *objective.Objective) *UpdateObjective {
	return &UpdateObjective{draft: draft}
}

// GetToolDefinition returns update_objective's name, description and parameters.
//
// Notes:
//   - The three parameters are optional, but at least one must be given; each holds a whole section.
func (t *UpdateObjective) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"update_objective",
		"Changes the draft objective the user is writing with you. Give only the sections that change, each as the full "+
			"new text of that section in Markdown, without the section's own heading. A section you leave out stays as it is; "+
			"an empty string clears Instructions or Context. The user does not see the draft when you call this: tell them "+
			"what you understood in your reply, and show the final draft with present_objective. "+
			"This never saves a file: the user saves with /save.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"objective": {
					"type": "string",
					"description": "What to achieve and why, in a few sentences. Required before the user can save; can't be cleared."
				},
				"instructions": {
					"type": "string",
					"description": "How to work: constraints, the quality bar, when it is done, rough steps."
				},
				"context": {
					"type": "string",
					"description": "Background facts the work needs."
				}
			}
		}`),
	)
}

// CallTool applies the sections given in args to the draft, without showing it.
//
// Notes:
//   - A section that is absent stays as it is; one given as "" clears it, except objective, which can't be empty.
//     Text is trimmed of leading and trailing blank space.
//   - No section given, or bad JSON, is an error.
//   - The new draft must read back the same through objective.Parse (it is written and read that way later). A line
//     that reads as a section heading ("# Instructions" inside the instructions, "# Notes" inside the objective) would
//     not, so the change is refused and the draft is left as it was.
func (t *UpdateObjective) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Objective    *string `json:"objective"`
		Instructions *string `json:"instructions"`
		Context      *string `json:"context"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Objective == nil && params.Instructions == nil && params.Context == nil {
		return "", errors.New("give at least one of objective, instructions or context")
	}

	next := *t.draft
	if params.Objective != nil {
		next.ObjText = strings.TrimSpace(*params.Objective)
		if next.ObjText == "" {
			return "", errors.New("objective can't be empty: give the text of what to achieve")
		}
	}
	if params.Instructions != nil {
		next.Instructions = strings.TrimSpace(*params.Instructions)
	}
	if params.Context != nil {
		next.Context = strings.TrimSpace(*params.Context)
	}

	// [agent] the draft is saved as next.Markdown() and read back with objective.Parse later; if that doesn't give the
	// same draft (a line in a section reads as a section heading), refuse now rather than save something that changes
	if back, err := objective.Parse([]byte(next.Markdown())); err != nil || back != next {
		return "", errors.New("the draft would not read back as written: a line in a section reads as a section heading " +
			"(# Objective, # Instructions, # Context, or another heading at that level). Leave out the section's own " +
			"heading and use deeper headings (##) inside a section")
	}

	*t.draft = next
	return "Draft updated. The user has not seen it.", nil
}
