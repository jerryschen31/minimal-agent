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
	Run      int   // the run that last started or finished it
	Usage    Usage // tokens over every attempt in every run (attempt_usage)
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

// State is a plan's state rebuilt from its runs' journals.
type State struct {
	Runs   []Run                // the plan's runs, oldest first
	Tasks  map[string]TaskState // by task ID; a task never started has no entry
	Latest RunState             // the last run; zero when there are no runs
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
//   - task_finished and attempt_usage (an attempt that ended without task_finished) add to the run's and the task's Usage; RunState.Running is the run's running time (runningTime).
//   - Unknown event types are skipped (a newer harness's journal). Known types with data that can't be decoded are an
//     error naming the run and seq.
func Rebuild(runs []RunEvents) (State, error) {
	s := State{Tasks: map[string]TaskState{}}
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
				return State{}, fmt.Errorf("run %d: %w", re.Run.Number, err)
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
			return State{}, fmt.Errorf("run %d: %w", re.Run.Number, err)
		}
		rs.Running = max(running-waited, 0)
		s.Runs = append(s.Runs, re.Run)
		s.Latest = rs
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
