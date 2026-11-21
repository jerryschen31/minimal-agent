package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/state"
)

// used returns a reported Usage with the given input and output tokens.
func used(in, out int) model.Usage {
	return model.Usage{InputTokens: in, OutputTokens: out, Reported: true}
}

//   - Verify the meter refuses once its allowance is used up (not before), never with allowance 0, and counts calls with
//     no usage as 0 while noting them
func Test_Unit_UsageMeter_CheckAndAdd(t *testing.T) {
	m := &usageMeter{allowance: 100}
	m.add(used(60, 30))
	if err := m.check(); err != nil {
		t.Errorf("90 of 100: expected no error, got %v", err)
	}
	m.add(used(5, 5))
	if err := m.check(); !errors.Is(err, errAllowanceUsed) || !strings.Contains(err.Error(), "100 of 100 tokens") {
		t.Errorf("100 of 100: expected errAllowanceUsed with the numbers, got %v", err)
	}
	m.add(model.Usage{})
	if m.unreported != 1 || m.used.Total() != 100 {
		t.Errorf("an unreported call: unreported %d, total %d; want 1 and 100", m.unreported, m.used.Total())
	}
	unlimited := &usageMeter{}
	unlimited.add(used(1_000_000, 1))
	if err := unlimited.check(); err != nil {
		t.Errorf("allowance 0 (no limit): expected no error, got %v", err)
	}
}

//   - Verify a session with a meter counts each call's usage, and a call over the allowance is refused without reaching
//     the server
func Test_Unit_MeteredSession_RefusesWithoutCallingTheServer(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("one"), modeltest.AssistantText("two")}
	fx.Provider.Usages = []model.Usage{used(80, 30)}
	fx.Session.Meter = &usageMeter{allowance: 100}
	p := fx.Session.getSessionProvider("task-1")

	if _, err := p.Chat(context.Background(), nil, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := p.Chat(context.Background(), nil, nil); !errors.Is(err, errAllowanceUsed) {
		t.Errorf("second call: expected errAllowanceUsed, got %v", err)
	}
	if len(fx.Provider.Calls) != 1 || fx.Session.Meter.used.Total() != 110 {
		t.Errorf("server calls %d, meter total %d; want 1 and 110", len(fx.Provider.Calls), fx.Session.Meter.used.Total())
	}
}

//   - Verify the allowance: no budgets = no limit; the run's left = budget minus used (and pending); taskTokenBudget caps
//     it when smaller; a used-up run budget allows nothing
func Test_Unit_AutoRun_Allowance(t *testing.T) {
	ar := &autoRun{used: used(600, 100)}
	cases := []struct {
		name                  string
		runBudget, taskBudget int
		pending               model.Usage
		want                  int
		runLimited, ok        bool
	}{
		{"no budgets", 0, 0, model.Usage{}, 0, false, true},
		{"task budget only", 0, 50, model.Usage{}, 50, false, true},
		{"run budget left", 1000, 0, model.Usage{}, 300, true, true},
		{"task budget smaller", 1000, 200, model.Usage{}, 200, false, true},
		{"task budget larger", 1000, 500, model.Usage{}, 300, true, true},
		{"pending counted", 1000, 0, used(100, 0), 200, true, true},
		{"used up", 700, 0, model.Usage{}, 0, true, false},
	}
	for _, c := range cases {
		n, runLimited, ok := ar.allowance(c.runBudget, c.taskBudget, c.pending)
		if n != c.want || runLimited != c.runLimited || ok != c.ok {
			t.Errorf("%s: got (%d, %v, %v), want (%d, %v, %v)", c.name, n, runLimited, ok, c.want, c.runLimited, c.ok)
		}
	}
}

//   - Verify a run that reaches runTokenBudget stops with "budget" and exit 3 (ErrNeedsPerson): the finished task's
//     tokens are in its task_finished, the stopped attempt's in attempt_usage, the stopped task runs again next time
//     (interrupted), later tasks are not started, and the report says what was used of what
func Test_Unit_AutoRun_TokenBudget_StopsTheRun(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Session.Config.RunTokenBudget = 1000
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("made a"), modeltest.AssistantText("made b")}
	fx.Provider.Usages = []model.Usage{used(500, 100), used(400, 100)} // t1: 600; t2's worker: 500 → 1100, no room for its director

	out, err := runAutoYes(t, fx)

	if !errors.Is(err, ErrNeedsPerson) {
		t.Fatalf("autoMode: %v, want ErrNeedsPerson\n%s", err, out)
	}
	for _, want := range []string{"✓ t1 done", "■ t2 stopped (budget)", "Stopped: budget. 1 done, 0 skipped, 0 failed, 1 not started.",
		"Token budget: 1100 of 1000 tokens used (runTokenBudget)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "reports no token usage") != 1 { // the scripted director reports none
		t.Errorf("expected the no-usage warning once:\n%s", out)
	}
	events := withoutToolEvents(journal(t, fx, 1))
	want := []string{"manifest", "task_started:t1", "task_finished:t1", "task_started:t2", "attempt_usage:t2", "run_ended"}
	if got := eventTypes(events); !slices.Equal(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
	var fin state.TaskFinished
	var au state.AttemptUsage
	json.Unmarshal(events[2].Data, &fin)
	json.Unmarshal(events[4].Data, &au)
	if fin.Usage.Total() != 600 || au.Usage.Total() != 500 || au.Attempt != 1 {
		t.Errorf("t1 task_finished usage %+v, t2 attempt_usage %+v; want 600 and 500 (attempt 1)", fin.Usage, au)
	}
	if r := lastRunEnded(t, events); r.Reason != state.StopBudget {
		t.Errorf("run_ended = %+v, want budget", r)
	}
	home, _ := state.OpenHome(fx.Session.Config.StateDir)
	ref, _ := state.NewPlanRef("", "plan.md")
	s, _ := home.Load(ref)
	if s.Tasks["t2"].Status != state.TaskInterrupted || s.Latest.Usage.Total() != 1100 {
		t.Errorf("t2 = %+v, run usage %d; want interrupted and 1100", s.Tasks["t2"], s.Latest.Usage.Total())
	}
}

