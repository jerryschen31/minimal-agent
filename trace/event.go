package trace

import "time"

// Version is the event format version written into an event's EventData field.
const Version = 1

type EventType string

const (
	RunStart      EventType = "run_start"
	TaskStart     EventType = "task_start"
	ModelRequest  EventType = "model_request"
	ModelResponse EventType = "model_response"
	ModelError    EventType = "model_error"
	ToolRequested EventType = "tool_requested"
	ToolFinished  EventType = "tool_finished"
	Compaction    EventType = "compaction"
	TaskEnd       EventType = "task_end"
	RunEnd        EventType = "run_end"
	// TODO: add more event types as needed
)

// Event represents a single event in the agent trace. Every agent event is recorded using this structure.
type Event struct {
	TimeStamp  time.Time `json:"ts"`
	RunID      string    `json:"run_id"`
	TaskID     string    `json:"task_id,omitempty"`
	Counter    uint64    `json:"counter"`
	EventType  EventType `json:"event_type"`
	EventData  any       `json:"event_data"`
	Redactions []string  `json:"redactions,omitempty"`
}

// RunStartData is the EventData for a RunStart event.
type RunStartData struct {
	TraceVersion int    `json:"trace_version"` // set from Version; the format of every event in this file
	AgentName    string `json:"agent_name"`
	AgentVersion string `json:"agent_version"`
	GitCommit    string `json:"git_commit,omitempty"` // runtime/debug.ReadBuildInfo -> vcs.revision; empty for `go run`
	PID          int    `json:"pid"`                  // os.Getpid
	ParentPID    int    `json:"parent_pid"`           // os.Getppid: the launching process (usually a shell); not a reliable agent-to-agent link
	Hostname     string `json:"hostname,omitempty"`   // os.Hostname; on error leave empty, tracing must not fail a run
	Mode         string `json:"mode"`                 // cfg.AgentMode: chat | headless | oneshot
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	WorkDir      string `json:"work_dir"`

	MaxSteps     int               `json:"max_steps"`
	MaxTokens    int               `json:"max_tokens"`              // 0 = unspecified
	SystemPrompt string            `json:"system_prompt,omitempty"` // full text; goes through redaction like any other string
	McpServers   map[string]string `json:"mcp_servers,omitempty"`   // server name -> type only; never command, args, env or headers (tokens)
}

// MessageRecord is one message as it was sent to the model.
type MessageRecord struct {
	ID         string           `json:"id,omitempty"`
	Role       string           `json:"role"`
	Type       string           `json:"type,omitempty"`
	Content    string           `json:"content,omitempty"`
	Reasoning  string           `json:"reasoning,omitempty"` // new: thinking / summary blocks, joined
	ToolCalls  []ToolCallRecord `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// ModelRequestData records new messages in a model request and the tools available at this step.
type ModelRequestData struct {
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	Step      int             `json:"step,omitempty"`
	Messages  []MessageRecord `json:"messages"`
	ToolNames []string        `json:"tool_names,omitempty"`
}

// ToolCallRecord is one tool call the model asked for, as it appears in a ModelResponseData.
type ToolCallRecord struct {
	CallID    string `json:"call_id"` // ties this to the tool_requested / tool_finished events
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // a string, not json.RawMessage: models sometimes emit invalid JSON
}

// Usage is token accounting for one model call. Zero means the provider did not report it.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ModelResponseData is the EventData for a ModelResponse event: everything the model produced in one call.
type ModelResponseData struct {
	Message    MessageRecord `json:"message"`
	StopReason string        `json:"stop_reason,omitempty"`
	Usage      Usage         `json:"usage"`
	DurationMS int64         `json:"duration_ms"`
}

// ModelErrorData is the EventData for a ModelError event: a model call that produced no response.
type ModelErrorData struct {
	Error      string `json:"error"`                 // the error text; passes through redaction (URLs can carry keys)
	HTTPStatus int    `json:"http_status,omitempty"` // 0 when there was no HTTP response (network failure, timeout)
	Canceled   bool   `json:"canceled,omitempty"`    // the context was cancelled (Ctrl+C), not a provider failure
	DurationMS int64  `json:"duration_ms"`           // time spent before the call failed
}
