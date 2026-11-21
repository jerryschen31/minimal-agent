package trace

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
)

// usageProvider is a model.Provider that always returns resp.
type usageProvider struct{ resp model.Response }

// Chat returns p.resp.
//
// Notes: none.
func (p usageProvider) Chat(context.Context, []model.ChatMessage, []model.ToolDef) (model.Response, error) {
	return p.resp, nil
}

// - Verify TracedProvider returns the wrapped provider's whole Response, so the usage and stop reason reach the callers
// above it (a wrapper that returned only the message would drop them)
func Test_Unit_TracedProvider_Chat_ReturnsWholeResponse(t *testing.T) {
	want := model.Response{
		Message:    model.ChatMessage{Role: "assistant", Content: "hi"},
		Usage:      model.Usage{InputTokens: 120, OutputTokens: 8, CacheReadTokens: 100, Reported: true},
		StopReason: model.StopEnd,
	}
	tp := GetTracedProvider(usageProvider{resp: want}, Nop(), "task-1", config.Config{})

	got, err := tp.Chat(context.Background(), []model.ChatMessage{{Role: "user", Content: "hello"}}, nil)
	if err != nil {
		t.Fatalf("Chat() returned an unexpected error: %v", err)
	}
	if got.Message.Content != want.Message.Content || got.Usage != want.Usage || got.StopReason != want.StopReason {
		t.Errorf("expected the wrapped Response unchanged %+v, got %+v", want, got)
	}
}

// - Verify the model_response event in the trace carries the call's usage and stop reason
func Test_Unit_TracedProvider_Chat_RecordsUsageAndStopReason(t *testing.T) {
	resp := model.Response{
		Message:    model.ChatMessage{Role: "assistant", Content: "hi"},
		Usage:      model.Usage{InputTokens: 120, OutputTokens: 8, CacheReadTokens: 100, Reported: true},
		StopReason: model.StopMaxTokens,
	}
	evs := scopeEvents(t, func(rec Recorder) {
		tp := GetTracedProvider(usageProvider{resp: resp}, rec, "task-1", config.Config{})
		if _, err := tp.Chat(context.Background(), []model.ChatMessage{{Role: "user", Content: "hello"}}, nil); err != nil {
			t.Fatalf("Chat() returned an unexpected error: %v", err)
		}
	})
	for _, ev := range evs {
		if ev.EventType != ModelResponse {
			continue
		}
		var d ModelResponseData
		if err := json.Unmarshal(ev.EventData, &d); err != nil {
			t.Fatalf("model_response payload does not decode: %v", err)
		}
		if d.Usage != resp.Usage || d.StopReason != model.StopMaxTokens {
			t.Errorf("expected usage %+v and stop %q in the trace, got %+v and %q", resp.Usage, model.StopMaxTokens, d.Usage, d.StopReason)
		}
		return
	}
	t.Fatalf("no model_response event recorded; got %d events", len(evs))
}

// - Verify the model_request event records the output limit from the config, and leaves it out when none is set
func Test_Unit_TracedProvider_Chat_RecordsMaxTokens(t *testing.T) {
	for _, maxTokens := range []int{512, 0} {
		evs := scopeEvents(t, func(rec Recorder) {
			tp := GetTracedProvider(usageProvider{}, rec, "task-1", config.Config{MaxTokens: maxTokens})
			if _, err := tp.Chat(context.Background(), []model.ChatMessage{{Role: "user", Content: "hello"}}, nil); err != nil {
				t.Fatalf("Chat() returned an unexpected error: %v", err)
			}
		})
		found := false
		for _, ev := range evs {
			if ev.EventType != ModelRequest {
				continue
			}
			found = true
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(ev.EventData, &raw); err != nil {
				t.Fatalf("model_request payload does not decode: %v", err)
			}
			got, present := raw["max_tokens"]
			if maxTokens > 0 && string(got) != "512" {
				t.Errorf("expected max_tokens 512 in model_request, got %q", got)
			}
			if maxTokens == 0 && present {
				t.Errorf("expected no max_tokens when none is set, got %s", got)
			}
		}
		if !found {
			t.Fatalf("no model_request event recorded")
		}
	}
}
