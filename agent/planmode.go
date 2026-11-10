package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryschen31/minimal-agent/agent/planner"
	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/tools"
)

// [agent] Plan mode (-mode plan, PLAN-plan-apply.md step 2) runs through SetupAgent and RunAgent like chat. Built so far:
// the objective chat for a new objective (the name prompts, update_objective, /save, /edit). Still to come: making the
// plan from the saved objective (propose_plan, Accept?, revise) and the -objective entry.

// planFooter is shown under the draft each time it changes.
const planFooter = "Type /save to save it and start planning, /edit to change it yourself, or tell me what to change."

// planModeDetails is what plan mode keeps on the session (ChatSession.ModeDetails).
type planModeDetails struct {
	ObjectivePath string               // where /save writes the objective
	Objective     *objective.Objective // the draft: update_objective and /edit change it, /save writes it
	plannerTools  []tools.Tool         // the built-in and MCP tools, without plan mode's own: what the planner run gets
	queued        string               // a line chatLoop takes as the user's next input before asking (the first answer)
	saved         bool                 // /save wrote the objective
}

// ModeName returns config.ModePlan.
//
// Notes: none.
func (*planModeDetails) ModeName() string { return config.ModePlan }

// setupPlanMode adds plan mode's details to cs and registers the update_objective tool.
//
// Notes:
//   - Called by SetupAgent in plan mode, after the session is built. baseTools (the built-in and MCP tools) is kept for
//     the planner run, which gets them without update_objective.
//   - The draft starts empty; update_objective shows it (showObjective) after each change.
func setupPlanMode(cs *ChatSession, baseTools []tools.Tool) error {
	pd := &planModeDetails{Objective: &objective.Objective{}, plannerTools: baseTools}
	cs.ModeDetails = pd
	return cs.Tools.Register(planner.NewUpdateObjective(pd.Objective, func() { showObjective(cs, pd) }))
}

// runPlanMode runs plan mode on the session's input and output.
//
// Notes:
//   - Without -objective plan mode asks questions, so it needs a terminal; piped or redirected stdin is an error rather
//     than a session nobody can answer.
func runPlanMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession) error {
	if cs.Config.ObjectiveFile == "" && !isTerminal(cs.InBuffer) {
		return errors.New("plan mode needs a terminal to write a new objective with you; use -objective <file|text> to plan one you have")
	}
	return planMode(ctx, interrupts, cs, newPromptReader(ctx, cs.InBuffer, cs.OutBuffer))
}

// planMode is runPlanMode with the prompt reader passed in, so tests can use a plainReader or a scripted terminal.
//
// Notes:
//   - Without -objective: ask for the new objective's file (askObjectivePath) and the objective (askObjectiveText),
//     then run the objective chat. Ctrl+D at either question ends plan mode with nothing written, without an error.
//   - -objective and making the plan after /save are not built yet (step 2, items 7 and 8).
func planMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession, reader promptReader) error {
	pd, ok := cs.ModeDetails.(*planModeDetails)
	if !ok {
		return errors.New("plan mode was not set up (no plan mode details on the session)")
	}
	w := cs.OutBuffer
	if cs.Config.ObjectiveFile != "" {
		return errors.New("planning an objective given with -objective is not built yet (PLAN-plan-apply.md step 2)")
	}

	path, err := askObjectivePath(ctx, reader, w)
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(w, "\nCancelled; nothing written.")
		return nil
	}
	if err != nil {
		return err
	}
	first, err := askObjectiveText(ctx, reader)
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(w, "\nCancelled; nothing written.")
		return nil
	}
	if err != nil {
		return err
	}
	pd.ObjectivePath = path

	saved, err := runObjectiveChat(ctx, interrupts, cs, reader, first)
	if err != nil {
		return err
	}
	if !saved {
		fmt.Fprintln(w, "No objective saved; nothing written.")
		return nil
	}
	fmt.Fprintln(w, "[plan mode] Making the plan from the saved objective is not built yet.")
	return nil
}

// askObjectivePath asks where to save the new objective until it gets a usable answer, and returns the path.
//
// Notes:
//   - An empty answer means objective.md; ".md" is added when missing; a relative path is relative to the current
//     directory (newObjectivePath). A missing parent directory, or a directory given as the file, asks again.
//   - An existing file asks "<path> already exists. Overwrite (y/N)?: "; anything but y or yes asks for the name again.
//   - Ctrl+D at either question returns io.EOF.
func askObjectivePath(ctx context.Context, reader promptReader, w io.Writer) (string, error) {
	for {
		answer, err := reader.AskLine(ctx, "Planning mode for a new objective: Name or path for the new objective file (default: objective.md | Ctrl+D to cancel): ")
		if err != nil {
			return "", err
		}
		path, err := newObjectivePath(answer)
		if err != nil {
			fmt.Fprintln(w, err)
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			fmt.Fprintf(w, "%s is a directory; give a file name.\n", path)
			continue
		}
		if err == nil {
			answer, err := reader.AskLine(ctx, path+" already exists. Overwrite (y/N)?: ")
			if err != nil {
				return "", err
			}
			if !isYes(answer, false) {
				continue
			}
		}
		return path, nil
	}
}

