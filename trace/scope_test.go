package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

func scopeEvents(t *testing.T, run func(rec Recorder)) []Event {
	t.Helper()
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	run(rec)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return sub.events(t)
}

func Test_Scope_TaskStatuses(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
		wantErr    string
	}{
		{"ok", nil, "ok", ""},
		{"error", errors.New("boom"), "error", "boom"},
		{"canceled", fmt.Errorf("turn: %w", context.Canceled), "canceled", "turn: context canceled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs := scopeEvents(t, func(rec Recorder) {
				task := StartTask(rec, "hello")
				defer task.End()
				task.Record(ModelRequest, ModelRequestData{Model: "m"})
				task.Fail(c.err)
			})
			if len(evs) != 3 || evs[0].EventType != TaskStart || evs[1].EventType != ModelRequest || evs[2].EventType != TaskEnd {
				t.Fatalf("events = %+v", evs)
			}
			if !regexpTaskID.MatchString(evs[0].ScopeID) || evs[1].ScopeID != evs[0].ScopeID || evs[2].ScopeID != evs[0].ScopeID {
				t.Errorf("scope ids = %q %q %q", evs[0].ScopeID, evs[1].ScopeID, evs[2].ScopeID)
			}
			var end TaskEndData
			if err := json.Unmarshal(evs[2].EventData, &end); err != nil {
				t.Fatal(err)
			}
			if end.Status != c.wantStatus || end.Error != c.wantErr {
				t.Errorf("task_end = %+v, want status %q error %q", end, c.wantStatus, c.wantErr)
			}
		})
	}
}

func Test_Scope_RunUsesRunIDAsScopeID(t *testing.T) {
	evs := scopeEvents(t, func(rec Recorder) {
		run := StartRun(rec, config.Config{AgentName: "test"})
		run.Fail(context.Canceled)
		run.End()
	})
	if len(evs) != 2 || evs[0].EventType != RunStart || evs[1].EventType != RunEnd {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].ScopeID != "run-1" || evs[1].ScopeID != "run-1" || evs[0].RunID != "run-1" {
		t.Errorf("run events: run_id %q, scope ids %q %q, want all run-1", evs[0].RunID, evs[0].ScopeID, evs[1].ScopeID)
	}
	var end RunEndData
	if err := json.Unmarshal(evs[1].EventData, &end); err != nil {
		t.Fatal(err)
	}
	if end.Status != "canceled" {
		t.Errorf("run_end = %+v", end)
	}
}
