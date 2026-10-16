package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
	"github.com/jerryschen31/minimal-agent/mvp4/contextwindow"
	"github.com/jerryschen31/minimal-agent/mvp4/director"
	"github.com/jerryschen31/minimal-agent/mvp4/memory"
	"github.com/jerryschen31/minimal-agent/mvp4/model"
	"github.com/jerryschen31/minimal-agent/mvp4/model/modeltest"
	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/tools"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// ---- runTurn and tools.ErrEndTurn ----

// endTool ends the turn when called (like the director's control tools).
type endTool struct{}

func (endTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("stop", "ends the turn", nil)
}
func (endTool) CallTool(context.Context, json.RawMessage) (string, error) {
	return "stopped", tools.ErrEndTurn
}

// echoTool returns its name, so a turn can call a tool and carry on.
type echoTool struct{}

func (echoTool) GetToolDefinition() model.ToolDef                          { return model.NewToolDef("echo", "echoes", nil) }
func (echoTool) CallTool(context.Context, json.RawMessage) (string, error) { return "echoed", nil }

// - Verify runTurn returns the final reply and the tools called, and commits the turn
func Test_RunTurn_ReturnsReplyAndToolsCalled(t *testing.T) {
	fx := newChatSessionFixture(t, 20, 19)
	fx.Tools.Register(echoTool{})
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("c1", "echo", "{}"),
		modeltest.AssistantText("all done"),
	}
	res, err := runTurn(context.Background(), fx.Session, "go")
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if res.Reply != "all done" || len(res.ToolsCalled) != 1 || res.ToolsCalled[0] != "echo" || res.EndedByTool {
		t.Errorf("result = %+v", res)
	}
	if n := len(fx.Context.GetMessages()); n != 4 { // user, assistant(tool call), tool, assistant
		t.Errorf("context has %d messages, want 4", n)
	}
}

// - Verify a tool returning ErrEndTurn ends the turn with no further model call, records the tool
// result, and skips later tool calls in the same reply
func Test_RunTurn_EndTurnTool_StopsWithoutAnotherModelCall(t *testing.T) {
	fx := newChatSessionFixture(t, 20, 19)
	fx.Tools.Register(endTool{})
	fx.Tools.Register(echoTool{})
	reply := modeltest.AssistantToolCall("c1", "stop", "{}")
	reply.ToolCalls = append(reply.ToolCalls, model.ToolCall{ID: "c2", Type: "function", Function: model.ToolCallFunc{Name: "echo", Arguments: "{}"}})
	fx.Provider.Script = []model.ChatMessage{reply} // a second Chat call would fail: script exhausted

	res, err := runTurn(context.Background(), fx.Session, "go")
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if !res.EndedByTool || len(fx.Provider.Calls) != 1 {
		t.Errorf("EndedByTool=%v, Chat calls=%d; want true, 1", res.EndedByTool, len(fx.Provider.Calls))
	}
	msgs := fx.Context.GetMessages()
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || last.Content != "stopped" || strings.Contains(fx.Out.String(), "Tool called: echo") {
		t.Errorf("last message %+v; output:\n%s", last, fx.Out.String())
	}
}

// ---- autonomous run ----

type autoFixture struct {
	run      *autonomousRun
	worker   *modeltest.FakeProvider
	director *modeltest.FakeProvider
	fx       *chatSessionFixture
}

