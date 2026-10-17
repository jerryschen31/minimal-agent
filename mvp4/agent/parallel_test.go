package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
	"github.com/jerryschen31/minimal-agent/mvp4/director"
	"github.com/jerryschen31/minimal-agent/mvp4/model"
	"github.com/jerryschen31/minimal-agent/mvp4/tasks"
	"github.com/jerryschen31/minimal-agent/mvp4/worker"
)

// ---- parallel tasks (orchestrator mode, director.maxParallel > 1) ----

// routeProvider is a fake director for parallel runs: several conversations call it at once, so a
// scripted reply list can't work. It answers from the system prompt (which task, which phase) and the
// last message instead.
type routeProvider struct {
	route func(taskID, last string) model.ChatMessage // taskID is "" outside the WORK phase
}

func (p routeProvider) Chat(_ context.Context, msgs []model.ChatMessage, _ []model.ToolDef) (model.ChatMessage, error) {
	system, last := msgs[0].Content, msgs[len(msgs)-1].Content
	id := ""
	if _, rest, ok := strings.Cut(system, "WORK, task "); ok {
		id, _, _ = strings.Cut(rest, ".")
	}
	return p.route(id, last), nil
}

// directorRoute is the usual director: write a prompt "do <id>" for a new task, then hand a worker
// turn's outcome to onTurn, and finish the review successfully when everything is done.
func directorRoute(onTurn func(id string) model.ChatMessage) func(string, string) model.ChatMessage {
	return func(id, last string) model.ChatMessage {
		switch {
		case id == "":
			return call("f", "finish", `{"success":true,"summary":"ok","gaps":[]}`)
		case strings.Contains(last, "Write the worker's first prompt"):
			return text("do " + id)
		}
		return onTurn(id)
	}
}

// funcWorker is a fake worker whose turns run fn.
type funcWorker struct {
	fn func(ctx context.Context, taskID string) (worker.Result, error)
}

func (w funcWorker) Info() worker.Info { return worker.Info{Model: "fake-worker"} }
func (w funcWorker) RunTurn(ctx context.Context, taskID, _ string) (worker.Result, error) {
	return w.fn(ctx, taskID)
}
func (funcWorker) EndTask(context.Context, string) error { return nil }
func (funcWorker) Close() error                          { return nil }

// newParallelFixture is newAutoFixture set up for orchestrator mode with n workers running fn, a
// routed director, and the given task list (already planned).
func newParallelFixture(t *testing.T, n int, specs []tasks.Spec, route func(string, string) model.ChatMessage, fn func(context.Context, string) (worker.Result, error)) *autonomousRun {
	t.Helper()
	r := newAutoFixture(t, "# Objective\nDo it all.\n", 1).run
	r.cfg.AgentMode, r.cfg.Director.MaxParallel = config.ModeOrchestrator, n
	var ws []worker.Worker
	for range n {
		ws = append(ws, funcWorker{fn})
	}
	setWorkers(r, ws...)
	r.dirProv = routeProvider{route}
	r.dir.Provider = r.dirProv
	r.plan = director.NewPlan(nil, 20, n, nil)
	if err := r.plan.SetTasks(specs); err != nil {
		t.Fatal(err)
	}
	return r
}

func done(id string) model.ChatMessage {
	return call("d-"+id, "task_done", fmt.Sprintf(`{"note":"%s ok"}`, id))
}

