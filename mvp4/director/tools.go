package director

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jerryschen31/minimal-agent/mvp4/model"
	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/tools"
)

// The director's control tools. They don't touch the outside world: each one changes the Plan.
// [agent] Every accepted control tool returns tools.ErrEndTurn, which ends the director's turn at once
// (no extra model call to acknowledge the result). The run loop then reads the new Plan state and decides
// what the director is asked next, with a fresh context. A rejected call (wrong phase, bad arguments)
// returns an ordinary error, which the director sees as "error: ..." and can correct in the same turn.

// PhaseTools returns the control tools offered in a phase (read-only file tools are added by the caller).
// In PhaseWork the tools act on run, the task this director conversation works on (nil outside PhaseWork).
// [agent] Binding task_done and task_failed to a task, rather than to "the current task", is what lets
// several director conversations run at once without the tools' schema changing: no task_id argument.
func PhaseTools(p *Plan, phase Phase, run *TaskRun) []tools.Tool {
	switch phase {
	case PhasePlan:
		return []tools.Tool{setTasksTool{p}}
	case PhaseWork:
		return []tools.Tool{taskDoneTool{p, run}, taskFailedTool{p, run}}
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
		fmt.Sprintf("Records the task list for the run: 1 to %d tasks. Each task should be one concrete, checkable piece of work the worker can finish in a few turns. Give each task a short unique id (t1, t2, ...). %s", t.p.Tasks.MaxTasks, schedulingRule(t.p.Parallel)),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"tasks": {
					"type": "array",
					"description": "The tasks",
					"items": {
						"type": "object",
						"properties": {
							"id": {"type": "string", "description": "Short unique id, e.g. t1"},
							"task": {"type": "string", "description": "What the worker should do"},
							"depends_on": {"type": "array", "items": {"type": "string"}, "description": "Ids of tasks that must be done first (optional)"},
							"worker_type": {"type": "string", "description": "Which worker type does the task (optional; see the Worker section)"}
						},
						"required": ["id", "task"]
					}
				}
			},
			"required": ["tasks"]
		}`))
}

// schedulingRule says how tasks are started, for the set_tasks description.
func schedulingRule(parallel int) string {
	if parallel <= 1 {
		return "Tasks run one at a time: the next task is the first one in the list whose depends_on tasks are all done, so list them in a sensible order and use depends_on where a task needs another task's results."
	}
	return fmt.Sprintf("Up to %d tasks run at the same time, on separate workers sharing one working directory: any task whose depends_on tasks are all done may start. Use depends_on wherever a task needs another task's results or changes the same files; tasks without it may run side by side.", parallel)
}

func (t setTasksTool) CallTool(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Tasks []tasks.Spec `json:"tasks"`
	}
	if err := decodeArgs(args, &params); err != nil {
		return "", err
	}
	if err := t.p.SetTasks(params.Tasks); err != nil {
		return "", err
	}
	return fmt.Sprintf("Recorded %d tasks.", len(t.p.Tasks.Tasks)), tools.ErrEndTurn
}

// ---- task_done ----

type taskDoneTool struct {
	p   *Plan
	run *TaskRun
}

func (taskDoneTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("task_done",
		"Marks the current task as complete; the run then moves on to the next task. Call it only when the worker's results show the task is really done. If anything is still missing or wrong, don't call it: reply with a text instruction to the worker instead. The note is kept for the rest of the run: record what later tasks will need (file names, findings, decisions).",
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
	if err := t.p.TaskDone(t.run, params.Note); err != nil {
		return "", err
	}
	return fmt.Sprintf("Task %s marked done.", t.run.Task.ID), tools.ErrEndTurn
}

// ---- task_failed ----

type taskFailedTool struct {
	p   *Plan
	run *TaskRun
}

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
	if t.run != nil && t.run.WorkerTurnsLeft > 0 && !t.run.failRefused {
		t.run.failRefused = true
		return "", fmt.Errorf("not recorded: the worker still has %d turn(s) for this task, and task_failed stops the whole run. "+
			"If you want the worker to change or fix something, end your turn with a plain-text reply containing the instruction, and no tool call. "+
			"If the task really cannot be done (for example required input is missing), call task_failed again now; it will be accepted", t.run.WorkerTurnsLeft)
	}
	if err := t.p.TaskFailed(t.run, params.Reason); err != nil {
		return "", err
	}
	return fmt.Sprintf("Task %s marked failed.", t.run.Task.ID), tools.ErrEndTurn
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
