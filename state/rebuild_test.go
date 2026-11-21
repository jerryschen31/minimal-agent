package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ev builds an event for Rebuild tests.
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
func Test_Unit_Rebuild(t *testing.T) {
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

	s, err := Rebuild([]RunEvents{run1})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tasks["t2"].Status != TaskInterrupted || s.Latest.Ended == nil || s.Latest.Ended.Reason != StopInterrupted {
		t.Errorf("after run 1: t2 %+v, ended %+v", s.Tasks["t2"], s.Latest.Ended)
	}

	s, err = Rebuild([]RunEvents{run1, run2})
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
func Test_Unit_Rebuild_ResumedClosesSettledCalls(t *testing.T) {
	run := RunEvents{Run: Run{Number: 1}, Events: []Event{
		ev(1, EventManifest, "", Manifest{PlanPath: "/p.md", PlanSHA256: "sha256:p"}),
		ev(2, EventTaskStarted, "t2", TaskStarted{Attempt: 1}),
		ev(3, EventToolBefore, "t2", ToolBefore{CallID: "c1", Tool: "email_send"}),
		ev(4, EventToolBefore, "t2", ToolBefore{CallID: "c2", Tool: "write_file"}),
	}}
	s, _ := Rebuild([]RunEvents{run})
	if len(s.Latest.OpenCalls) != 2 || s.Latest.OpenCalls[0].Task != "t2" || s.Latest.Manifest.PlanSHA256 != "sha256:p" {
		t.Fatalf("before the resume: open %+v, manifest %+v", s.Latest.OpenCalls, s.Latest.Manifest)
	}
	run.Events = append(run.Events, ev(5, EventResumed, "", Resumed{PID: 1, Settled: []SettledCall{{CallID: "c1", Outcome: OutcomeUserSaidDone}}}))
	s, _ = Rebuild([]RunEvents{run})
	if len(s.Latest.OpenCalls) != 1 || s.Latest.OpenCalls[0].CallID != "c2" || s.Latest.Ended != nil {
		t.Errorf("after settling c1: open %+v, ended %+v; want only c2, not ended", s.Latest.OpenCalls, s.Latest.Ended)
	}
}

// - Verify a known event whose data can't be decoded (task_finished, task_started) is an error naming the run and seq
func Test_Unit_Rebuild_BadData(t *testing.T) {
	for _, bad := range []Event{
		{V: 1, Seq: 4, Type: EventTaskFinished, Task: "t1", Data: json.RawMessage(`{"status": 7}`)},
		{V: 1, Seq: 4, Type: EventTaskStarted, Task: "t1", Data: json.RawMessage(`{"attempt": "bad"}`)},
	} {
		if _, err := Rebuild([]RunEvents{{Run: Run{Number: 3}, Events: []Event{bad}}}); err == nil {
			t.Errorf("%s: want an error", bad.Type)
		} else if got := err.Error(); !strings.Contains(got, "run 3") || !strings.Contains(got, "seq 4") {
			t.Errorf("%s: err = %v, want one naming run 3 and seq 4", bad.Type, err)
		}
	}
}

// at returns ev(...) with its time set to base + offset.
func at(e Event, base time.Time, offset time.Duration) Event {
	e.At = base.Add(offset)
	return e
}

//   - Verify Rebuild adds up usage from task_finished and attempt_usage, per run (Latest) and per task over all runs, and an
//     older journal without usage gives zero
func Test_Unit_Rebuild_Usage(t *testing.T) {
	u := func(in, out int) Usage { return Usage{InputTokens: in, OutputTokens: out, Reported: true} }
	run1 := RunEvents{Run: Run{Number: 1}, Events: []Event{
		ev(1, EventManifest, "", Manifest{PlanPath: "/p"}),
		ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}),
		ev(3, EventTaskFinished, "t1", TaskFinished{Status: TaskFailed, Usage: u(100, 10)}),
		ev(4, EventTaskStarted, "t1", TaskStarted{Attempt: 2}),
		ev(5, EventAttemptUsage, "t1", AttemptUsage{Attempt: 2, Usage: u(50, 5)}),
		ev(6, EventRunEnded, "", RunEnded{Reason: StopBudget}),
	}}
	run2 := RunEvents{Run: Run{Number: 2}, Events: []Event{
		ev(1, EventManifest, "", Manifest{PlanPath: "/p"}),
		ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}),
		ev(3, EventTaskFinished, "t1", TaskFinished{Status: TaskDone, Usage: u(200, 20)}),
		ev(4, EventTaskStarted, "t2", TaskStarted{Attempt: 1}),
		ev(5, EventTaskFinished, "t2", TaskFinished{Status: TaskDone}), // an older journal: no usage
	}}
	s, err := Rebuild([]RunEvents{run1, run2})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Latest.Usage.Total(); got != 220 {
		t.Errorf("run 2 usage = %d, want 220 (only its own attempts)", got)
	}
	if got := s.Tasks["t1"].Usage.Total(); got != 385 {
		t.Errorf("t1 usage = %d, want 385 (110 + 55 + 220, over both runs)", got)
	}
	if got := s.Tasks["t2"].Usage; got != (Usage{}) {
		t.Errorf("t2 usage = %+v, want zero", got)
	}
}

