package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
	"github.com/jerryschen31/minimal-agent/mvp4/model"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- localWorker ----

// - Verify the status mapping (completed / step_limit / error), that a follow-up with the same task ID
// keeps the conversation, and that a new task ID or EndTask clears it
func Test_LocalWorker_StatusAndConversationPerTask(t *testing.T) {
	fx := newChatSessionFixture(t, 50, 45)
	fx.Tools.Register(echoTool{})
	lw := newLocalWorker(context.Background(), fx.Session)
	ctx := context.Background()

	fx.Provider.Script = []model.ChatMessage{
		call("c", "echo", "{}"), text("first"), text("second"), text("third"),
		call("c", "echo", "{}"), // the step-limit turn below
	}
	res, err := lw.RunTurn(ctx, "t1", "go")
	if err != nil || res.Status != worker.StatusCompleted || res.Reply != "first" || !slices.Equal(res.ToolsCalled, []string{"echo"}) {
		t.Fatalf("turn 1 = %+v, %v", res, err)
	}
	lw.RunTurn(ctx, "t1", "follow-up")
	if n := len(fx.Provider.Calls[2]); n < 5 { // system + turn 1 (4 messages) + the follow-up
		t.Errorf("follow-up request has %d messages; the task's conversation was lost", n)
	}
	lw.RunTurn(ctx, "t2", "new task")
	if n := len(fx.Provider.Calls[3]); n != 2 { // system + the new prompt
		t.Errorf("new task request has %d messages, want 2", n)
	}
	lw.EndTask(ctx, "t2")
	if n := len(fx.Context.GetMessages()); n != 0 {
		t.Errorf("context has %d messages after EndTask, want 0", n)
	}

	fx.Session.Config.MaxSteps = 1
	if res, err := lw.RunTurn(ctx, "t3", "loop"); err != nil || res.Status != worker.StatusStepLimit || res.ToolsCalled != nil {
		t.Errorf("step limit = %+v, %v", res, err)
	}
	fx.Provider.Err = errors.New("503 overloaded")
	if res, err := lw.RunTurn(ctx, "t3", "again"); err != nil || res.Status != worker.StatusError || !strings.Contains(res.Error, "503") {
		t.Errorf("provider error = %+v, %v", res, err)
	}
	if info := lw.Info(); !slices.Equal(info.Tools, []string{"echo"}) {
		t.Errorf("Info = %+v", info)
	}
}

// ---- MCP: -mode worker server + MCPWorker client, joined in memory ----

// connectMCPWorker serves lw over an in-memory MCP connection and returns the client side.
func connectMCPWorker(t *testing.T, lw *localWorker) *worker.MCPWorker {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newWorkerServer(lw, lw.cs.Config).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-orchestrator"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.NewMCPWorker(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// - Verify the worker protocol over MCP: worker_info, a completed turn, a follow-up that keeps the
// conversation, end_task, a step_limit status, and a bad call reported as an error
func Test_MCPWorker_RoundTrip(t *testing.T) {
	fx := newChatSessionFixture(t, 50, 45)
	fx.Session.Config.Model = "fake-worker"
	fx.Tools.Register(echoTool{})
	w := connectMCPWorker(t, newLocalWorker(context.Background(), fx.Session))
	ctx := context.Background()

	if info := w.Info(); info.Model != "fake-worker" || !slices.Equal(info.Tools, []string{"echo"}) {
		t.Errorf("Info = %+v", info)
	}
	fx.Provider.Script = []model.ChatMessage{
		call("c", "echo", "{}"), text("done it"), text("shown"),
		call("c", "echo", "{}"), // the step-limit turn below
	}
	res, err := w.RunTurn(ctx, "t1", "do it")
	if err != nil || res.Status != worker.StatusCompleted || res.Reply != "done it" || !slices.Equal(res.ToolsCalled, []string{"echo"}) {
		t.Fatalf("run_task = %+v, %v", res, err)
	}
	if res, err := w.RunTurn(ctx, "t1", "show me"); err != nil || res.Reply != "shown" || len(fx.Provider.Calls[2]) < 5 {
		t.Errorf("follow-up = %+v, %v; request had %d messages", res, err, len(fx.Provider.Calls[2]))
	}
	if err := w.EndTask(ctx, "t1"); err != nil || len(fx.Context.GetMessages()) != 0 {
		t.Errorf("end_task: %v; context has %d messages", err, len(fx.Context.GetMessages()))
	}

	fx.Session.Config.MaxSteps = 1
	if res, err := w.RunTurn(ctx, "t2", "loop"); err != nil || res.Status != worker.StatusStepLimit || !strings.Contains(res.Error, "max steps") {
		t.Errorf("step limit over MCP = %+v, %v", res, err)
	}
	if _, err := w.RunTurn(ctx, "t2", " "); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("empty prompt: err = %v", err)
	}
}

// - Verify a whole orchestrator-style run with the worker behind MCP: the director's view carries the
// worker's reply and status, and the report shows the worker ID
func Test_Orchestrator_RunOverMCP(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	w := connectMCPWorker(t, f.run.slots[0].w.(*localWorker))
	setWorkers(f.run, w)
	f.run.cfg.AgentMode = config.ModeOrchestrator
	f.director.Script = []model.ChatMessage{
		text("Do A"), call("d1", "task_done", `{"note":"A done"}`),
		text("Do B"), call("d2", "task_done", `{"note":"B done"}`),
		call("f", "finish", `{"success":true,"summary":"ok","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("A result"), text("B result")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonFinished || !rep.Plan.Success || rep.WorkerTurns != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if got := lastContent(f.director, 3); !strings.Contains(got, "Task t2: Task B") || !strings.Contains(got, "Status: completed") || !strings.Contains(got, "B result") {
		t.Errorf("director's view of task t2:\n%s", got)
	}
	if n := len(f.worker.Calls[1]); n != 2 { // a fresh conversation for t2: system + prompt
		t.Errorf("worker request for t2 has %d messages, want 2", n)
	}
	var out strings.Builder
	printReport(&out, rep)
	if !strings.Contains(out.String(), "t2 [done] Task B (1 worker turn(s), w1)") {
		t.Errorf("report:\n%s", out.String())
	}
}

// failingWorker is a worker whose turns can't run at all (like a worker process that has died).
type failingWorker struct{}

func (failingWorker) Info() worker.Info { return worker.Info{Model: "gone"} }
func (failingWorker) RunTurn(context.Context, string, string) (worker.Result, error) {
	return worker.Result{}, errors.New("run_task: connection closed")
}
func (failingWorker) EndTask(context.Context, string) error { return nil }
func (failingWorker) Close() error                          { return nil }

// - Verify a worker that can't be reached is an observation for the director (status error), not a crash
func Test_Autonomous_UnreachableWorker_IsObservation(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	setWorkers(f.run, failingWorker{})
	f.director.Script = []model.ChatMessage{
		text("Do A"),
		call("x", "task_failed", `{"reason":"worker gone"}`), call("y", "task_failed", `{"reason":"worker gone"}`),
		call("f", "finish", `{"success":false,"summary":"s","gaps":[]}`),
	}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonTaskFailed || rep.WorkerTurns != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if got := lastContent(f.director, 1); !strings.Contains(got, "Status: error") || !strings.Contains(got, "connection closed") {
		t.Errorf("director's view:\n%s", got)
	}
}
