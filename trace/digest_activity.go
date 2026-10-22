package trace

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ActivityDigest is the first Digester: "what did the agent do and change", for a quick human review (a PR review, an audit).
// Per task it lists the user input, then either the tool calls grouped into one block, or, for a text-only turn, a one-liner.
// It is built from tool_requested / tool_finished, never from model text, so it shows what the agent did, not what it claimed.
type ActivityDigest struct {
	run           runSummary
	tasks         []*taskSummary          // in order of first appearance
	byScope       map[string]*taskSummary // scope ID -> task
	calls         map[string]*pendingCall // tool_requested seen, waiting for its tool_finished; key = call_id
	redacted      int                     // total redaction rule hits in the whole trace
	modelErrs     int
	runIncomplete int // same, for run-level events (their scope is the run ID, which is not a task)
	compacts      int
}

type runSummary struct {
	id, provider, model, mode string
	status, errText           string // empty status = no run_end seen (the run is unfinished or crashed)
	durationMS                int64
}

type taskSummary struct {
	id, input       string
	status, errText string   // empty status = no task_end seen
	calls           []string // one described line per finished tool call, in finish order
	pending         int      // tool calls requested but never finished
	incomplete      int      // events of this task whose payload was replaced by a truncated / dropped marker
	lastReplyChars  int      // length of the model's last text reply in this task
	hasReply        bool
	effect          string // what a slash command did to the context, e.g. "context cleared (4 messages)"; empty for chat turns
}

// pendingCall is a tool call between tool_requested and tool_finished.
type pendingCall struct {
	name, args, scope string
}

const (
	maxInputChars = 100 // user input shown in a task heading
	maxArgsChars  = 80  // arguments shown by the generic describer
	maxShellChars = 300 // a shell command is shown in full up to this length
)

// NewActivityDigest returns an empty ActivityDigest.
func NewActivityDigest() *ActivityDigest {
	return &ActivityDigest{byScope: map[string]*taskSummary{}, calls: map[string]*pendingCall{}}
}

// task returns the task for a scope ID, making one if the task_start was not seen (e.g. a replay of a partial file).
func (a *ActivityDigest) task(scope string) *taskSummary {
	t, ok := a.byScope[scope]
	if !ok {
		t = &taskSummary{id: scope}
		a.byScope[scope] = t
		a.tasks = append(a.tasks, t)
	}
	return t
}

// Add folds one event into the digest. A payload that does not decode is skipped: the digest is a summary, L0 has the truth.
func (a *ActivityDigest) Add(ev Event) {
	a.redacted += len(ev.Redactions)
	if isMissingPayload(ev.EventData) {
		// The recorder swapped the payload for a marker. It would decode into any typed payload as all zero values
		// (a nameless tool_finished that matches nothing), so count the gap and do not read the event.
		if ev.ScopeID == ev.RunID {
			a.runIncomplete++
		} else {
			a.task(ev.ScopeID).incomplete++
		}
		return
	}
	switch ev.EventType {
	case RunStart:
		var d RunStartData
		if json.Unmarshal(ev.EventData, &d) == nil {
			a.run.id, a.run.provider, a.run.model, a.run.mode = ev.RunID, d.Provider, d.Model, d.Mode
		}
	case TaskStart:
		var d TaskStartData
		if json.Unmarshal(ev.EventData, &d) == nil {
			a.task(ev.ScopeID).input = d.Input
		}
	case ToolRequested:
		var d ToolRequestedData
		if json.Unmarshal(ev.EventData, &d) == nil {
			a.calls[d.CallID] = &pendingCall{name: d.Name, args: d.Arguments, scope: ev.ScopeID}
			a.task(ev.ScopeID).pending++
		}
	case ToolFinished:
		var d ToolFinishedData
		if json.Unmarshal(ev.EventData, &d) != nil {
			return
		}
		t := a.task(ev.ScopeID)
		if p, ok := a.calls[d.CallID]; ok {
			delete(a.calls, d.CallID)
			t.pending--
			t.calls = append(t.calls, describe(p.name, p.args, d.Content, d.Failed))
		} else { // finished without a request: still show it
			t.calls = append(t.calls, describe(d.Name, "", d.Content, d.Failed))
		}
	case ModelResponse:
		var d ModelResponseData
		if ev.ScopeID != ev.RunID && json.Unmarshal(ev.EventData, &d) == nil && len(d.Message.ToolCalls) == 0 {
			t := a.task(ev.ScopeID)
			t.hasReply, t.lastReplyChars = true, len([]rune(d.Message.Content))
		}
	case ModelError:
		a.modelErrs++
	case ContextCleared:
		var d ContextClearedData
		if json.Unmarshal(ev.EventData, &d) == nil {
			a.task(ev.ScopeID).effect = fmt.Sprintf("context cleared (%d %s)", d.MessagesDropped, plural(d.MessagesDropped, "message", "messages"))
		}
	case Compaction:
		a.compacts++
		var d CompactionData
		// a /compact is a task of its own; an auto compaction is recorded under the run ID, which is not a task, so it only counts
		if t, ok := a.byScope[ev.ScopeID]; ok && json.Unmarshal(ev.EventData, &d) == nil {
			t.effect = fmt.Sprintf("compaction %s: %d -> %d messages", d.Status, d.MessagesBefore, d.MessagesAfter)
		}
	case TaskEnd:
		var d TaskEndData
		if json.Unmarshal(ev.EventData, &d) == nil {
			t := a.task(ev.ScopeID)
			t.status, t.errText = d.Status, d.Error
		}
	case RunEnd:
		var d RunEndData
		if json.Unmarshal(ev.EventData, &d) == nil {
			a.run.status, a.run.errText, a.run.durationMS = d.Status, d.Error, d.DurationMS
		}
	}
}

