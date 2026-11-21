package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/jerryschen31/minimal-agent/agent/director"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/jerryschen31/minimal-agent/utils/textutils"
)

// ErrNeedsPerson is returned (wrapped) by auto mode when the run stopped because it needs a person: a gate or a resume
// question with nobody to answer it ("waiting"), or a person said no to a gate ("declined"). The report was printed;
// main exits with code 3 so a script can tell "needs a person" from "failed".
var ErrNeedsPerson = errors.New("the run needs a person")

// autoRun is one open run of a plan: its folder and journal, the plan's lock, and the plan's state before this run.
type autoRun struct {
	run     state.Run
	journal *state.Journal
	tools   *runToolJournal // records the run's tool calls; also on the session (cs.ToolJournal) while the run is open
	unlock  func() error
	before  state.PlanState // what the plan's earlier runs left (state.Home.Load)
	// crashNotes: after a resume, per task, what happened to the calls a crash cut off (for its next worker)
	crashNotes map[string]string
	home       state.Home    // the state folder, and
	ref        state.PlanRef // the plan's identity in it: end reads the journals back for the report
	plan       plan.Plan     // the plan being run (the report lists its tasks)

	// [agent] Budgets (step 2). The run's totals are kept here, in the code that schedules tasks, and only this code
	// changes them: each worker and director has its own meter and reports its total when it ends.
	used             model.Usage   // tokens of this run's ended attempts, earlier sessions included (after a resume)
	runningBefore    time.Duration // running time of this run's earlier sessions (after a resume)
	sessionStart     time.Time     // when this session started
	gateWaits        time.Duration // this session's time waiting at gate questions (not running time)
	warnedUnreported bool          // the "server reports no usage" warning was printed
}

// errRunTimeout is the cause of a task context that reached runTimeoutMinutes (taskContext). It tells the time limit
// apart from Ctrl+C, which cancels the same context (stopReasonOf).
var errRunTimeout = errors.New("run time limit reached")

// runTimeoutUnit is the unit of runTimeoutMinutes. A variable so tests can make a "minute" a few milliseconds.
var runTimeoutUnit = time.Minute

// running returns how long this run has been running: its earlier sessions plus this one, without gate waits.
//
// Notes: none.
func (ar *autoRun) running() time.Duration {
	return ar.runningBefore + time.Since(ar.sessionStart) - ar.gateWaits
}

// taskContext returns the context a task runs under: ctx with a deadline at the end of the run's time limit.
//
// Notes:
//   - limitMinutes 0 = no limit: ctx itself. ok is false when no time is left (the task must not start).
//   - The deadline's cause is errRunTimeout, so a task stopped by it is told apart from Ctrl+C (stopReasonOf).
//   - Made per task, not once per run, so time spent at a gate question (asked before the task, outside this context)
//     is not counted.
func (ar *autoRun) taskContext(ctx context.Context, limitMinutes int) (context.Context, context.CancelFunc, bool) {
	if limitMinutes <= 0 {
		return ctx, func() {}, true
	}
	left := time.Duration(limitMinutes)*runTimeoutUnit - ar.running()
	if left <= 0 {
		return nil, nil, false
	}
	tctx, cancel := context.WithTimeoutCause(ctx, left, errRunTimeout)
	return tctx, cancel, true
}

// stopReasonOf returns why ctx ended: the run's time limit (taskContext) or an interruption (Ctrl+C, SIGTERM).
//
// Notes: none.
func stopReasonOf(ctx context.Context) state.StopReason {
	if errors.Is(context.Cause(ctx), errRunTimeout) {
		return state.StopTimeout
	}
	return state.StopInterrupted
}

