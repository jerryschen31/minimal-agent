package state

import (
	"encoding/json"
	"strings"
	"testing"
)

// ev builds an event for Fold tests.
func ev(seq int, typ EventType, task string, data any) Event {
	e := Event{V: 1, Seq: seq, Type: typ, Task: task}
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	return e
}

//   - Verify started → running with attempts; finished → done/failed with hash, summary and verdict; done in run 1 and
//     untouched in run 2 stays done; a task started again in a later run counts its attempts over all runs
//   - Verify a task running when its run ended is interrupted, and one running in an un-ended run stays running
//   - Verify a tool_before with no tool_after in the last run is an open call; one with an after is not; open calls of an
//     earlier run are not kept
//   - Verify unknown event types are skipped
func Test_Unit_Fold(t *testing.T) {
	run1 := RunEvents{Run: Run{Number: 1}, Events: []Event{
		ev(1, EventManifest, "", Manifest{PlanPath: "/p"}),
		ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}),
		ev(3, EventToolBefore, "t1", ToolBefore{CallID: "c1", Tool: "write_file"}),
		ev(4, EventToolAfter, "t1", ToolAfter{CallID: "c1"}),
		ev(5, EventTaskFinished, "t1", TaskFinished{Status: TaskDone, DefHash: "h1", Summary: "wrote it", Verdict: "ok"}),
		ev(6, EventTaskStarted, "t2", TaskStarted{Attempt: 1}),
		ev(7, EventToolBefore, "t2", ToolBefore{CallID: "c9", Tool: "x"}),
		ev(8, "from_the_future", "", nil),
		ev(9, EventRunEnded, "", RunEnded{Reason: StopInterrupted}),
	}}
	run2 := RunEvents{Run: Run{Number: 2}, Events: []Event{
		ev(1, EventManifest, "", Manifest{PlanPath: "/p"}),
		ev(2, EventTaskStarted, "t2", TaskStarted{Attempt: 1}),
		ev(3, EventToolBefore, "t2", ToolBefore{CallID: "c2", Tool: "email_send"}),
		ev(4, EventToolBefore, "t2", ToolBefore{CallID: "c3", Tool: "read_file"}),
		ev(5, EventToolAfter, "t2", ToolAfter{CallID: "c3"}),
	}, TornTail: true}

	s, err := Fold([]RunEvents{run1})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tasks["t2"].Status != TaskInterrupted || s.Latest.Ended == nil || s.Latest.Ended.Reason != StopInterrupted {
		t.Errorf("after run 1: t2 %+v, ended %+v", s.Tasks["t2"], s.Latest.Ended)
	}

	s, err = Fold([]RunEvents{run1, run2})
	if err != nil {
		t.Fatal(err)
	}
	t1 := s.Tasks["t1"]
	if t1.Status != TaskDone || t1.DefHash != "h1" || t1.Summary != "wrote it" || t1.Verdict != "ok" || t1.Attempts != 1 || t1.Run != 1 {
		t.Errorf("t1 = %+v", t1)
	}
	if t2 := s.Tasks["t2"]; t2.Status != TaskRunning || t2.Attempts != 2 || t2.Run != 2 {
		t.Errorf("t2 = %+v", t2)
	}
	if s.Latest.Run.Number != 2 || s.Latest.Ended != nil || !s.Latest.TornTail {
		t.Errorf("latest = %+v", s.Latest)
	}
	if len(s.Latest.OpenCalls) != 1 || s.Latest.OpenCalls[0].CallID != "c2" || s.Latest.OpenCalls[0].Tool != "email_send" {
		t.Errorf("open calls = %+v, want only c2", s.Latest.OpenCalls)
	}
	if len(s.Runs) != 2 {
		t.Errorf("runs = %+v", s.Runs)
	}
}

//   - Verify a resumed event closes the calls it settled (a second crash has nothing to settle), keeps the task of each
//     open call, and the run's manifest is on its RunState
func Test_Unit_Fold_ResumedClosesSettledCalls(t *testing.T) {
	run := RunEvents{Run: Run{Number: 1}, Events: []Event{
		ev(1, EventManifest, "", Manifest{PlanPath: "/p.md", PlanSHA256: "sha256:p"}),
		ev(2, EventTaskStarted, "t2", TaskStarted{Attempt: 1}),
		ev(3, EventToolBefore, "t2", ToolBefore{CallID: "c1", Tool: "email_send"}),
		ev(4, EventToolBefore, "t2", ToolBefore{CallID: "c2", Tool: "write_file"}),
	}}
	s, _ := Fold([]RunEvents{run})
	if len(s.Latest.OpenCalls) != 2 || s.Latest.OpenCalls[0].Task != "t2" || s.Latest.Manifest.PlanSHA256 != "sha256:p" {
		t.Fatalf("before the resume: open %+v, manifest %+v", s.Latest.OpenCalls, s.Latest.Manifest)
	}
	run.Events = append(run.Events, ev(5, EventResumed, "", Resumed{PID: 1, Settled: []SettledCall{{CallID: "c1", Outcome: OutcomeUserSaidDone}}}))
	s, _ = Fold([]RunEvents{run})
	if len(s.Latest.OpenCalls) != 1 || s.Latest.OpenCalls[0].CallID != "c2" || s.Latest.Ended != nil {
		t.Errorf("after settling c1: open %+v, ended %+v; want only c2, not ended", s.Latest.OpenCalls, s.Latest.Ended)
	}
}

// - Verify a known event whose data can't be decoded is an error naming the run and seq
func Test_Unit_Fold_BadData(t *testing.T) {
	bad := Event{V: 1, Seq: 4, Type: EventTaskFinished, Task: "t1", Data: json.RawMessage(`{"status": 7}`)}
	if _, err := Fold([]RunEvents{{Run: Run{Number: 3}, Events: []Event{bad}}}); err == nil {
		t.Error("want an error")
	} else if got := err.Error(); !strings.Contains(got, "run 3") || !strings.Contains(got, "seq 4") {
		t.Errorf("err = %v, want one naming run 3 and seq 4", err)
	}
}
