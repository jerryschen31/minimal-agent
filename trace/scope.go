package trace

import (
	"context"
	"errors"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

// Scope is one traced unit of agentic work: a run or a task. Perhaps there's other units of agentic work in the future.
// For now, StartRun / StartTask record a start event, End records the matching end event.
// Record stamps every event in between with the scope's ID, in the scope_id field: the run ID for a run, the task ID for a task.
// Record and ID are safe to call from several goroutines at once (they only read fields set at creation, and the recorder
// has its own lock), e.g. parallel tool calls. Fail and End belong to the goroutine that owns the scope.
type Scope struct {
	rec     Recorder
	id      string    // the scope_id on every event of this scope: the run ID for a run, a task ID for a task
	endType EventType // RunEnd or TaskEnd
	start   time.Time
	status  string // "ok" | "canceled" | "error"; changed by Fail
	errText string
}

// StartRun records run_start and returns the run's Scope. The run's own ID is its scope ID, so run events carry it as scope_id too.
func StartRun(rec Recorder, cfg config.Config) *Scope {
	id := rec.RunID()
	rec.Record(id, RunStart, GetRunStartData(cfg))
	return &Scope{rec: rec, id: id, endType: RunEnd, start: time.Now(), status: "ok"}
}

// StartTask makes a new task ID, records task_start and returns the task's Scope.
func StartTask(rec Recorder, input string) *Scope {
	id := NewTaskID()
	rec.Record(id, TaskStart, GetTaskStartData(input))
	return &Scope{rec: rec, id: id, endType: TaskEnd, start: time.Now(), status: "ok"}
}

// Record records an event inside this scope (the scope supplies the scope_id)
func (s *Scope) Record(eventType EventType, data any) {
	s.rec.Record(s.id, eventType, data)
}

// Fail marks the scope as failed, to be reported by End. A cancelled context (Ctrl+C, SIGTERM) is "canceled",
// any other error is "error"; the error text is recorded either way. A nil error changes nothing, so callers can
// pass whatever they got.
func (s *Scope) Fail(err error) {
	if err == nil {
		return
	}
	s.status, s.errText = OutcomeOf(err)
}

// OutcomeOf turns an error into the status and error text used by end events: nil is "ok", a cancelled context
// (Ctrl+C, SIGTERM) is "canceled", anything else is "error"; the text is kept for both failures.
func OutcomeOf(err error) (status, errText string) {
	switch {
	case err == nil:
		return "ok", ""
	case errors.Is(err, context.Canceled):
		return "canceled", err.Error()
	default:
		return "error", err.Error()
	}
}

// ID is the scope ID: the run ID for a run, the task ID for a task. Use it to record an event for this scope from code that has no Scope.
func (s *Scope) ID() string { return s.id }

// End records the end event with the final status and the time since the start. Call it once, with defer.
func (s *Scope) End() {
	ms := time.Since(s.start).Milliseconds()
	if s.endType == RunEnd {
		s.rec.Record(s.id, RunEnd, GetRunEndData(s.status, s.errText, ms))
		return
	}
	s.rec.Record(s.id, TaskEnd, GetTaskEndData(s.status, s.errText, ms))
}
