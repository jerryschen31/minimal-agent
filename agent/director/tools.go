package director

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
)

// Verdict is what the director decided about one attempt at a task; the first verdict tool it calls sets it.
type Verdict struct {
	Given  bool   // a verdict tool was called; false = the director gave no verdict
	Done   bool   // task_done (true) or task_failed (false)
	Reason string // why, in a sentence or two
	Advice string // task_failed only: what the next attempt should do differently; "" if none
}

// TaskDone is the task_done tool: the director says the task is done.
type TaskDone struct{ v *Verdict }

// TaskFailed is the task_failed tool: the director says the task is not done.
type TaskFailed struct{ v *Verdict }

// verdictArgs are both tools' arguments.
type verdictArgs struct {
	Reason string `json:"reason"`
	Advice string `json:"advice"`
}

// NewTaskDone returns the task_done tool, which records its verdict in v.
//
// Notes: none.
func NewTaskDone(v *Verdict) *TaskDone { return &TaskDone{v: v} }

// NewTaskFailed returns the task_failed tool, which records its verdict in v.
//
// Notes: none.
func NewTaskFailed(v *Verdict) *TaskFailed { return &TaskFailed{v: v} }

// GetToolDefinition returns task_done's name, description and parameters.
//
// Notes: none.
func (t *TaskDone) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"task_done",
		"Your verdict: the task is done, because its \"Done when\" holds and you checked it. Give the evidence in reason. "+
			"Your turn ends after this call.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"reason": {"type": "string", "description": "What shows the task is done (what you checked), in a sentence or two"}
			},
			"required": ["reason"]
		}`),
	)
}

// CallTool records a "done" verdict and ends the director's turn.
//
// Notes:
//   - See record.
func (t *TaskDone) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return record(t.v, true, args)
}

// GetToolDefinition returns task_failed's name, description and parameters.
//
// Notes: none.
func (t *TaskFailed) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"task_failed",
		"Your verdict: the task is not done (its \"Done when\" does not hold, or you could not check it). Say why in reason, "+
			"and in advice what a new attempt should do differently; a new worker may get it. Your turn ends after this call.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"reason": {"type": "string", "description": "What is missing or wrong, in a sentence or two"},
				"advice": {"type": "string", "description": "What the next attempt should do differently; empty if nothing"}
			},
			"required": ["reason"]
		}`),
	)
}

// CallTool records a "not done" verdict and ends the director's turn.
//
// Notes:
//   - See record.
func (t *TaskFailed) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return record(t.v, false, args)
}

// record sets v from a verdict tool's arguments and returns tools.ErrEndTurn.
//
// Notes:
//   - Bad JSON or an empty reason is an error the model sees (it calls again). A second verdict in the same reply (both
//     tools called at once) is refused: the first one stands.
//   - [agent] ErrEndTurn ends the ReAct loop after this call, so the director never talks after its verdict.
func record(v *Verdict, done bool, args json.RawMessage) (string, error) {
	if v.Given {
		return "", errors.New("a verdict was already given; it stands")
	}
	var a verdictArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Reason) == "" {
		return "", errors.New("reason is required: say what you checked")
	}
	*v = Verdict{Given: true, Done: done, Reason: strings.TrimSpace(a.Reason)}
	if !done {
		v.Advice = strings.TrimSpace(a.Advice)
	}
	return "Verdict recorded.", tools.ErrEndTurn
}
