// Package worker defines what the run loop needs from a worker agent: run one turn of a task, end a
// task, describe itself. The in-process worker (agent.localWorker) and the MCP client to a `-mode worker`
// child process (MCPWorker) both satisfy it. It is a capability package: it never imports agent.
// See PLAN-orchestrator-mode-serial.md.
package worker

import "context"

// Status is how a worker turn ended. It is set by Go code, never by the worker LLM
// (DECISIONS.md § Step 2 MVP).
type Status string

const (
	StatusCompleted Status = "completed"  // the turn ended normally with a reply
	StatusStepLimit Status = "step_limit" // the worker hit its maxSteps limit
	StatusError     Status = "error"      // anything else: an LLM error, a failed MCP call, the call timeout
)

// DefaultType is the only worker type for now (named worker types are deferred).
const DefaultType = "general"

// Result is one worker turn's outcome. It is also run_task's JSON result.
type Result struct {
	Status      Status   `json:"status"`
	Reply       string   `json:"reply"`
	Error       string   `json:"error,omitempty"`
	ToolsCalled []string `json:"tools_called"` // nil when the turn failed: unknown, not "none"
}

// Info describes a worker for the director's prompt. It is also worker_info's JSON result.
type Info struct {
	Model string   `json:"model"`
	Tools []string `json:"tools"`
}

// Worker runs tasks for the director. A task's turns share one conversation: the first RunTurn for a
// new taskID starts it empty, later ones are follow-ups, and EndTask drops it.
type Worker interface {
	Info() Info
	// RunTurn runs one turn. The error is only for "couldn't run the turn at all" (cancelled, connection
	// lost); a turn that ran and failed is a Result with StatusStepLimit or StatusError.
	RunTurn(ctx context.Context, taskID, prompt string) (Result, error)
	EndTask(ctx context.Context, taskID string) error
	Close() error
}
