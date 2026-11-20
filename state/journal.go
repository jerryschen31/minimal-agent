package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// FormatVersion is the "v" of every event this package writes.
const FormatVersion = 1

// EventType names what an event records.
type EventType string

// The event types. Each run's journal starts with EventManifest; the rest are written as the run goes.
const (
	EventManifest     EventType = "manifest"      // Manifest: which plan this run is, and what ran it
	EventTaskStarted  EventType = "task_started"  // TaskStarted
	EventTaskFinished EventType = "task_finished" // TaskFinished
	EventToolBefore   EventType = "tool_before"   // ToolBefore: written before a tool call runs
	EventToolAfter    EventType = "tool_after"    // ToolAfter: written after it returns
	EventResumed      EventType = "resumed"       // Resumed: a later process took up this run after it was cut off
	EventRunEnded     EventType = "run_ended"     // RunEnded: the run is over, and why
)

// Event is one line of a journal.
type Event struct {
	V    int             `json:"v"`
	Seq  int             `json:"seq"` // 1, 2, 3, … through the run, continuing across a resume
	At   time.Time       `json:"at"`
	Type EventType       `json:"type"`
	Task string          `json:"task,omitempty"` // the task ID, for task and tool events
	Data json.RawMessage `json:"data,omitempty"`
}

// Manifest is the first event of every run: which plan it runs. Step 1c adds what ran it (model, prompts, tools).
type Manifest struct {
	PlanPath   string `json:"plan_path"`   // the plan's real path (PlanKey), how Load finds a plan's runs
	PlanSHA256 string `json:"plan_sha256"` // "sha256:<hex>" of the plan file's bytes (plan.md in the run folder)
}

// TaskStatus is a task's state in the journal.
type TaskStatus string

const (
	TaskRunning     TaskStatus = "running"     // started, not finished: in an un-ended run, the task that was cut off
	TaskDone        TaskStatus = "done"        // finished and passed
	TaskFailed      TaskStatus = "failed"      // finished and failed
	TaskInterrupted TaskStatus = "interrupted" // its run ended (run_ended) while it was running
)

// TaskStarted is EventTaskStarted's data.
type TaskStarted struct {
	Attempt int `json:"attempt"` // 1 for the first try in this run
}

// TaskFinished is EventTaskFinished's data.
type TaskFinished struct {
	Status  TaskStatus `json:"status"`             // TaskDone or TaskFailed
	DefHash string     `json:"def_hash,omitempty"` // plan.Task.DefinitionHash() of the task that ran
	Summary string     `json:"summary,omitempty"`  // what the worker reported
	Verdict string     `json:"verdict,omitempty"`  // what the director decided, and why
}

// FileHashes records one file a tool call changes: its hash before the call and the hash it will have after
// ("" = the file doesn't exist / not known in advance).
type FileHashes struct {
	Path   string `json:"path"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// ToolBefore is EventToolBefore's data, written before a tool call runs.
type ToolBefore struct {
	CallID         string       `json:"call_id"`
	Tool           string       `json:"tool"`
	ArgsSHA256     string       `json:"args_sha256,omitempty"`
	ArgsPreview    string       `json:"args_preview,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	Files          []FileHashes `json:"files,omitempty"` // file tools only
}

// ToolAfter is EventToolAfter's data, written after a tool call returns.
type ToolAfter struct {
	CallID string `json:"call_id"`
	Failed bool   `json:"failed,omitempty"`
}

// Resumed is EventResumed's data.
type Resumed struct {
	PID int `json:"pid"`
}

// StopReason says why a run ended.
type StopReason string

const (
	StopDone        StopReason = "done"
	StopMaxSteps    StopReason = "max_steps"
	StopBudget      StopReason = "budget"
	StopTimeout     StopReason = "timeout"
	StopInterrupted StopReason = "interrupted"
	StopError       StopReason = "error"
	StopWaiting     StopReason = "waiting" // a question (a gate, a cut-off call) with nobody to answer it
)

// RunEnded is EventRunEnded's data.
type RunEnded struct {
	Reason StopReason `json:"reason"`
	Error  string     `json:"error,omitempty"`
}

// Journal appends events to one run's journal file. Safe for concurrent use.
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	seq  int    // the last seq written
	path string // for error messages
	now  func() time.Time
}

// Append writes one event (the next seq, now, typ, task and data as JSON) as one line, and syncs it to disk.
//
// Notes:
//   - When it returns nil the event is on disk: a crash or power cut after that can't lose it. The line is written with
//     one Write call, so a crash during it leaves at most a torn last line, which Read drops.
//   - data may be nil (no "data" key). A data value that can't be marshalled is an error and nothing is written.
//   - After a write or sync error the journal may end in a torn line; the caller should stop the run.
func (j *Journal) Append(typ EventType, task string, data any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return fmt.Errorf("journal %s is closed", j.path)
	}
	e := Event{V: FormatVersion, Seq: j.seq + 1, At: j.now().UTC(), Type: typ, Task: task}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("journal %s: %s event: %w", j.path, typ, err)
		}
		e.Data = raw
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("journal %s: %w", j.path, err)
	}
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("journal %s: %w", j.path, err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("journal %s: %w", j.path, err)
	}
	j.seq = e.Seq
	return nil
}

// Seq returns the seq of the last event written (0 for none).
//
// Notes: none.
func (j *Journal) Seq() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}

// Close closes the journal file; Append fails after it. A second Close returns nil.
//
// Notes: none.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// Read reads a journal's events. tornTail is true when the last line has no newline (a crash cut it): it is dropped.
//
// Notes:
//   - Blank lines are skipped. A line that is not a valid event, anywhere but the unterminated end, is an error naming
//     its line number: that is damage, not a crash.
//   - Lines may be any length (no bufio.Scanner limit).
func Read(r io.Reader) (events []Event, tornTail bool, err error) {
	events, _, tornTail, err = readEvents(r)
	return events, tornTail, err
}

// readEvents is Read that also returns goodLen, the byte length of the journal up to the end of its last complete line
// (what ContinueRun truncates to).
//
// Notes: none.
func readEvents(r io.Reader) (events []Event, goodLen int64, tornTail bool, err error) {
	br := bufio.NewReader(r)
	for lineNum := 1; ; lineNum++ {
		line, readErr := br.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, 0, false, readErr
		}
		if errors.Is(readErr, io.EOF) { // no newline: the end of the file
			return events, goodLen, len(bytes.TrimSpace(line)) > 0, nil
		}
		goodLen += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, 0, false, fmt.Errorf("line %d: not a journal event: %w", lineNum, err)
		}
		events = append(events, e)
	}
}

// decodeData decodes an event's data into v.
//
// Notes:
//   - Missing data decodes as the zero value.
func decodeData(e Event, v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("seq %d (%s): bad data: %w", e.Seq, e.Type, err)
	}
	return nil
}
