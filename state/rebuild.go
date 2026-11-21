package state

import (
	"fmt"
	"slices"
	"time"
)

// RunEvents is one run's journal as read.
type RunEvents struct {
	Run      Run
	Events   []Event
	TornTail bool // the journal ended in a torn line (a crash during Append), which was dropped
}

// TaskState is the latest word on one task, over all runs of the plan.
type TaskState struct {
	Status   TaskStatus
	Attempts int    // task_started events over all runs
	DefHash  string // the definition hash recorded when it last finished
	Summary  string
	Verdict  string
	Run      int           // the run that last started or finished it
	Usage    Usage         // tokens over every attempt in every run (task_finished, attempt_usage)
	Running  time.Duration // time over every ended attempt in every run: task_started to its task_finished or attempt_usage
}

// OpenCall is a call a crash cut off: its tool_before and the task it belonged to.
type OpenCall struct {
	Task string
	ToolBefore
}

// RunState is what is known about one run.
type RunState struct {
	Run       Run
	Ended     *RunEnded  // nil: the run never ended (still going, or cut off by a crash)
	Manifest  Manifest   // the run's manifest (its first event)
	OpenCalls []OpenCall // tool_before events with no tool_after and not settled by a resume: calls a crash cut off, in the order they started
	TornTail  bool
	Usage     Usage         // tokens of the run's ended attempts (attempt_usage)
	Running   time.Duration // time the run was running: its sessions, without the gaps between a crash and a resume or gate waits (runningTime)
}

// PlanState is a plan's state rebuilt from its runs' journals.
type PlanState struct {
	Runs   []Run                // the plan's runs, oldest first
	Tasks  map[string]TaskState // by task ID; a task never started has no entry
	Latest RunState             // the last run; zero when there are no runs

	Total        Usage         // tokens over every run
	TotalRunning time.Duration // running time over every run
}

