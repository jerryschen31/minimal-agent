package trace

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
)

// response builds a model_response payload with the given usage and stop reason; toolCall adds one tool call.
func response(usage model.Usage, stop string, toolCall bool) ModelResponseData {
	d := ModelResponseData{Message: MessageRecord{Role: "assistant", Content: "ok"}, StopReason: stop, Usage: usage}
	if toolCall {
		d.Message.ToolCalls = []ToolCallRecord{{CallID: "c", Name: "read_file", Arguments: "{}"}}
	}
	return d
}

// - Verify the digest shows each task's token use and a run total that also counts model calls outside any task
// (auto compaction, recorded under the run ID), plus how many replies were cut off at max tokens
func Test_Digest_TokensPerTaskAndRunTotal(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
		defer run.End()

		a := StartTask(rec, "task a")
		a.Record(ModelResponse, response(model.Usage{InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 800, Reported: true}, model.StopToolCalls, true))
		a.Record(ModelResponse, response(model.Usage{InputTokens: 1200, OutputTokens: 70, CacheReadTokens: 1000, Reported: true}, model.StopEnd, false))
		a.End()

		b := StartTask(rec, "task b")
		b.Record(ModelResponse, response(model.Usage{InputTokens: 300, OutputTokens: 100, Reported: true}, model.StopMaxTokens, false))
		b.End()

		run.Record(ModelResponse, response(model.Usage{InputTokens: 500, OutputTokens: 40, Reported: true}, model.StopEnd, false)) // auto compaction
	})
	for _, want := range []string{
		"## task a\n\nanswered, no tools (2 chars)\ntokens: 2200 in (1800 cached), 120 out, 2 model calls\n",
		"## task b\n\nanswered, no tools (2 chars)\ntokens: 300 in, 100 out, 1 model call\n",
		"tokens: 3000 in (1800 cached), 260 out, 4 model calls; 1 reply cut off at max tokens\n",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
}

// - Verify calls without usage are never shown as zero tokens: all unreported says "not reported", some unreported
// says how many, and a task with no model calls gets no tokens line
func Test_Digest_Tokens_UnreportedAndPartial(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
		defer run.End()

		none := StartTask(rec, "none reported")
		none.Record(ModelResponse, response(model.Usage{}, model.StopToolCalls, true))
		none.Record(ModelResponse, response(model.Usage{}, model.StopEnd, false))
		none.End()

		some := StartTask(rec, "some reported")
		some.Record(ModelResponse, response(model.Usage{InputTokens: 40, OutputTokens: 4, Reported: true}, model.StopToolCalls, true))
		some.Record(ModelResponse, response(model.Usage{}, model.StopEnd, false))
		some.End()

		cmd := StartTask(rec, "/clear")
		cmd.Record(ContextCleared, ContextClearedData{MessagesDropped: 3})
		cmd.End()
	})
	for _, want := range []string{
		"tokens: not reported (2 model calls)\n",
		"tokens: 40 in, 4 out, 2 model calls (1 reported no usage)\n",
		"tokens: 40 in, 4 out, 4 model calls (3 reported no usage)\n", // run total
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
	_, clearSection, _ := strings.Cut(digest, "## /clear")
	clearSection, _, _ = strings.Cut(clearSection, "\n---\n") // the run footer follows the last task
	if strings.Contains(clearSection, "tokens:") {
		t.Errorf("a task with no model calls must have no tokens line:\n%s", digest)
	}
	if strings.Contains(digest, "cut off") {
		t.Errorf("no reply was cut off, but the digest says so:\n%s", digest)
	}
}

// - Verify a model_response from a trace written before usage was read (no "reported" key, zero counts) still
// renders, as "not reported"
func Test_Digest_Tokens_OldTraceWithoutReported(t *testing.T) {
	d := NewActivityDigest()
	d.Add(Event{RunID: "r", ScopeID: "t1", EventType: TaskStart, EventData: []byte(`{"input":"old task"}`)})
	d.Add(Event{RunID: "r", ScopeID: "t1", EventType: ModelResponse,
		EventData: []byte(`{"message":{"role":"assistant","content":"hi"},"usage":{"input_tokens":0,"output_tokens":0},"duration_ms":5}`)})
	var out bytes.Buffer
	if err := d.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tokens: not reported (1 model call)") {
		t.Errorf("expected the old response to count as not reported:\n%s", out.String())
	}
}