// allowance returns how many tokens the next worker or director may use, and whether the run's budget (rather than
// taskTokenBudget) is what limits it.
//
// Notes:
//   - The run's left = runTokenBudget minus what its ended attempts used; unused tokens of an earlier worker are
//     therefore left for the next one without any bookkeeping.
//   - taskBudget is taskTokenBudget for a worker, 0 for a director (only the run's budget limits it).
//   - pending is spent but not yet recorded by endAttempt (the worker's tokens, when the director's allowance is worked out).
//   - 0 with runLimited false = no limit at all. ok is false when the run's budget is used up: nothing may start.
func (ar *autoRun) allowance(runBudget, taskBudget int, pending model.Usage) (n int, runLimited, ok bool) {
	if runBudget <= 0 {
		return taskBudget, false, true
	}
	left := runBudget - ar.used.Total() - pending.Total()
	if left <= 0 {
		return 0, true, false
	}
	if taskBudget > 0 && taskBudget < left {
		return taskBudget, false, true
	}
	return left, true, true
}

// budgetError describes a run stopped by its token budget; pending is spent but not yet recorded (as in allowance).
//
// Notes: none.
func (ar *autoRun) budgetError(runBudget int, pending model.Usage) error {
	return fmt.Errorf("%w: %d of %d tokens used (runTokenBudget)", errAllowanceUsed, ar.used.Total()+pending.Total(), runBudget)
}

// endAttempt adds an attempt's tokens (its worker's and director's meters; nil ones are skipped) to the run's total and
// returns them, for the attempt's task_finished or attempt_usage.
//
// Notes:
//   - Called once per attempt however it ends. Not called when the journal already failed (ar.tools.err): nothing
//     more is written then, and the run is left open.
//   - With a budget set and a server that reported no usage, prints once that the budget can't be enforced.
//   - The calls that reported no usage are counted in the result's UnreportedCalls, so the report can say its sum is a
//     lower bound.
func (ar *autoRun) endAttempt(cs *ChatSession, meters ...*usageMeter) state.Usage {
	var u model.Usage
	unreported := 0
	for _, m := range meters {
		if m != nil {
			mu, _, mn := m.snapshot()
			u = u.Add(mu)
			unreported += mn
		}
	}
	ar.used = ar.used.Add(u)
	budgeted := cs.Config.RunTokenBudget > 0 || cs.Config.TaskTokenBudget > 0
	if budgeted && unreported > 0 && !ar.warnedUnreported {
		ar.warnedUnreported = true
		fmt.Fprintln(cs.OutBuffer, "  warning: the server reports no token usage for some calls; they count as 0, so the token budget can't be fully enforced")
	}
	su := toStateUsage(u)
	su.UnreportedCalls = unreported
	return su
}

// toStateUsage converts a model.Usage into the journal's state.Usage (the state package can't import model).
//
// Notes: none.
func toStateUsage(u model.Usage) state.Usage {
	return state.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens, Reported: u.Reported}
}

// fromStateUsage converts the journal's state.Usage back into a model.Usage.
//
// Notes: none.
func fromStateUsage(u state.Usage) model.Usage {
	return model.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens, Reported: u.Reported}
}

// runResult is how a run ended, for its run_ended event and the report.
type runResult struct {
	reason                            state.StopReason
	err                               error // what stopped the run; nil for done, waiting and interrupted
	done, skipped, failed, notStarted int
}

// taskStep is one task in the order a run takes them (tasksToRun).
type taskStep struct {
	Task    plan.Task
	Skip    bool // done in an earlier run with the same definition hash
	DoneRun int  // Skip: the run that finished it
}

