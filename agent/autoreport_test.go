package agent

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/state"
)

// reportRow finds the report row of task id and returns its whitespace-separated fields joined by one space.
func reportRow(t *testing.T, report, id string) string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == id {
			return strings.Join(f, " ")
		}
	}
	t.Fatalf("no row for %s in:\n%s", id, report)
	return ""
}

//   - Verify usageReport: one row per plan task over all runs (status, attempts, tokens, time, and the run that did it
//     when not this one), "not started" for a task with no attempts, "tokens not reported" when none was reported, and
//     this run's total; the all-runs line only when there was more than one run
func Test_Unit_UsageReport(t *testing.T) {
	p, _, err := plan.Cleanup([]byte("# Plan: x\n\n## Tasks\n\n### t1: a\n\n### t2: b\n\n### t3: c\n\n### t4: d\n\n### t5: e\n"))
	if err != nil {
		t.Fatal(err)
	}
	st := state.PlanState{
		Runs: []state.Run{{Number: 1}, {Number: 2}},
		Tasks: map[string]state.TaskState{
			"t1": {Status: state.TaskDone, Attempts: 1, Run: 1, Running: 38 * time.Second,
				Usage: state.Usage{InputTokens: 4471, OutputTokens: 90, CacheReadTokens: 1335, Reported: true}},
			"t2": {Status: state.TaskDone, Attempts: 2, Run: 2, Running: 72 * time.Second,
				Usage: state.Usage{InputTokens: 12340, OutputTokens: 410, Reported: true}},
			"t3": {Status: state.TaskFailed, Attempts: 1, Run: 2}, // no usage reported, no ended attempt
			"t5": {Status: state.TaskDone, Attempts: 1, Run: 2, Running: time.Second, // some calls reported none
				Usage: state.Usage{InputTokens: 100, OutputTokens: 10, Reported: true, UnreportedCalls: 2}},
		},
		Latest:       state.RunState{Usage: state.Usage{InputTokens: 12340, OutputTokens: 410, Reported: true}, Running: 75 * time.Second},
		Total:        state.Usage{InputTokens: 16811, OutputTokens: 500, CacheReadTokens: 1335, Reported: true},
		TotalRunning: 2 * time.Minute,
	}
	report := usageReport(st, p, 2)

	for id, want := range map[string]string{
		"t1": "t1 done 1 4561 tokens: 4471 in (1335 cached), 90 out 38s (run 0001)",
		"t2": "t2 done 2 12750 tokens: 12340 in, 410 out 1m12s",
		"t3": "t3 failed 1 tokens not reported —",
		"t4": "t4 not started",
		"t5": "t5 done 1 110 tokens: 100 in, 10 out (+2 calls with no usage) 1s",
	} {
		if got := reportRow(t, report, id); got != want {
			t.Errorf("row %s = %q, want %q", id, got, want)
		}
	}
	for _, want := range []string{"this run: 12750 tokens: 12340 in, 410 out, running 1m15s", "all runs: 17311 tokens: 16811 in (1335 cached), 500 out, running 2m0s (2 runs)"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	st.Runs = st.Runs[:1]
	if strings.Contains(usageReport(st, p, 2), "all runs") {
		t.Errorf("one run: expected no all-runs line")
	}
}

//   - Verify a full run prints the report from the journal: each task's tokens, this run's total, and the done line's
//     tokens and time
func Test_Unit_AutoRun_PrintsTheUsageReport(t *testing.T) {
	fx := newThreeTaskFixture(t)
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantText("made a"), modeltest.AssistantText("made b"), modeltest.AssistantText("made c"),
	}
	fx.Provider.Usages = []model.Usage{used(500, 100), used(300, 50), used(200, 25)}

	out, err := runAutoYes(t, fx)
	if err != nil {
		t.Fatalf("autoMode: %v\n%s", err, out)
	}
	for id, want := range map[string]string{"t1": "600 tokens: 500 in, 100 out", "t2": "350 tokens: 300 in, 50 out", "t3": "225 tokens: 200 in, 25 out"} {
		if row := reportRow(t, out, id); !strings.HasPrefix(row, id+" done 1 "+want) {
			t.Errorf("row %s = %q, want it to start %q", id, row, id+" done 1 "+want)
		}
	}
	// the scripted director reports no usage: the sums say so (PR #46 review), here 3 director calls
	if !strings.Contains(out, "this run: 1175 tokens: 1000 in, 175 out (+3 calls with no usage), running") || strings.Contains(out, "all runs") {
		t.Errorf("expected this run's total (and no all-runs line for one run):\n%s", out)
	}
	if !regexp.MustCompile(`✓ t1 done — checked \(600 tokens \(\+1 call with no usage\), [^)]+\)`).MatchString(out) {
		t.Errorf("expected the done line to carry the attempt's tokens and time:\n%s", out)
	}
}

//   - Verify /usage reports the session's calls and tokens (every turn counted), says "no model calls yet" before any,
//     and notes calls that reported no usage
func Test_Unit_Chat_UsageCommand(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	handleUserInput(context.Background(), fx.Session, "/usage")
	if !strings.Contains(fx.Out.String(), "this session: no model calls yet") {
		t.Errorf("before any call:\n%s", fx.Out.String())
	}
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("one"), modeltest.AssistantText("two"), modeltest.AssistantText("three")}
	fx.Provider.Usages = []model.Usage{used(100, 10), {InputTokens: 200, OutputTokens: 20, CacheReadTokens: 150, Reported: true}}
	for _, line := range []string{"hello", "again", "and again"} { // the third reply has no usage
		handleUserInput(context.Background(), fx.Session, line)
	}
	fx.Out.Reset()
	handleUserInput(context.Background(), fx.Session, "/usage")
	if want := "this session: 3 model calls, 330 tokens: 300 in (150 cached), 30 out (1 reported no usage)"; !strings.Contains(fx.Out.String(), want) {
		t.Errorf("expected %q, got:\n%s", want, fx.Out.String())
	}
}

// - Verify the meter is safe for concurrent use (chat's auto-compaction counts from another goroutine); run with -race
func Test_Unit_UsageMeter_ConcurrentAdd(t *testing.T) {
	m := &usageMeter{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				m.add(used(1, 1))
				m.snapshot()
				m.check()
			}
		})
	}
	wg.Wait()
	if u, calls, _ := m.snapshot(); calls != 800 || u.Total() != 1600 {
		t.Errorf("calls %d, total %d; want 800 and 1600", calls, u.Total())
	}
}