// Rebuild rebuilds a plan's state from its runs' journals, applying their events in order (runs oldest first).
//
// Notes:
//   - task_started → running (attempts +1); task_finished → its status, def hash, summary and verdict. Later events win,
//     so a task done in run 1 and not touched since is still done in run 5.
//   - run_ended marks the tasks still running in that run as interrupted. A run with no run_ended keeps them running:
//     that is the task a crash cut off.
//   - Open calls are counted per run; only the last run's are kept (Latest.OpenCalls). A resumed event closes the calls
//     it settled, so a second crash doesn't settle them again.
//   - task_finished and attempt_usage (an attempt that ended without task_finished) add to the run's and the task's Usage,
//     and the attempt's time (from its task_started) to the task's Running; an attempt a crash cut off has neither, so
//     it counts for neither. PlanState.Total and TotalRunning add up every run. RunState.Running is the run's running time (runningTime).
//   - Unknown event types are skipped (a newer harness's journal). Known types with data that can't be decoded are an
//     error naming the run and seq.
func Rebuild(runs []RunEvents) (PlanState, error) {
	s := PlanState{Tasks: map[string]TaskState{}}
	attemptStart := map[string]time.Time{} // task ID → its current attempt's task_started time
	for _, re := range runs {
		rs := RunState{Run: re.Run, TornTail: re.TornTail}
		open := map[string]OpenCall{}
		var waited time.Duration // gate waits, taken off the running time
		var order []string       // call IDs in the order they started
		for _, e := range re.Events {
			var err error
			switch e.Type {
			case EventTaskStarted:
				var d TaskStarted
				if err = decodeData(e, &d); err == nil { // decoded only to catch a damaged line, like the other types
					attemptStart[e.Task] = e.At
					ts := s.Tasks[e.Task]
					ts.Status, ts.Run = TaskRunning, re.Run.Number
					ts.Attempts++
					s.Tasks[e.Task] = ts
				}
			case EventTaskFinished:
				var d TaskFinished
				if err = decodeData(e, &d); err == nil {
					ts := s.Tasks[e.Task]
					ts.Status, ts.DefHash, ts.Summary, ts.Verdict, ts.Run = d.Status, d.DefHash, d.Summary, d.Verdict, re.Run.Number
					ts.Usage = ts.Usage.Add(d.Usage)
					ts.Running += attemptTime(attemptStart, e)
					s.Tasks[e.Task] = ts
					rs.Usage = rs.Usage.Add(d.Usage)
				}
			case EventManifest:
				err = decodeData(e, &rs.Manifest)
			case EventAttemptUsage:
				var d AttemptUsage
				if err = decodeData(e, &d); err == nil {
					rs.Usage = rs.Usage.Add(d.Usage)
					ts := s.Tasks[e.Task]
					ts.Usage = ts.Usage.Add(d.Usage)
					ts.Running += attemptTime(attemptStart, e)
					s.Tasks[e.Task] = ts
				}
			case EventGateAnswered:
				var d GateAnswered
				if err = decodeData(e, &d); err == nil {
					waited += time.Duration(d.WaitedMS) * time.Millisecond
				}
			case EventToolBefore:
				var d ToolBefore
				if err = decodeData(e, &d); err == nil {
					open[d.CallID] = OpenCall{Task: e.Task, ToolBefore: d}
					order = append(order, d.CallID)
				}
			case EventResumed:
				var d Resumed
				if err = decodeData(e, &d); err == nil {
					for _, c := range d.Settled {
						delete(open, c.CallID) // settled: no longer cut off
					}
				}
			case EventToolAfter:
				var d ToolAfter
				if err = decodeData(e, &d); err == nil {
					delete(open, d.CallID)
				}
			case EventRunEnded:
				var d RunEnded
				if err = decodeData(e, &d); err == nil {
					rs.Ended = &d
					for id, ts := range s.Tasks {
						if ts.Status == TaskRunning && ts.Run == re.Run.Number {
							ts.Status = TaskInterrupted
							s.Tasks[id] = ts
						}
					}
				}
			}
			if err != nil {
				return PlanState{}, fmt.Errorf("run %d: %w", re.Run.Number, err)
			}
		}
		for _, id := range slices.Compact(order) {
			if b, ok := open[id]; ok {
				rs.OpenCalls = append(rs.OpenCalls, b)
				delete(open, id)
			}
		}
		running, err := runningTime(re.Events)
		if err != nil {
			return PlanState{}, fmt.Errorf("run %d: %w", re.Run.Number, err)
		}
		rs.Running = max(running-waited, 0)
		s.Runs = append(s.Runs, re.Run)
		s.Latest = rs
		s.Total = s.Total.Add(rs.Usage)
		s.TotalRunning += rs.Running
	}
	return s, nil
}

// runningTime returns how long a run's sessions ran: for each session, from its start to its last event.
//
// Notes:
//   - A session starts at the manifest (the first) or at a resumed event's StartedAt (the event's own time for older
//     journals). The resumed event is written after the cut-off calls were re-sent, so StartedAt, not the event's
//     time, is where the session began; events from StartedAt on belong to the new session.
//   - The time between a session's last event and the crash that ended it is not known and not counted, nor is the time
//     between the crash and the resume.
//   - Gate waits are not taken off here (Rebuild does that).
func runningTime(events []Event) (time.Duration, error) {
	var starts []time.Time
	for _, e := range events {
		switch e.Type {
		case EventManifest:
			starts = append(starts, e.At)
		case EventResumed:
			var d Resumed
			if err := decodeData(e, &d); err != nil {
				return 0, err
			}
			start := d.StartedAt
			if start.IsZero() {
				start = e.At
			}
			starts = append(starts, start)
		}
	}
	slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
	var total time.Duration
	for i, start := range starts {
		last := start
		for _, e := range events {
			if !e.At.Before(start) && (i+1 == len(starts) || e.At.Before(starts[i+1])) && e.At.After(last) {
				last = e.At
			}
		}
		total += last.Sub(start)
	}
	return total, nil
}

// attemptTime returns the time from the task's current attempt start (its task_started) to end, the event that ended
// the attempt, and forgets the start.
//
// Notes:
//   - 0 when no start was seen (a damaged or partial journal), or when the clock went backwards.
func attemptTime(starts map[string]time.Time, end Event) time.Duration {
	start, ok := starts[end.Task]
	if !ok {
		return 0
	}
	delete(starts, end.Task)
	return max(end.At.Sub(start), 0)
}