// openRun takes the plan's lock, loads its history and creates the run folder, with the manifest as the journal's
// first event.
//
// Notes:
//   - The plan's identity is its plan_id, else its real path (state.PlanRef). Another process running the same plan is
//     a *state.LockedError.
//   - Prints what the history says before the run starts (notePlanHistory): a moved plan, a copy sharing the plan_id
//     (allowed, with a warning).
//   - The last run never ended (a crash): resumeRun settles its cut-off calls (ask answers its questions; ctx is the
//     run's, so Ctrl+C stops it) and continues that run when the plan and config are unchanged, else a new run starts.
//     errResumeWaiting: an answer was needed and none could be had; nothing was done.
//   - The run folder gets the plan file's bytes as read (ad.planData), so what ran can be seen later even if the file
//     changes. The manifest records the trace's run ID; the trace is in <stateDir>/traces (SetupAgent).
//   - On an error after the lock was taken, the lock is released.
//   - Sets cs.ToolJournal, so from now on every tool call is recorded in the run's journal (end clears it).
func openRun(ctx context.Context, cs *ChatSession, ad *autoModeDetails, ask promptReader) (*autoRun, error) {
	dir, err := cs.Config.ResolveStateDir()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, errors.New("stateDir is empty: auto mode keeps its runs in the state folder")
	}
	home, err := state.OpenHome(dir)
	if err != nil {
		return nil, err
	}
	ref, err := state.NewPlanRef(ad.Plan.PlanID, ad.PlanPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ad.PlanPath, err)
	}
	unlock, err := home.Lock(ref)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			unlock()
		}
	}()
	before, err := home.Load(ref)
	if err != nil {
		return nil, err
	}
	notePlanHistory(cs.OutBuffer, ref, before)
	m, snapshot, err := buildManifest(cs, ad.conns, ref)
	if err != nil {
		return nil, err
	}
	var notes map[string]string
	if len(before.Runs) > 0 && before.Latest.Ended == nil {
		cont, n, err := resumeRun(ctx, cs, home, before, m, ad.planData, snapshot, ask)
		if err != nil {
			return nil, err
		}
		notes = n
		if cont != nil {
			opened = true
			cont.unlock, cont.before, cont.crashNotes, cont.home, cont.ref, cont.plan = unlock, before, notes, home, ref, ad.Plan
			// [agent] the same run goes on: its budgets carry over (the crashed sessions' tokens and running time)
			cont.used, cont.runningBefore = fromStateUsage(before.Latest.Usage), before.Latest.Running
			return cont, nil
		}
	}
	m.TraceRunID = cs.Recorder.RunID()
	sessionStart := time.Now()
	j, r, err := home.NewRun(m, ad.planData, snapshot)
	if err != nil {
		return nil, err
	}
	rj, err := newRunToolJournal(j, cs.Config.RedactionLevel)
	if err != nil {
		j.Close()
		return nil, err
	}
	opened = true
	cs.ToolJournal = rj
	return &autoRun{run: r, journal: j, tools: rj, unlock: unlock, before: before, crashNotes: notes, sessionStart: sessionStart,
		home: home, ref: ref, plan: ad.Plan}, nil
}

// notePlanHistory prints what the plan's earlier runs say before a new run starts.
//
// Notes:
//   - The plan has a plan_id and its last run was at another path: if a file at that path has the same plan_id, it is a
//     copy, and a warning says the two share one history and one lock (a copy is allowed: refusing would let anyone who
//     can copy a file stop a plan from running; protecting the plan file is planned for later, DECISIONS.md
//     § 2026-11-20 — 1d). Otherwise the plan was moved, and its history carries over.
func notePlanHistory(w io.Writer, ref state.PlanRef, st state.PlanState) {
	if len(st.Runs) == 0 {
		return
	}
	if last := st.Runs[len(st.Runs)-1].PlanPath; ref.ID != "" && last != ref.Path {
		if hasPlanID(last, ref.ID) {
			fmt.Fprintf(w, "Warning: %s and %s have the same plan_id (%s), so they share one history and one lock: a task "+
				"done by either counts as done for both. To make one a separate plan, delete its plan_id line.\n", ref.Path, last, ref.ID)
		} else {
			fmt.Fprintf(w, "This plan was moved from %s; its history carries over.\n", last)
		}
	}
}

// hasPlanID reports whether the file at path is a plan whose plan_id is id.
//
// Notes:
//   - A missing or unreadable file, or one that isn't a plan, is false.
func hasPlanID(path, id string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	p, _, err := plan.Cleanup(data)
	return err == nil && p.PlanID == id
}

