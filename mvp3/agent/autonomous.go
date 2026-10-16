package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/jerryschen31/minimal-agent/mvp3/config"
	"github.com/jerryschen31/minimal-agent/mvp3/contextwindow"
	"github.com/jerryschen31/minimal-agent/mvp3/director"
	"github.com/jerryschen31/minimal-agent/mvp3/memory"
	"github.com/jerryschen31/minimal-agent/mvp3/model"
	"github.com/jerryschen31/minimal-agent/mvp3/tools"
)

// Autonomous mode: a director LLM works through objective.md's task list with the worker agent,
// one task at a time (PLAN → WORK → REVIEW). See PLAN-autonomous-mode.md.
// With director.maxIterations > 1, a review that finds gaps starts another round: the gaps are
// planned as new tasks, worked and reviewed again.

var (
	// ErrBadObjective wraps any problem reading or parsing the objective file (a usage error: exit 2).
	ErrBadObjective = errors.New("bad objective file")
	// ErrObjectiveNotMet is returned by RunAgent when an autonomous run ended without success.
	ErrObjectiveNotMet = errors.New("objective not met")
	// ErrRunInterrupted is returned by RunAgent when an autonomous run was stopped by Ctrl+C or SIGTERM.
	ErrRunInterrupted = errors.New("run interrupted")
)

// maxNudges is how many reminders the director gets when it replies with text where a tool call was
// required (or replies with nothing), before the run ends with an error.
const maxNudges = 2

// directorFileTools are the director's read-only built-in tools (it never gets the worker's tools).
var directorFileTools = []string{"ReadFile", "ListDir"}

// readOnlyTools are the worker tools that can't change anything; any other worker tool triggers the
// "runs unattended" warning.
var readOnlyTools = map[string]bool{"read_file": true, "list_dir": true}

// Report reasons.
const (
	ReasonFinished    = "finished"
	ReasonTaskFailed  = "task_failed"
	ReasonInterrupted = "interrupted"
	ReasonTimeout     = "timeout"
	ReasonError       = "error"
)

// RunReport is the outcome of an autonomous run. Reason, Reviewed and Err describe the last round.
type RunReport struct {
	Reason      string           // finished | task_failed | interrupted | timeout | error
	Plan        *director.Plan   // the last round's plan
	Earlier     []*director.Plan // the plans of the earlier rounds, oldest first (each one reviewed, with gaps)
	WorkerTurns int              // worker turns across the whole run
	Reviewed    bool             // the REVIEW call (finish) succeeded, so Plan.Summary and Plan.Gaps are the director's
	Err         error            // what went wrong, for interrupted | timeout | error, or a failed review after task_failed
}

// autonomousRun holds everything one run needs. The worker is the agent's normal ChatSession.
type autonomousRun struct {
	worker      *ChatSession
	dir         *ChatSession // the director: its own provider, context and (per-phase) tools
	objective   director.Objective
	plan        *director.Plan   // the current round's plan
	earlier     []*director.Plan // the plans of the finished rounds
	iteration   int              // the current round, from 1
	fileTools   []tools.Tool     // the director's read-only file tools, offered in every phase
	workerTools []string         // tool names, shown to the director
	cfg         config.Config
	out         io.Writer
	rootCtx     context.Context // for auto-compaction, which must outlive a cancelled run (CLAUDE.md invariant)
}

