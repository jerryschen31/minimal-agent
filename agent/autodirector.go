package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerryschen31/minimal-agent/agent/director"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// directorToolNames are the session tools the director gets besides its verdict tools: it reads and runs checks, it
// never writes. A tool the session doesn't have (run_command not enabled) is left out.
var directorToolNames = []string{"read_file", "list_files", "search_files", "run_command"}

// directorReminders is how many times a director that replies without a verdict is reminded (director.Reminder).
const directorReminders = 1

// judgeTask runs the director on one attempt at task t and returns its verdict.
//
// Notes:
//   - A fresh ReAct loop on a shallow copy of cs: system prompt director.Prompt, tools directorToolNames (those the
//     session has) plus task_done and task_failed, output labelled "director:". Hooks are shared, so the deny and ask
//     rules bind the director as they bind the worker (an ask rule is denied: nobody is asked).
//   - Its message is director.Message: the task, the worker's summary and its tool calls (workerMsgs).
//   - No verdict after directorReminders reminders, or the director running out of steps, is a "not done" verdict
//     (Verdict.Given false) with the reason saying so: the attempt counts as failed.
//   - Any other error (the provider, Ctrl+C) is returned: the caller stops the run.
//   - Traced as its own task scope, "judge <id>".
func judgeTask(ctx context.Context, cs *ChatSession, t plan.Task, attempt int, summary string, workerMsgs []model.ChatMessage) (director.Verdict, error) {
	var v director.Verdict
	toolList := []tools.Tool{director.NewTaskDone(&v), director.NewTaskFailed(&v)}
	for _, name := range directorToolNames {
		if tool, ok := cs.Tools.Lookup(name); ok {
			toolList = append(toolList, tool)
		}
	}
	registry, err := tools.NewToolRegistry(toolList)
	if err != nil {
		return v, err
	}
	dcs := *cs // [agent] a shallow copy: same provider, recorder and hooks; its own system message, tools and output
	dcs.Tools = registry
	dcs.SystemMsg = createSystemMessage(director.Prompt)
	if o, ok := cs.Output.(autoOutput); ok {
		o.label = "director: "
		dcs.Output = o
	}

	taskTrace := trace.StartTask(cs.Recorder, "judge "+t.ID)
	defer taskTrace.End()
	var msgs []model.ChatMessage
	next := model.CreateChatMessage("user", "user", director.Message(t, attempt, cs.Config.MaxTaskAttempts, summary, workerMsgs), "", nil)
	for reminded := 0; ; reminded++ {
		turnMsgs, err := reActLoop(ctx, &dcs, taskTrace, msgs, next)
		if errors.Is(err, ErrMaxSteps) {
			return director.Verdict{Reason: fmt.Sprintf("the director ran out of steps (maxSteps %d) without a verdict", cs.Config.MaxSteps)}, nil
		}
		if err != nil {
			taskTrace.Fail(err)
			return v, err
		}
		if v.Given {
			return v, nil
		}
		if reminded == directorReminders {
			return director.Verdict{Reason: "the director gave no verdict"}, nil
		}
		msgs = append(msgs, turnMsgs...) // turnMsgs starts with next: the whole exchange so far is the next loop's context
		next = model.CreateChatMessage("user", "user", director.Reminder, "", nil)
	}
}
