package director

import (
	"bytes"
	"fmt"
	"strings"
)

// MaxReplyChars caps how much of one worker reply the director sees, so one long reply can't flood
// the director's context.
const MaxReplyChars = 4000

// Instructions is the director's built-in instruction text. Config can add to it
// (director.additionalInstructions) but never replace it.
const Instructions = `You are the director of an autonomous AI agent run. You do not do the work yourself: a separate
worker agent does it. You talk only to the worker, and the worker has its own tools (listed below).

How the run works:
- The run works through a task list, one task at a time. The code tracks the list and moves between
  phases; your job in each phase is described under "Current phase".
- To instruct the worker, end your turn with a plain-text reply and no tool call. That final reply is
  sent to the worker word for word as its next prompt, so write it as a direct instruction to the
  worker, never as commentary. Text you write in the same reply as a tool call is NOT sent to anyone:
  check files first, then give the instruction as your final reply.
- Say what to achieve and how it will be checked; leave the actual content (code, prose) to the worker
  unless the task requires exact text.
- The worker never sees the objective or this task list, only your prompts. It also forgets everything
  between tasks. If a task needs results from an earlier task (file names, findings), put them in the
  prompt.
- After each worker turn you get its reply, the tools it called, and any error. Judge it critically
  against the current task. Don't trust claims without evidence: ask the worker to show results (for
  example, the contents of a file it wrote) or check them yourself with your read-only file tools.
  Those tools are for checking results, not for exploring: don't call them repeatedly, and expect the
  working directory to start empty or unfamiliar.
- Judge the work against what the task asks for and against real checks (builds, tests, file contents),
  not against design preferences the task never stated. Working code that meets the task is done.
- When the current task is complete, call task_done with a note recording anything later tasks need.
  If it is not complete, reply with a follow-up instruction (plain text). Do not repeat an instruction
  that already failed; change the approach.
- task_failed ends the task and stops the whole run. Never call it to ask for changes or fixes: send
  them as a follow-up instruction. Use it only when the task cannot be done (for example required
  input is missing) or the worker is out of turns and the task is still not done.
- Use only the tools offered to you. Never reply with commentary like "the task is done": call the tool.`

// PromptInput is everything the director's system prompt is built from.
type PromptInput struct {
	Objective       Objective
	Plan            *Plan
	WorkerTools     []string // names of the worker's tools, so the director writes feasible prompts
	MaxTurnsPerTask int
	Extra           string // director.additionalInstructions

	Iteration, MaxIterations int     // the current plan → work → review round (from 1), and the limit
	Earlier                  []*Plan // the plans of the earlier rounds, oldest first
}

// SystemPrompt builds the director's system prompt. It is rebuilt before every director turn, so the
// task statuses and notes in it are always current and survive context clearing and compaction.
func SystemPrompt(in PromptInput) string {
	var b strings.Builder
	b.WriteString(Instructions)
	if extra := strings.TrimSpace(in.Extra); extra != "" {
		b.WriteString("\n\nAdditional instructions:\n" + extra)
	}

	b.WriteString("\n\n## Worker tools\n")
	if len(in.WorkerTools) == 0 {
		b.WriteString("(none: the worker can only answer from its own knowledge)")
	} else {
		b.WriteString(strings.Join(in.WorkerTools, ", "))
	}
	fmt.Fprintf(&b, "\nThe worker gets at most %d turns per task.", in.MaxTurnsPerTask)

	b.WriteString("\n\n## Objective\n" + in.Objective.Text)
	for _, s := range in.Objective.Sections {
		fmt.Fprintf(&b, "\n\n## %s\n%s", s.Heading, s.Body)
	}

	if len(in.Earlier) > 0 {
		// [agent] contexts are cleared per task, so the system prompt is the only place earlier rounds can live
		b.WriteString("\n\n## Earlier rounds\nThe files from these rounds are still in the working directory.\n")
		for k, p := range in.Earlier {
			fmt.Fprintf(&b, "\nRound %d:\n", k+1)
			writeTasks(&b, p)
			fmt.Fprintf(&b, "Review: %s\n", p.Summary)
			for _, g := range p.Gaps {
				fmt.Fprintf(&b, "- gap: %s\n", g)
			}
		}
	}

	b.WriteString("\n\n## Tasks")
	if in.Iteration > 1 {
		fmt.Fprintf(&b, " (round %d of %d)", in.Iteration, in.MaxIterations)
	}
	b.WriteString("\n")
	if notes := strings.TrimSpace(in.Objective.TaskNotes); notes != "" {
		b.WriteString(notes + "\n")
	}
	if len(in.Plan.Tasks) == 0 {
		b.WriteString("(no tasks yet)")
	}
	writeTasks(&b, in.Plan)

	b.WriteString("\n## Current phase\n")
	switch in.Plan.Phase {
	case PhasePlan:
		if len(in.Earlier) > 0 {
			fmt.Fprintf(&b, "PLAN. Break the gaps from the last review into 1-%d concrete tasks for the worker, in order, and call set_tasks.", in.Plan.MaxTasks)
		} else {
			fmt.Fprintf(&b, "PLAN. Break the objective into 1-%d concrete tasks for the worker, in order, and call set_tasks.", in.Plan.MaxTasks)
		}
	case PhaseWork:
		fmt.Fprintf(&b, "WORK, task %d of %d. Reply with the worker's next prompt for this task, or call task_done or task_failed.", in.Plan.Current+1, len(in.Plan.Tasks))
	case PhaseReview:
		b.WriteString("REVIEW. Check the results against the objective")
		if in.Objective.HasDone() {
			b.WriteString(" and its Done criteria")
		}
		b.WriteString(", then call finish.")
	case PhaseDone:
		b.WriteString("DONE.")
	}
	return b.String()
}

