package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP tools a `-mode worker` process serves (PLAN-orchestrator-mode-serial.md § 3).
const (
	ToolRunTask = "run_task"
	ToolEndTask = "end_task"
	ToolInfo    = "worker_info"
)

// RunTaskArgs are run_task's arguments.
type RunTaskArgs struct {
	TaskID string `json:"task_id" jsonschema:"the task this turn belongs to; a new id starts a new conversation"`
	Prompt string `json:"prompt" jsonschema:"the prompt for the worker agent"`
}

// EndTaskArgs are end_task's arguments.
type EndTaskArgs struct {
	TaskID string `json:"task_id" jsonschema:"the task that is over"`
}

// CallTimeout bounds each MCP call to a worker. The run's -timeout still applies on top.
const CallTimeout = 10 * time.Minute

// MCPWorker is a worker reached over MCP: in orchestrator mode, a `-mode worker` child process.
type MCPWorker struct {
	session *mcp.ClientSession
	info    Info
}

var _ Worker = (*MCPWorker)(nil)

// StartMCPWorker starts this program again as `-mode worker` and connects to it over stdio.
// [agent] The child reads the same config file, so the top-level config is the worker's (DECISIONS.md
// § Step 2 MVP: no third config block). It inherits the environment (API keys); its stderr goes to stderr
// (our own, or a writer that labels its lines), so its [worker] lines show in the same terminal. Its
// stdout carries only MCP messages.
func StartMCPWorker(ctx context.Context, cfg config.Config, stderr io.Writer) (*MCPWorker, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding this program to start the worker: %w", err)
	}
	cmd := exec.Command(exe, childArgs(cfg)...)
	cmd.Stderr = stderr
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.AgentName + "-orchestrator", Version: cfg.AgentVersion}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("starting the worker process (%s %s): %w", exe, strings.Join(cmd.Args[1:], " "), err)
	}
	w, err := NewMCPWorker(ctx, session)
	if err != nil {
		session.Close()
		return nil, err
	}
	return w, nil
}

// childArgs are the worker process's flags. The model and step limit come from the merged config, so a
// -model or -maxsteps flag given to the orchestrator reaches the worker too.
func childArgs(cfg config.Config) []string {
	args := []string{"-mode", config.ModeWorker, "-workdir", cfg.WorkDir, "-model", cfg.Model, "-maxsteps", strconv.Itoa(cfg.MaxSteps)}
	if cfg.ConfigFile != "" {
		args = append(args, "-config", cfg.ConfigFile)
		if cfg.WorkerConfigFile != "" {
			args = append(args, "-workerconfig", cfg.WorkerConfigFile) // this worker's type, over the main config
		}
	}
	if cfg.Debug {
		args = append(args, "-debug")
	}
	return args
}

// NewMCPWorker wraps a connected MCP session and asks the worker for its model and tools.
func NewMCPWorker(ctx context.Context, session *mcp.ClientSession) (*MCPWorker, error) {
	w := &MCPWorker{session: session}
	if err := w.call(ctx, ToolInfo, struct{}{}, &w.info); err != nil {
		return nil, fmt.Errorf("asking the worker for its info: %w", err)
	}
	return w, nil
}

func (w *MCPWorker) Info() Info { return w.info }

func (w *MCPWorker) RunTurn(ctx context.Context, taskID, prompt string) (Result, error) {
	var res Result
	err := w.call(ctx, ToolRunTask, RunTaskArgs{TaskID: taskID, Prompt: prompt}, &res)
	return res, err
}

func (w *MCPWorker) EndTask(ctx context.Context, taskID string) error {
	return w.call(ctx, ToolEndTask, EndTaskArgs{TaskID: taskID}, nil)
}

// Close ends the session; for a child process that closes its stdin, so it exits (it is killed if it doesn't).
func (w *MCPWorker) Close() error { return w.session.Close() }

// call calls one worker tool with CallTimeout and decodes its structured result into out (if not nil).
func (w *MCPWorker) call(ctx context.Context, tool string, args, out any) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	res, err := w.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			return fmt.Errorf("%s: no reply within %s", tool, CallTimeout)
		}
		return fmt.Errorf("%s: %w", tool, err)
	}
	if res.IsError {
		var msg []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg = append(msg, tc.Text)
			}
		}
		return fmt.Errorf("%s: %s", tool, strings.Join(msg, " "))
	}
	if out == nil {
		return nil
	}
	// [agent] the client decodes structuredContent into a generic map; a JSON round trip turns it into out
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: unexpected result %s: %w", tool, raw, err)
	}
	return nil
}
