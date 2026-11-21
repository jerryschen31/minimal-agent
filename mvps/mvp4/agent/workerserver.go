package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// -mode worker: the in-process worker served as an MCP server, for an orchestrator to drive.
// See PLAN-orchestrator-mode-serial.md § 3 for the three tools.

// newWorkerServer returns an MCP server with run_task, end_task and worker_info backed by lw.
// [agent] mcp.AddTool derives each tool's input and output JSON schema from the Go types, checks the
// arguments against it, and puts the returned value in the result's structuredContent.
func newWorkerServer(lw *localWorker, cfg config.Config) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: cfg.AgentName + "-worker", Version: cfg.AgentVersion}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: worker.ToolRunTask, Description: "Runs one turn of a task on the worker agent. The first call for a task_id starts a new conversation; later calls with the same task_id are follow-ups."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in worker.RunTaskArgs) (*mcp.CallToolResult, worker.Result, error) {
			if strings.TrimSpace(in.TaskID) == "" || strings.TrimSpace(in.Prompt) == "" {
				return nil, worker.Result{}, errors.New("task_id and prompt must not be empty")
			}
			res, err := lw.RunTurn(ctx, in.TaskID, in.Prompt)
			return nil, res, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: worker.ToolEndTask, Description: "Ends a task: the worker forgets its conversation."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in worker.EndTaskArgs) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, lw.EndTask(ctx, in.TaskID)
		})
	mcp.AddTool(s, &mcp.Tool{Name: worker.ToolInfo, Description: "Returns the worker's model and tool names."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, worker.Info, error) {
			return nil, lw.Info(), nil
		})
	return s
}

// serveWorker serves lw on stdin and the real stdout until the orchestrator closes the connection
// (end of input) or ctx is cancelled (SIGTERM).
func serveWorker(ctx context.Context, lw *localWorker, cfg config.Config) error {
	t := &mcp.IOTransport{Reader: io.NopCloser(cfg.InBuffer), Writer: cfg.ProtocolOut}
	err := newWorkerServer(lw, cfg).Run(ctx, t)
	if errors.Is(err, io.EOF) {
		return nil // the orchestrator closed our stdin: a normal end
	}
	return err
}
