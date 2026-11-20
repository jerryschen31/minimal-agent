package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/plan"
)

// Auto mode (-mode auto -plan <file>, also -mode autonomous) runs a plan file's tasks without a person
// (PLAN-auto-mode.md). The plan file is its only input, like Terraform's saved plan: auto mode never reads the objective
// file and doesn't check whether the plan is stale; the plan carries the objective in its "## Objective" section.
// [agent] 1a: read and check the plan, show the summary, ask Run? [Y/n]. 1d: run the tasks one at a time
// (autorun.go), each with a fresh worker (autoworker.go). 1e: the director judges each attempt (autodirector.go,
// agent/director), with retries, and gates are asked.

// autoModeDetails is what auto mode keeps on the session (ChatSession.ModeDetails).
type autoModeDetails struct {
	PlanPath string                      // the -plan file
	Plan     plan.Plan                   // the plan as read (after plan.Cleanup); set by autoMode
	planData []byte                      // the plan file's bytes, read once; the run folder keeps them as plan.md
	conns    []*mcpconnect.McpConnection // the MCP servers that connected, for the run's manifest
}

// ModeName returns config.ModeAuto.
//
// Notes: none.
func (*autoModeDetails) ModeName() string { return config.ModeAuto }

// setupAutoMode adds auto mode's details, output, worker prompt and approval setting to cs; conns are the MCP servers
// that connected (recorded in each run's manifest).
//
// Notes:
//   - Called by SetupAgent in auto mode, after the session is built. Nothing is read yet: the plan is read when the
//     mode runs, so a missing file is reported by autoMode, not by setup.
//   - The output is autoOutput: tool names, not the worker's text (that is in the trace; -debug shows it).
//   - The system prompt is workerPrompt, whatever the config's systemPrompt (that one is chat's).
//   - Nobody is asked during a run (PromptAllowDenyCall stays nil), so an ask-rule call is denied, unless approval is
//     "allow-all": then it runs. Answering "Run?" (or -yes) is the confirmation chat asks for at startup.
func setupAutoMode(cs *ChatSession, conns []*mcpconnect.McpConnection) {
	cs.ModeDetails = &autoModeDetails{PlanPath: cs.Config.PlanFile, conns: conns}
	cs.Output = autoOutput{planOutput: planOutput{chatOutput: chatOutput{w: cs.OutBuffer, showProgress: isTerminal(cs.OutBuffer)}, showTools: cs.Config.Debug, hideReplies: true}}
	cs.SystemMsg = createSystemMessage(workerPrompt)
	cs.AllowAskRules = cs.Config.Approval == config.ApprovalAllowAll
}

// runAutoMode runs auto mode on the session's input and output.
//
// Notes:
//   - Without -yes it asks "Run? [Y/n]", so it needs a terminal; piped or redirected stdin is an error naming -yes.
//   - Gates are asked only on a terminal, -yes or not: without one, a gated task stops the run with "waiting".
func runAutoMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession) error {
	terminal := isTerminal(cs.InBuffer)
	if !cs.Config.AssumeYes && !terminal {
		return errors.New("auto mode asks before it runs, so it needs a terminal; to run without one, add -yes")
	}
	reader := newPromptReader(ctx, cs.InBuffer, cs.OutBuffer)
	var gates promptReader
	if terminal {
		gates = reader
	}
	return autoMode(ctx, interrupts, cs, reader, gates)
}

