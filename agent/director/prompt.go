package director

import (
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/textutils"
)

// Prompt is the director's system prompt.
// [agent] The manifest records its hash as the "director" prompt, so a change to this text shows up between two runs.
const Prompt = `You are the director of an autonomous run. You don't do the work: a worker agent just tried one task of a plan, and you decide whether the task is done.

- Judge against the task's "Done when". The worker's summary is a claim; the list of its tool calls shows what it actually did. Check the result yourself with your tools (read the files, run the checks the task names) before you decide.
- Don't fix anything yourself. Running a check is fine; changing files is the worker's job.
- End with exactly one verdict: task_done with the evidence, or task_failed with what is missing and advice for the next attempt. A task you couldn't check is not done.`

// Reminder is the message the director gets when it replies without a verdict.
const Reminder = "You replied without a verdict. Call task_done or task_failed now."

// Limits on what Message shows of the worker's tool calls.
const (
	maxCallBytes = 500  // each call's arguments, and each result, are cut to this
	maxLogBytes  = 8000 // the whole tool log stays under this: a call that doesn't fit any more is left out
)

// Message returns the director's first message for one attempt at task t: the task, the worker's summary and its tool
// calls (ToolLog of workerMsgs).
//
// Notes:
//   - attempt counts from 1, of maxAttempts. The worker's summary is shown as it was given ("(none)" if empty).
func Message(t plan.Task, attempt, maxAttempts int, summary string, workerMsgs []model.ChatMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Task %s: %s (attempt %d of %d)\n\n", t.ID, t.Title, attempt, maxAttempts)
	if t.DoneWhen != "" {
		fmt.Fprintf(&b, "Done when: %s\n", t.DoneWhen)
	}
	if t.Needs != "" {
		fmt.Fprintf(&b, "Needs: %s\n", t.Needs)
	}
	if body := strings.TrimSpace(t.Body); body != "" {
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	if strings.TrimSpace(summary) == "" {
		summary = "(none)"
	}
	fmt.Fprintf(&b, "\n## The worker's summary\n\n%s\n\n## The worker's tool calls\n\n%s", strings.TrimSpace(summary), ToolLog(workerMsgs))
	return b.String()
}

// ToolLog returns the tool calls in msgs, one block per call: name, arguments and result, each cut to maxCallBytes.
//
// Notes:
//   - Results are matched to calls by tool call ID. A call with no result (the loop stopped first) shows "(no result)".
//   - The whole log stays under maxLogBytes: calls that no longer fit are left out, with a note of how many. No calls:
//     "(none)".
func ToolLog(msgs []model.ChatMessage) string {
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	var b strings.Builder
	total, shown := 0, 0
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			total++
			result, ok := results[c.ID]
			if !ok {
				result = "(no result)"
			}
			block := fmt.Sprintf("- %s %s\n  → %s\n", c.Function.Name, cut(c.Function.Arguments), strings.ReplaceAll(cut(result), "\n", "\n    "))
			if b.Len()+len(block) > maxLogBytes {
				continue
			}
			b.WriteString(block)
			shown++
		}
	}
	if total == 0 {
		return "(none)\n"
	}
	if shown < total {
		fmt.Fprintf(&b, "(%d more calls not shown)\n", total-shown)
	}
	return b.String()
}

// cut returns s cut to maxCallBytes, with " …" when something was cut.
//
// Notes: none.
func cut(s string) string {
	if c, ok := textutils.CutBytes(strings.TrimSpace(s), maxCallBytes); ok {
		return c + " …"
	}
	return strings.TrimSpace(s)
}
