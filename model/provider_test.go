package model

import (
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

// - Verify that SetupProvider correctly returns a configured OpenAI provider.
func Test_Unit_SetupProvider_OpenAI_ReturnsConfiguredProvider(t *testing.T) {
	cfg := config.Config{
		Provider: "openai",
		Model:    "gpt-4o-mini",
		BaseURL:  "https://example.test/v1",
	}

	provider, err := SetupProvider(cfg)
	if err != nil {
		t.Fatalf("SetupProvider() returned an error: %v", err)
	}

	openaiProvider, ok := provider.(*OpenAICompat)
	if !ok {
		t.Fatalf("expected a *OpenAICompat, got %T", provider)
	}
	if openaiProvider.Model != cfg.Model {
		t.Errorf("expected Model %q, got %q", cfg.Model, openaiProvider.Model)
	}
	if openaiProvider.BaseURL != cfg.BaseURL {
		t.Errorf("expected BaseURL %q, got %q", cfg.BaseURL, openaiProvider.BaseURL)
	}
}

// - Verify that SetupProvider returns an error for an unsupported provider.
func Test_Unit_SetupProvider_UnsupportedProvider_ReturnsError(t *testing.T) {
	provider, err := SetupProvider(config.Config{Provider: "not-a-real-provider"})

	if err == nil {
		t.Errorf("expected an error for an unsupported provider, got nil")
	}
	if provider != nil {
		t.Errorf("expected a nil provider for an unsupported provider, got %v", provider)
	}
}

// - Verify PrepareChatRequest orders the request as: system message, then context, then this turn's messages.
func Test_Unit_PrepareChatRequest_OrdersSystemThenContextThenTurn(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system"}
	ctxMsgs := []ChatMessage{{ID: "c1", Role: "user"}, {ID: "c2", Role: "assistant"}}
	turn := []ChatMessage{{ID: "t1", Role: "user"}, {ID: "t2", Role: "assistant"}}

	got := PrepareChatRequest(ctxMsgs, system, turn)

	wantIDs := []string{"sys", "c1", "c2", "t1", "t2"}
	if len(got) != len(wantIDs) {
		t.Fatalf("expected %d messages, got %d: %+v", len(wantIDs), len(got), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("expected message %d to have ID %q, got %q", i, id, got[i].ID)
		}
	}
}

// - Verify PrepareChatRequest with no context and no turn messages returns just the system message.
func Test_Unit_PrepareChatRequest_EmptyContextAndTurn_ReturnsOnlySystem(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system"}

	got := PrepareChatRequest(nil, system, nil)

	if len(got) != 1 || got[0].ID != "sys" {
		t.Errorf("expected only the system message, got %+v", got)
	}
}

// - Verify the request is a new slice: changing it must not modify the caller's context or turn slices.
func Test_Unit_PrepareChatRequest_DoesNotAliasInputs(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system"}
	ctxMsgs := []ChatMessage{{ID: "c1", Content: "original context"}}
	turn := []ChatMessage{{ID: "t1", Content: "original turn"}}

	got := PrepareChatRequest(ctxMsgs, system, turn)
	got[1].Content = "mutated"
	got[2].Content = "mutated"

	if ctxMsgs[0].Content != "original context" {
		t.Errorf("expected the context slice to be unchanged, got %q", ctxMsgs[0].Content)
	}
	if turn[0].Content != "original turn" {
		t.Errorf("expected the turn slice to be unchanged, got %q", turn[0].Content)
	}
}