// writeTasks writes a plan's numbered task list with statuses and notes.
func writeTasks(b *strings.Builder, p *Plan) {
	for i, t := range p.Tasks {
		status := string(t.Status)
		if p.Phase == PhaseWork && i == p.Current {
			status = "current"
		}
		fmt.Fprintf(b, "%d. [%s] %s", i+1, status, t.Text)
		if t.Note != "" {
			fmt.Fprintf(b, "\n   note: %s", t.Note)
		}
		b.WriteString("\n")
	}
}

// PlanPrompt asks the director for the task list (PLAN phase).
func PlanPrompt() string {
	return "Create the task list for the objective and call set_tasks. You may look at the working directory first with your file tools."
}

// GapsPlanPrompt asks the director for the next round's task list, built from the last review's gaps.
func GapsPlanPrompt(last *Plan, iteration, maxIterations int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d of %d. The last review found the objective not met:\n%s\n\nGaps:\n", iteration, maxIterations, last.Summary)
	for _, g := range last.Gaps {
		fmt.Fprintf(&b, "- %s\n", g)
	}
	b.WriteString("\nCreate a task list that closes only these gaps and call set_tasks. Build on the existing files " +
		"(check them with your file tools first) rather than starting over, and don't redo work that is already done.")
	return b.String()
}

// TaskStartPrompt asks the director for the worker's first prompt for a task.
func TaskStartPrompt(i, n int, task string) string {
	return fmt.Sprintf("Task %d of %d: %s\n\nWrite the worker's first prompt for this task. Reply with the prompt text only; it is sent to the worker word for word. (If the task turns out to be done already, the worker can confirm that and you can mark it done after its reply.)", i+1, n, task)
}

// WorkerOutcome is what the director is told about one worker turn.
type WorkerOutcome struct {
	Turn, MaxTurns int // this task's worker turn number, and the per-task limit
	Reply          string
	ToolsCalled    []string
	Err            error
}

// WrapTurn builds the director's message for a worker turn: the task header, the (capped) reply,
// the tools the worker called, any error, and the question to answer.
func WrapTurn(i, n int, task string, o WorkerOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %d of %d: %s\n", i+1, n, task)
	fmt.Fprintf(&b, "Worker turn %d of %d for this task.\n\n", o.Turn, o.MaxTurns)
	if o.Err != nil {
		fmt.Fprintf(&b, "The worker's turn failed with an error: %v\n", o.Err)
		// [agent] A failed turn isn't added to the worker's memory and its tool list is lost, but the files it
		// wrote are still there. Without this line the director read "failed ... Tools called: none" as
		// "nothing happened" and gave up on a task whose tests were in fact passing (real run, 2026-10-14).
		b.WriteString("\nThe worker may have done useful work before the error: any files it wrote or changed are kept, " +
			"but the worker won't remember this turn. Check the files with your tools, then decide; a follow-up prompt " +
			"should tell the worker what state the work is in.\n")
	} else {
		b.WriteString("Worker reply:\n" + truncate(o.Reply, MaxReplyChars) + "\n")
	}
	switch {
	case o.Err != nil:
		// the tools called in a failed turn are unknown; don't claim "none"
	case len(o.ToolsCalled) == 0:
		b.WriteString("\nTools called: none\n")
	default:
		b.WriteString("\nTools called: " + strings.Join(o.ToolsCalled, ", ") + "\n")
	}
	b.WriteString("\nIs the task complete? Call task_done (with a note), reply with a follow-up prompt for the worker, or call task_failed.")
	if o.Turn >= o.MaxTurns {
		b.WriteString(" This was the last worker turn allowed for this task: a follow-up prompt will not be sent, so call task_done or task_failed.")
	}
	return b.String()
}

// ReviewPrompt asks the director for the round's review (REVIEW phase). roundsLeft is how many more
// rounds can follow this one.
func ReviewPrompt(p *Plan, hasDone bool, roundsLeft int) string {
	var b strings.Builder
	if p.Failed() {
		for i, t := range p.Tasks {
			if t.Status == TaskFailed {
				fmt.Fprintf(&b, "The run stopped because task %d failed: %s\n", i+1, t.Note)
			}
		}
	} else {
		fmt.Fprintf(&b, "All %d tasks are done.\n", len(p.Tasks))
	}
	b.WriteString("\nReview the results against the objective")
	if hasDone {
		b.WriteString(" and its Done criteria")
	}
	b.WriteString(". You may check files with your read-only file tools. Then call finish: success=true only if the objective is met, a short summary for the user, and gaps listing anything missing or incomplete.")
	if roundsLeft > 0 {
		// [agent] the guard against a picky reviewer: gaps cost a whole new round, so ask only for real ones
		fmt.Fprintf(&b, "\n\nIf the objective is not met, the gaps you list become the tasks of a new round of work (%d more round(s) allowed). "+
			"So make each gap concrete and fixable, and list only what the objective and its quality bar actually require: no nice-to-haves or style preferences.", roundsLeft)
	}
	return b.String()
}

// Nudges, sent when the director replies with text where a tool call was required (or with nothing).
const (
	NudgePlan   = "You must call set_tasks now, with the task list."
	NudgeWork   = "Your reply was empty. Reply with the worker's next prompt, or call task_done or task_failed."
	NudgeReview = "You must call finish now, with success, summary and gaps."
	NudgeLimit  = "The worker has used all its turns for this task, so no more prompts can be sent. Call task_done if the task is complete, otherwise task_failed."
)

// truncate cuts s to at most max bytes (never inside a UTF-8 character) and says so.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := string(bytes.ToValidUTF8([]byte(s[:max]), nil))
	return cut + fmt.Sprintf("\n[... reply truncated: first %d of %d bytes shown]", len(cut), len(s))
}
