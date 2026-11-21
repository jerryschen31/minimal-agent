package agent

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/state"
)

// usageReport returns the end-of-run table of tokens and running time: one row per task of p (over all runs), then this
// run's total and, when the plan has had more than one run, the total over all runs.
//
// Notes:
//   - Built from st (state.Rebuild of the journals after run_ended), not from memory, so a resumed run includes what its
//     crashed sessions recorded.
//   - A task's row covers every run, so the rows need not add up to "this run" (a task done earlier is listed with the
//     run that did it, and resume work outside tasks counts only in the totals).
//   - A task with attempts but no reported usage shows "not reported" (a server that sends none, or a journal older
//     than token tracking); a time of 0 (no ended attempt) shows "—".
func usageReport(st state.State, p plan.Plan, run int) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  task\tstatus\tattempts\ttokens\ttime\t")
	for _, t := range p.Tasks {
		ts, ok := st.Tasks[t.ID]
		if !ok || ts.Attempts == 0 {
			fmt.Fprintf(tw, "  %s\tnot started\t\t\t\t\n", t.ID)
			continue
		}
		note := ""
		if ts.Run != run {
			note = fmt.Sprintf("(run %04d)", ts.Run)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%s\t%s\n", t.ID, ts.Status, ts.Attempts, usageText(fromStateUsage(ts.Usage)), durationText(ts.Running), note)
	}
	tw.Flush()
	fmt.Fprintf(&b, "  this run: %s, running %s\n", usageText(fromStateUsage(st.Latest.Usage)), durationText(st.Latest.Running))
	if len(st.Runs) > 1 {
		fmt.Fprintf(&b, "  all runs: %s, running %s (%d runs)\n", usageText(fromStateUsage(st.Total)), durationText(st.TotalRunning), len(st.Runs))
	}
	return b.String()
}

// usageText renders token usage for a person: "4651 tokens: 4561 in (1335 cached), 90 out", or "tokens not reported".
//
// Notes:
//   - The total (input + output) comes first: it is the number a token budget counts, so it can be compared with the
//     budget line directly.
//   - The cache counts are shown only when above 0; both are already inside "in".
func usageText(u model.Usage) string {
	if !u.Reported {
		return "tokens not reported"
	}
	var cache []string
	if u.CacheReadTokens > 0 {
		cache = append(cache, fmt.Sprintf("%d cached", u.CacheReadTokens))
	}
	if u.CacheWriteTokens > 0 {
		cache = append(cache, fmt.Sprintf("%d written to cache", u.CacheWriteTokens))
	}
	in := fmt.Sprintf("%d in", u.InputTokens)
	if len(cache) > 0 {
		in += " (" + strings.Join(cache, ", ") + ")"
	}
	return fmt.Sprintf("%d tokens: %s, %d out", u.Total(), in, u.OutputTokens)
}

// durationText renders a running time to the second ("1m12s"), or "—" for none.
//
// Notes: none.
func durationText(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return d.Round(time.Second).String()
}

// sessionUsageLine is /usage's answer: the session's model calls and their tokens so far.
//
// Notes:
//   - m nil or no calls → "no model calls yet". Calls without usage are counted and said ("K reported no usage").
//   - Chat and plan mode count every call of the session: turns, /summary, /compact and auto-compaction. Nothing is
//     kept after the program ends (the trace keeps every call).
func sessionUsageLine(m *usageMeter) string {
	if m == nil {
		return "this session: no model calls yet"
	}
	used, calls, unreported := m.snapshot()
	if calls == 0 {
		return "this session: no model calls yet"
	}
	noun := "calls"
	if calls == 1 {
		noun = "call"
	}
	line := fmt.Sprintf("this session: %d model %s, %s", calls, noun, usageText(used))
	if used.Reported && unreported > 0 {
		line += fmt.Sprintf(" (%d reported no usage)", unreported)
	}
	return line
}