// setupAutonomous reads the objective and builds the director session around an already set-up worker.
func setupAutonomous(ctx context.Context, cfg config.Config, worker *ChatSession) (*autonomousRun, error) {
	data, err := os.ReadFile(cfg.ObjectiveFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadObjective, err)
	}
	obj, err := director.ParseObjective(data, cfg.Director.MaxTasks)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %v", ErrBadObjective, cfg.ObjectiveFile, err)
	}

	// [agent] the director gets a copy of the config with its own provider settings; SetupProvider and the
	// session constructor take a whole Config, so this avoids changing their signatures
	dcfg := cfg
	dcfg.Provider, dcfg.Model = cfg.Director.Provider, cfg.Director.Model
	dcfg.BaseURL, dcfg.ApiKeyName = cfg.Director.BaseURL, cfg.Director.ApiKeyName
	dcfg.MaxSteps = cfg.Director.MaxSteps
	dcfg.SystemPrompt = "" // rebuilt before every director turn

	// a missing API key would otherwise only show up as a 401 halfway through the run
	for _, c := range []struct{ who, key string }{{"worker", cfg.ApiKeyName}, {"director", dcfg.ApiKeyName}} {
		if c.key != "" && os.Getenv(c.key) == "" {
			return nil, fmt.Errorf("%s: environment variable %s (apiKeyName) is not set", c.who, c.key)
		}
	}

	provider, err := model.SetupProvider(dcfg)
	if err != nil {
		return nil, fmt.Errorf("director: %w", err)
	}
	history, err := memory.SetupMemoryStore(dcfg)
	if err != nil {
		return nil, err
	}
	chatCtx, err := contextwindow.SetupChatContext(dcfg)
	if err != nil {
		return nil, err
	}
	fileTools, err := tools.SetupBuiltinTools(directorFileTools, cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("director: %w", err)
	}
	emptyTools, err := tools.NewToolRegistry(nil) // replaced per phase by directorTurn
	if err != nil {
		return nil, err
	}
	dir, err := NewChatSession(provider, history, chatCtx, emptyTools, dcfg)
	if err != nil {
		return nil, err
	}
	dir.Name = "director"
	worker.Name = "worker"

	var workerTools []string
	for _, def := range worker.Tools.GetToolDefs() {
		workerTools = append(workerTools, def.Function.Name)
	}
	return &autonomousRun{
		worker:      worker,
		dir:         dir,
		objective:   obj,
		plan:        director.NewPlan(obj.Tasks, cfg.Director.MaxTasks),
		fileTools:   fileTools,
		workerTools: workerTools,
		cfg:         cfg,
		out:         cfg.OutBuffer,
		rootCtx:     ctx,
	}, nil
}

// printStart prints the run header and the unattended-tools warning.
func (r *autonomousRun) printStart() {
	fmt.Fprintf(r.out, "[run] autonomous mode: objective %s\n", r.cfg.ObjectiveFile)
	fmt.Fprintf(r.out, "[run] worker model %s, director model %s, max %d worker turns per task, max %d round(s)\n",
		r.cfg.Model, r.cfg.Director.Model, r.cfg.Director.MaxTurnsPerTask, r.cfg.Director.MaxIterations)
	var risky []string
	for _, name := range r.workerTools {
		if !readOnlyTools[name] {
			risky = append(risky, name)
		}
	}
	if len(risky) > 0 {
		fmt.Fprintf(r.out, "[warning] the worker's tool calls run unattended, with no approval step: %s\n", strings.Join(risky, ", "))
	}
	if len(r.plan.Tasks) > 0 {
		fmt.Fprintf(r.out, "[run] %d tasks from the objective file\n", len(r.plan.Tasks))
	}
}

// run works through the rounds and returns the report. ctx is the run's context: cancelled by Ctrl+C,
// SIGTERM or the -timeout.
// [agent] Another round starts only after a successful review that says "not met" and lists gaps, while
// rounds are left. The gaps are the next round's input; with none there is nothing to plan from. A round
// that ended with task_failed counts too: a turn-limit failure is often fixable with a fresh plan.
// Honest downside: a task that truly can't be done (missing input) is retried until maxIterations.
func (r *autonomousRun) run(ctx context.Context) RunReport {
	var report RunReport
	for r.iteration = 1; ; r.iteration++ {
		report.Plan, report.Earlier = r.plan, r.earlier
		r.runRound(ctx, &report)
		p := r.plan
		if r.iteration >= r.cfg.Director.MaxIterations || !report.Reviewed || p.Success || len(p.Gaps) == 0 {
			return report
		}
		r.earlier = append(r.earlier, p)
		r.plan = director.NewPlan(nil, r.cfg.Director.MaxTasks)
		fmt.Fprintf(r.out, "[run] === ROUND %d of %d: %d gap(s) from the review become new tasks ===\n",
			r.iteration+1, r.cfg.Director.MaxIterations, len(p.Gaps))
	}
}

