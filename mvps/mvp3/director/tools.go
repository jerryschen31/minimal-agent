package director

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jerryschen31/minimal-agent/mvp3/model"
	"github.com/jerryschen31/minimal-agent/mvp3/tools"
)

// The director's control tools. They don't touch the outside world: each one changes the Plan.
// [agent] Every accepted control tool returns tools.ErrEndTurn, which ends the director's turn at once
// (no extra model call to acknowledge the result). The run loop then reads the new Plan state and decides
// what the director is asked next, with a fresh context. A rejected call (wrong phase, bad arguments)
// returns an ordinary error, which the director sees as "error: ..." and can correct in the same turn.

// PhaseTools returns the control tools offered in a phase (read-only file tools are added by the caller).
func PhaseTools(p *Plan, phase Phase) []tools.Tool {
	switch phase {
	case PhasePlan:
		return []tools.Tool{setTasksTool{p}}
	case PhaseWork:
		return []tools.Tool{taskDoneTool{p}, taskFailedTool{p}}
	case PhaseReview:
		return []tools.Tool{finishTool{p}}
	}
	return nil
}

// decodeArgs unmarshals a tool's JSON arguments into v.
func decodeArgs(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ---- set_tasks ----

type setTasksTool struct{ p *Plan }

func (t setTasksTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("set_tasks",
		fmt.Sprintf("Records the task list for the run: 1 to %d tasks, in the order the worker should do them. Each task should be one concrete, checkable piece of work the worker can finish in a few turns.", t.p.MaxTasks),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"tasks": {"type": "array", "items": {"type": "string"}, "description": "The tasks, in order"}
			},
			"required": ["tasks"]
		}`))
}

func (t setTasksTool) CallTool(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Tasks []string `json:"tasks"`
	}
	if err := decodeArgs(args, &params); err != nil {
		return "", err
	}
	if err := t.p.SetTasks(params.Tasks); err != nil {
		return "", err
	}
	return fmt.Sprintf("Recorded %d tasks.", len(t.p.Tasks)), tools.ErrEndTurn
}

// ---- task_done ----

type taskDoneTool struct{ p *Plan }

func (taskDoneTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("task_done",
		"Marks the current task as complete and moves on to the next task. Call it only when the worker's results show the task is really done. If anything is still missing or wrong, don't call it: reply with a text instruction to the worker instead. The note is kept for the rest of the run: record what later tasks will need (file names, findings, decisions).",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"note": {"type": "string", "description": "What was done and anything later tasks need to know"}
			},
			"required": ["note"]
		}`))
}

func (t taskDoneTool) CallTool(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Note string `json:"note"`
	}
	if err := decodeArgs(args, &params); err != nil {
		return "", err
	}
	i, _, _ := t.p.CurrentTask()
	if err := t.p.TaskDone(params.Note); err != nil {
		return "", err
	}
	return fmt.Sprintf("Task %d marked done.", i+1), tools.ErrEndTurn
}

// ---- task_failed ----

type taskFailedTool struct{ p *Plan }

func (taskFailedTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("task_failed",
		"Gives up on the current task because it cannot be completed (e.g. a required file or service is missing, or repeated attempts with different instructions have failed). This ends the task AND stops the whole run. Never use it to ask the worker for changes or fixes: for that, reply with a text instruction instead.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"reason": {"type": "string", "description": "Why the task cannot be completed"}
			},
			"required": ["reason"]
		}`))
}

func (t taskFailedTool) CallTool(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Reason string `json:"reason"`
	}
	if err := decodeArgs(args, &params); err != nil {
		return "", err
	}
	// [agent] Guard (real run, 2026-10-14): a director that meant to send the worker back to fix something
	// called task_failed instead, which stopped a 19-task run on task 2. While the worker still has turns,
	// the first task_failed of a task is refused with an explanation; calling it again (in this or a later
	// director turn) means the director really means it. Once per task, not once per director turn: per turn,
	// a director that took the refusal as "use up the turns first" spent every remaining worker turn
	// before it could give up (real run, same day). Cost: one extra model call, maybe one worker turn.
	if t.p.WorkerTurnsLeft > 0 && !t.p.failRefused && t.p.Phase == PhaseWork {
		t.p.failRefused = true
		return "", fmt.Errorf("not recorded: the worker still has %d turn(s) for this task, and task_failed stops the whole run. "+
			"If you want the worker to change or fix something, end your turn with a plain-text reply containing the instruction, and no tool call. "+
			"If the task really cannot be done (for example required input is missing), call task_failed again now; it will be accepted", t.p.WorkerTurnsLeft)
	}
	i, _, _ := t.p.CurrentTask()
	if err := t.p.TaskFailed(params.Reason); err != nil {
		return "", err
	}
	return fmt.Sprintf("Task %d marked failed.", i+1), tools.ErrEndTurn
}

// ---- finish ----

type finishTool struct{ p *Plan }

func (finishTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("finish",
		"Ends the run with the final review. success is true only if every task is done and the objective (and its Done criteria, if any) is met. List anything not achieved, or worth doing next, as gaps.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"success": {"type": "boolean", "description": "Whether the objective is met"},
				"summary": {"type": "string", "description": "A short summary of what was achieved, for the user"},
				"gaps": {"type": "array", "items": {"type": "string"}, "description": "What is missing or incomplete; empty if nothing"}
			},
			"required": ["success", "summary", "gaps"]
		}`))
}

func (t finishTool) CallTool(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Success *bool    `json:"success"` // pointer: a missing "success" must not silently mean false
		Summary string   `json:"summary"`
		Gaps    []string `json:"gaps"`
	}
	if err := decodeArgs(args, &params); err != nil {
		return "", err
	}
	if params.Success == nil {
		return "", fmt.Errorf("missing required argument: success")
	}
	if err := t.p.Finish(*params.Success, params.Summary, params.Gaps); err != nil {
		return "", err
	}
	return "Review recorded.", tools.ErrEndTurn
}