// isMissingPayload reports whether data is one of the recorder's markers for a payload that could not be recorded:
// {"truncated":true,"original_bytes":N} (over traceMaxEventBytes) or {"dropped":"..."} (redaction failed).
// No real payload has either key.
func isMissingPayload(data json.RawMessage) bool {
	var m struct {
		Truncated bool   `json:"truncated"`
		Dropped   string `json:"dropped"`
	}
	return json.Unmarshal(data, &m) == nil && (m.Truncated || m.Dropped != "")
}

// Render writes the digest as markdown.
func (a *ActivityDigest) Render(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run %s\n\n", a.run.id)
	status := a.run.status
	if status == "" {
		status = "unfinished (no run_end)"
	}
	fmt.Fprintf(&b, "%s / %s, mode %s, %s, %.1fs", a.run.provider, a.run.model, a.run.mode, status, float64(a.run.durationMS)/1000)
	if a.run.errText != "" {
		fmt.Fprintf(&b, " (%s)", oneLine(a.run.errText, maxInputChars))
	}
	b.WriteString("\n")
	if a.runIncomplete > 0 {
		fmt.Fprintf(&b, "\n%d run-level %s had incomplete data (payload truncated or dropped); see the trace\n", a.runIncomplete, plural(a.runIncomplete, "event", "events"))
	}

	for _, t := range a.tasks {
		fmt.Fprintf(&b, "\n## %s\n\n", oneLine(t.input, maxInputChars))
		if t.effect != "" {
			fmt.Fprintf(&b, "%s\n", t.effect)
		}
		switch {
		case len(t.calls) > 0 || t.pending > 0:
			fmt.Fprintf(&b, "%d tool %s:\n", len(t.calls)+t.pending, plural(len(t.calls)+t.pending, "call", "calls"))
			for _, c := range t.calls {
				fmt.Fprintf(&b, "- %s\n", c)
			}
			if t.pending > 0 {
				if t.incomplete > 0 { // the finish event may be one of the missing ones
					fmt.Fprintf(&b, "- %d requested, result missing (trace incomplete)\n", t.pending)
				} else {
					fmt.Fprintf(&b, "- %d requested but never finished\n", t.pending)
				}
			}
		case t.effect != "":
			// the command's effect line above is the whole story
		case t.hasReply:
			fmt.Fprintf(&b, "answered, no tools (%d chars)\n", t.lastReplyChars)
		default:
			b.WriteString("no tools, no reply\n")
		}
		if t.incomplete > 0 {
			fmt.Fprintf(&b, "%d %s had incomplete data (payload truncated or dropped): the list above may be missing activity; see the trace\n", t.incomplete, plural(t.incomplete, "event", "events"))
		}
		if t.status != "" && t.status != "ok" {
			fmt.Fprintf(&b, "task %s", t.status)
			if t.errText != "" {
				fmt.Fprintf(&b, ": %s", oneLine(t.errText, maxInputChars))
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "\n---\nredactions: %d, model errors: %d, compactions: %d\n", a.redacted, a.modelErrs, a.compacts)
	_, err := io.WriteString(w, b.String())
	return err
}

// describe turns one finished tool call into its digest line, e.g. "read_file : read script1.py".
// Known tool names get a tailored line, everything else the generic one.
func describe(name, args, content string, failed bool) string {
	var line string
	switch name {
	case "read_file":
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &in) == nil && in.Path != "" {
			line = name + " : read " + in.Path
		}
	case "shell", "run_command", "bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(args), &in) == nil && in.Command != "" {
			line = name + " : " + oneLine(in.Command, maxShellChars)
		}
	}
	if line == "" {
		line = name + " : " + oneLine(args, maxArgsChars)
	}
	if failed {
		line += " (failed: " + oneLine(content, maxArgsChars) + ")"
	}
	return strings.Join(strings.Fields(line), " ")
}

// oneLine collapses whitespace (so a multi-line value stays one markdown line) and cuts to max characters.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
