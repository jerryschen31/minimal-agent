package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/state"
)

// threeTasks is a hand-written plan (no front matter): t2 and t3 depend on t1.
const threeTasks = `# Plan: Three steps

## Objective

Do three things.

## Tasks

### t1: First
done when: a exists

Make a.

### t2: Second
depends: t1
done when: b exists

Make b.

### t3: Third
depends: t1
done when: c exists

Make c.
`

// runAutoYes runs autoMode on fx with -yes (no question), and returns its output and error.
func runAutoYes(t *testing.T, fx *chatSessionFixture) (string, error) {
	t.Helper()
	fx.Out.Reset()
	fx.Session.Config.AssumeYes = true
	err := runAutoModeWith(t, fx, "")
	return fx.Out.String(), err
}

// journal reads run n's journal in the fixture's state folder.
func journal(t *testing.T, fx *chatSessionFixture, n int) []state.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fx.Session.Config.StateDir, "runs", fmt.Sprintf("%04d", n), "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	events, torn, err := state.Read(bytes.NewReader(data))
	if err != nil || torn {
		t.Fatalf("journal of run %d: torn %v, %v", n, torn, err)
	}
	return events
}

// withoutToolEvents returns events without tool_before and tool_after (tests of the task events).
func withoutToolEvents(events []state.Event) []state.Event {
	return slices.DeleteFunc(slices.Clone(events), func(e state.Event) bool {
		return e.Type == state.EventToolBefore || e.Type == state.EventToolAfter
	})
}

// eventTypes returns the type and task of each event, as "type" or "type:task".
func eventTypes(events []state.Event) []string {
	var out []string
	for _, e := range events {
		if e.Task != "" {
			out = append(out, string(e.Type)+":"+e.Task)
		} else {
			out = append(out, string(e.Type))
		}
	}
	return out
}

// lastRunEnded decodes the last event of events as run_ended.
func lastRunEnded(t *testing.T, events []state.Event) state.RunEnded {
	t.Helper()
	last := events[len(events)-1]
	var r state.RunEnded
	if last.Type != state.EventRunEnded || json.Unmarshal(last.Data, &r) != nil {
		t.Fatalf("last event = %s, want run_ended", last.Type)
	}
	return r
}

// newThreeTaskFixture writes threeTasks to plan.md in a new temp dir (also the cwd) and returns an auto fixture for it.
func newThreeTaskFixture(t *testing.T) *chatSessionFixture {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.WriteFile("plan.md", []byte(threeTasks), 0o644); err != nil {
		t.Fatal(err)
	}
	fx, _ := newAutoFixture(t, "plan.md")
	return fx
}

// - Verify tasksToRun's order (each task after its dependencies, else file order) and its skip rule (done + same hash)
// - Verify every other state runs (changed, failed, interrupted, cut off, new) and IDs gone from the plan are listed
func Test_Unit_TasksToRun(t *testing.T) {
	p, _, err := plan.Cleanup([]byte("# Plan: x\n\n## Tasks\n\n" +
		"### t3: c\ndepends: t2\n\n" +
		"### t1: a\n\n" +
		"### t2: b\ndepends: t1\n\n" +
		"### t4: d\n\n" +
		"### t5: e\n\n" +
		"### t6: f\n"))
	if err != nil {
		t.Fatal(err)
	}
	hash := map[string]string{}
	for _, task := range p.Tasks {
		hash[task.ID] = task.DefinitionHash()
	}
	st := state.PlanState{Tasks: map[string]state.TaskState{
		"t1": {Status: state.TaskDone, DefHash: hash["t1"], Run: 2}, // unchanged → skip
		"t2": {Status: state.TaskDone, DefHash: "sha256:old"},       // changed → run
		"t3": {Status: state.TaskFailed, DefHash: hash["t3"]},       // failed → run
		"t4": {Status: state.TaskInterrupted, DefHash: hash["t4"]},  // interrupted → run
		"t5": {Status: state.TaskRunning},                           // cut off by a crash → run
		"t9": {Status: state.TaskDone},                              // gone from the plan
		"t8": {Status: state.TaskFailed},                            // gone from the plan
	}}
	steps, removed := tasksToRun(p, st)
	var order, skipped []string
	for _, s := range steps {
		order = append(order, s.Task.ID)
		if s.Skip {
			skipped = append(skipped, s.Task.ID)
		}
	}
	if want := []string{"t1", "t2", "t3", "t4", "t5", "t6"}; !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if !slices.Equal(skipped, []string{"t1"}) || steps[0].DoneRun != 2 {
		t.Errorf("skipped = %v (t1 done in run %d), want only t1, done in run 2", skipped, steps[0].DoneRun)
	}
	if !slices.Equal(removed, []string{"t8", "t9"}) {
		t.Errorf("removed = %v, want [t8 t9]", removed)
	}
}

