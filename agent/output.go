package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Output is how the agent reports to the user during a session: a turn's progress, system notices and errors.
// reActLoop and chat mode report through it and the Output decides how each message looks, so a later mode (e.g.
// autonomous) implements this once instead of copying prints.
// [agent] Policy item 7 (DECISIONS.md § Policy out of the core). Output is not logging: the trace is the record of
// what happened. Add a method only for a new *kind* of message, never one per message. Not routed through it:
// the -debug context dump, the ask prompt (interactive, tied to the line editor) and info mode's report; they
// write to ChatSession.OutBuffer directly.
type Output interface {
	// a turn in progress (reActLoop)
	Assistant(text string)   // the model's reply text for one step (may be empty when it only calls tools)
	ToolCalled(name string)  // a tool call is about to run, by model-facing name
	Thinking() (stop func()) // waiting for the model; stop is called when the reply (or an error) arrives

	// anything else the agent reports
	System(text string)           // a notice, e.g. "Chat context cleared"
	Error(what string, err error) // what failed and why; a cancel (Ctrl+C) is reported as Interrupted instead
	Interrupted()                 // the user cancelled the current turn or command
}

// chatOutput is chat mode's Output: "[assistant] …", "[tool] …", "[system] …", "[error] …" and "[interrupted]" lines
// on w, and thinking dots while waiting when showProgress is set (a real terminal; see shouldShowProgress).
type chatOutput struct {
	w            io.Writer
	showProgress bool
}

// Assistant prints the model's reply text for one step as an "[assistant]" line.
//
// Notes: none.
func (o chatOutput) Assistant(text string) { fmt.Fprintf(o.w, "[assistant] %s\n", text) }

// ToolCalled prints a "[tool] Tool called:" line with the model-facing tool name.
//
// Notes: none.
func (o chatOutput) ToolCalled(name string) { fmt.Fprintln(o.w, "[tool] Tool called:", name) }

// Thinking starts the thinking dots while waiting for the model and returns the function that stops them.
//
// Notes:
//   - Without showProgress (not a real terminal) it prints nothing and stop is a no-op.
func (o chatOutput) Thinking() (stop func()) {
	if !o.showProgress {
		return func() {}
	}
	return startProgressDots(o.w, progressDotInterval)
}

// System prints a "[system]" notice.
//
// Notes: none.
func (o chatOutput) System(text string) { fmt.Fprintln(o.w, "[system]", text) }

// Error prints an "[error]" line saying what failed and why.
//
// Notes:
//   - A context.Canceled error (Ctrl+C) is not a failure to report, so it is printed as [interrupted] instead.
func (o chatOutput) Error(what string, err error) {
	if errors.Is(err, context.Canceled) { // Ctrl+C cancelled the context: not a failure to report
		o.Interrupted()
		return
	}
	fmt.Fprintf(o.w, "[error] %s: %v\n", what, err)
}

// Interrupted prints the "[interrupted]" line, for a turn or command the user cancelled.
//
// Notes: none.
func (o chatOutput) Interrupted() { fmt.Fprintln(o.w, "[interrupted]") }

// planOutput is plan mode's Output: the objective chat reads like a conversation, so the model's replies and notices
// are printed without "[assistant]" / "[system]" labels, and tool calls only with -debug. Thinking, Error and
// Interrupted are chat mode's (errors keep their "[error]" label so they stand out).
// [agent] Embedding chatOutput reuses its methods; the three defined below replace chat's versions.
type planOutput struct {
	chatOutput
	showTools   bool // -debug: print "[tool] Tool called: …" lines (and replies hidden by hideReplies)
	hideReplies bool // the planner run: its reply text is not meant for the user (it can't ask anything); -debug shows it
}

// newPlanOutput returns plan mode's Output on w; debug turns on the tool-call lines.
//
// Notes:
//   - Thinking dots are shown when w is a real terminal.
func newPlanOutput(w io.Writer, debug bool) planOutput {
	return planOutput{chatOutput: chatOutput{w: w, showProgress: isTerminal(w)}, showTools: debug}
}

// Assistant prints the model's reply text as it is, and nothing for an empty reply.
//
// Notes:
//   - A step that only calls tools has no text; chat mode prints an empty "[assistant]" line for it, plan mode nothing.
//   - With hideReplies (the planner run) nothing is printed unless -debug: a model may write the whole plan as JSON
//     before calling propose_plan, and plan mode shows the checked plan itself.
func (o planOutput) Assistant(text string) {
	if o.hideReplies && !o.showTools {
		return
	}
	if strings.TrimSpace(text) != "" {
		fmt.Fprintln(o.w, text)
	}
}

// ToolCalled prints chat mode's "[tool] Tool called:" line, only with -debug.
//
// Notes: none.
func (o planOutput) ToolCalled(name string) {
	if o.showTools {
		o.chatOutput.ToolCalled(name)
	}
}

// System prints a notice without a label.
//
// Notes: none.
func (o planOutput) System(text string) { fmt.Fprintln(o.w, text) }