// runRound runs one round, plan → work → review, and records its outcome in report.
func (r *autonomousRun) runRound(ctx context.Context, report *RunReport) {
	report.Reason, report.Reviewed, report.Err = "", false, nil
	stop := func(err error) {
		report.Reason, report.Err = stopReason(ctx, err), err
	}

	if r.plan.Phase == director.PhasePlan {
		if err := r.runPlan(ctx); err != nil {
			stop(err)
			return
		}
	}
	for r.plan.Phase == director.PhaseWork {
		if err := r.runTask(ctx, report); err != nil {
			stop(err)
			return
		}
	}

	// REVIEW: after the last task, or (best effort) after a failed task
	err := r.runReview(ctx)
	report.Reviewed = err == nil
	switch {
	case err != nil && ctx.Err() != nil:
		stop(err) // interrupted or timed out during the review
	case r.plan.Failed():
		report.Reason, report.Err = ReasonTaskFailed, err // err: a failed best-effort review, if any
	case err != nil:
		stop(err)
	default:
		report.Reason = ReasonFinished
	}
}

// stopReason classifies why a run stopped early. The run's own ctx tells a Ctrl+C or SIGTERM
// (Canceled) from the -timeout (DeadlineExceeded); anything else is an error.
func stopReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ReasonTimeout
	case ctx.Err() != nil:
		return ReasonInterrupted
	}
	return ReasonError
}

// resetContext clears a session's context window (not its history), after any background compaction
// of it has finished.
func resetContext(cs *ChatSession) {
	cs.MsgContext.WaitForCompaction()
	cs.MsgContext.Clear()
}

// directorTurn runs one director turn with the given tools and a freshly built system prompt.
func (r *autonomousRun) directorTurn(ctx context.Context, prompt string, offered []tools.Tool) (turnResult, error) {
	reg, err := tools.NewToolRegistry(offered)
	if err != nil {
		return turnResult{}, err
	}
	r.dir.Tools = reg
	r.dir.SystemMsg = createSystemMessage(director.SystemPrompt(director.PromptInput{
		Objective:       r.objective,
		Plan:            r.plan,
		WorkerTools:     r.workerTools,
		MaxTurnsPerTask: r.cfg.Director.MaxTurnsPerTask,
		Extra:           r.cfg.Director.AdditionalInstructions,
		Iteration:       r.iteration,
		MaxIterations:   r.cfg.Director.MaxIterations,
		Earlier:         r.earlier,
	}))
	res, err := runTurn(ctx, r.dir, prompt)
	if err != nil {
		return turnResult{}, fmt.Errorf("director turn failed: %w", err)
	}
	maybeStartAutoCompaction(r.rootCtx, r.dir)
	return res, nil
}

// phaseTools is what the director is offered in the current phase: its read-only file tools plus the
// phase's control tools.
func (r *autonomousRun) phaseTools() []tools.Tool {
	return append(slices.Clone(r.fileTools), director.PhaseTools(r.plan, r.plan.Phase)...)
}

// runPlan asks the director for the task list (only when objective.md has no ## Tasks).
func (r *autonomousRun) runPlan(ctx context.Context) error {
	resetContext(r.dir)
	fmt.Fprintln(r.out, "[run] === PLAN: the director writes the task list ===")
	prompt := director.PlanPrompt()
	if n := len(r.earlier); n > 0 {
		prompt = director.GapsPlanPrompt(r.earlier[n-1], r.iteration, r.cfg.Director.MaxIterations)
	}
	for range maxNudges + 1 {
		if _, err := r.directorTurn(ctx, prompt, r.phaseTools()); err != nil {
			return err
		}
		if r.plan.Phase != director.PhasePlan {
			fmt.Fprintf(r.out, "[run] plan: %d tasks\n", len(r.plan.Tasks))
			for i, t := range r.plan.Tasks {
				fmt.Fprintf(r.out, "  %d. %s\n", i+1, t.Text)
			}
			return nil
		}
		prompt = director.NudgePlan
	}
	return fmt.Errorf("the director did not call set_tasks after %d reminders", maxNudges)
}

