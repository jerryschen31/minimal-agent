package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jerryschen31/minimal-agent/agent/planner"
	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// Plan mode (-mode plan) runs through SetupAgent and RunAgent like chat. Two ways in:
// with -objective <file|text> it plans that objective; without, it writes a new objective with the user in a chat
// (update_objective, /save, /edit) and then plans it. The plan step runs the planner (propose_plan), shows the summary
// and asks Accept? [Y/n]; turning a plan down can reopen the chat to revise the objective, then the plan is made again.

// planFooter is shown under the draft each time it changes.
const planFooter = "Type /save to save it and start planning, /edit to change it yourself, or tell me what to change."

// planModeDetails is what plan mode keeps on the session (ChatSession.ModeDetails).
type planModeDetails struct {
	ObjectivePath string               // where /save writes the objective
	Objective     *objective.Objective // the draft: update_objective and /edit change it, /save writes it
	plannerTools  []tools.Tool         // the built-in and MCP tools, without plan mode's own: what the planner run gets
	queued        string               // a line chatLoop takes as the user's next input before asking (the first answer)
	editNote      string               // set by /edit: put at the start of the user's next message, so the model sees the change
	confirmSave   func() bool          // asked before the first /save over a file the user supplied (-objective <file>); nil = don't ask
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
//   - Plan mode asks questions, so it needs a terminal; piped or redirected stdin is an error rather than a session
//     nobody can answer. The one exception is -objective with -yes, which asks nothing.
func runPlanMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession) error {
	if (cs.Config.ObjectiveFile == "" || !cs.Config.AssumeYes) && !isTerminal(cs.InBuffer) {
		return errors.New("plan mode needs a terminal: it asks you questions; to plan without one, use -objective <file|text> with -yes")
	}
	return planMode(ctx, interrupts, cs, newPromptReader(ctx, cs.InBuffer, cs.OutBuffer))
}

