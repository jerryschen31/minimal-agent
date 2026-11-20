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
	EventGateAnswered EventType = "gate_answered" // GateAnswered: a person answered a task's gate
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

// Manifest is the first event of every run: which plan it ran and what ran it (the "agent lockfile"). Two runs' manifests
// say what changed between them; config.json in the run folder says how.
type Manifest struct {
	PlanID     string `json:"plan_id,omitempty"` // the plan's "plan_id:" ("" for a hand-written plan): its identity
	PlanPath   string `json:"plan_path"`         // the plan's real path when the run started (the identity if no PlanID)
	PlanSHA256 string `json:"plan_sha256"`       // "sha256:<hex>" of the plan file's bytes (plan.md in the run folder)

	Harness Harness `json:"harness"`
	Host    string  `json:"host,omitempty"`
	PID     int     `json:"pid,omitempty"`
	WorkDir string  `json:"work_dir,omitempty"` // absolute

	Provider     string            `json:"provider,omitempty"`
	Model        string            `json:"model,omitempty"`
	BaseURL      string            `json:"base_url,omitempty"`      // as config.DisplayURL shows it (no user:password, no query)
	ConfigSHA256 string            `json:"config_sha256,omitempty"` // "sha256:<hex>" of config.json in the run folder
	Prompts      map[string]string `json:"prompts,omitempty"`       // role ("worker", "director", "summarizer") → "sha256:<hex>" of its prompt
	Tools        []string          `json:"tools,omitempty"`         // the model-facing tool names, sorted
	ToolsSHA256  string            `json:"tools_sha256,omitempty"`  // over every tool's name, description and parameters
	MCPServers   []MCPServer       `json:"mcp_servers,omitempty"`   // by name
	TraceRunID   string            `json:"trace_run_id,omitempty"`  // the run's trace (set from 1d)
}

// Harness is the minagent that ran a run.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"` // the build's git commit (+dirty); empty under `go run`
}

// MCPServer is one MCP server a run had: the config's name and transport, and what the server said it is.
type MCPServer struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Server    string `json:"server,omitempty"`  // the server's own name, from its initialize reply
	Version   string `json:"version,omitempty"` // the server's own version, from its initialize reply
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

// What a FileHashes hash is when the path holds no file's content.
const (
	HashAbsent = "absent" // nothing at the path
	HashDir    = "dir"    // a directory
)

// FileHashes records one path a file tool call changes: "sha256:<hex>" of the file, HashAbsent or HashDir. tool_before
// carries Before (the state before the call), tool_after carries After (the state after it returned).
type FileHashes struct {
	Path   string `json:"path"` // absolute, symlinks resolved
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// ToolBefore is EventToolBefore's data, written before a tool call runs.
type ToolBefore struct {
	CallID         string       `json:"call_id"`                   // unique in the run ("c1", "c2", …); tool_after names it
	ModelCallID    string       `json:"model_call_id,omitempty"`   // the model's own ID for the call (may repeat), for the trace
	Tool           string       `json:"tool"`                      // the model-facing name
	ArgsSHA256     string       `json:"args_sha256,omitempty"`     // of the exact arguments, so a re-send can be checked
	ArgsPreview    string       `json:"args_preview,omitempty"`    // redacted and cut, for a person to read
	IdempotencyKey string       `json:"idempotency_key,omitempty"` // MCP calls: sent in the request's _meta
	Idempotent     bool         `json:"idempotent,omitempty"`      // MCP calls: the server says the tool is idempotent (idempotentHint)
	Files          []FileHashes `json:"files,omitempty"`           // file tools only: Before of each path the call changes
}

// ToolAfter is EventToolAfter's data, written after a tool call returns.
type ToolAfter struct {
	CallID string       `json:"call_id"`
	Failed bool         `json:"failed,omitempty"`
	Files  []FileHashes `json:"files,omitempty"` // file tools only: After of each path in tool_before
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
	StopWaiting     StopReason = "waiting"     // a question (a gate, a cut-off call) with nobody to answer it
	StopTaskFailed  StopReason = "task_failed" // a task was judged not done on every attempt (maxTaskAttempts)
	StopDeclined    StopReason = "declined"    // a person answered no to a task's gate
)

// GateAnswered is EventGateAnswered's data.
type GateAnswered struct {
	Answer string `json:"answer"` // "yes" or "no"
}

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