// runTask works on the current task until the director calls task_done or task_failed, or the
// per-task turn limit is reached. Both contexts start empty: the director's notes in its system
// prompt are what carries over from earlier tasks.
func (r *autonomousRun) runTask(ctx context.Context, report *RunReport) error {
	i, task, _ := r.plan.CurrentTask()
	n := len(r.plan.Tasks)
	maxTurns := r.cfg.Director.MaxTurnsPerTask
	resetContext(r.worker)
	resetContext(r.dir)
	fmt.Fprintf(r.out, "[run] === TASK %d/%d: %s ===\n", i+1, n, firstLine(task))

	prompt := director.TaskStartPrompt(i, n, task)
	turns, emptyReplies, limitNudged := 0, 0, false
	for {
		// [agent] the turn that writes a task's first prompt offers no tools, so the only possible answer is
		// text (the prompt). In real runs (gpt-4o-mini, 2026-10-13) a director offered tools there either kept
		// listing an empty directory until it ran out of steps, or called task_failed before the worker had
		// tried anything. Judging "is it done?" only makes sense after a worker turn.
		var offered []tools.Tool
		if turns > 0 {
			offered = r.phaseTools()
		}
		r.plan.WorkerTurnsLeft = maxTurns - turns
		res, err := r.directorTurn(ctx, prompt, offered)

		if err != nil {
			return err
		}
		// task_done or task_failed moved the plan on: this task is over
		if r.plan.Phase != director.PhaseWork || r.plan.Current != i {
			t := r.plan.Tasks[i]
			fmt.Fprintf(r.out, "[run] task %d/%d %s: %s\n", i+1, n, t.Status, t.Note)
			return nil
		}

		next := strings.TrimSpace(res.Reply)
		switch {
		case next == "":
			if emptyReplies++; emptyReplies > maxNudges {
				return fmt.Errorf("the director gave no worker prompt for task %d after %d reminders", i+1, maxNudges)
			}
			prompt = director.NudgeWork
			continue
		case turns >= maxTurns:
			// [agent] the director wants another follow-up but the task is out of worker turns: one reminder
			// to decide, then the code fails the task itself
			if !limitNudged {
				limitNudged = true
				prompt = director.NudgeLimit
				continue
			}
			reason := fmt.Sprintf("turn limit reached (%d worker turns) without the director marking the task done", maxTurns)
			if err := r.plan.TaskFailed(reason); err != nil {
				return err
			}
			fmt.Fprintf(r.out, "[run] task %d/%d failed: %s\n", i+1, n, reason)
			return nil
		}

		turns++
		report.WorkerTurns++
		wres, werr := runTurn(ctx, r.worker, next)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		maybeStartAutoCompaction(r.rootCtx, r.worker)
		prompt = director.WrapTurn(i, n, task, director.WorkerOutcome{
			Turn: turns, MaxTurns: maxTurns,
			Reply: wres.Reply, ToolsCalled: wres.ToolsCalled, Err: werr,
		})
	}
}

// runReview asks the director for the final review (finish).
func (r *autonomousRun) runReview(ctx context.Context) error {
	resetContext(r.dir)
	fmt.Fprintln(r.out, "[run] === REVIEW ===")
	prompt := director.ReviewPrompt(r.plan, r.objective.HasDone(), r.cfg.Director.MaxIterations-r.iteration)
	for range maxNudges + 1 {
		if _, err := r.directorTurn(ctx, prompt, r.phaseTools()); err != nil {
			return err
		}
		if r.plan.Phase == director.PhaseDone {
			return nil
		}
		prompt = director.NudgeReview
	}
	return fmt.Errorf("the director did not call finish after %d reminders", maxNudges)
}

