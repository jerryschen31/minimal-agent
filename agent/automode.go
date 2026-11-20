package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/plan"
)

// Auto mode (-mode auto -plan <file>, also -mode autonomous) runs a plan file's tasks without a person
// (PLAN-auto-mode.md). The plan file is its only input, like Terraform's saved plan: auto mode never reads the objective
// file and doesn't check whether the plan is stale; the plan carries the objective in its "## Objective" section.
// [agent] Step 1a: read and check the plan, show the summary, ask Run? [Y/n]. Running the tasks arrives in 1d.

// autoModeDetails is what auto mode keeps on the session (ChatSession.ModeDetails).
type autoModeDetails struct {
	PlanPath string    // the -plan file
	Plan     plan.Plan // the plan as read (after plan.Cleanup); set by autoMode
}

// ModeName returns config.ModeAuto.
//
// Notes: none.
func (*autoModeDetails) ModeName() string { return config.ModeAuto }

// setupAutoMode adds auto mode's details and output to cs.
//
// Notes:
//   - Called by SetupAgent in auto mode, after the session is built. Nothing is read yet: the plan is read when the
//     mode runs, so a missing file is reported by autoMode, not by setup.
//   - The output is plan mode's (no "[assistant]"/"[system]" labels, tool calls only with -debug).
func setupAutoMode(cs *ChatSession) {
	cs.ModeDetails = &autoModeDetails{PlanPath: cs.Config.PlanFile}
	cs.Output = newPlanOutput(cs.OutBuffer, cs.Config.Debug)
}

// runAutoMode runs auto mode on the session's input and output.
//
// Notes:
//   - Without -yes it asks "Run? [Y/n]", so it needs a terminal; piped or redirected stdin is an error naming -yes.
func runAutoMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession) error {
	if !cs.Config.AssumeYes && !isTerminal(cs.InBuffer) {
		return errors.New("auto mode asks before it runs, so it needs a terminal; to run without one, add -yes")
	}
	return autoMode(ctx, interrupts, cs, newPromptReader(ctx, cs.InBuffer, cs.OutBuffer))
}

// autoMode is runAutoMode with the prompt reader passed in, so tests can use a plainReader or a scripted terminal.
//
// Notes:
//   - Reads the plan (loadRunnablePlan), prints its summary (plan.Summary), then asks "Run? [Y/n]: " (-yes answers
//     yes). No, or Ctrl+D at the question, prints "Not run." and ends with no error.
//   - [agent] 1a stops after the question; interrupts is used from 1d on (each task gets an interrupt watcher).
func autoMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession, reader promptReader) error {
	ad, ok := cs.ModeDetails.(*autoModeDetails)
	if !ok {
		return errors.New("auto mode was not set up (no auto mode details on the session)")
	}
	w := cs.OutBuffer

	p, err := loadRunnablePlan(ad.PlanPath, cs.Config.MaxPlanTasks, w)
	if err != nil {
		return err
	}
	ad.Plan = p
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
	fmt.Fprintln(w, "Running a plan arrives in the next step (1d); nothing was run.")
	return nil
}

// loadRunnablePlan reads the plan file at path and returns it if it can run.
//
// Notes:
//   - Any file name works: a file is a plan by its content (plan.Cleanup reads it and it has tasks), not by its name.
//   - Errors, each naming the file: it doesn't exist; Cleanup can't read it; it has no tasks ("not a plan");
//     CheckComplete refuses it ("not ready to run", with every problem).
//   - The fixes Cleanup made (small hand-edit slips) are printed to w, one per line, because what runs is the
//     cleaned-up plan and the user should see where it differs from what they wrote. The file is never rewritten.
//   - Neither the objective file nor the plan's objective_hash is looked at: the plan is the whole input.
func loadRunnablePlan(path string, maxTasks int, w io.Writer) (plan.Plan, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return plan.Plan{}, fmt.Errorf("%s: no such file", path)
	}
	if err != nil {
		return plan.Plan{}, err
	}
	p, fixes, err := plan.Cleanup(data)
	if err != nil {
		return plan.Plan{}, fmt.Errorf("%s can't be read as a plan: %w", path, err)
	}
	if len(p.Tasks) == 0 {
		return plan.Plan{}, fmt.Errorf("%s is not a plan: it has no \"## Tasks\" section with \"### t1: …\" tasks", path)
	}
	for _, f := range fixes {
		fmt.Fprintf(w, "%s: read as fixed: %s\n", path, f)
	}
	if err := p.CheckComplete(maxTasks); err != nil {
		return plan.Plan{}, fmt.Errorf("%s is not ready to run; fix it:\n%w", path, err)
	}
	return p, nil
}
