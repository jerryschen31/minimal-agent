package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
	"github.com/jerryschen31/minimal-agent/mvp4/contextwindow"
	"github.com/jerryschen31/minimal-agent/mvp4/director"
	"github.com/jerryschen31/minimal-agent/mvp4/memory"
	"github.com/jerryschen31/minimal-agent/mvp4/model"
	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/tools"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// Autonomous and orchestrator mode: a director LLM works through objective.md's task list with worker
// agents (PLAN → WORK → REVIEW). See PLAN-autonomous-mode.md. In autonomous mode the one worker runs in
// this process (localWorker); in orchestrator mode each worker is a `-mode worker` child process reached
// over MCP (worker.MCPWorker), and with director.maxParallel > 1 several tasks run at once, each with its
// own worker and its own director conversation. See PLAN-orchestrator-mode-serial.md and
// PLAN-orchestrator-mode-parallel.md.
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
	PeakTasks   int              // the most tasks that ran at the same time
	Reviewed    bool             // the REVIEW call (finish) succeeded, so Plan.Summary and Plan.Gaps are the director's
	Err         error            // what went wrong, for interrupted | timeout | error, or a failed review after task_failed
}

// autonomousRun holds everything one run needs.
type autonomousRun struct {
	slots     []workerSlot           // every worker, with its ID and type; one in autonomous mode
	groups    []director.WorkerGroup // the worker types (the first is the default), shown to the director
	dir       *ChatSession           // the director for PLAN and REVIEW; each task gets its own (newDirector)
	dirProv   model.Provider         // the director's provider, shared by all its conversations
	dcfg      config.Config          // the director's copy of the config
	objective director.Objective
	plan      *director.Plan   // the current round's plan
	earlier   []*director.Plan // the plans of the finished rounds
	iteration int              // the current round, from 1
	fileTools []tools.Tool     // the director's read-only file tools, offered in every phase
	cfg       config.Config
	out       io.Writer
	outMu     sync.Mutex      // one line at a time on out while tasks run in parallel (see lineWriter)
	rootCtx   context.Context // for auto-compaction, which must outlive a cancelled run (CLAUDE.md invariant)
}

// workerSlot is one worker of the pool: the worker, its ID for the task list ("w1", or "engineer2" with
// worker types) and its worker type.
type workerSlot struct {
	w   worker.Worker
	id  string
	typ string
}

// workerTypes returns the groups' type names, the first being the default.
func workerTypes(groups []director.WorkerGroup) []string {
	var types []string
	for _, g := range groups {
		types = append(types, g.Type)
	}
	return types
}

