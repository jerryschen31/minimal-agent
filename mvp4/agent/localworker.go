package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// localWorker is the in-process worker: the agent's own ChatSession behind the worker.Worker interface.
// Autonomous mode uses it directly; -mode worker serves it over MCP (workerserver.go).
// [agent] It holds one conversation, the current task's: a new task ID clears the context first. That is
// all a one-task-at-a-time run needs; parallel tasks on one worker would need one ChatSession per task.
type localWorker struct {
	mu      sync.Mutex // one turn at a time (an MCP server may deliver calls concurrently)
	cs      *ChatSession
	rootCtx context.Context // for auto-compaction, which must outlive a cancelled turn (CLAUDE.md invariant)
	task    string          // the task the context belongs to; "" = none
}

var _ worker.Worker = (*localWorker)(nil)

func newLocalWorker(rootCtx context.Context, cs *ChatSession) *localWorker {
	cs.Name = "worker"
	return &localWorker{cs: cs, rootCtx: rootCtx}
}

func (w *localWorker) Info() worker.Info {
	info := worker.Info{Model: w.cs.Config.Model}
	for _, def := range w.cs.Tools.GetToolDefs() {
		info.Tools = append(info.Tools, def.Function.Name)
	}
	return info
}

func (w *localWorker) RunTurn(ctx context.Context, taskID, prompt string) (worker.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if taskID != w.task {
		resetContext(w.cs)
		w.task = taskID
	}
	res, err := runTurn(ctx, w.cs, prompt)
	if ctx.Err() != nil {
		return worker.Result{}, ctx.Err() // a cancelled turn adds nothing to memory; the caller stops
	}
	maybeStartAutoCompaction(w.rootCtx, w.cs)
	switch {
	case err == nil:
		return worker.Result{Status: worker.StatusCompleted, Reply: res.Reply, ToolsCalled: res.ToolsCalled}, nil
	case errors.Is(err, ErrMaxSteps):
		return worker.Result{Status: worker.StatusStepLimit, Error: err.Error()}, nil
	}
	return worker.Result{Status: worker.StatusError, Error: err.Error()}, nil
}

func (w *localWorker) EndTask(_ context.Context, taskID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if taskID == w.task {
		resetContext(w.cs)
		w.task = ""
	}
	return nil
}

func (w *localWorker) Close() error { return nil }
