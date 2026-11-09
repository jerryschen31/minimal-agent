package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/objective"
)

// [agent] Plan mode (-mode plan) is still a stub (PLAN-plan-apply.md step 2 replaces it): it reads the objective file
// (or asks for the objective), prints it the way the objective package reads it, and saves nothing. The planner
// (propose_plan) comes next, and plan mode then starts calling SetupAgent.

// RunPlanMode runs plan mode on the real terminal and returns when it is done.
//
// Notes:
//   - Plan mode needs a person at a terminal (it asks questions and, later, asks before saving), so piped or redirected
//     stdin is an error rather than a session nobody can answer.
//   - It doesn't call SetupAgent yet: the stub makes no model call, starts no MCP server and writes no trace.
func RunPlanMode(ctx context.Context, cfg config.Config) error {
	if !isTerminal(cfg.InBuffer) {
		return errors.New("plan mode needs a terminal: it asks you questions and asks before saving")
	}
	return planStub(ctx, cfg, newPromptReader(ctx, cfg.InBuffer, cfg.OutBuffer))
}

// planStub reads the objective file named by cfg.ObjectiveFile, or asks for the objective, and prints it.
//
// Notes:
//   - The objective file goes through objective.Parse; a file Parse refuses is an error naming the file.
//   - Without an objective file, the answer to "What do you want to build?" becomes the objective's text. Ctrl+D, end of input or
//     a blank answer ends plan mode without an error.
//   - What it prints is o.Markdown(): the objective in the standard form, as the planner will see it.
//   - The reader is passed in so tests can use a plainReader; RunPlanMode passes the terminal one.
func planStub(ctx context.Context, cfg config.Config, reader promptReader) error {
	w := cfg.OutBuffer
	var o objective.Objective

	if cfg.ObjectiveFile != "" {
		data, err := os.ReadFile(cfg.ObjectiveFile)
		if err != nil {
			return err
		}
		o, err = objective.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.ObjectiveFile, err)
		}
		fmt.Fprintf(w, "Read the objective %s.\n", cfg.ObjectiveFile)
	} else {
		fmt.Fprintln(w, "What do you want to build?")
		line, err := reader.ReadPrompt(ctx)
		if errors.Is(err, io.EOF) { // Ctrl+D or end of input: nothing to plan
			fmt.Fprintln(w, "\nNo objective given; nothing written.")
			return nil
		}
		if err != nil {
			return err
		}
		o.ObjText = strings.TrimSpace(line)
		if o.ObjText == "" {
			fmt.Fprintln(w, "No objective given; nothing written.")
			return nil
		}
	}

	fmt.Fprintln(w, "\nThe objective, as plan mode reads it:")
	fmt.Fprintln(w)
	fmt.Fprint(w, o.Markdown())
	fmt.Fprintln(w, "\n[plan mode] The planner is not built yet; nothing was written.")
	return nil
}