// workerGroups describes the started workers by type, in the config's order, for the director's prompt:
// each type's description, its worker count and the first worker's model and tools (all workers of one
// type share a config).
func workerGroups(cfg config.Config, slots []workerSlot) []director.WorkerGroup {
	var groups []director.WorkerGroup
	for _, wt := range cfg.WorkerPool() {
		g := director.WorkerGroup{Type: wt.Type, Description: wt.Description}
		for _, s := range slots {
			if s.typ == wt.Type {
				if g.Count == 0 {
					g.Info = s.w.Info()
				}
				g.Count++
			}
		}
		if g.Count > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

// setupAutonomous reads the objective and builds the director session around already started workers.
func setupAutonomous(ctx context.Context, cfg config.Config, slots []workerSlot) (*autonomousRun, error) {
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
	fileTools, err := tools.SetupBuiltinTools(directorFileTools, cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("director: %w", err)
	}
	groups := workerGroups(cfg, slots)
	if len(groups) == 0 {
		return nil, errors.New("no workers were started") // a bug: SetupAgent starts at least one
	}
	r := &autonomousRun{
		slots:     slots,
		groups:    groups,
		dirProv:   provider,
		dcfg:      dcfg,
		objective: obj,
		plan:      director.NewPlan(obj.Tasks, cfg.Director.MaxTasks, len(slots), workerTypes(groups)),
		fileTools: fileTools,
		cfg:       cfg,
		out:       cfg.OutBuffer,
		rootCtx:   ctx,
	}
	if r.dir, err = r.newDirector(r.out); err != nil {
		return nil, err
	}
	return r, nil
}

// newDirector returns a director conversation with an empty context that writes its output to out.
// [agent] Each running task gets its own, so tasks running in parallel never share a context. They share
// the provider: the LLM API is stateless (every request carries its whole conversation), so N
// conversations are just N independent requests to the same model, with no coordination needed here.
func (r *autonomousRun) newDirector(out io.Writer) (*ChatSession, error) {
	history, err := memory.SetupMemoryStore(r.dcfg)
	if err != nil {
		return nil, err
	}
	chatCtx, err := contextwindow.SetupChatContext(r.dcfg)
	if err != nil {
		return nil, err
	}
	emptyTools, err := tools.NewToolRegistry(nil) // replaced per turn by directorTurn
	if err != nil {
		return nil, err
	}
	dir, err := NewChatSession(r.dirProv, history, chatCtx, emptyTools, r.dcfg)
	if err != nil {
		return nil, err
	}
	dir.Name, dir.OutBuffer = "director", out
	return dir, nil
}

// printStart prints the run header and the unattended-tools warning.
func (r *autonomousRun) printStart() {
	fmt.Fprintf(r.out, "[run] %s mode: objective %s\n", r.cfg.AgentMode, r.cfg.ObjectiveFile)
	where := "in this process"
	if r.cfg.AgentMode == config.ModeOrchestrator {
		// [agent] the top-level config configures the worker, which here is a child process: say so
		where = "in a child process (-mode worker, MCP over stdio)"
	}
	if n := len(r.slots); n > 1 {
		where = fmt.Sprintf("in %d child processes (-mode worker, MCP over stdio), up to %d tasks at once", n, n)
	}
	models := r.groups[0].Info.Model
	if len(r.groups) > 1 || r.groups[0].Type != worker.DefaultType {
		var parts []string
		for _, g := range r.groups {
			parts = append(parts, fmt.Sprintf("%s ×%d %s", g.Type, g.Count, g.Info.Model))
		}
		models = "(" + strings.Join(parts, ", ") + ")"
	}
	fmt.Fprintf(r.out, "[run] worker model %s %s, director model %s, max %d worker turns per task, max %d round(s)\n",
		models, where, r.cfg.Director.Model, r.cfg.Director.MaxTurnsPerTask, r.cfg.Director.MaxIterations)
	var risky []string
	for _, g := range r.groups {
		for _, name := range g.Info.Tools {
			if !readOnlyTools[name] && !slices.Contains(risky, name) {
				risky = append(risky, name)
			}
		}
	}
	if len(risky) > 0 {
		fmt.Fprintf(r.out, "[warning] the worker's tool calls run unattended, with no approval step: %s\n", strings.Join(risky, ", "))
	}
	if n := len(r.plan.Tasks.Tasks); n > 0 {
		fmt.Fprintf(r.out, "[run] %d tasks from the objective file\n", n)
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
		r.plan = director.NewPlan(nil, r.cfg.Director.MaxTasks, len(r.slots), workerTypes(r.groups))
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
	if r.plan.Phase == director.PhaseWork {
		if err := r.runWork(ctx, report); err != nil {
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
// of it has finished, or once ctx is done. [agent] A compaction still running then is harmless: Clear bumps
// the context generation, so its summary is discarded, and shutdown still waits for it (ShutdownAgent).
func resetContext(ctx context.Context, cs *ChatSession) {
	done := make(chan struct{})
	go func() {
		cs.MsgContext.WaitForCompaction()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	cs.MsgContext.Clear()
}

// directorTurn runs one turn of the director conversation dir, with the given tools and a freshly built
// system prompt. run is the task dir works on (nil for PLAN and REVIEW).
func (r *autonomousRun) directorTurn(ctx context.Context, dir *ChatSession, run *director.TaskRun, prompt string, offered []tools.Tool) (turnResult, error) {
	reg, err := tools.NewToolRegistry(offered)
	if err != nil {
		return turnResult{}, err
	}
	in := director.PromptInput{
		Objective:       r.objective,
		Plan:            r.plan,
		Workers:         r.groups,
		MaxTurnsPerTask: r.cfg.Director.MaxTurnsPerTask,
		Extra:           r.cfg.Director.AdditionalInstructions,
		Iteration:       r.iteration,
		MaxIterations:   r.cfg.Director.MaxIterations,
		Earlier:         r.earlier,
	}
	if run != nil {
		in.Current = run.Task
	}
	r.plan.Lock() // a snapshot of the task list, while other tasks may be changing it
	system := director.SystemPrompt(in)
	r.plan.Unlock()
	dir.Tools, dir.SystemMsg = reg, createSystemMessage(system)
	res, err := runTurn(ctx, dir, prompt)
	if err != nil {
		return turnResult{}, fmt.Errorf("director turn failed: %w", err)
	}
	maybeStartAutoCompaction(r.rootCtx, dir)
	return res, nil
}

// phaseTools is what the director is offered in a phase: its read-only file tools plus the phase's
// control tools (in PhaseWork, bound to run).
func (r *autonomousRun) phaseTools(phase director.Phase, run *director.TaskRun) []tools.Tool {
	return append(slices.Clone(r.fileTools), director.PhaseTools(r.plan, phase, run)...)
}

// runPlan asks the director for the task list (only when objective.md has no ## Tasks).
func (r *autonomousRun) runPlan(ctx context.Context) error {
	resetContext(ctx, r.dir)
	fmt.Fprintln(r.out, "[run] === PLAN: the director writes the task list ===")
	prompt := director.PlanPrompt(r.plan.Parallel)
	if n := len(r.earlier); n > 0 {
		prompt = director.GapsPlanPrompt(r.earlier[n-1], r.iteration, r.cfg.Director.MaxIterations)
	}
	for range maxNudges + 1 {
		if _, err := r.directorTurn(ctx, r.dir, nil, prompt, r.phaseTools(director.PhasePlan, nil)); err != nil {
			return err
		}
		if r.plan.Phase != director.PhasePlan {
			fmt.Fprintf(r.out, "[run] plan: %d tasks\n", len(r.plan.Tasks.Tasks))
			for _, t := range r.plan.Tasks.Tasks {
				typ := ""
				if len(r.groups) > 1 {
					typ = " [" + t.WorkerType + "]"
				}
				fmt.Fprintf(r.out, "  %s.%s %s%s\n", t.ID, typ, t.Text, after(t))
			}
			return nil
		}
		prompt = director.NudgePlan
	}
	return fmt.Errorf("the director did not call set_tasks after %d reminders", maxNudges)
}

// runWork is the WORK phase: a plain-Go scheduler (no LLM) that starts every ready task on an idle worker,
// up to one task per worker, until the plan leaves PhaseWork. With one worker that is exactly the serial
// run: one task at a time, in dependency order.
// [agent] A failed task drains the round (DECISIONS.md § Slice D, item 3): Plan.StartNext starts nothing new,
// the running tasks finish, and the plan moves to REVIEW when the last one ends. A hard error (a director
// turn that fails, e.g. a 401) also stops new tasks; once the rest have ended it is returned, and the round
// ends without a review, as in the serial run. Ctrl+C, SIGTERM and -timeout cancel ctx, which ends every
// task at once. Either way runWork returns only after every task goroutine has ended.
func (r *autonomousRun) runWork(ctx context.Context, report *RunReport) error {
	// [agent] one idle pool per worker type: a task waits for a free worker of its own type. Only this
	// goroutine takes from the pools, so "len > 0" here means the receive below can't block
	idle := map[string]chan workerSlot{}
	for _, g := range r.groups {
		idle[g.Type] = make(chan workerSlot, g.Count)
	}
	for _, s := range r.slots {
		idle[s.typ] <- s
	}
	free := func(workerType string) bool { return len(idle[workerType]) > 0 }
	ended := make(chan error)
	running := 0
	var firstErr error
	for {
		for firstErr == nil && ctx.Err() == nil {
			run := r.plan.StartNext(free)
			if run == nil {
				break // nothing ready (or no free worker for it) until a running task ends, or the work is over
			}
			slot := <-idle[run.Task.WorkerType]
			running++
			report.PeakTasks = max(report.PeakTasks, running)
			r.plan.Lock()
			nth := r.plan.Tasks.Started() // for "TASK t3 (3/5)", counted now: tasks started later don't change it
			r.plan.Unlock()
			go func() {
				err := r.runTask(ctx, run, slot, nth, report)
				idle[slot.typ] <- slot // never blocks: each pool has room for all its workers
				ended <- err
			}()
		}
		if running == 0 {
			break
		}
		if err := <-ended; err != nil && firstErr == nil {
			firstErr = err
		}
		running--
	}
	switch {
	case firstErr != nil:
		return firstErr
	case ctx.Err() != nil:
		return ctx.Err()
	case r.plan.Phase == director.PhaseWork:
		return errors.New("no task is ready to start") // Set rejects cycles, so this is a bug
	}
	return nil
}

// runTask works on one started task with one worker until the director calls task_done or task_failed,
// or the per-task turn limit is reached. It runs on its own goroutine, with its own director conversation
// (empty context: the notes in the system prompt are what carries over from other tasks). The worker
// starts the task with an empty context too (a new task ID), and EndTask drops it afterwards.
// nth is how many tasks had started when this one did.
func (r *autonomousRun) runTask(ctx context.Context, run *director.TaskRun, slot workerSlot, nth int, report *RunReport) error {
	t := run.Task
	out := r.out
	if len(r.slots) > 1 {
		// [agent] tasks running at once print to the same terminal: prefix each line with the task and worker
		lw := &lineWriter{w: r.out, mu: &r.outMu, prefix: fmt.Sprintf("[%s %s] ", t.ID, slot.id)}
		defer lw.Flush()
		out = lw
	}
	dir, err := r.newDirector(out)
	if err != nil {
		return err
	}
	defer dir.MsgContext.WaitForCompaction() // a background compaction must not outlive the task's director
	defer slot.w.EndTask(ctx, t.ID)          // best effort: a failure only leaves a conversation the next task ID replaces

	r.plan.Lock()
	t.WorkerID = slot.id
	r.plan.Unlock()
	maxTurns := r.cfg.Director.MaxTurnsPerTask
	fmt.Fprintf(out, "[run] === TASK %s (%d/%d): %s ===\n", t.ID, nth, len(r.plan.Tasks.Tasks), firstLine(t.Text))

	prompt := director.TaskStartPrompt(t)
	emptyReplies, limitNudged := 0, false
	for {
		// [agent] only this goroutine changes t's status and history (the plan's lock is taken for the writes,
		// because other tasks' directors read the task list), so reading them here needs no lock
		turns := len(t.History)
		// [agent] the turn that writes a task's first prompt offers no tools, so the only possible answer is
		// text (the prompt). In real runs (gpt-4o-mini, 2026-10-13) a director offered tools there either kept
		// listing an empty directory until it ran out of steps, or called task_failed before the worker had
		// tried anything. Judging "is it done?" only makes sense after a worker turn.
		var offered []tools.Tool
		if turns > 0 {
			offered = r.phaseTools(director.PhaseWork, run)
		}
		run.WorkerTurnsLeft = maxTurns - turns
		res, err := r.directorTurn(ctx, dir, run, prompt, offered)

		if err != nil {
			return err
		}
		// task_done or task_failed ended this task
		if t.Status != tasks.Running {
			fmt.Fprintf(out, "[run] task %s %s: %s\n", t.ID, t.Status, t.Note)
			return nil
		}

		next := strings.TrimSpace(res.Reply)
		switch {
		case next == "":
			if emptyReplies++; emptyReplies > maxNudges {
				return fmt.Errorf("the director gave no worker prompt for task %s after %d reminders", t.ID, maxNudges)
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
			if err := r.plan.TaskFailed(run, reason); err != nil {
				return err
			}
			fmt.Fprintf(out, "[run] task %s failed: %s\n", t.ID, reason)
			return nil
		}

		r.plan.Lock()
		report.WorkerTurns++
		r.plan.Unlock()
		wres, werr := slot.w.RunTurn(ctx, t.ID, next)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil {
			// [agent] the turn couldn't run at all (e.g. the worker process is gone): still an observation for
			// the director, never a crash; the turn limit bounds how often it can happen
			wres = worker.Result{Status: worker.StatusError, Error: werr.Error()}
		}
		r.plan.Lock()
		t.History = append(t.History, tasks.Turn{Prompt: next, Status: string(wres.Status), Reply: wres.Reply})
		r.plan.Unlock()
		prompt = director.WrapTurn(t, director.WorkerOutcome{Turn: len(t.History), MaxTurns: maxTurns, Result: wres})
	}
}

// runReview asks the director for the final review (finish).
func (r *autonomousRun) runReview(ctx context.Context) error {
	resetContext(ctx, r.dir)
	fmt.Fprintln(r.out, "[run] === REVIEW ===")
	prompt := director.ReviewPrompt(r.plan, r.objective.HasDone(), r.cfg.Director.MaxIterations-r.iteration)
	for range maxNudges + 1 {
		if _, err := r.directorTurn(ctx, r.dir, nil, prompt, r.phaseTools(director.PhaseReview, nil)); err != nil {
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
	fmt.Fprintf(w, "Turns:    %d worker turns", rep.WorkerTurns)
	if rep.PeakTasks > 1 {
		fmt.Fprintf(w, ", up to %d tasks at once", rep.PeakTasks)
	}
	fmt.Fprintln(w)
}

// printTasks writes one round's tasks (with notes) and gaps.
func printTasks(w io.Writer, p *director.Plan) {
	fmt.Fprintln(w, "Tasks:")
	if len(p.Tasks.Tasks) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, t := range p.Tasks.Tasks {
		fmt.Fprintf(w, "  %s [%s] %s%s", t.ID, t.Status, firstLine(t.Text), after(t))
		if n := len(t.History); n > 0 {
			fmt.Fprintf(w, " (%d worker turn(s), %s)", n, t.WorkerID)
		}
		fmt.Fprintln(w)
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

// after returns " (after t1, t2)" for a task with dependencies, else "".
func after(t *tasks.Task) string {
	if len(t.DependsOn) == 0 {
		return ""
	}
	return " (after " + strings.Join(t.DependsOn, ", ") + ")"
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
