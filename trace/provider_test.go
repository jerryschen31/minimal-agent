package trace

import (
	"context"
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