// - Verify independent tasks run at the same time on different workers, a dependent task waits for
// both, output lines carry the task and worker, and the report shows the overlap
func Test_Parallel_IndependentTasksRunAtOnce(t *testing.T) {
	var mu sync.Mutex
	var events []string
	bothStarted := make(chan struct{})
	var started sync.WaitGroup
	started.Add(2)
	go func() { started.Wait(); close(bothStarted) }()

	fn := func(ctx context.Context, id string) (worker.Result, error) {
		mu.Lock()
		events = append(events, "start "+id)
		mu.Unlock()
		if id != "c" {
			// [agent] a and b each wait until the other has started: run one at a time, this would time out
			started.Done()
			select {
			case <-bothStarted:
			case <-time.After(5 * time.Second):
				return worker.Result{}, errors.New("the other task never started: tasks did not run in parallel")
			}
		}
		mu.Lock()
		events = append(events, "end "+id)
		mu.Unlock()
		return worker.Result{Status: worker.StatusCompleted, Reply: "did " + id}, nil
	}
	r := newParallelFixture(t, 2, []tasks.Spec{
		{ID: "a", Task: "task a"}, {ID: "b", Task: "task b"}, {ID: "c", Task: "task c", DependsOn: []string{"a", "b"}},
	}, directorRoute(done), fn)

	rep := r.run(context.Background())

	if rep.Reason != ReasonFinished || !rep.Plan.Success || rep.WorkerTurns != 3 || rep.PeakTasks != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if got := strings.Join(events, ","); !strings.HasSuffix(got, "start c,end c") {
		t.Errorf("events = %s; c must start after a and b ended", got)
	}
	if a, b := r.plan.Tasks.Get("a").WorkerID, r.plan.Tasks.Get("b").WorkerID; a == b || a == "" || b == "" {
		t.Errorf("a ran on %q and b on %q; want two different workers", a, b)
	}
	printReport(r.out, rep)
	out := r.out.(fmt.Stringer).String()
	for _, want := range []string{"] [run] === TASK a (1/3)", "] [run] === TASK b (2/3)", "] [director] do c", "up to 2 tasks at once"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// - Verify a failed task drains the round: the running sibling finishes, no new task starts, and the
// review still runs
func Test_Parallel_FailureDrains(t *testing.T) {
	var r *autonomousRun
	fn := func(ctx context.Context, id string) (worker.Result, error) {
		if id == "b" {
			// b is still working when a fails
			for !r.plan.Failed() {
				select {
				case <-ctx.Done():
					return worker.Result{}, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
		}
		return worker.Result{Status: worker.StatusCompleted, Reply: "did " + id}, nil
	}
	route := directorRoute(func(id string) model.ChatMessage {
		if id == "a" {
			return call("x", "task_failed", `{"reason":"a is impossible"}`) // no refusal: no worker turns left
		}
		return done(id)
	})
	r = newParallelFixture(t, 2, []tasks.Spec{{ID: "a", Task: "a"}, {ID: "b", Task: "b"}, {ID: "c", Task: "c"}}, route, fn)
	finish := route
	r.dirProv = routeProvider{func(id, last string) model.ChatMessage {
		if id == "" {
			return call("f", "finish", `{"success":false,"summary":"a failed","gaps":["a"]}`)
		}
		return finish(id, last)
	}}
	r.dir.Provider = r.dirProv

	rep := r.run(context.Background())

	status := func(id string) tasks.Status { return r.plan.Tasks.Get(id).Status }
	if rep.Reason != ReasonTaskFailed || !rep.Reviewed || rep.WorkerTurns != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if status("a") != tasks.Failed || status("b") != tasks.Done || status("c") != tasks.Pending {
		t.Errorf("statuses a=%s b=%s c=%s; want failed, done, pending", status("a"), status("b"), status("c"))
	}
}

// - Verify Ctrl+C (a cancelled run context) stops every running task, and the run ends as interrupted
func Test_Parallel_CancelStopsAllTasks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var waiting sync.WaitGroup
	waiting.Add(2)
	fn := func(ctx context.Context, id string) (worker.Result, error) {
		waiting.Done()
		<-ctx.Done()
		return worker.Result{}, ctx.Err()
	}
	r := newParallelFixture(t, 2, []tasks.Spec{{ID: "a", Task: "a"}, {ID: "b", Task: "b"}}, directorRoute(done), fn)
	go func() { waiting.Wait(); cancel() }()

	rep := r.run(ctx)

	if rep.Reason != ReasonInterrupted || rep.Reviewed {
		t.Fatalf("report = %+v", rep)
	}
}

// - Verify lineWriter prefixes whole lines, holds a partial line until its newline, and Flush ends it
func Test_LineWriter(t *testing.T) {
	var out strings.Builder
	lw := &lineWriter{w: &out, mu: &sync.Mutex{}, prefix: "[t1 w1] "}
	fmt.Fprint(lw, "one\ntw")
	if out.String() != "[t1 w1] one\n" {
		t.Errorf("after a partial line: %q", out.String())
	}
	fmt.Fprint(lw, "o\nthree")
	lw.Flush()
	if out.String() != "[t1 w1] one\n[t1 w1] two\n[t1 w1] three\n" {
		t.Errorf("output = %q", out.String())
	}
}

// - Verify worker types: a task runs only on a worker of its type, so two engineer tasks wait for the
// one engineer while the tester task runs beside them; the director's prompt lists the types
func Test_Parallel_WorkerTypes(t *testing.T) {
	var mu sync.Mutex
	busy := map[string]int{} // worker type → tasks running on it now
	overlap := false
	fn := func(typ string) func(context.Context, string) (worker.Result, error) {
		return func(ctx context.Context, id string) (worker.Result, error) {
			mu.Lock()
			if busy[typ]++; busy[typ] > 1 {
				overlap = true
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			busy[typ]--
			mu.Unlock()
			return worker.Result{Status: worker.StatusCompleted, Reply: "did " + id}, nil
		}
	}
	var prompts []string
	route := directorRoute(done)
	r := newParallelFixture(t, 1, []tasks.Spec{{ID: "x", Task: "x"}}, route, nil)
	r.cfg.Workers = []config.WorkerType{{Type: "engineer", Description: "Writes code."}, {Type: "tester"}}
	r.slots = []workerSlot{{funcWorker{fn("engineer")}, "engineer1", "engineer"}, {funcWorker{fn("tester")}, "tester1", "tester"}}
	r.groups = workerGroups(r.cfg, r.slots)
	r.plan = director.NewPlan(nil, 20, 2, workerTypes(r.groups))
	if err := r.plan.SetTasks([]tasks.Spec{
		{ID: "e1", Task: "code 1"}, {ID: "e2", Task: "code 2"}, {ID: "t1", Task: "test", WorkerType: "tester"},
	}); err != nil {
		t.Fatal(err)
	}
	r.dirProv = recordingProvider{r.dirProv, &mu, &prompts} // the task directors; PLAN/REVIEW use r.dir

	rep := r.run(context.Background())

	if rep.Reason != ReasonFinished || rep.WorkerTurns != 3 || overlap {
		t.Fatalf("report = %+v, two tasks on one worker at once = %v", rep, overlap)
	}
	for id, want := range map[string]string{"e1": "engineer1", "e2": "engineer1", "t1": "tester1"} {
		if got := r.plan.Tasks.Get(id).WorkerID; got != want {
			t.Errorf("%s ran on %q, want %q", id, got, want)
		}
	}
	if len(prompts) == 0 || !strings.Contains(prompts[0], "- engineer (1 worker(s), model fake-worker): Writes code.") {
		t.Errorf("director's system prompt doesn't list the worker types:\n%v", prompts)
	}
}

// recordingProvider records the system prompt of every call before passing it on.
type recordingProvider struct {
	model.Provider
	mu      *sync.Mutex
	systems *[]string
}

func (p recordingProvider) Chat(ctx context.Context, msgs []model.ChatMessage, defs []model.ToolDef) (model.ChatMessage, error) {
	p.mu.Lock()
	*p.systems = append(*p.systems, msgs[0].Content)
	p.mu.Unlock()
	return p.Provider.Chat(ctx, msgs, defs)
}