// autoMode is runAutoMode with the prompt readers passed in, so tests can use a plainReader or a scripted terminal:
// reader asks "Run?", gates asks a gated task's question (nil = nobody to ask).
//
// Notes:
//   - Reads the plan (loadRunnablePlan), prints its summary (plan.Summary), then asks "Run? [Y/n]: " (-yes answers
//     yes). No, or Ctrl+D at the question, prints "Not run." and ends with no error.
//   - Yes: opens a run (openRun: lock, history, resume of a run a crash cut off, run folder), runs the tasks (runPlan)
//     and ends the run (end: run_ended, report). A resume that needs an answer it can't get stops with "waiting". Ctrl+C stops the run (an interrupt watcher covers all of it); the run is recorded as interrupted.
func autoMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession, reader, gates promptReader) error {
	ad, ok := cs.ModeDetails.(*autoModeDetails)
	if !ok {
		return errors.New("auto mode was not set up (no auto mode details on the session)")
	}
	w := cs.OutBuffer

	p, data, err := loadRunnablePlan(ad.PlanPath, cs.Config.MaxPlanTasks, w)
	if err != nil {
		return err
	}
	ad.Plan, ad.planData = p, data
	fmt.Fprint(w, plan.Summary(p))

	run := cs.Config.AssumeYes
	if !run {
		answer, err := reader.AskLine(ctx, "Run? [Y/n]: ")
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(w, "\nNot run.")
			return nil
		}
		if err != nil {
			return err
		}
		run = isYes(answer, true)
	}
	if !run {
		fmt.Fprintln(w, "Not run.")
		return nil
	}

	discardStrayInterrupts(interrupts)
	runCtx, stop := createInterruptWatcher(ctx, interrupts)
	defer stop()
	ar, err := openRun(runCtx, cs, ad, gates)
	if errors.Is(err, errResumeWaiting) {
		fmt.Fprintln(w, "Stopped: waiting. Nothing was run.")
		return fmt.Errorf("resume: %w", ErrNeedsPerson)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Run %04d (%s)\n", ar.run.Number, ar.run.Dir)
	res := runPlan(runCtx, cs, ar, p, gates)
	return ar.end(cs, res, cs.Config.TraceDir, cs.Recorder.RunID(), w)
}

// loadRunnablePlan reads the plan file at path and returns it, and the file's bytes, if it can run.
//
// Notes:
//   - Any file name works: a file is a plan by its content (plan.Cleanup reads it and it has tasks), not by its name.
//   - Errors, each naming the file: it doesn't exist; Cleanup can't read it; it has no tasks ("not a plan");
//     CheckComplete refuses it ("not ready to run", with every problem).
//   - The fixes Cleanup made (small hand-edit slips) are printed to w, one per line, because what runs is the
//     cleaned-up plan and the user should see where it differs from what they wrote. The file is never rewritten.
//   - Neither the objective file nor the plan's objective_hash is looked at: the plan is the whole input.
func loadRunnablePlan(path string, maxTasks int, w io.Writer) (plan.Plan, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return plan.Plan{}, nil, fmt.Errorf("%s: no such file", path)
	}
	if err != nil {
		return plan.Plan{}, nil, err
	}
	p, fixes, err := plan.Cleanup(data)
	if err != nil {
		return plan.Plan{}, nil, fmt.Errorf("%s can't be read as a plan: %w", path, err)
	}
	if len(p.Tasks) == 0 {
		return plan.Plan{}, nil, fmt.Errorf("%s is not a plan: it has no \"## Tasks\" section with \"### t1: …\" tasks", path)
	}
	for _, f := range fixes {
		fmt.Fprintf(w, "%s: read as fixed: %s\n", path, f)
	}
	if err := p.CheckComplete(maxTasks); err != nil {
		return plan.Plan{}, nil, fmt.Errorf("%s is not ready to run; fix it:\n%w", path, err)
	}
	return p, data, nil
}

// autoOutput is auto mode's Output: plan mode's (the worker's and director's replies hidden unless -debug), but every
// tool call is shown as an indented "[tool] <name>" line, so a long task shows it is moving.
type autoOutput struct {
	planOutput
	label string // before "[tool]": "" for the worker, "director: " for the director
}

// ToolCalled prints an indented "<label>[tool] <name>" line for every tool call.
//
// Notes: none.
func (o autoOutput) ToolCalled(name string) { fmt.Fprintf(o.w, "    %s[tool] %s\n", o.label, name) }
