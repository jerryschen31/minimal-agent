package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/textutils"
	"github.com/jerryschen31/minimal-agent/trace"
)

// workerPrompt is the system prompt of auto mode's worker: the agent doing one task of a plan.
// [agent] The manifest records its hash as the "worker" prompt, so a change to this text shows up between two runs.
const workerPrompt = `You are a worker agent carrying out one task of a plan. Nobody is watching and nobody can answer questions, so decide for yourself and keep going.

- Use your tools to do the task. Work only inside the working directory.
- Do only your task. Other tasks of the plan are done by other workers, before or after you; the summaries of the tasks yours depends on are in the message.
- A tool call can be refused by a rule. Do not try to get around a refusal; find another way, or report that the task can't be done.
- When the task is done (its "Done when" holds), or you can't do it, stop calling tools and reply with a short summary: what you did, how you checked it, and anything the next tasks need to know. If you could not finish, say so first, and why.`

// maxSummaryBytes caps the summary the journal keeps for a task (the worker's final reply), so a long reply doesn't
// bloat the journal or the next task's message.
const maxSummaryBytes = 4000

// retryNote is what a new attempt at a task is told about the attempt before it.
type retryNote struct {
	Attempt int    // the attempt that was judged not done
	Summary string // its worker's summary
	Reason  string // why the director judged it not done
	Advice  string // what the director advises; "" if none
}

// taskMessage returns the first message a task's worker gets: the plan's context, the summaries of the tasks t depends
// on, the task itself and, on a retry, what happened to the attempt before (retry; nil on the first attempt).
//
// Notes:
//   - From the plan: the title and summary, "## Objective" and "## Assumptions"; the other tasks are left out (the
//     worker does one task).
//   - deps maps a dependency's ID to its summary (from this run or an earlier one); only t.Depends are shown, in that
//     order. A dependency with no summary is listed as done with "(no summary)".
//   - An "effects: external" task is told so: its actions reach outside the working directory.
//   - A retry is told that the earlier attempt's changes are still in the working directory, so it builds on them
//     rather than starting over blindly.
//   - crash (non-empty after a resume) lists what happened to the calls a crash cut off in an earlier attempt, so the
//     worker checks instead of repeating an action.
func taskMessage(p plan.Plan, t plan.Task, deps map[string]string, retry *retryNote, crash string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Plan: %s\n\n", p.Title)
	if s := strings.TrimSpace(p.Summary); s != "" {
		b.WriteString(s + "\n\n")
	}
	if s := strings.TrimSpace(p.Objective); s != "" {
		b.WriteString("## Objective\n\n" + s + "\n\n")
	}
	if s := strings.TrimSpace(p.Assumptions); s != "" {
		b.WriteString("## Assumptions\n\n" + s + "\n\n")
	}
	if len(t.Depends) > 0 {
		b.WriteString("## Finished tasks your task depends on\n\n")
		for _, id := range t.Depends {
			summary := strings.TrimSpace(deps[id])
			if summary == "" {
				summary = "(no summary)"
			}
			fmt.Fprintf(&b, "### %s\n\n%s\n\n", id, summary)
		}
	}
	fmt.Fprintf(&b, "## Your task: %s: %s\n\n", t.ID, t.Title)
	if t.Needs != "" {
		fmt.Fprintf(&b, "Needs: %s\n", t.Needs)
	}
	if t.DoneWhen != "" {
		fmt.Fprintf(&b, "Done when: %s\n", t.DoneWhen)
	}
	if t.External() {
		b.WriteString("Effects: external (this task acts outside the working directory; each action is real and may not be undoable)\n")
	}
	if body := strings.TrimSpace(t.Body); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	if crash != "" {
		fmt.Fprintf(&b, "\n## An earlier attempt was cut off by a crash\n\nIts changes are still in the working directory. What "+
			"happened to the tool calls it had in progress:\n\n%s\n\nCheck the current state before you act, and don't repeat "+
			"an action that already happened.\n", crash)
	}
	if retry != nil {
		fmt.Fprintf(&b, "\n## Attempt %d was judged not done\n\nIts changes are still in the working directory: check them and build on "+
			"them.\n\nWhy it was judged not done: %s\n", retry.Attempt, retry.Reason)
		if retry.Advice != "" {
			fmt.Fprintf(&b, "Advice: %s\n", retry.Advice)
		}
		if s := strings.TrimSpace(retry.Summary); s != "" {
			fmt.Fprintf(&b, "\nThat attempt's summary:\n\n%s\n", s)
		}
	}
	return b.String()
}

// runWorker runs one attempt at a task with a fresh worker and returns its summary (the final reply, cut to
// maxSummaryBytes) and the worker's messages (for the director's tool log).
//
// Notes:
//   - Fresh context: reActLoop gets no earlier messages, only the session's system prompt (workerPrompt) and
//     taskMessage. Nothing is added to the session's history or context window, so one task never sees another's
//     conversation.
//   - Traced as its own task scope, named "<id>: <title>".
//   - Whether the task is done is the director's call (judgeTask), not the worker's.
//   - meter (may be nil) counts and limits the worker's tokens: the worker runs on a shallow copy of cs with Meter set, so
//     the session itself is not changed. A used-up allowance is an error wrapping errAllowanceUsed.
func runWorker(ctx context.Context, cs *ChatSession, meter *usageMeter, p plan.Plan, t plan.Task, deps map[string]string, retry *retryNote, crash string) (string, []model.ChatMessage, error) {
	taskTrace := trace.StartTask(cs.Recorder, t.ID+": "+t.Title)
	defer taskTrace.End()
	msg := model.CreateChatMessage("user", "user", taskMessage(p, t, deps, retry, crash), "", nil)
	wcs := *cs // [agent] a shallow copy: the same provider, tools, hooks and journal; its own meter
	wcs.Meter = meter
	msgs, err := reActLoop(ctx, &wcs, taskTrace, nil, msg)
	if err != nil {
		taskTrace.Fail(err)
		return "", nil, err
	}
	summary := ""
	if n := len(msgs); n > 0 {
		summary = strings.TrimSpace(msgs[n-1].Content)
	}
	if cut, ok := textutils.CutBytes(summary, maxSummaryBytes); ok {
		summary = cut + " …"
	}
	return summary, msgs, nil
}
