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

	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/state"
)

// autoRun is one open run of a plan: its folder and journal, the plan's lock, and the plan's state before this run.
type autoRun struct {
	run     state.Run
	journal *state.Journal
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
	opened = true
	return &autoRun{run: r, journal: j, unlock: unlock, before: before}, nil
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
//   - Serial: one task at a time. A failed task stops the run; the tasks after it are "not started".
//   - Each task that runs gets task_started (attempt 1: retries arrive in 1e) and, when it ends, task_finished with its
//     status, definition hash and summary (for a failure, the error). An interrupted task gets no task_finished: the
//     run_ended event marks it interrupted (state.Fold).
//   - The worker of a task is told the summaries of the tasks it depends on, from this run or an earlier one.
//   - A task with a gate stops the run with "waiting" before it starts. [agent] 1e asks the gate's question instead.
//   - A journal write that fails stops the run with "error": a run must not go on without its record.
func runPlan(ctx context.Context, cs *ChatSession, ar *autoRun, p plan.Plan) runResult {
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
			fmt.Fprintf(w, "Waiting at %s: it has a gate (%s), and gates can't be answered yet (step 1e).\n", t.ID, t.Gate)
			return stop(i, state.StopWaiting, nil)
		}
		if err := ar.journal.Append(state.EventTaskStarted, t.ID, state.TaskStarted{Attempt: 1}); err != nil {
			return stop(i, state.StopError, err)
		}
		fmt.Fprintf(w, "▶ %s: %s\n", t.ID, t.Title)
		summary, err := runWorker(ctx, cs, p, t, summaries)
		if err != nil && ctx.Err() != nil {
			fmt.Fprintf(w, "■ %s interrupted\n", t.ID)
			return stop(i+1, state.StopInterrupted, nil)
		}
		if err != nil {
			res.failed++
			fmt.Fprintf(w, "✗ %s failed: %v\n", t.ID, err)
			fin := state.TaskFinished{Status: state.TaskFailed, DefHash: t.DefinitionHash(), Summary: err.Error()}
			if jerr := ar.journal.Append(state.EventTaskFinished, t.ID, fin); jerr != nil {
				err = errors.Join(err, jerr)
			}
			return stop(i+1, stopReasonFor(err), fmt.Errorf("%s failed: %w", t.ID, err))
		}
		fin := state.TaskFinished{Status: state.TaskDone, DefHash: t.DefinitionHash(), Summary: summary}
		if err := ar.journal.Append(state.EventTaskFinished, t.ID, fin); err != nil {
			return stop(i+1, state.StopError, err)
		}
		res.done++
		summaries[t.ID] = summary
		fmt.Fprintf(w, "✓ %s done\n", t.ID)
	}
	return stop(len(steps), state.StopDone, nil)
}

// stopReasonFor returns the stop reason for a task that failed with err.
//
// Notes:
//   - ErrMaxSteps → max_steps; a cancel (Ctrl+C, SIGTERM) → interrupted; anything else → error.
func stopReasonFor(err error) state.StopReason {
	switch {
	case errors.Is(err, ErrMaxSteps):
		return state.StopMaxSteps
	case errors.Is(err, context.Canceled):
		return state.StopInterrupted
	}
	return state.StopError
}

// end records run_ended, closes the journal, releases the lock and prints the report; it returns an error when the run
// stopped on a failure.
//
// Notes:
//   - done, waiting and interrupted return nil (the report says what happened); max_steps and error return res.err, so
//     the caller sees the run did not finish.
//   - The report names the run folder and the trace file (<traceDir>/<trace run ID>.jsonl; none when tracing is off).
func (ar *autoRun) end(res runResult, traceDir, traceRunID string, w io.Writer) error {
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