// printReport writes the end-of-run report.
func printReport(w io.Writer, rep RunReport) {
	p := rep.Plan
	fmt.Fprintln(w, "\n=== Autonomous run report ===")
	result := rep.Reason
	switch {
	case rep.Reason == ReasonFinished && p.Success:
		result += " (success)"
	case rep.Reason == ReasonFinished:
		result += " (objective not met)"
	case rep.Reason == ReasonError && rep.Err != nil:
		result += ": " + rep.Err.Error() // interrupted/timeout errors are just "context canceled": not shown
	}
	if len(rep.Earlier) > 0 {
		result += fmt.Sprintf(", after %d rounds", len(rep.Earlier)+1)
	}
	fmt.Fprintf(w, "Result:   %s\n", result)
	switch {
	case rep.Reviewed:
		fmt.Fprintf(w, "Summary:  %s\n", p.Summary)
	case rep.Err != nil && rep.Reason == ReasonTaskFailed:
		fmt.Fprintf(w, "Summary:  no review (%v)\n", rep.Err)
	default:
		fmt.Fprintln(w, "Summary:  no review")
	}
	for k, ep := range rep.Earlier {
		fmt.Fprintf(w, "Round %d:  objective not met: %s\n", k+1, ep.Summary)
		printTasks(w, ep)
	}
	if len(rep.Earlier) > 0 {
		fmt.Fprintf(w, "Round %d (last):\n", len(rep.Earlier)+1)
	}
	printTasks(w, p)
	fmt.Fprintf(w, "Turns:    %d worker turns\n", rep.WorkerTurns)
}

// printTasks writes one round's tasks (with notes) and gaps.
func printTasks(w io.Writer, p *director.Plan) {
	fmt.Fprintln(w, "Tasks:")
	if len(p.Tasks) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for i, t := range p.Tasks {
		fmt.Fprintf(w, "  %d. [%s] %s\n", i+1, t.Status, firstLine(t.Text))
		if t.Note != "" {
			fmt.Fprintf(w, "       %s\n", t.Note)
		}
	}
	if len(p.Gaps) > 0 {
		fmt.Fprintln(w, "Gaps:")
		for _, g := range p.Gaps {
			fmt.Fprintf(w, "  - %s\n", g)
		}
	}
}

// runError turns a report into RunAgent's return value: nil only for a finished, successful run.
func (rep RunReport) runError() error {
	switch {
	case rep.Reason == ReasonFinished && rep.Plan.Success:
		return nil
	case rep.Reason == ReasonInterrupted:
		return ErrRunInterrupted
	case rep.Err != nil:
		return fmt.Errorf("%w (%s): %v", ErrObjectiveNotMet, rep.Reason, rep.Err)
	}
	return fmt.Errorf("%w (%s)", ErrObjectiveNotMet, rep.Reason)
}

// runAutonomous runs a whole autonomous run: one Ctrl+C (or SIGTERM, or the timeout) ends it, the report
// is printed for whatever state it reached, and the caller then shuts down as usual.
func runAutonomous(ctx context.Context, interrupts <-chan os.Signal, r *autonomousRun) error {
	timeout, err := r.cfg.TimeoutDuration()
	if err != nil {
		return err
	}
	r.printStart()

	// [agent] in autonomous mode the whole run is the "foreground work" that Ctrl+C stops (CLAUDE.md
	// invariant), so the run gets one watcher, the same helper the chat loop uses per turn
	discardStrayInterrupts(interrupts)
	runCtx, stop := createTurnWatcher(ctx, interrupts)
	defer stop()
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, timeout)
		defer cancel()
	}

	rep := r.run(runCtx)
	printReport(r.out, rep)
	return rep.runError()
}

// firstLine returns the first line of s (task texts can carry nested lines).
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
