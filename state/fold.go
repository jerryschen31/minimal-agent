package state

import (
	"fmt"
	"slices"
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
	Run      int // the run that last started or finished it
}

// RunState is what is known about one run.
type RunState struct {
	Run       Run
	Ended     *RunEnded    // nil: the run never ended (still going, or cut off by a crash)
	OpenCalls []ToolBefore // tool_before events with no tool_after: calls a crash cut off, in the order they started
	TornTail  bool
}

// State is a plan's state rebuilt from its runs' journals.
type State struct {
	Runs   []Run                // the plan's runs, oldest first
	Tasks  map[string]TaskState // by task ID; a task never started has no entry
	Latest RunState             // the last run; zero when there are no runs
}

// Fold rebuilds the state from runs' journals, applying their events in order (runs oldest first).
//
// Notes:
//   - task_started → running (attempts +1); task_finished → its status, def hash, summary and verdict. Later events win,
//     so a task done in run 1 and not touched since is still done in run 5.
//   - run_ended marks the tasks still running in that run as interrupted. A run with no run_ended keeps them running:
//     that is the task a crash cut off.
//   - Open calls are counted per run; only the last run's are kept (Latest.OpenCalls).
//   - Unknown event types are skipped (a newer harness's journal). Known types with data that can't be decoded are an
//     error naming the run and seq.
func Fold(runs []RunEvents) (State, error) {
	s := State{Tasks: map[string]TaskState{}}
	for _, re := range runs {
		rs := RunState{Run: re.Run, TornTail: re.TornTail}
		open := map[string]ToolBefore{}
		var order []string // call IDs in the order they started
		for _, e := range re.Events {
			var err error
			switch e.Type {
			case EventTaskStarted:
				ts := s.Tasks[e.Task]
				ts.Status, ts.Run = TaskRunning, re.Run.Number
				ts.Attempts++
				s.Tasks[e.Task] = ts
			case EventTaskFinished:
				var d TaskFinished
				if err = decodeData(e, &d); err == nil {
					ts := s.Tasks[e.Task]
					ts.Status, ts.DefHash, ts.Summary, ts.Verdict, ts.Run = d.Status, d.DefHash, d.Summary, d.Verdict, re.Run.Number
					s.Tasks[e.Task] = ts
				}
			case EventToolBefore:
				var d ToolBefore
				if err = decodeData(e, &d); err == nil {
					open[d.CallID] = d
					order = append(order, d.CallID)
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
		s.Runs = append(s.Runs, re.Run)
		s.Latest = rs
	}
	return s, nil
}