// newAutoFixture builds an autonomous run over the shared chat fixture (as the worker) and a second
// fake provider (as the director), without reading files or API keys.
func newAutoFixture(t *testing.T, objective string, maxTurnsPerTask int) *autoFixture {
	t.Helper()
	fx := newChatSessionFixture(t, 50, 45)
	obj, err := director.ParseObjective([]byte(objective), config.GetDefaultDirectorConfig().MaxTasks)
	if err != nil {
		t.Fatalf("ParseObjective: %v", err)
	}
	cfg := fx.Session.Config
	cfg.AgentMode = config.ModeAutonomous
	cfg.ObjectiveFile = "objective.md"
	cfg.Director = config.DirectorConfig{Model: "fake-director", MaxSteps: 10, MaxTurnsPerTask: maxTurnsPerTask, MaxTasks: 20, MaxIterations: 1}

	dprov := &modeltest.FakeProvider{}
	hist, _ := memory.NewInMemoryChatHistory()
	dctx, _ := contextwindow.NewChatContext(contextwindow.NewOffsetWindow(50), 45)
	reg, _ := tools.NewToolRegistry(nil)
	dir, err := NewChatSession(dprov, hist, dctx, reg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir.Name = "director"
	fileTools, err := tools.SetupBuiltinTools(directorFileTools, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lw := newLocalWorker(context.Background(), fx.Session)
	run := &autonomousRun{
		workers: []worker.Worker{lw}, workerInfo: lw.Info(), dir: dir, objective: obj,
		plan: director.NewPlan(obj.Tasks, config.GetDefaultDirectorConfig().MaxTasks), fileTools: fileTools, cfg: cfg, out: fx.Out,
		rootCtx: context.Background(),
	}
	return &autoFixture{run: run, worker: fx.Provider, director: dprov, fx: fx}
}

const twoTasks = "# Objective\nDo A and B.\n\n## Tasks\n- Task A\n- Task B\n"

func call(id, name, args string) model.ChatMessage {
	return modeltest.AssistantToolCall(id, name, args)
}
func text(s string) model.ChatMessage { return modeltest.AssistantText(s) }

// lastContent returns the content of the last message of the n-th Chat call.
func lastContent(p *modeltest.FakeProvider, n int) string {
	msgs := p.Calls[n]
	return msgs[len(msgs)-1].Content
}

// - Verify the happy path with given tasks: one worker turn per task, notes carried in the system
// prompt, contexts cleared between tasks, no extra director call after control tools, success report
func Test_Autonomous_GivenTasks_HappyPath(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Script = []model.ChatMessage{
		text("Do A"), call("d1", "task_done", `{"note":"A is in a.txt"}`),
		text("Do B using a.txt"), call("d2", "task_done", `{"note":"B done"}`),
		call("f", "finish", `{"success":true,"summary":"all good","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("A result"), text("B result")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonFinished || !rep.Plan.Success || rep.WorkerTurns != 2 || !rep.Reviewed || rep.runError() != nil {
		t.Fatalf("report = %+v, runError = %v", rep, rep.runError())
	}
	if len(f.director.Calls) != 5 {
		t.Errorf("director Chat calls = %d, want 5 (no acknowledgement calls)", len(f.director.Calls))
	}
	if got := lastContent(f.director, 1); !strings.Contains(got, "A result") || !strings.Contains(got, "Task t1: Task A") || !strings.Contains(got, "Status: completed") {
		t.Errorf("director's view of the worker turn:\n%s", got)
	}
	if sys := f.director.Calls[2][0].Content; !strings.Contains(sys, "note: A is in a.txt") || !strings.Contains(sys, "- t2 [running] Task B") {
		t.Errorf("system prompt for task 2 lacks the note/current task:\n%s", sys)
	}
	if n := len(f.director.Calls[2]); n != 2 { // system + task-start prompt: task 1's messages were cleared
		t.Errorf("director request for task 2 has %d messages, want 2", n)
	}
	for _, m := range f.worker.Calls[1] {
		if strings.Contains(m.Content, "Do A") {
			t.Errorf("worker's task 2 request still contains task 1's prompt: %+v", f.worker.Calls[1])
		}
	}
	toolNames := func(call int) string {
		var n []string
		for _, d := range f.director.ToolDefs[call] {
			n = append(n, d.Function.Name)
		}
		return strings.Join(n, ",")
	}
	for call, want := range map[int]string{
		0: "",                                         // writing a task's first prompt: no tools, so the answer must be text
		1: "list_dir,read_file,task_done,task_failed", // judging a worker turn
		4: "finish,list_dir,read_file",                // review
	} {
		if got := toolNames(call); got != want {
			t.Errorf("director call %d offered tools %q, want %q", call, got, want)
		}
	}
}

// - Verify the PLAN phase: a text reply gets a nudge, then set_tasks creates the task list
func Test_Autonomous_NoTasks_PlanPhase(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nDo X.\n", 5)
	f.director.Script = []model.ChatMessage{
		text("I will plan now."), call("p", "set_tasks", `{"tasks":[{"id":"x","task":"Task X"}]}`),
		text("Do X"), call("d", "task_done", `{"note":"ok"}`),
		call("f", "finish", `{"success":true,"summary":"done","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("X result")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonFinished || len(rep.Plan.Tasks.Tasks) != 1 || rep.Plan.Tasks.Tasks[0].Text != "Task X" {
		t.Fatalf("report = %+v", rep)
	}
	if got := lastContent(f.director, 1); got != director.NudgePlan {
		t.Errorf("second director call = %q, want the plan nudge", got)
	}
	if !strings.Contains(f.fx.Out.String(), "plan: 1 tasks") {
		t.Errorf("plan not printed:\n%s", f.fx.Out.String())
	}
}

// - Verify a follow-up within a task keeps the worker's context for that task
func Test_Autonomous_FollowUp_KeepsWorkerContextWithinTask(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.director.Script = []model.ChatMessage{
		text("Do A"), text("Now show me the file"), call("d", "task_done", `{"note":"ok"}`),
		call("f", "finish", `{"success":true,"summary":"s","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("r1"), text("r2")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonFinished || rep.WorkerTurns != 2 {
		t.Fatalf("report = %+v", rep)
	}
	var sawFirst bool
	for _, m := range f.worker.Calls[1] {
		sawFirst = sawFirst || m.Content == "r1"
	}
	if !sawFirst {
		t.Errorf("follow-up request lost the first reply: %+v", f.worker.Calls[1])
	}
}

// - Verify task_failed stops the work, the review still runs, and the run counts as not met
func Test_Autonomous_TaskFailed_ReviewStillRuns(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Script = []model.ChatMessage{
		text("Do A"),
		call("x", "task_failed", `{"reason":"file missing"}`), // refused once: the worker has turns left
		call("y", "task_failed", `{"reason":"file missing"}`), // confirmed in the same director turn
		call("f", "finish", `{"success":false,"summary":"stopped at A","gaps":["A and B not done"]}`),
	}
	f.worker.Script = []model.ChatMessage{text("cannot find it")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonTaskFailed || !rep.Reviewed || rep.Plan.Tasks.Get("t2").Status != tasks.Pending {
		t.Fatalf("report = %+v", rep)
	}
	if got := lastContent(f.director, 2); !strings.Contains(got, "not recorded") || !strings.Contains(got, "4 turn(s)") {
		t.Errorf("director's view of the first task_failed:\n%s", got)
	}
	if err := rep.runError(); !errors.Is(err, ErrObjectiveNotMet) {
		t.Errorf("runError = %v", err)
	}
}

// - Verify the task_failed guard: with worker turns left, a director that calls task_failed and then replies
// with text sends the worker back instead of stopping the run (the 2026-10-14 real-run failure)
func Test_Autonomous_TaskFailed_RefusedWhileTurnsLeft(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.director.Script = []model.ChatMessage{
		text("Do A"),
		call("x", "task_failed", `{"reason":"needs a fix"}`), // refused
		text("Fix the Pos method"),                           // the follow-up the director meant to send
		call("d", "task_done", `{"note":"fixed"}`),
		call("f", "finish", `{"success":true,"summary":"ok","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("wrote it"), text("fixed it")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonFinished || rep.Plan.Tasks.Get("t1").Status != tasks.Done || len(f.worker.Calls) != 2 {
		t.Fatalf("report = %+v, worker calls = %d", rep, len(f.worker.Calls))
	}
}

// - Verify the per-task turn limit: one reminder, then the code fails the task itself
func Test_Autonomous_TurnLimit_FailsTask(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 1)
	f.director.Script = []model.ChatMessage{
		text("Do A"), text("try again"), text("please try again"),
		call("f", "finish", `{"success":false,"summary":"limit","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("nope")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonTaskFailed || !strings.Contains(rep.Plan.Tasks.Get("t1").Note, "turn limit") || len(f.worker.Calls) != 1 {
		t.Fatalf("report = %+v, worker calls = %d", rep, len(f.worker.Calls))
	}
	if got := lastContent(f.director, 2); got != director.NudgeLimit {
		t.Errorf("third director call = %q, want the limit reminder", got)
	}
}

// - Verify a failed worker turn (here ErrMaxSteps) reaches the director as an observation
func Test_Autonomous_WorkerError_IsObservation(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.fx.Session.Config.MaxSteps = 1
	f.director.Script = []model.ChatMessage{
		text("Do A"),
		call("x", "task_failed", `{"reason":"worker keeps failing"}`), call("y", "task_failed", `{"reason":"worker keeps failing"}`),
		call("f", "finish", `{"success":false,"summary":"s","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{call("w", "missing_tool", "{}")} // one step, still calling tools

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonTaskFailed {
		t.Fatalf("report = %+v", rep)
	}
	if got := lastContent(f.director, 1); !strings.Contains(got, "Status: step_limit") || !strings.Contains(got, "max steps") {
		t.Errorf("director's view of the failed turn:\n%s", got)
	}
}

// - Verify cancelling mid worker turn reports "interrupted", skips the review, and adds nothing to the worker's memory
func Test_Autonomous_Interrupt_NoReview(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Script = []model.ChatMessage{text("Do A")}
	f.worker.Gate = make(chan struct{})
	f.worker.Waiting = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan RunReport)
	go func() { done <- f.run.run(ctx) }()
	<-f.worker.Waiting
	cancel()
	rep := <-done

	if rep.Reason != ReasonInterrupted || rep.Reviewed || len(f.director.Calls) != 1 {
		t.Fatalf("report = %+v, director calls = %d", rep, len(f.director.Calls))
	}
	if !errors.Is(rep.runError(), ErrRunInterrupted) {
		t.Errorf("runError = %v", rep.runError())
	}
	if n := len(f.fx.History.GetMessages()); n != 0 {
		t.Errorf("worker history has %d messages after an interrupted turn, want 0", n)
	}
}

// - Verify the run's deadline is reported as "timeout"
func Test_Autonomous_Timeout(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Script = []model.ChatMessage{text("Do A")}
	f.worker.Gate = make(chan struct{}) // never opens: the worker "thinks" until the deadline

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rep := f.run.run(ctx)

	if rep.Reason != ReasonTimeout {
		t.Fatalf("report = %+v", rep)
	}
}

// - Verify a director provider error ends the run with "error" before any worker turn
func Test_Autonomous_DirectorError_EndsRun(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Err = errors.New("401 unauthorized")

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonError || len(f.worker.Calls) != 0 || !strings.Contains(rep.Err.Error(), "401") {
		t.Fatalf("report = %+v", rep)
	}
}

// - Verify empty director replies get reminders, then end the run with an error
func Test_Autonomous_EmptyDirectorReplies_EndRun(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Script = []model.ChatMessage{text(""), text("  "), text("")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonError || !strings.Contains(rep.Err.Error(), "no worker prompt") || len(f.worker.Calls) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

// - Verify a failing best-effort review after a failed task keeps reason task_failed and prints "no review"
func Test_Autonomous_TaskFailed_ReviewFails_StillReported(t *testing.T) {
	f := newAutoFixture(t, twoTasks, 5)
	f.director.Script = []model.ChatMessage{text("Do A"), call("x", "task_failed", `{"reason":"down"}`), call("y", "task_failed", `{"reason":"down"}`)} // review call exhausts the script
	f.worker.Script = []model.ChatMessage{text("error")}

	rep := f.run.run(context.Background())
	if rep.Reason != ReasonTaskFailed || rep.Reviewed || rep.Err == nil {
		t.Fatalf("report = %+v", rep)
	}
	var out strings.Builder
	printReport(&out, rep)
	if !strings.Contains(out.String(), "Summary:  no review") || !strings.Contains(out.String(), "t1 [failed] Task A (1 worker turn(s), w1)") {
		t.Errorf("report:\n%s", out.String())
	}
}

// - Verify the director's text is never treated as a slash command by the worker
func Test_Autonomous_SlashTextGoesToWorkerAsPrompt(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.director.Script = []model.ChatMessage{
		text("/clear"), call("d", "task_done", `{"note":"ok"}`),
		call("f", "finish", `{"success":true,"summary":"s","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("ok")}

	f.run.run(context.Background())
	if len(f.worker.Calls) != 1 || lastContent(f.worker, 0) != "/clear" {
		t.Errorf("worker calls = %+v", f.worker.Calls)
	}
}

// ---- rounds (director.maxIterations) ----

// - Verify a review with gaps starts a new round: the gaps reach the plan prompt, the earlier round stays in
// the system prompt, the review is told rounds are left, and success in round 2 makes the run a success
func Test_Autonomous_Iterations_GapsBecomeNextRound(t *testing.T) {
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.run.cfg.Director.MaxIterations = 3
	f.director.Script = []model.ChatMessage{
		text("Do A"), call("d1", "task_done", `{"note":"A is in a.go"}`),
		call("f1", "finish", `{"success":false,"summary":"A works, tests missing","gaps":["no tests for A"]}`),
		call("p", "set_tasks", `{"tasks":[{"id":"t1","task":"Add tests for A"}]}`),
		text("Write a_test.go"), call("d2", "task_done", `{"note":"tests pass"}`),
		call("f2", "finish", `{"success":true,"summary":"done","gaps":[]}`),
	}
	f.worker.Script = []model.ChatMessage{text("A written"), text("tests written")}

	rep := f.run.run(context.Background())

	if rep.Reason != ReasonFinished || !rep.Plan.Success || len(rep.Earlier) != 1 || rep.WorkerTurns != 2 || rep.runError() != nil {
		t.Fatalf("report = %+v, runError = %v", rep, rep.runError())
	}
	if got := lastContent(f.director, 2); !strings.Contains(got, "2 more round(s)") {
		t.Errorf("round 1 review prompt doesn't mention the rounds left:\n%s", got)
	}
	if got := lastContent(f.director, 3); !strings.Contains(got, "Round 2 of 3") || !strings.Contains(got, "- no tests for A") {
		t.Errorf("round 2 plan prompt:\n%s", got)
	}
	if sys := f.director.Calls[3][0].Content; !strings.Contains(sys, "## Earlier rounds") || !strings.Contains(sys, "note: A is in a.go") ||
		!strings.Contains(sys, "gap: no tests for A") || !strings.Contains(sys, "Break the gaps") {
		t.Errorf("round 2 system prompt lacks round 1:\n%s", sys)
	}
	if got := lastContent(f.director, 6); !strings.Contains(got, "1 more round(s)") {
		t.Errorf("round 2 review prompt:\n%s", got)
	}
	var out strings.Builder
	printReport(&out, rep)
	if s := out.String(); !strings.Contains(s, "after 2 rounds") || !strings.Contains(s, "Round 1:  objective not met: A works") {
		t.Errorf("report:\n%s", s)
	}
}

// - Verify rounds stop at maxIterations, and earlier when the review lists no gaps (nothing to plan from)
func Test_Autonomous_Iterations_Stop(t *testing.T) {
	notMet := func(id, gaps string) model.ChatMessage {
		return call(id, "finish", `{"success":false,"summary":"not yet","gaps":`+gaps+`}`)
	}
	f := newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.run.cfg.Director.MaxIterations = 2
	f.director.Script = []model.ChatMessage{
		text("Do A"), call("d1", "task_done", `{"note":"ok"}`), notMet("f1", `["gap"]`),
		call("p", "set_tasks", `{"tasks":[{"id":"t1","task":"Fix gap"}]}`), text("Fix it"), call("d2", "task_done", `{"note":"ok"}`), notMet("f2", `["gap"]`),
	}
	f.worker.Script = []model.ChatMessage{text("r1"), text("r2")}
	rep := f.run.run(context.Background())
	if rep.Reason != ReasonFinished || rep.Plan.Success || len(rep.Earlier) != 1 || !errors.Is(rep.runError(), ErrObjectiveNotMet) {
		t.Errorf("at the limit: report = %+v", rep)
	}

	f = newAutoFixture(t, "# Objective\nx\n## Tasks\n- Task A\n", 5)
	f.run.cfg.Director.MaxIterations = 3
	f.director.Script = []model.ChatMessage{text("Do A"), call("d1", "task_done", `{"note":"ok"}`), notMet("f1", `[]`)}
	f.worker.Script = []model.ChatMessage{text("r1")}
	if rep := f.run.run(context.Background()); len(rep.Earlier) != 0 || len(f.director.Calls) != 3 {
		t.Errorf("no gaps: earlier rounds %d, director calls %d; want 0, 3", len(rep.Earlier), len(f.director.Calls))
	}
}
