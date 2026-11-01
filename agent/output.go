package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func (o chatOutput) Assistant(text string) { fmt.Fprintf(o.w, "[assistant] %s\n", text) }

func (o chatOutput) ToolCalled(name string) { fmt.Fprintln(o.w, "[tool] Tool called:", name) }

func (o chatOutput) Thinking() (stop func()) {
	if !o.showProgress {
		return func() {}
	}
	return startProgressDots(o.w, progressDotInterval)
}

func (o chatOutput) System(text string) { fmt.Fprintln(o.w, "[system]", text) }

func (o chatOutput) Error(what string, err error) {
	if errors.Is(err, context.Canceled) { // Ctrl+C cancelled the context: not a failure to report
		o.Interrupted()
		return
	}
	fmt.Fprintf(o.w, "[error] %s: %v\n", what, err)
}

func (o chatOutput) Interrupted() { fmt.Fprintln(o.w, "[interrupted]") }