//   - Verify a run's running time is its sessions' time: from the manifest to the last event before the crash, then from
//     the resumed session's StartedAt (before its re-sent calls) to its last event; the gap between is not counted, nor
//     are gate waits; an older resumed event without StartedAt starts its session at its own time
func Test_Unit_Rebuild_RunningTime(t *testing.T) {
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	events := []Event{
		at(ev(1, EventManifest, "", Manifest{PlanPath: "/p"}), base, 0),
		at(ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}), base, time.Minute),
		at(ev(3, EventToolBefore, "t1", ToolBefore{CallID: "c1", Tool: "x"}), base, 10*time.Minute), // then a crash
		// the next session starts at +1h: it re-sends c1, then writes resumed
		at(ev(4, EventToolBefore, "t1", ToolBefore{CallID: "c2", Tool: "x", ResendOf: "c1"}), base, time.Hour+time.Minute),
		at(ev(5, EventToolAfter, "t1", ToolAfter{CallID: "c2"}), base, time.Hour+2*time.Minute),
		at(ev(6, EventResumed, "", Resumed{StartedAt: base.Add(time.Hour), Settled: []SettledCall{{CallID: "c1"}}}), base, time.Hour+2*time.Minute),
		at(ev(7, EventGateAnswered, "t2", GateAnswered{Answer: "yes", WaitedMS: 2 * 60 * 1000}), base, time.Hour+4*time.Minute),
		at(ev(8, EventRunEnded, "", RunEnded{Reason: StopDone}), base, time.Hour+15*time.Minute),
	}
	s, err := Rebuild([]RunEvents{{Run: Run{Number: 1}, Events: events}})
	if err != nil {
		t.Fatal(err)
	}
	if want := 10*time.Minute + 15*time.Minute - 2*time.Minute; s.Latest.Running != want {
		t.Errorf("running = %s, want %s (10m + 15m, minus a 2m gate wait)", s.Latest.Running, want)
	}

	// an older journal: resumed without StartedAt starts the session at the resumed event's own time
	old := []Event{
		at(ev(1, EventManifest, "", Manifest{PlanPath: "/p"}), base, 0),
		at(ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}), base, 5*time.Minute),
		at(ev(3, EventResumed, "", Resumed{}), base, time.Hour),
		at(ev(4, EventRunEnded, "", RunEnded{Reason: StopDone}), base, time.Hour+3*time.Minute),
	}
	s, err = Rebuild([]RunEvents{{Run: Run{Number: 1}, Events: old}})
	if err != nil {
		t.Fatal(err)
	}
	if want := 8 * time.Minute; s.Latest.Running != want {
		t.Errorf("older journal: running = %s, want %s", s.Latest.Running, want)
	}
}

//   - Verify a task's Running adds up its ended attempts over runs (task_started to task_finished or attempt_usage), an
//     attempt a crash cut off counts for nothing, and Total / TotalRunning add up every run
func Test_Unit_Rebuild_TaskTimeAndTotals(t *testing.T) {
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	u := Usage{InputTokens: 10, OutputTokens: 1, Reported: true}
	run1 := RunEvents{Run: Run{Number: 1}, Events: []Event{
		at(ev(1, EventManifest, "", Manifest{PlanPath: "/p"}), base, 0),
		at(ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}), base, time.Minute),
		at(ev(3, EventTaskFinished, "t1", TaskFinished{Status: TaskFailed, Usage: u}), base, 3*time.Minute), // 2m
		at(ev(4, EventTaskStarted, "t1", TaskStarted{Attempt: 2}), base, 4*time.Minute),                     // cut off by a crash
		at(ev(5, EventTaskStarted, "t1", TaskStarted{Attempt: 3}), base, time.Hour),                         // (resumed)
		at(ev(6, EventAttemptUsage, "t1", AttemptUsage{Attempt: 3, Usage: u}), base, time.Hour+time.Minute), // 1m
		at(ev(7, EventRunEnded, "", RunEnded{Reason: StopBudget}), base, time.Hour+time.Minute),
	}}
	run2 := RunEvents{Run: Run{Number: 2}, Events: []Event{
		at(ev(1, EventManifest, "", Manifest{PlanPath: "/p"}), base, 2*time.Hour),
		at(ev(2, EventTaskStarted, "t1", TaskStarted{Attempt: 1}), base, 2*time.Hour),
		at(ev(3, EventTaskFinished, "t1", TaskFinished{Status: TaskDone, Usage: u}), base, 2*time.Hour+30*time.Second), // 30s
		at(ev(4, EventRunEnded, "", RunEnded{Reason: StopDone}), base, 2*time.Hour+30*time.Second),
	}}
	s, err := Rebuild([]RunEvents{run1, run2})
	if err != nil {
		t.Fatal(err)
	}
	if want := 3*time.Minute + 30*time.Second; s.Tasks["t1"].Running != want {
		t.Errorf("t1 running = %s, want %s (2m + 1m + 30s; the crashed attempt not counted)", s.Tasks["t1"].Running, want)
	}
	if s.Total.Total() != 33 || s.Latest.Usage.Total() != 11 {
		t.Errorf("total %d, latest %d; want 33 over both runs and 11 for run 2", s.Total.Total(), s.Latest.Usage.Total())
	}
	// run 1 has no resumed event here, so it is one session: 0 to 1h01m; run 2 is 30s
	if want := time.Hour + time.Minute + 30*time.Second; s.TotalRunning != want {
		t.Errorf("total running = %s, want %s", s.TotalRunning, want)
	}
}

// - Verify Usage.Add adds up UnreportedCalls, so a sum with some calls that reported no usage still says how many
func Test_Unit_Usage_Add_KeepsUnreportedCalls(t *testing.T) {
	a := Usage{InputTokens: 10, OutputTokens: 1, Reported: true, UnreportedCalls: 1}
	b := Usage{UnreportedCalls: 2}
	if got := a.Add(b); got.UnreportedCalls != 3 || !got.Reported || got.Total() != 11 {
		t.Errorf("a + b = %+v, want 11 tokens, reported, 3 unreported calls", got)
	}
}