//   - Verify a worker that uses up taskTokenBudget (while the run has no budget) is a failed attempt with a reason that
//     says so, not the end of the run
func Test_Unit_AutoRun_TaskTokenBudget_IsAFailedAttempt(t *testing.T) {
	fx := newOneTaskFixture(t)
	fx.Session.Config.TaskTokenBudget = 100
	fx.Session.Config.MaxTaskAttempts = 1
	if err := fx.Tools.Register(namedTool{name: "look", result: "seen"}); err != nil {
		t.Fatal(err)
	}
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantToolCall("c1", "look", "{}"), modeltest.AssistantText("never sent")}
	fx.Provider.Usages = []model.Usage{used(120, 10)}

	out, err := runAutoYes(t, fx)

	if err == nil || errors.Is(err, ErrNeedsPerson) {
		t.Fatalf("autoMode: %v, want a task_failed error\n%s", err, out)
	}
	for _, want := range []string{"not done — the worker used its token allowance (taskTokenBudget 100)", "Stopped: task_failed."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(fx.Provider.Calls) != 1 {
		t.Errorf("the second call should have been refused before the server; server calls = %d", len(fx.Provider.Calls))
	}
}

//   - Verify a run that reaches runTimeoutMinutes stops with "timeout" (not "interrupted") and exit 3, even while a
//     model call is in progress, and the report says so
func Test_Unit_AutoRun_RunTimeout_StopsTheRun(t *testing.T) {
	old := runTimeoutUnit
	runTimeoutUnit = 50 * time.Millisecond // [agent] a "minute" of 50ms
	t.Cleanup(func() { runTimeoutUnit = old })
	fx := newThreeTaskFixture(t)
	fx.Session.Config.RunTimeoutMinutes = 1
	fx.Provider.Gate = make(chan struct{}) // never opened: the worker's model call waits until the deadline

	out, err := runAutoYes(t, fx)

	if !errors.Is(err, ErrNeedsPerson) {
		t.Fatalf("autoMode: %v, want ErrNeedsPerson\n%s", err, out)
	}
	for _, want := range []string{"■ t1 stopped (timeout)", "Stopped: timeout.", "Time limit: running for"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if r := lastRunEnded(t, journal(t, fx, 1)); r.Reason != state.StopTimeout {
		t.Errorf("run_ended = %+v, want timeout", r)
	}
}

//   - Verify a resumed run carries its budget over: a crashed session's tokens (from the journal) count, so the resumed
//     worker's call goes through (900 of 1000) but the director after it is refused, and the same run (0001) stops with
//     "budget"
func Test_Unit_AutoResume_CarriesTheTokenBudgetOver(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Session.Config.RunTokenBudget = 1000
	// a crashed run 0001, written as openRun would: t1's attempt stopped with 900 tokens, then no run_ended
	home, err := state.OpenHome(fx.Session.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := state.NewPlanRef("", "plan.md")
	m, snapshot, err := buildManifest(fx.Session, nil, ref)
	if err != nil {
		t.Fatal(err)
	}
	planData, _ := os.ReadFile("plan.md")
	j, _, err := home.NewRun(m, planData, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	j.Append(state.EventTaskStarted, "t1", state.TaskStarted{Attempt: 1})
	j.Append(state.EventAttemptUsage, "t1", state.AttemptUsage{Attempt: 1, Usage: state.Usage{InputTokens: 800, OutputTokens: 100, Reported: true}})
	j.Append(state.EventTaskStarted, "t1", state.TaskStarted{Attempt: 2})
	j.Close()
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("made a")}
	fx.Provider.Usages = []model.Usage{used(150, 50)}

	out, err := runAutoYes(t, fx)

	if !errors.Is(err, ErrNeedsPerson) {
		t.Fatalf("autoMode: %v, want ErrNeedsPerson\n%s", err, out)
	}
	for _, want := range []string{"Resuming run 0001", "Stopped: budget.", "Token budget: 1100 of 1000 tokens used"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(fx.Provider.Calls) != 1 {
		t.Errorf("worker calls = %d, want 1 (900 < 1000 lets it start)", len(fx.Provider.Calls))
	}
}

// - Verify checkConfig accepts 0 (no limit) for the three budget keys and refuses a negative value, naming the key
func Test_Unit_CheckConfig_Budgets(t *testing.T) {
	if err := checkConfig(config.GetDefaultConfig(config.ModeAuto)); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for _, key := range []string{"runTokenBudget", "taskTokenBudget", "runTimeoutMinutes"} {
		cfg := config.GetDefaultConfig(config.ModeAuto)
		switch key {
		case "runTokenBudget":
			cfg.RunTokenBudget = -1
		case "taskTokenBudget":
			cfg.TaskTokenBudget = -1
		case "runTimeoutMinutes":
			cfg.RunTimeoutMinutes = -1
		}
		if err := checkConfig(cfg); err == nil || !strings.Contains(err.Error(), key+" must not be negative") {
			t.Errorf("%s -1: err = %v", key, err)
		}
	}
}
