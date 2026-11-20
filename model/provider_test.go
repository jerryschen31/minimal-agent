package model

import (
	"testing"
	"time"

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
	system := ChatMessage{ID: "sys", Role: "system", Content: "fake system prompt"}
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
	system := ChatMessage{ID: "sys", Role: "system", Content: "fake system prompt"}

	got := PrepareChatRequest(nil, system, nil)

	if len(got) != 1 || got[0].ID != "sys" {
		t.Errorf("expected only the system message, got %+v", got)
	}
}

// - Verify the request is a new slice: changing it must not modify the caller's context or turn slices.
func Test_Unit_PrepareChatRequest_DoesNotAliasInputs(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system", Content: "fake system prompt"}
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

// - Verify an empty system message is left out: the request is exactly the context then this turn, with no system role.
func Test_Unit_PrepareChatRequest_EmptySystem_LeftOut(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system", Content: ""}
	ctxMsgs := []ChatMessage{{ID: "c1", Role: "user"}, {ID: "c2", Role: "assistant"}}
	turn := []ChatMessage{{ID: "t1", Role: "user"}, {ID: "t2", Role: "assistant"}}

	got := PrepareChatRequest(ctxMsgs, system, turn)

	wantIDs := []string{"c1", "c2", "t1", "t2"}
	if len(got) != len(wantIDs) {
		t.Fatalf("expected %d messages, got %d: %+v", len(wantIDs), len(got), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("expected message %d to have ID %q, got %q", i, id, got[i].ID)
		}
		if got[i].Role == "system" {
			t.Errorf("expected no system message, found one at index %d", i)
		}
	}
}

// - Verify a whitespace-only system message is left out the same way as an empty one.
func Test_Unit_PrepareChatRequest_WhitespaceOnlySystem_LeftOut(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system", Content: "  \n\t "}
	ctxMsgs := []ChatMessage{{ID: "c1", Role: "user"}}
	turn := []ChatMessage{{ID: "t1", Role: "user"}}

	got := PrepareChatRequest(ctxMsgs, system, turn)

	if len(got) != 2 || got[0].ID != "c1" || got[1].ID != "t1" {
		t.Errorf("expected only [c1 t1], got %+v", got)
	}
	for i, m := range got {
		if m.Role == "system" {
			t.Errorf("expected no system message, found one at index %d", i)
		}
	}
}

// - Verify an empty system message with no context and no turn messages gives an empty request.
func Test_Unit_PrepareChatRequest_EmptySystemContextAndTurn_ReturnsEmpty(t *testing.T) {
	got := PrepareChatRequest(nil, ChatMessage{ID: "sys", Role: "system"}, nil)

	if len(got) != 0 {
		t.Errorf("expected an empty request, got %+v", got)
	}
}

// - Verify that with an empty system message the request is still a new slice: changing it must not modify the caller's
// slices, and building it must not write into the context slice's spare capacity (a context with room to grow is
// what would let a plain append(context, turn...) share the caller's array).
func Test_Unit_PrepareChatRequest_EmptySystem_DoesNotAliasInputs(t *testing.T) {
	system := ChatMessage{ID: "sys", Role: "system"}
	ctxMsgs := make([]ChatMessage, 1, 4)
	ctxMsgs[0] = ChatMessage{ID: "c1", Content: "original context"}
	turn := []ChatMessage{{ID: "t1", Content: "original turn"}}

	got := PrepareChatRequest(ctxMsgs, system, turn)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(got), got)
	}
	got[0].Content = "mutated"
	got[1].Content = "mutated"

	if ctxMsgs[0].Content != "original context" {
		t.Errorf("expected the context slice to be unchanged, got %q", ctxMsgs[0].Content)
	}
	if turn[0].Content != "original turn" {
		t.Errorf("expected the turn slice to be unchanged, got %q", turn[0].Content)
	}
	if spare := ctxMsgs[:cap(ctxMsgs)][1]; spare.ID != "" {
		t.Errorf("expected the context slice's spare capacity to be untouched, got %+v", spare)
	}
}

// - Verify SetupProvider turns the config's responseTimeoutSeconds into the provider's Timeout
func Test_Unit_SetupProvider_OpenAI_SetsTimeoutFromConfig(t *testing.T) {
	provider, err := SetupProvider(config.Config{Provider: "openai", BaseURL: "https://example.test/v1", ResponseTimeoutSeconds: 42})
	if err != nil {
		t.Fatalf("SetupProvider() returned an error: %v", err)
	}
	p, ok := provider.(*OpenAICompat)
	if !ok {
		t.Fatalf("expected a *OpenAICompat, got %T", provider)
	}
	if p.Timeout != 42*time.Second {
		t.Errorf("expected Timeout 42s, got %v", p.Timeout)
	}
}

// - Verify Usage.Add sums every count and is Reported when either part was
func Test_Unit_Usage_Add_SumsCountsAndReported(t *testing.T) {
	a := Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 80, CacheWriteTokens: 5, Reported: true}
	b := Usage{InputTokens: 50, OutputTokens: 4, CacheReadTokens: 20, CacheWriteTokens: 1, Reported: true}

	got := a.Add(b)
	want := Usage{InputTokens: 150, OutputTokens: 14, CacheReadTokens: 100, CacheWriteTokens: 6, Reported: true}
	if got != want {
		t.Errorf("expected %+v, got %+v", want, got)
	}

	// a call with no usage adds nothing and does not make a reported sum unknown
	if got := a.Add(Usage{}); got != a {
		t.Errorf("adding an unreported Usage: expected %+v, got %+v", a, got)
	}
	if got := (Usage{}).Add(Usage{}); got.Reported {
		t.Errorf("a sum of unreported calls must stay unreported, got %+v", got)
	}
}

// - Verify Usage.Total is input plus output, without counting the cache fields twice
func Test_Unit_Usage_Total_InputPlusOutput(t *testing.T) {
	u := Usage{InputTokens: 1200, OutputTokens: 85, CacheReadTokens: 1024, CacheWriteTokens: 50, Reported: true}
	if got := u.Total(); got != 1285 {
		t.Errorf("expected 1285 (input 1200 + output 85; cache tokens are inside input), got %d", got)
	}
}