//   - Verify a full run: every task runs in order with a fresh worker, the journal holds manifest, task_started and
//     task_finished (done, with each task's definition hash and summary) per task, then run_ended "done"; the run
//     folder has plan.md and config.json; nothing is written to the current directory
func Test_Unit_AutoRun_RunsEveryTask(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantText("made a"), modeltest.AssistantText("made b"), modeltest.AssistantText("made c"),
	}
	out, err := runAutoYes(t, fx)
	if err != nil {
		t.Fatalf("autoMode: %v\n%s", err, out)
	}
	for _, want := range []string{"Run 0001", "▶ t1: First", "✓ t1 done", "✓ t3 done", "Finished: 3 done, 0 skipped."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	events := withoutToolEvents(journal(t, fx, 1)) // the director's verdict calls are recorded too; tested in autojournal_test.go
	want := []string{"manifest", "task_started:t1", "task_finished:t1", "task_started:t2", "task_finished:t2",
		"task_started:t3", "task_finished:t3", "run_ended"}
	if got := eventTypes(events); !slices.Equal(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
	p, _, _ := plan.Cleanup([]byte(threeTasks))
	for i, task := range p.Tasks {
		var fin state.TaskFinished
		if err := json.Unmarshal(events[2+2*i].Data, &fin); err != nil {
			t.Fatal(err)
		}
		if fin.Status != state.TaskDone || fin.DefHash != task.DefinitionHash() || fin.Summary != "made "+string(rune('a'+i)) {
			t.Errorf("%s finished = %+v", task.ID, fin)
		}
	}
	if r := lastRunEnded(t, events); r.Reason != state.StopDone || r.Error != "" {
		t.Errorf("run_ended = %+v, want done", r)
	}
	runDir := filepath.Join(fx.Session.Config.StateDir, "runs", "0001")
	if got, _ := os.ReadFile(filepath.Join(runDir, "plan.md")); string(got) != threeTasks {
		t.Errorf("plan.md in the run folder = %q", got)
	}
	if _, err := os.Stat(filepath.Join(runDir, "config.json")); err != nil {
		t.Errorf("config.json: %v", err)
	}
	// fresh worker: each model call holds only the system prompt and that task's message
	for i, call := range fx.Provider.Calls {
		if len(call) != 2 || call[0].Content != workerPrompt || !strings.Contains(call[1].Content, "## Your task: t"+strconv.Itoa(i+1)) {
			t.Errorf("call %d: %d messages, want the worker prompt and task t%d's message", i+1, len(call), i+1)
		}
	}
	checkNothingWritten(t, "plan.md")
}

//   - Verify a second run of the same plan skips every done task with no model call, and after a task's body is edited
//     only that task runs (its dependents stay skipped: no cascade)
func Test_Unit_AutoRun_SkipsDoneTasks(t *testing.T) {
	fx := newThreeTaskFixture(t)
	if _, err := runAutoYes(t, fx); err != nil {
		t.Fatal(err)
	}
	calls := len(fx.Provider.Calls)
	out, err := runAutoYes(t, fx)
	if err != nil || len(fx.Provider.Calls) != calls || !strings.Contains(out, "skip t2: Second (done in run 0001, unchanged)") ||
		!strings.Contains(out, "Finished: 0 done, 3 skipped.") {
		t.Fatalf("second run: %d new model calls, err %v:\n%s", len(fx.Provider.Calls)-calls, err, out)
	}
	if err := os.WriteFile("plan.md", []byte(strings.Replace(threeTasks, "Make a.", "Make a, twice.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runAutoYes(t, fx)
	if err != nil || !strings.Contains(out, "▶ t1: First") || !strings.Contains(out, "Finished: 1 done, 2 skipped.") {
		t.Errorf("after editing t1: err %v:\n%s", err, out)
	}
}

//   - Verify a task's worker gets the summaries of the tasks it depends on, also from an earlier run: run 1 finishes t1
//     and fails t2 (stop reason "error", t3 not started, an error returned); run 2 skips t1 and t2's message has t1's
//     summary
func Test_Unit_AutoRun_DependencySummaryAcrossRuns(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("a is in a.txt")} // t2's call finds the script exhausted
	out, err := runAutoYes(t, fx)
	if err == nil || !strings.Contains(err.Error(), "t2 failed") {
		t.Errorf("run 1: err = %v, want t2 failed", err)
	}
	if !strings.Contains(out, "Stopped: error. 1 done, 0 skipped, 1 failed, 1 not started.") {
		t.Errorf("run 1 report:\n%s", out)
	}
	if r := lastRunEnded(t, journal(t, fx, 1)); r.Reason != state.StopError || !strings.Contains(r.Error, "t2 failed") {
		t.Errorf("run 1 run_ended = %+v", r)
	}

	fx.Provider.Script = nil // every call replies ""
	fx.Provider.Calls = nil
	if out, err := runAutoYes(t, fx); err != nil || !strings.Contains(out, "skip t1") {
		t.Fatalf("run 2: %v\n%s", err, out)
	}
	if first := fx.Provider.Calls[0][1].Content; !strings.Contains(first, "## Your task: t2") || !strings.Contains(first, "### t1\n\na is in a.txt") {
		t.Errorf("t2's message lacks t1's summary:\n%s", first)
	}
}

//   - Verify a worker that runs out of steps is a failed attempt (the director isn't asked) and is tried again; after
//     maxTaskAttempts such attempts the run stops with "task_failed", the tasks after it not started, an error returned
func Test_Unit_AutoRun_WorkerOutOfStepsRetries(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Session.Config.MaxSteps = 1
	call := modeltest.AssistantToolCall("c1", "no_such_tool", `{}`)
	fx.Provider.Script = []model.ChatMessage{call, call, call}
	out, err := runAutoYes(t, fx)
	if err == nil || !strings.Contains(err.Error(), "t1 not done after 3 attempts") ||
		!strings.Contains(out, "Stopped: task_failed. 0 done, 0 skipped, 1 failed, 2 not started.") {
		t.Errorf("err = %v:\n%s", err, out)
	}
	if !strings.Contains(out, "the worker ran out of steps (maxSteps 1) (attempt 3 of 3)") {
		t.Errorf("output lacks the out-of-steps verdict:\n%s", out)
	}
	if n := directorOf(fx).calls; n != 0 {
		t.Errorf("director asked %d times, want 0", n)
	}
	if r := lastRunEnded(t, journal(t, fx, 1)); r.Reason != state.StopTaskFailed {
		t.Errorf("run_ended = %+v, want task_failed", r)
	}
}

//   - Verify Ctrl+C during a task stops the run: "interrupted" in run_ended, no task_finished for the task (Load then
//     says interrupted), context.Canceled returned (main: exit 130), and the lock is released (a second run starts)
func Test_Unit_AutoRun_Interrupted(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Session.Config.AssumeYes = true
	fx.Provider.Gate, fx.Provider.Waiting = make(chan struct{}), make(chan struct{})
	interrupts := make(chan os.Signal, 1)
	done := make(chan error)
	go func() {
		reader := newPlainReader(t.Context(), strings.NewReader(""), fx.Out)
		done <- autoMode(t.Context(), interrupts, fx.Session, reader, reader)
	}()
	<-fx.Provider.Waiting
	interrupts <- os.Interrupt
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("autoMode: %v, want context.Canceled", err)
	}
	events := journal(t, fx, 1)
	// [agent] attempt_usage (2d): an attempt stopped without a task_finished still records its tokens
	if got := eventTypes(events); !slices.Equal(got, []string{"manifest", "task_started:t1", "attempt_usage:t1", "run_ended"}) {
		t.Errorf("journal = %v", got)
	}
	if r := lastRunEnded(t, events); r.Reason != state.StopInterrupted {
		t.Errorf("run_ended = %+v, want interrupted", r)
	}
	home, _ := state.OpenHome(fx.Session.Config.StateDir)
	ref, _ := state.NewPlanRef("", "plan.md")
	if s, _ := home.Load(ref); s.Tasks["t1"].Status != state.TaskInterrupted {
		t.Errorf("t1 = %+v, want interrupted", s.Tasks["t1"])
	}
	fx.Provider.Gate, fx.Provider.Waiting = nil, nil
	if _, err := runAutoYes(t, fx); err != nil {
		t.Errorf("second run after the interrupt: %v", err)
	}
}

//   - Verify a plan with a plan_id keeps its history after a move ("moved from", tasks skipped), and a copy that shares
//     the plan_id still runs, with a warning naming both files
func Test_Unit_AutoRun_MovedAndCopiedPlan(t *testing.T) {
	path := writeAutoPlan(t, "setup.md", "goal.md") // has a plan_id
	fx, _ := newAutoFixture(t, path)
	if _, err := runAutoYes(t, fx); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename("setup.md", "renamed.md"); err != nil {
		t.Fatal(err)
	}
	fx.Session.ModeDetails.(*autoModeDetails).PlanPath = "renamed.md"
	out, err := runAutoYes(t, fx)
	if err != nil || !strings.Contains(out, "This plan was moved from") || !strings.Contains(out, "Finished: 0 done, 2 skipped.") {
		t.Errorf("moved: err %v:\n%s", err, out)
	}
	data, _ := os.ReadFile("renamed.md")
	if err := os.WriteFile("copy.md", data, 0o644); err != nil {
		t.Fatal(err)
	}
	fx.Session.ModeDetails.(*autoModeDetails).PlanPath = "copy.md"
	out, err = runAutoYes(t, fx)
	if err != nil || !strings.Contains(out, "have the same plan_id") || !strings.Contains(out, "renamed.md") || !strings.Contains(out, "Finished:") {
		t.Errorf("copy: err %v:\n%s", err, out)
	}
}

// - Verify a plan another process is running is refused with the lock's error, and no run folder is made
func Test_Unit_AutoRun_Locked(t *testing.T) {
	fx := newThreeTaskFixture(t)
	home, _ := state.OpenHome(fx.Session.Config.StateDir)
	ref, _ := state.NewPlanRef("", "plan.md")
	unlock, err := home.Lock(ref)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = runAutoYes(t, fx)
	var locked *state.LockedError
	if !errors.As(err, &locked) {
		t.Errorf("err = %v, want a LockedError", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(fx.Session.Config.StateDir, "runs")); len(entries) != 0 {
		t.Errorf("a locked plan made %d run folders", len(entries))
	}
}

// gatedThreeTasks is threeTasks with a gate on t2.
var gatedThreeTasks = strings.Replace(threeTasks, "depends: t1\ndone when: b exists", "depends: t1\ngate: a person checks a first\ndone when: b exists", 1)

//   - Verify a gated task's question: "y" runs it (gate_answered yes), "n" or Ctrl+D stops the run with "declined"
//     (gate_answered no) before it starts, and -yes does not answer it (it is still asked)
func Test_Unit_AutoRun_GateAnswers(t *testing.T) {
	tests := []struct {
		name, input string
		yes         bool
		reason      state.StopReason
		answer      string
	}{
		{"yes", "y\ny\n", false, state.StopDone, "yes"},
		{"no", "y\nn\n", false, state.StopDeclined, "no"},
		{"Ctrl+D", "y\n", false, state.StopDeclined, "no"},
		{"-yes still asks", "y\n", true, state.StopDone, "yes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newThreeTaskFixture(t)
			if err := os.WriteFile("plan.md", []byte(gatedThreeTasks), 0o644); err != nil {
				t.Fatal(err)
			}
			fx.Session.Config.AssumeYes = tt.yes
			err := runAutoModeWith(t, fx, tt.input)
			if (tt.reason == state.StopDeclined) != errors.Is(err, ErrNeedsPerson) || tt.reason == state.StopDone && err != nil {
				t.Fatalf("autoMode: %v (want ErrNeedsPerson only when declined)", err)
			}
			out := fx.Out.String()
			if !strings.Contains(out, `t2: Second has a gate: "a person checks a first". Run it? [y/N]`) {
				t.Errorf("gate not asked:\n%s", out)
			}
			events := journal(t, fx, 1)
			if r := lastRunEnded(t, events); r.Reason != tt.reason {
				t.Errorf("run_ended = %+v, want %s\n%s", r, tt.reason, out)
			}
			i := slices.Index(eventTypes(events), "gate_answered:t2")
			var ga state.GateAnswered
			if i < 0 || json.Unmarshal(events[i].Data, &ga) != nil || ga.Answer != tt.answer {
				t.Errorf("gate_answered = %+v (at %d), want %q", ga, i, tt.answer)
			}
			if tt.reason == state.StopDeclined && (len(fx.Provider.Calls) != 1 || !strings.Contains(out, "Stopped: declined. 1 done, 0 skipped, 0 failed, 2 not started.")) {
				t.Errorf("declined: %d worker calls, want 1 (t1 only):\n%s", len(fx.Provider.Calls), out)
			}
		})
	}
}

//   - Verify a gated task with no terminal to ask stops the run with "waiting" before it starts (nothing recorded for
//     the gate), and the next run on a terminal skips the done task and asks
func Test_Unit_AutoRun_GateWithoutTerminalWaits(t *testing.T) {
	fx := newThreeTaskFixture(t)
	if err := os.WriteFile("plan.md", []byte(gatedThreeTasks), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.Session.Config.AssumeYes = true
	reader := newPlainReader(t.Context(), strings.NewReader(""), fx.Out)
	if err := autoMode(t.Context(), nil, fx.Session, reader, nil); !errors.Is(err, ErrNeedsPerson) {
		t.Fatalf("autoMode: %v, want ErrNeedsPerson", err)
	}
	out := fx.Out.String()
	if !strings.Contains(out, "no terminal to ask") || !strings.Contains(out, "Stopped: waiting. 1 done, 0 skipped, 0 failed, 2 not started.") {
		t.Errorf("output:\n%s", out)
	}
	if got := eventTypes(journal(t, fx, 1)); slices.Contains(got, "gate_answered:t2") {
		t.Errorf("journal records an answer nobody gave: %v", got)
	}
	out, err := runAutoYes(t, fx) // the -yes run, now with a reader that answers (Ctrl+D: no)
	if !errors.Is(err, ErrNeedsPerson) || !strings.Contains(out, "skip t1") || !strings.Contains(out, "Run it? [y/N]") {
		t.Errorf("second run: %v\n%s", err, out)
	}
}

// - Verify taskMessage holds the plan's objective, the dependencies' summaries in depends order, and the task's fields
func Test_Unit_TaskMessage(t *testing.T) {
	p, _, _ := plan.Cleanup([]byte(threeTasks))
	msg := taskMessage(p, p.Tasks[1], map[string]string{"t1": "a is ready", "t3": "not a dependency"}, nil, "")
	for _, want := range []string{"# Plan: Three steps", "## Objective\n\nDo three things.", "### t1\n\na is ready",
		"## Your task: t2: Second", "Done when: b exists", "Make b."} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "not a dependency") || strings.Contains(msg, "Make a.") {
		t.Errorf("message holds another task's text:\n%s", msg)
	}
}