// tasksToRun returns p's tasks in the order a run takes them, each marked to skip or run, and the IDs earlier runs had
// that p no longer has.
//
// Notes:
//   - Order: each task after the tasks it depends on; among the tasks that are ready, file order. [agent] Picking "the
//     first ready task in file order" each time is a topological sort that keeps the order you wrote where it can.
//   - Skip: done in an earlier run and its definition hash is unchanged. Everything else runs: new, changed, failed,
//     interrupted, or cut off by a crash (still "running" in a run that never ended).
//   - A done task whose dependency runs again is still skipped (no cascade in v1; docs/plans/PLAN-auto-mode.md "Which tasks run").
//   - A cycle or an unknown dependency can't happen (CheckComplete refuses both); if one did, the rest keep file order.
func tasksToRun(p plan.Plan, st state.PlanState) (steps []taskStep, removed []string) {
	placed := map[string]bool{}
	remaining := slices.Clone(p.Tasks)
	for len(remaining) > 0 {
		i := slices.IndexFunc(remaining, func(t plan.Task) bool {
			return !slices.ContainsFunc(t.Depends, func(d string) bool { return !placed[d] })
		})
		if i < 0 {
			i = 0
		}
		t := remaining[i]
		remaining = slices.Delete(remaining, i, i+1)
		placed[t.ID] = true
		ts, seen := st.Tasks[t.ID]
		skip := seen && ts.Status == state.TaskDone && ts.DefHash == t.DefinitionHash()
		steps = append(steps, taskStep{Task: t, Skip: skip, DoneRun: ts.Run})
	}
	inPlan := map[string]bool{}
	for _, t := range p.Tasks {
		inPlan[t.ID] = true
	}
	for id := range st.Tasks {
		if !inPlan[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return steps, removed
}

// runPlan runs p's tasks one at a time in tasksToRun's order, recording each in the run's journal, and returns how the
// run ended.
//
// Notes:
//   - Serial: one task at a time (runTask: worker, director, retries). A task that doesn't end done stops the run; the
//     tasks after it are "not started".
//   - A task with a gate is asked about first (askGate; gates is nil when there is nobody to ask): no answer possible
//     → "waiting", no → "declined", yes → it runs. -yes never answers a gate.
//   - The worker of a task is told the summaries of the tasks it depends on, from this run or an earlier one.
//   - Each task runs under taskContext (the run's time limit; gates are asked outside it). No time left → "timeout"
//     before the task starts; no tokens left → "budget" before its gate is asked. Either way the task counts as not
//     started (PR #46 review). A task stopped by the budget or the time limit is not counted as failed.
func runPlan(ctx context.Context, cs *ChatSession, ar *autoRun, p plan.Plan, gates promptReader) runResult {
	w := cs.OutBuffer
	steps, removed := tasksToRun(p, ar.before)
	if len(removed) > 0 {
		fmt.Fprintf(w, "Not in the plan any more (earlier runs had them): %v\n", removed)
	}
	summaries := map[string]string{}
	for id, ts := range ar.before.Tasks {
		if ts.Status == state.TaskDone {
			summaries[id] = ts.Summary
		}
	}
	var res runResult
	stop := func(i int, reason state.StopReason, err error) runResult {
		res.reason, res.err = reason, err
		for _, s := range steps[i:] {
			if !s.Skip {
				res.notStarted++
			}
		}
		return res
	}
	for i, s := range steps {
		t := s.Task
		if s.Skip {
			fmt.Fprintf(w, "  skip %s: %s (done in run %04d, unchanged)\n", t.ID, t.Title, s.DoneRun)
			res.skipped++
			continue
		}
		// the run's budget is used up: this task never starts, so it counts as not started (stop at i, not i+1), and its
		// gate is not asked
		if _, _, ok := ar.allowance(cs.Config.RunTokenBudget, 0, model.Usage{}); !ok {
			return stop(i, state.StopBudget, ar.budgetError(cs.Config.RunTokenBudget, model.Usage{}))
		}
		if t.Gate != "" {
			if reason, err := askGate(ctx, ar, t, gates, w); reason != "" {
				return stop(i, reason, err)
			}
		}
		taskCtx, cancel, ok := ar.taskContext(ctx, cs.Config.RunTimeoutMinutes)
		if !ok {
			return stop(i, state.StopTimeout, nil)
		}
		summary, reason, err := runTask(taskCtx, cs, ar, p, t, summaries, ar.crashNotes[t.ID])
		cancel()
		switch reason {
		case "":
			res.done++
			summaries[t.ID] = summary
			continue
		case state.StopInterrupted:
			fmt.Fprintf(w, "■ %s interrupted\n", t.ID)
		case state.StopBudget, state.StopTimeout: // not the task's fault: it runs again next time
			fmt.Fprintf(w, "■ %s stopped (%s)\n", t.ID, reason)
		default:
			res.failed++
		}
		return stop(i+1, reason, err)
	}
	return stop(len(steps), state.StopDone, nil)
}

// askGate asks whether gated task t may run, records the answer, and returns the stop reason when it may not ("" when
// it may).
//
// Notes:
//   - gates nil (no terminal) → "waiting": nothing is recorded, and the next run on a terminal asks again (the done
//     tasks are skipped). -yes never answers a gate: a gate exists for a person to decide.
//   - The answer goes to the journal (gate_answered: "yes" or "no") before anything else happens. No, or Ctrl+D, →
//     "declined". Ctrl+C at the question → "interrupted".
//   - The time the question waited is recorded (waited_ms) and added to ar.gateWaits: waiting for a person is not
//     running time.
func askGate(ctx context.Context, ar *autoRun, t plan.Task, gates promptReader, w io.Writer) (state.StopReason, error) {
	if gates == nil {
		fmt.Fprintf(w, "%s has a gate (%s) and there is no terminal to ask; run it again on a terminal.\n", t.ID, t.Gate)
		return state.StopWaiting, nil
	}
	asked := time.Now()
	answer, err := gates.AskLine(ctx, fmt.Sprintf("%s: %s has a gate: %q. Run it? [y/N]: ", t.ID, t.Title, t.Gate))
	waited := time.Since(asked)
	ar.gateWaits += waited // waiting for a person is not running time (running, taskContext)
	if err != nil && !errors.Is(err, io.EOF) {
		if ctx.Err() != nil {
			return state.StopInterrupted, nil
		}
		return state.StopError, err
	}
	yes := err == nil && isYes(answer, false)
	recorded := "no"
	if yes {
		recorded = "yes"
	}
	if jerr := ar.journal.Append(state.EventGateAnswered, t.ID, state.GateAnswered{Answer: recorded, WaitedMS: waited.Milliseconds()}); jerr != nil {
		return state.StopError, jerr
	}
	if !yes {
		fmt.Fprintf(w, "Declined: %s does not run.\n", t.ID)
		return state.StopDeclined, nil
	}
	return "", nil
}

// runTask runs task t until the director judges it done or it has had cs.Config.MaxTaskAttempts attempts, and returns
// its summary and, when it did not end done, the stop reason and error.
//
// Notes:
//   - Each attempt: task_started{attempt}, a fresh worker (runWorker; a retry is told why the attempt before was not
//     done), the director's verdict (judgeTask), task_finished{status, def_hash, summary, verdict}. Every attempt stays in
//     the journal; the last one decides the task's state (state.Rebuild).
//   - A worker that runs out of steps is a failed attempt (the director isn't asked). A director with no verdict is a
//     failed attempt too (judgeTask). Out of attempts → "task_failed".
//   - crash (after a resume: what happened to this task's cut-off calls) goes to the first attempt's worker.
//   - Ctrl+C → "interrupted", with no task_finished for the attempt (run_ended marks it). Any other error (the
//     provider, a journal write) → "error", no retry. A tool call whose record failed (ar.tools.err) stops it at once,
//     writing nothing more.
//   - Tokens: each attempt's worker gets an allowance (ar.allowance: taskTokenBudget, never more than the run has left)
//     and its own meter; the director then gets what the run has left. A worker that uses up a taskTokenBudget
//     allowance is a failed attempt; one stopped by the run's budget (or a director that is) ends the run with
//     "budget". The attempt's tokens go in its task_finished, or in attempt_usage when it ends without one (budget,
//     the time limit — "timeout", told apart from Ctrl+C by stopReasonOf — or Ctrl+C); either way the task runs again
//     next time.
func runTask(ctx context.Context, cs *ChatSession, ar *autoRun, p plan.Plan, t plan.Task, deps map[string]string, crash string) (string, state.StopReason, error) {
	w := cs.OutBuffer
	maxAttempts := cs.Config.MaxTaskAttempts
	ar.tools.setTask(t.ID) // the worker's and the director's tool calls are recorded under this task
	var retry *retryNote
	for attempt := 1; ; attempt++ {
		// [agent] each attempt's worker gets its own allowance from what the run has left (see allowance)
		allowance, runLimited, ok := ar.allowance(cs.Config.RunTokenBudget, cs.Config.TaskTokenBudget, model.Usage{})
		if !ok {
			return "", state.StopBudget, ar.budgetError(cs.Config.RunTokenBudget, model.Usage{})
		}
		if err := ar.journal.Append(state.EventTaskStarted, t.ID, state.TaskStarted{Attempt: attempt}); err != nil {
			return "", state.StopError, err
		}
		attemptStart := time.Now()
		if attempt == 1 {
			fmt.Fprintf(w, "▶ %s: %s\n", t.ID, t.Title)
		} else {
			fmt.Fprintf(w, "▶ %s: %s (attempt %d of %d)\n", t.ID, t.Title, attempt, maxAttempts)
		}
		if attempt > 1 {
			crash = "" // the first attempt after a resume was told; later ones are told about that attempt instead
		}
		workerMeter := &usageMeter{allowance: allowance}
		var directorMeter *usageMeter
		// stopAttempt ends the attempt without a task_finished (budget, time limit, Ctrl+C): its tokens are recorded, and
		// run_ended marks the task interrupted, so the next start runs it again
		stopAttempt := func(reason state.StopReason, err error) (string, state.StopReason, error) {
			used := state.AttemptUsage{Attempt: attempt, Usage: ar.endAttempt(cs, workerMeter, directorMeter)}
			if uerr := ar.journal.Append(state.EventAttemptUsage, t.ID, used); uerr != nil {
				return "", state.StopError, uerr
			}
			return "", reason, err
		}
		summary, workerMsgs, err := runWorker(ctx, cs, workerMeter, p, t, deps, retry, crash)
		if jerr := ar.tools.err(); jerr != nil { // a call's record failed: stop now, record nothing more (end)
			return "", state.StopError, fmt.Errorf("the run's journal: %w", jerr)
		}
		var v director.Verdict
		switch {
		case err != nil && ctx.Err() != nil:
			return stopAttempt(stopReasonOf(ctx), nil)
		case errors.Is(err, errAllowanceUsed) && runLimited:
			return stopAttempt(state.StopBudget, ar.budgetError(cs.Config.RunTokenBudget, workerMeter.used))
		case errors.Is(err, errAllowanceUsed):
			v.Reason = fmt.Sprintf("the worker used its token allowance (taskTokenBudget %d)", cs.Config.TaskTokenBudget)
		case errors.Is(err, ErrMaxSteps):
			v.Reason = fmt.Sprintf("the worker ran out of steps (maxSteps %d)", cs.Config.MaxSteps)
		case err != nil:
			return "", state.StopError, finishFailed(ar, t, err.Error(), "", ar.endAttempt(cs, workerMeter), err)
		default:
			// the director draws from what is left after the worker: only the run's budget limits it
			dAllowance, _, dOK := ar.allowance(cs.Config.RunTokenBudget, 0, workerMeter.used)
			if !dOK {
				return stopAttempt(state.StopBudget, ar.budgetError(cs.Config.RunTokenBudget, workerMeter.used))
			}
			directorMeter = &usageMeter{allowance: dAllowance}
			v, err = judgeTask(ctx, cs, directorMeter, t, attempt, summary, workerMsgs)
			if jerr := ar.tools.err(); jerr != nil {
				return "", state.StopError, fmt.Errorf("the run's journal: %w", jerr)
			}
			if err != nil && ctx.Err() != nil {
				return stopAttempt(stopReasonOf(ctx), nil)
			}
			if errors.Is(err, errAllowanceUsed) { // the director's limit is always the run's budget
				return stopAttempt(state.StopBudget, ar.budgetError(cs.Config.RunTokenBudget, workerMeter.used.Add(directorMeter.used)))
			}
			if err != nil {
				err = fmt.Errorf("the director failed: %w", err)
				return "", state.StopError, finishFailed(ar, t, summary, "", ar.endAttempt(cs, workerMeter, directorMeter), err)
			}
		}
		fin := state.TaskFinished{Status: state.TaskFailed, DefHash: t.DefinitionHash(), Summary: summary, Verdict: v.Reason,
			Usage: ar.endAttempt(cs, workerMeter, directorMeter)}
		if v.Done {
			fin.Status = state.TaskDone
		} else if v.Advice != "" {
			fin.Verdict += " Advice: " + v.Advice
		}
		if err := ar.journal.Append(state.EventTaskFinished, t.ID, fin); err != nil {
			return "", state.StopError, err
		}
		if v.Done {
			tokens := "tokens not reported"
			if fin.Usage.Reported {
				tokens = fmt.Sprintf("%d tokens", fin.Usage.Total())
				if n := fin.Usage.UnreportedCalls; n > 0 {
					tokens += fmt.Sprintf(" (+%d %s with no usage)", n, textutils.Plural(n, "call", "calls"))
				}
			}
			fmt.Fprintf(w, "✓ %s done — %s (%s, %s)\n", t.ID, v.Reason, tokens, durationText(time.Since(attemptStart)))
			return summary, "", nil
		}
		fmt.Fprintf(w, "  not done — %s (attempt %d of %d)\n", v.Reason, attempt, maxAttempts)
		if attempt >= maxAttempts {
			fmt.Fprintf(w, "✗ %s failed after %d attempts\n", t.ID, attempt)
			return "", state.StopTaskFailed, fmt.Errorf("%s not done after %d attempts: %s", t.ID, attempt, v.Reason)
		}
		retry = &retryNote{Attempt: attempt, Summary: summary, Reason: v.Reason, Advice: v.Advice}
	}
}

// finishFailed records a failed task_finished for t with summary, verdict and the attempt's tokens, and returns err (joined with the journal's
// error if that write fails too).
//
// Notes:
//   - For an attempt that ended in an error: the run stops, but the journal still says which task failed and why.
func finishFailed(ar *autoRun, t plan.Task, summary, verdict string, used state.Usage, err error) error {
	fin := state.TaskFinished{Status: state.TaskFailed, DefHash: t.DefinitionHash(), Summary: summary, Verdict: verdict, Usage: used}
	if jerr := ar.journal.Append(state.EventTaskFinished, t.ID, fin); jerr != nil {
		return errors.Join(err, jerr)
	}
	return fmt.Errorf("%s failed: %w", t.ID, err)
}

// end stops recording cs's tool calls, records run_ended, closes the journal, releases the lock and prints the report; it returns an error when the run
// stopped on a failure.
//
// Notes:
//   - A tool call whose record failed (ar.tools.err): no run_ended, so the run stays open and the next start settles
//     that call; an error is returned.
//   - budget and timeout print what was used against which limit and how to go on.
//   - Then the token and time report (usageReport), read back from the journals after run_ended so it includes earlier
//     sessions of a resumed run; if that read fails, a note says so and the run's result is unchanged.
//   - done returns nil. waiting, declined, budget and timeout return ErrNeedsPerson (exit 3: raising a limit is a
//     person's call), interrupted context.Canceled (wrapped), task_failed
//     and error res.err: the caller (main) turns each into an exit code.
//   - The report names the run folder and the trace file (<traceDir>/<trace run ID>.jsonl; none when tracing is off).
func (ar *autoRun) end(cs *ChatSession, res runResult, traceDir, traceRunID string, w io.Writer) error {
	cs.ToolJournal = nil // the journal closes below: no call may be recorded after it
	if terr := ar.tools.err(); terr != nil {
		// [agent] no run_ended: a run marked ended has no open calls (Rebuild), and the call whose end wasn't recorded may
		// have done something. Left open, the next start resumes it and settles that call.
		ar.journal.Close()
		ar.unlock()
		fmt.Fprintf(w, "Stopped: a tool call could not be recorded in the journal (%v).\nThe run was left open: the next start "+
			"settles that call before anything else runs.\nRun folder: %s\n", terr, ar.run.Dir)
		return fmt.Errorf("run %04d: the journal failed: %w", ar.run.Number, terr)
	}
	ended := state.RunEnded{Reason: res.reason}
	if res.err != nil {
		ended.Error = res.err.Error()
	}
	jerr := ar.journal.Append(state.EventRunEnded, "", ended)
	ar.journal.Close()
	ar.unlock()

	if res.reason == state.StopDone {
		fmt.Fprintf(w, "Finished: %d done, %d skipped.\n", res.done, res.skipped)
	} else {
		fmt.Fprintf(w, "Stopped: %s. %d done, %d skipped, %d failed, %d not started.\n", res.reason, res.done, res.skipped, res.failed, res.notStarted)
	}
	switch res.reason {
	case state.StopBudget:
		fmt.Fprintf(w, "Token budget: %d of %d tokens used (runTokenBudget). Raise it to go on: a new run starts and the done tasks are skipped.\n",
			ar.used.Total(), cs.Config.RunTokenBudget)
	case state.StopTimeout:
		fmt.Fprintf(w, "Time limit: running for %s of %dm (runTimeoutMinutes). Raise it to go on: a new run starts and the done tasks are skipped.\n",
			ar.running().Round(time.Second), cs.Config.RunTimeoutMinutes)
	}
	if st, err := ar.home.Load(ar.ref); err == nil && jerr == nil {
		fmt.Fprintf(w, "\n%s\n", usageReport(st, ar.plan, ar.run.Number))
	} else if jerr == nil {
		fmt.Fprintf(w, "(no token report: reading the journals back failed: %v)\n", err)
	}
	fmt.Fprintf(w, "Run folder: %s\n", ar.run.Dir)
	if traceRunID != "" {
		fmt.Fprintf(w, "Trace: %s\n", filepath.Join(traceDir, traceRunID+".jsonl"))
	}
	if jerr != nil {
		return errors.Join(res.err, fmt.Errorf("run %04d: recording its end: %w", ar.run.Number, jerr))
	}
	switch res.reason {
	case state.StopDone:
		return nil
	case state.StopWaiting, state.StopDeclined, state.StopBudget, state.StopTimeout:
		return fmt.Errorf("run %04d stopped (%s): %w", ar.run.Number, res.reason, ErrNeedsPerson)
	case state.StopInterrupted:
		return fmt.Errorf("run %04d interrupted: %w", ar.run.Number, context.Canceled)
	}
	return fmt.Errorf("run %04d stopped (%s): %w", ar.run.Number, res.reason, res.err)
}