// planMode is runPlanMode with the prompt reader passed in, so tests can use a plainReader or a scripted terminal.
//
// Notes:
//   - With -objective: the objective comes from objectiveFromFlag (a file, or text saved to a new file) and is parsed;
//     a file Parse refuses is an error naming the file. Then the plan step, where an up-to-date plan is just shown. If
//     the file was the user's own (it existed before), the first /save of a revision asks "Save over <file>? (y/N): "
//     (pd.confirmSave), since /save writes it in the standard form.
//   - Without: ask for the new objective's file (askObjectivePath) and the objective (askObjectiveText), run the
//     objective chat, and after /save the plan step, which then replaces any plan the file had.
//   - Ctrl+D at a question, or leaving the chat without /save, ends plan mode with nothing written and no error.
func planMode(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession, reader promptReader) error {
	pd, ok := cs.ModeDetails.(*planModeDetails)
	if !ok {
		return errors.New("plan mode was not set up (no plan mode details on the session)")
	}
	w := cs.OutBuffer

	if cs.Config.ObjectiveFile != "" {
		_, statErr := os.Stat(strings.TrimSpace(cs.Config.ObjectiveFile))
		userFile := statErr == nil // the user's own file, not text saved just now
		path, err := objectiveFromFlag(cs.Config.ObjectiveFile, time.Now(), w)
		if err != nil {
			return err
		}
		if userFile {
			// /save after a revision rewrites the file in the standard form, so ask once before writing over it
			pd.confirmSave = func() bool {
				answer, err := reader.AskLine(ctx, "Save over "+path+"? (y/N): ")
				return err == nil && isYes(answer, false)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		o, err := objective.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		pd.ObjectivePath = path
		*pd.Objective = o
		return planStep(ctx, interrupts, cs, reader, true)
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
	return planStep(ctx, interrupts, cs, reader, false)
}

// objectiveFromFlag turns -objective's value into the path of an objective file, saving text to a new file first.
//
// Notes:
//   - The value is trimmed first. A value naming an existing file is that file (a directory is an error).
//   - One word ending in ".md" (any case) that names no file is an error ("<value>: no such file"): most likely a typo,
//     not an objective.
//   - Anything else is the objective's text. It is saved to objective.md in the current directory, or to
//     objective-<YYYYMMDD-HHMMSS>.md (from now) if that exists; an existing file is never overwritten (O_EXCL). The
//     text is written trimmed, ending in one newline, and "Saved the objective to <file>." is printed on w.
func objectiveFromFlag(value string, now time.Time, w io.Writer) (string, error) {
	text := strings.TrimSpace(value)
	if info, err := os.Stat(text); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("-objective: %s is a directory, want a file or the objective's text", text)
		}
		return text, nil
	}
	if !strings.ContainsAny(text, " \t\r\n") && strings.EqualFold(filepath.Ext(text), ".md") {
		return "", fmt.Errorf("-objective: %s: no such file", text)
	}

	path := "objective.md"
	if _, err := os.Stat(path); err == nil {
		path = "objective-" + now.Format("20060102-150405") + ".md"
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // never overwrite
	if err != nil {
		return "", fmt.Errorf("saving the objective: %w", err)
	}
	if _, err := f.WriteString(text + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("saving the objective: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("saving the objective: %w", err)
	}
	fmt.Fprintf(w, "Saved the objective to %s.\n", path)
	return path, nil
}

// planStep makes the plan for the saved objective in pd and asks the user to accept it.
//
// Notes:
//   - fromFlag (the objective came from -objective): an existing plan that is up to date (plan.IsStale is false) is
//     shown and nothing else happens; a stale one is an error until plan updates exist (step 4). A plan file that can't
//     be read back is an error naming it. Without fromFlag the user chose to write the objective anew, so an existing
//     plan is replaced.
//   - The old plan file is only replaced when a new plan is accepted (writeFileAtomic).
//   - -yes (AssumeYes) accepts without asking. Otherwise "Accept this plan? [Y/n]: "; no asks
//     "Enter chat mode to discuss and revise the plan (Y/n)? : ". Yes there asks "What would you like to change in the
//     plan? " and reopens the objective chat with one message holding the turned-down plan and the change
//     (planner.RevisionNote); after
//     /save the plan is made again and the questions repeat. No, Ctrl+D, or leaving the chat without /save write
//     nothing.
//   - Ctrl+C during the planner run ends plan mode with nothing written (runPlanner reports it).
func planStep(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession, reader promptReader, fromFlag bool) error {
	pd := cs.ModeDetails.(*planModeDetails)
	w := cs.OutBuffer
	planPath := plan.PathFor(pd.ObjectivePath)
	noPlan := func() error {
		fmt.Fprintf(w, "No plan written. To plan again: minagent -mode plan -objective %s\n", pd.ObjectivePath)
		return nil
	}

	if fromFlag {
		if data, err := os.ReadFile(planPath); err == nil {
			old, _, err := plan.Cleanup(data)
			if err != nil {
				return fmt.Errorf("%s: %w", planPath, err)
			}
			if old.IsStale(pd.Objective.Hash()) {
				return fmt.Errorf("%s changed since %s was made. Updating a plan comes in step 4; for now, delete %s to plan from scratch",
					pd.ObjectivePath, planPath, planPath)
			}
			fmt.Fprint(w, plan.Summary(old))
			fmt.Fprintf(w, "%s is up to date with %s.\n", planPath, pd.ObjectivePath)
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	for {
		p, err := runPlanner(ctx, interrupts, cs)
		if errors.Is(err, errPlannerInterrupted) {
			fmt.Fprintln(w, "Planning interrupted.")
			return noPlan()
		}
		if err != nil {
			return err
		}
		fmt.Fprint(w, "\n"+plan.Summary(p))

		accept := cs.Config.AssumeYes
		if !accept {
			answer, err := reader.AskLine(ctx, "Accept this plan? [Y/n]: ")
			if errors.Is(err, io.EOF) {
				return noPlan()
			}
			if err != nil {
				return err
			}
			accept = isYes(answer, true)
		}
		if accept {
			if err := writeFileAtomic(planPath, []byte(p.Markdown())); err != nil {
				return fmt.Errorf("could not save %s: %w", planPath, err)
			}
			fmt.Fprintf(w, "Wrote %s (%d tasks).\n", planPath, len(p.Tasks))
			return nil
		}

		answer, err := reader.AskLine(ctx, "Enter chat mode to discuss and revise the plan (Y/n)? : ")
		if errors.Is(err, io.EOF) || (err == nil && !isYes(answer, true)) {
			return noPlan()
		}
		if err != nil {
			return err
		}
		first, err := askLineNotBlank(ctx, reader, "What would you like to change in the plan? ")
		if errors.Is(err, io.EOF) {
			return noPlan()
		}
		if err != nil {
			return err
		}
		// one user message (the plan and the change together), never two in a row: Anthropic refuses that
		pd.saved = false
		saved, err := runObjectiveChat(ctx, interrupts, cs, reader, planner.RevisionNote(p, first))
		if err != nil {
			return err
		}
		if !saved {
			return noPlan()
		}
	}
}

// errPlannerInterrupted is returned by runPlanner when Ctrl+C stopped the planner run.
var errPlannerInterrupted = errors.New("planner run interrupted")

// runPlanner runs the planner on the saved objective in pd and returns the plan propose_plan accepted.
//
// Notes:
//   - It runs one ReAct turn on a copy of cs with planner.PlanPrompt as the system message, no prior messages (the
//     planner sees only the saved objective, not the chat), and its own tools: the built-in and MCP tools
//     (pd.plannerTools, without update_objective) plus a new propose_plan for this objective.
//   - The turn is traced as a task "plan", is limited by turnTimeoutSeconds like a chat turn, and is cancelled by
//     Ctrl+C (then it returns errPlannerInterrupted).
//   - A turn that ends without an accepted plan (the model stopped calling tools, or ran out of steps) is an error.
func runPlanner(ctx context.Context, interrupts <-chan os.Signal, cs *ChatSession) (plan.Plan, error) {
	pd := cs.ModeDetails.(*planModeDetails)
	propose := planner.NewProposePlan(pd.ObjectivePath, *pd.Objective, cs.Config.MaxPlanTasks)
	registry, err := tools.NewToolRegistry(slices.Concat(pd.plannerTools, []tools.Tool{propose}))
	if err != nil {
		return plan.Plan{}, err
	}
	pcs := *cs // [agent] a shallow copy: same provider, recorder, hooks and output; its own system message and tools
	pcs.Tools = registry
	pcs.SystemMsg = createSystemMessage(planner.PlanPrompt)

	cs.Output.System("Making the plan for " + pd.ObjectivePath + " ...")
	taskTrace := trace.StartTask(cs.Recorder, "plan "+pd.ObjectivePath)
	defer taskTrace.End()

	discardStrayInterrupts(interrupts)
	turnCtx, stop := createInterruptWatcher(ctx, interrupts)
	defer stop()
	if secs := cs.Config.TurnTimeoutSeconds; secs > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(turnCtx, time.Duration(secs)*time.Second)
		defer cancel()
	}

	userMsg := model.CreateChatMessage("user", "user", planner.PlanMessage(pd.ObjectivePath, *pd.Objective), "", nil)
	if _, err := reActLoop(turnCtx, &pcs, taskTrace, nil, userMsg); err != nil {
		taskTrace.Fail(err)
		if ctx.Err() == nil && errors.Is(err, context.Canceled) {
			return plan.Plan{}, errPlannerInterrupted
		}
		return plan.Plan{}, fmt.Errorf("the planner failed: %w", err)
	}
	if propose.Accepted == nil {
		err := errors.New("the planner stopped without proposing a plan")
		taskTrace.Fail(err)
		return plan.Plan{}, err
	}
	return *propose.Accepted, nil
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
	return askLineNotBlank(ctx, reader, "What is your objective? : ")
}

// askLineNotBlank asks prompt until the answer isn't blank, and returns it trimmed.
//
// Notes:
//   - Ctrl+D returns io.EOF.
func askLineNotBlank(ctx context.Context, reader promptReader, prompt string) (string, error) {
	for {
		answer, err := reader.AskLine(ctx, prompt)
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
//   - pd.confirmSave, when set (the user's own -objective file), is asked first, once; no is an error and nothing is
//     written.
//   - The file is replaced in one step (writeFileAtomic); the user already agreed to overwrite an existing one.
func saveObjective(cs *ChatSession, pd *planModeDetails) error {
	if strings.TrimSpace(pd.Objective.ObjText) == "" {
		return errors.New("Nothing to save yet: the draft has no objective text.")
	}
	if pd.confirmSave != nil {
		if !pd.confirmSave() {
			return errors.New("Not saved; tell me what to change, or /save again.")
		}
		pd.confirmSave = nil // asked once
	}
	if err := writeFileAtomic(pd.ObjectivePath, []byte(pd.Objective.Markdown())); err != nil {
		return fmt.Errorf("Could not save %s: %w", pd.ObjectivePath, err)
	}
	pd.saved = true
	pd.editNote = "" // the saved draft is what the planner sees; an edit note from before is no longer news
	cs.Output.System("Saved " + pd.ObjectivePath + ".")
	return nil
}

// editObjective runs plan mode's /edit: the prompt is filled with the draft, and what the user submits becomes the new
// draft if it reads as an objective.
//
// Notes:
//   - Ctrl+D (or a cancelled context) leaves the draft unchanged.
//   - The text goes through objective.Parse: an error is shown and the draft is unchanged.
//   - On success the draft is shown again, and a note saying what the draft now is (pd.editNote) is put at the start of
//     the user's next message, so the model sees the change on its next turn. No model call is made now.
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
	// told to the model at the start of the user's next message (handleUserInput), not as a message of its own: two user
	// messages in a row would be refused by some providers (Anthropic)
	pd.editNote = "I edited the objective draft myself. It is now:\n\n" + o.Markdown()
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
