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

	"github.com/jerryschen31/minimal-agent/agent/director"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/state"
)

// autoRun is one open run of a plan: its folder and journal, the plan's lock, and the plan's state before this run.
type autoRun struct {
	run     state.Run
	journal *state.Journal
	tools   *runToolJournal // records the run's tool calls; also on the session (cs.ToolJournal) while the run is open
	unlock  func() error
	before  state.State // what the plan's earlier runs left (state.Home.Load)
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
//     (allowed, with a warning), a run that never ended.
//   - The run folder gets the plan file's bytes as read (ad.planData), so what ran can be seen later even if the file
//     changes. The manifest records the trace's run ID; the trace is in <stateDir>/traces (SetupAgent).
//   - On an error after the lock was taken, the lock is released.
//   - Sets cs.ToolJournal, so from now on every tool call is recorded in the run's journal (end clears it).
func openRun(cs *ChatSession, ad *autoModeDetails) (*autoRun, error) {
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
	m.TraceRunID = cs.Recorder.RunID()
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
	return &autoRun{run: r, journal: j, tools: rj, unlock: unlock, before: before}, nil
}

// notePlanHistory prints what the plan's earlier runs say before a new run starts.
//
// Notes:
//   - The plan has a plan_id and its last run was at another path: if a file at that path has the same plan_id, it is a
//     copy, and a warning says the two share one history and one lock (a copy is allowed: refusing would let anyone who
//     can copy a file stop a plan from running; protecting the plan file is planned for later, DECISIONS.md
//     § 2026-11-20 — 1d). Otherwise the plan was moved, and its history carries over.
//   - The last run never ended (no run_ended: a crash or a kill): its unfinished task runs again. [agent] 1h turns this
//     into a real resume (cut-off calls settled, the same journal continued).
func notePlanHistory(w io.Writer, ref state.PlanRef, st state.State) {
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
	if st.Latest.Ended == nil {
		fmt.Fprintf(w, "Run %04d did not finish (a crash or a kill); its unfinished task runs again.\n", st.Latest.Run.Number)
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
//   - A done task whose dependency runs again is still skipped (no cascade in v1; PLAN-auto-mode.md "Which tasks run").
//   - A cycle or an unknown dependency can't happen (CheckComplete refuses both); if one did, the rest keep file order.
func tasksToRun(p plan.Plan, st state.State) (steps []taskStep, removed []string) {
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
		if t.Gate != "" {
			if reason, err := askGate(ctx, ar, t, gates, w); reason != "" {
				return stop(i, reason, err)
			}
		}
		summary, reason, err := runTask(ctx, cs, ar, p, t, summaries)
		switch reason {
		case "":
			res.done++
			summaries[t.ID] = summary
			continue
		case state.StopInterrupted:
			fmt.Fprintf(w, "■ %s interrupted\n", t.ID)
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
func askGate(ctx context.Context, ar *autoRun, t plan.Task, gates promptReader, w io.Writer) (state.StopReason, error) {
	if gates == nil {
		fmt.Fprintf(w, "%s has a gate (%s) and there is no terminal to ask; run it again on a terminal.\n", t.ID, t.Gate)
		return state.StopWaiting, nil
	}
	answer, err := gates.AskLine(ctx, fmt.Sprintf("%s: %s has a gate: %q. Run it? [y/N]: ", t.ID, t.Title, t.Gate))
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
	if jerr := ar.journal.Append(state.EventGateAnswered, t.ID, state.GateAnswered{Answer: recorded}); jerr != nil {
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
//     the journal; the last one decides the task's state (state.Fold).
//   - A worker that runs out of steps is a failed attempt (the director isn't asked). A director with no verdict is a
//     failed attempt too (judgeTask). Out of attempts → "task_failed".
//   - Ctrl+C → "interrupted", with no task_finished for the attempt (run_ended marks it). Any other error (the
//     provider, a journal write) → "error", no retry.
func runTask(ctx context.Context, cs *ChatSession, ar *autoRun, p plan.Plan, t plan.Task, deps map[string]string) (string, state.StopReason, error) {
	w := cs.OutBuffer
	maxAttempts := cs.Config.MaxTaskAttempts
	ar.tools.setTask(t.ID) // the worker's and the director's tool calls are recorded under this task
	var retry *retryNote
	for attempt := 1; ; attempt++ {
		if err := ar.journal.Append(state.EventTaskStarted, t.ID, state.TaskStarted{Attempt: attempt}); err != nil {
			return "", state.StopError, err
		}
		if attempt == 1 {
			fmt.Fprintf(w, "▶ %s: %s\n", t.ID, t.Title)
		} else {
			fmt.Fprintf(w, "▶ %s: %s (attempt %d of %d)\n", t.ID, t.Title, attempt, maxAttempts)
		}
		summary, workerMsgs, err := runWorker(ctx, cs, p, t, deps, retry)
		var v director.Verdict
		switch {
		case err != nil && ctx.Err() != nil:
			return "", state.StopInterrupted, nil
		case errors.Is(err, ErrMaxSteps):
			v.Reason = fmt.Sprintf("the worker ran out of steps (maxSteps %d)", cs.Config.MaxSteps)
		case err != nil:
			return "", state.StopError, finishFailed(ar, t, err.Error(), "", err)
		default:
			v, err = judgeTask(ctx, cs, t, attempt, summary, workerMsgs)
			if err != nil && ctx.Err() != nil {
				return "", state.StopInterrupted, nil
			}
			if err != nil {
				err = fmt.Errorf("the director failed: %w", err)
				return "", state.StopError, finishFailed(ar, t, summary, "", err)
			}
		}
		fin := state.TaskFinished{Status: state.TaskFailed, DefHash: t.DefinitionHash(), Summary: summary, Verdict: v.Reason}
		if v.Done {
			fin.Status = state.TaskDone
		} else if v.Advice != "" {
			fin.Verdict += " Advice: " + v.Advice
		}
		if err := ar.journal.Append(state.EventTaskFinished, t.ID, fin); err != nil {
			return "", state.StopError, err
		}
		if v.Done {
			fmt.Fprintf(w, "✓ %s done — %s\n", t.ID, v.Reason)
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

// finishFailed records a failed task_finished for t with summary and verdict, and returns err (joined with the journal's
// error if that write fails too).
//
// Notes:
//   - For an attempt that ended in an error: the run stops, but the journal still says which task failed and why.
func finishFailed(ar *autoRun, t plan.Task, summary, verdict string, err error) error {
	fin := state.TaskFinished{Status: state.TaskFailed, DefHash: t.DefinitionHash(), Summary: summary, Verdict: verdict}
	if jerr := ar.journal.Append(state.EventTaskFinished, t.ID, fin); jerr != nil {
		return errors.Join(err, jerr)
	}
	return fmt.Errorf("%s failed: %w", t.ID, err)
}

// end stops recording cs's tool calls, records run_ended, closes the journal, releases the lock and prints the report; it returns an error when the run
// stopped on a failure.
//
// Notes:
//   - done, waiting, declined and interrupted return nil (the report says what happened); task_failed and error
//     return res.err, so the caller sees the run did not finish.
//   - The report names the run folder and the trace file (<traceDir>/<trace run ID>.jsonl; none when tracing is off).
func (ar *autoRun) end(cs *ChatSession, res runResult, traceDir, traceRunID string, w io.Writer) error {
	cs.ToolJournal = nil // the journal closes below: no call may be recorded after it
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
	fmt.Fprintf(w, "Run folder: %s\n", ar.run.Dir)
	if traceRunID != "" {
		fmt.Fprintf(w, "Trace: %s\n", filepath.Join(traceDir, traceRunID+".jsonl"))
	}
	if jerr != nil {
		return errors.Join(res.err, fmt.Errorf("run %04d: recording its end: %w", ar.run.Number, jerr))
	}
	if res.err != nil && res.reason != state.StopInterrupted {
		return fmt.Errorf("run %04d stopped (%s): %w", ar.run.Number, res.reason, res.err)
	}
	return nil
}