// newObjectivePath turns the answer to the file-name question into the objective's path.
//
// Notes:
//   - Blank means "objective.md"; ".md" (any case) is added when the name doesn't end in it; the path is cleaned.
//   - The parent directory must exist (it is never created): otherwise the error is "No such directory: <dir>".
func newObjectivePath(answer string) (string, error) {
	path := strings.TrimSpace(answer)
	if path == "" {
		path = "objective.md"
	}
	if !strings.EqualFold(filepath.Ext(path), ".md") {
		path += ".md"
	}
	path = filepath.Clean(path)
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return "", fmt.Errorf("No such directory: %s", filepath.Dir(path))
	}
	return path, nil
}

// askObjectiveText asks "What is your objective? : " until the answer isn't blank, and returns it trimmed.
//
// Notes:
//   - Ctrl+D returns io.EOF.
func askObjectiveText(ctx context.Context, reader promptReader) (string, error) {
	for {
		answer, err := reader.AskLine(ctx, "What is your objective? : ")
		if err != nil {
			return "", err
		}
		if answer = strings.TrimSpace(answer); answer != "" {
			return answer, nil
		}
	}
}

// runObjectiveChat runs the objective chat, starting with first as the user's first message, and reports whether
// /save wrote the objective.
//
// Notes:
//   - It is chatLoop with plan mode's details on the session: first is queued (chatLoop takes it before asking), /save
//     and /edit work, and update_objective changes the draft.
//   - /exit, /quit or Ctrl+D leave without saving: saved is false and nothing is written.
func runObjectiveChat(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession, reader promptReader, first string) (bool, error) {
	pd := cs.ModeDetails.(*planModeDetails)
	pd.queued = first
	cs.Output.System("Type /save to save the objective and start planning, /edit to change it yourself, or /exit to leave without saving.")
	err := chatLoop(ctx, interrupts, cs, reader)
	return pd.saved, err
}

// showObjective prints the draft and how to save or edit it.
//
// Notes:
//   - Called by update_objective after each change and after /edit.
func showObjective(cs *ChatSession, pd *planModeDetails) {
	cs.Output.System(fmt.Sprintf("Here is the objective (%s, not saved yet):\n\n%s\n%s", pd.ObjectivePath, pd.Objective.Markdown(), planFooter))
}

// saveObjective writes the draft to pd.ObjectivePath (plan mode's /save) and marks it saved.
//
// Notes:
//   - A draft with no objective text is not saved: the error says there is nothing to save yet.
//   - The file is replaced in one step (writeFileAtomic); the user already agreed to overwrite an existing one.
func saveObjective(cs *ChatSession, pd *planModeDetails) error {
	if strings.TrimSpace(pd.Objective.ObjText) == "" {
		return errors.New("Nothing to save yet: the draft has no objective text.")
	}
	if err := writeFileAtomic(pd.ObjectivePath, []byte(pd.Objective.Markdown())); err != nil {
		return fmt.Errorf("Could not save %s: %w", pd.ObjectivePath, err)
	}
	pd.saved = true
	cs.Output.System("Saved " + pd.ObjectivePath + ".")
	return nil
}

// editObjective runs plan mode's /edit: the prompt is filled with the draft, and what the user submits becomes the new
// draft if it reads as an objective.
//
// Notes:
//   - Ctrl+D (or a cancelled context) leaves the draft unchanged.
//   - The text goes through objective.Parse: an error is shown and the draft is unchanged.
//   - On success the draft is shown again, and a user message saying what the draft now is goes into the history and
//     context, so the model sees the change on its next turn. No model call is made now.
func editObjective(ctx context.Context, cs *ChatSession, pd *planModeDetails, reader promptReader) {
	text, err := reader.ReadPromptFilled(ctx, strings.Split(strings.TrimSuffix(pd.Objective.Markdown(), "\n"), "\n"))
	if err != nil {
		cs.Output.System("Edit cancelled; the draft is unchanged.")
		return
	}
	o, err := objective.Parse([]byte(text))
	if err != nil {
		cs.Output.System(fmt.Sprintf("The edit was not applied (%v); the draft is unchanged.", err))
		return
	}
	*pd.Objective = o
	showObjective(cs, pd)
	note := model.CreateChatMessage("user", "user", "I edited the objective draft myself. It is now:\n\n"+o.Markdown(), "", nil)
	cs.MsgHistory.Append([]model.ChatMessage{note})
	cs.MsgContext.AddMessages([]model.ChatMessage{note})
}

// writeFileAtomic writes data to path through a temporary file in the same directory and a rename, so path never
// holds half a file.
//
// Notes:
//   - The file gets mode 0644. An existing file at path is replaced; the temporary file is removed on any error.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has moved it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// isYes reports whether answer is yes: "y" or "yes" in any case; blank means def.
//
// Notes:
//   - Anything else, "n" and "no" included, is no.
func isYes(answer string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}
