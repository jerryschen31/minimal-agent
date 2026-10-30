package contextwindow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
)

// - An operation on messages returned by GetMessages() method for ChatContext does not modify the current context
// (renamed from "...ChatHistory": GetMessages() here is ChatContext's, not ChatHistory's — same
// naming fix already applied to the compaction tests, see DECISIONS.md.)
func Test_Unit_OpOnGetMessagesDoesNotModifyContext(t *testing.T) {
	window := NewOffsetWindow(10)
	chatContext, err := NewChatContext(window, 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(modeltest.Msgs(2))

	messages := chatContext.GetMessages()
	messages[0].Content = "mutated"
	messages = append(messages, modeltest.Msg("user", "extra", "should not leak back"))

	after := chatContext.GetMessages()
	if len(after) != 2 {
		t.Fatalf("expected context to still have 2 messages, got %d", len(after))
	}
	if after[0].Content == "mutated" {
		t.Errorf("expected mutating the returned slice not to affect the context's internal state")
	}
}

// - An operation on messages returned by Snapshot() method for ChatContext does not modify the current context
func Test_Unit_OpOnSnapshotDoesNotModifyContext(t *testing.T) {
	window := NewOffsetWindow(10)
	chatContext, err := NewChatContext(window, 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(modeltest.Msgs(2))

	state := chatContext.Snapshot()
	state.Messages[0].Content = "mutated"
	state.Messages = append(state.Messages, modeltest.Msg("user", "extra", "should not leak back"))

	after := chatContext.GetMessages()
	if len(after) != 2 {
		t.Fatalf("expected context to still have 2 messages, got %d", len(after))
	}
	if after[0].Content == "mutated" {
		t.Errorf("expected mutating the snapshot's Messages slice not to affect the context's internal state")
	}
}

// Test_Unit_ChatContext_RemoveLast_DelegatesToWindow confirms the ChatContext pass-through
// wrapper works, built via the real NewChatContext constructor rather than a raw struct literal.
func Test_Unit_ChatContext_RemoveLast_DelegatesToWindow(t *testing.T) {
	forEachWindow(t, 10, func(t *testing.T, w ContextWindow) {
		chatContext, err := NewChatContext(w, 100)
		if err != nil {
			t.Fatalf("NewChatContext() returned an error: %v", err)
		}

		messages := modeltest.Msgs(3)
		chatContext.AddMessages(messages)

		chatContext.RemoveLast(1)

		expected := messages[:len(messages)-1]
		actual := chatContext.GetMessages()
		if len(actual) != len(expected) {
			t.Fatalf("expected %d messages after removal, got %d", len(expected), len(actual))
		}
		for i := range expected {
			if actual[i].ID != expected[i].ID {
				t.Errorf("expected message at index %d to be %+v, got %+v", i, expected[i], actual[i])
			}
		}
	})
}

// - Verify that SetupChatContext computes the expected auto-compaction threshold from the default
// config: a window of contextWindowMessages (no hidden buffer) that needs compaction at
// int(contextWindowMessages * autoCompactThreshold) messages
func Test_Unit_SetupChatContext_ComputesExpectedThreshold(t *testing.T) {
	cfg := config.GetDefaultConfig()
	chatContext, err := SetupChatContext(cfg)
	if err != nil {
		t.Fatalf("SetupChatContext() returned an error: %v", err)
	}

	wantMaxSize := cfg.ContextWindowMessages
	if chatContext.GetMaxSize() != wantMaxSize {
		t.Fatalf("expected max size %d, got %d", wantMaxSize, chatContext.GetMaxSize())
	}

	// ChatContext doesn't expose the raw threshold value directly, so exercise it indirectly
	// via IsAutoCompactionNeeded at the boundary.
	wantThreshold := int(float64(wantMaxSize) * cfg.AutoCompactThreshold)
	chatContext.AddMessages(modeltest.Msgs(wantThreshold - 1))
	if chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction not to be needed yet at %d messages (threshold %d)", wantThreshold-1, wantThreshold)
	}
	chatContext.AddMessages(modeltest.Msgs(1))
	if !chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction to be needed at %d messages (threshold %d)", wantThreshold, wantThreshold)
	}
}

// - Verify that ClampToMax does not modify the message slice if it is under the maximum capacity
func Test_Unit_ClampToMax_UnderCapacity_ReturnsUnchanged(t *testing.T) {
	messages := modeltest.Msgs(3)
	max := 5
	clamped := clampToMax(messages, 1, max)
	if len(clamped) != len(messages) {
		t.Errorf("expected clamped slice to have length %d, got %d", len(messages), len(clamped))
	}
	for i := range clamped {
		if clamped[i].ID != messages[i].ID {
			t.Errorf("expected message at index %d to be unchanged", i)
		}
	}
}

// - Verify that ClampToMax does not modify the message slice if it is exactly at the maximum capacity
func Test_Unit_ClampToMax_ExactlyAtCapacity_ReturnsUnchanged(t *testing.T) {
	messages := modeltest.Msgs(5)
	max := 5
	clamped := clampToMax(messages, 1, max)
	if len(clamped) != len(messages) {
		t.Errorf("expected clamped slice to have length %d, got %d", len(messages), len(clamped))
	}
	for i := range clamped {
		if clamped[i].ID != messages[i].ID {
			t.Errorf("expected message at index %d to be unchanged", i)
		}
	}
}

// - Verify that ClampToMax truncates the message slice if it exceeds the maximum capacity, keeping the summary and newest survivors
func Test_Unit_ClampToMax_OverCapacity_KeepsSummaryAndNewestSurvivors(t *testing.T) {
	messages := modeltest.Msgs(8) // even, so the oldest survivor kept ("4") is a user message, not a mid-turn assistant

	max := 5
	clamped := clampToMax(messages, 1, max)
	if len(clamped) != max {
		t.Errorf("expected clamped slice to have length %d, got %d", max, len(clamped))
	}
	// assuming the first message is the summary and the last (max-1) messages are the newest survivors
	if clamped[0].ID != messages[0].ID {
		t.Errorf("expected the first message (summary) to be unchanged")
	}
	for i := 1; i < max; i++ {
		if clamped[i].ID != messages[len(messages)-max+i].ID {
			t.Errorf("expected message at index %d to be one of the newest survivors", i)
		}
	}
}

// - Verify that context compaction does not remove messages added after compaction started but before compaction completes.
func Test_ContextCompaction_DoesNotRemoveMessagesAddedDuringCompaction(t *testing.T) {
	// tested directly against ChatContext.Compact rather than through handleUserInput: the
	// fakeProvider is synchronous, so there's no way to make a "real" message arrive mid-flight
	// through the normal call path. This reproduces the scenario compactChatContext protects
	// against by hand: snapshot, then simulate a concurrent add, then compact against the stale
	// snapshot.
	window := NewOffsetWindow(10)
	chatContext, err := NewChatContext(window, 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}

	chatContext.AddMessages(modeltest.Msgs(2)) // IDs "0","1"
	state := chatContext.Snapshot()

	// simulate a message arriving while compaction's summarization call is in flight
	chatContext.AddMessages([]model.ChatMessage{modeltest.Msg("user", "2", "arrived during compaction")})

	summaryMsg := modeltest.Msg("user", "summary", "a summary")
	if !chatContext.Compact(state, []model.ChatMessage{summaryMsg}) {
		t.Fatalf("expected Compact() to succeed")
	}

	found := false
	for _, m := range chatContext.GetMessages() {
		if m.ID == "2" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the message added during compaction (ID %q) to survive, got %+v", "2", chatContext.GetMessages())
	}
}

// - Verify that compacting an empty context returns an error and leaves the context unchanged.
func Test_ContextCompaction_CompactingEmptyContextReturnsError(t *testing.T) {
	chatContext, err := NewChatContext(NewOffsetWindow(10), 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	provider := &modeltest.FakeProvider{Reply: "ok"}

	_, err = CompactChatContext(context.Background(), chatContext, provider, fakeSummarizerPrompt, "")
	if err == nil {
		t.Errorf("expected compacting an empty context to return an error")
	}
	if len(provider.Calls) != 0 {
		t.Errorf("expected no provider call when there's nothing to summarize, got %d", len(provider.Calls))
	}
	if chatContext.GetSize() != 0 {
		t.Errorf("expected the context to stay empty, got %+v", chatContext.GetMessages())
	}
}

// - Verify clampToMax drops survivors that start with a tool result whose tool call was clamped away,
// and keeps dropping up to the next user message (so the assistant reply after it goes too)
func Test_Unit_ClampToMax_LeadingToolSurvivor_DropsUpToNextUser(t *testing.T) {
	// [summary, call, tool, final, user] clamped to 4 keeps the summary and the last 3,
	// which would start at the tool result
	messages := append([]model.ChatMessage{{ID: "summary", Role: "user", Type: "summary"}}, toolTurn("A", 1)[1:]...)
	messages = append(messages, model.ChatMessage{ID: "B-user", Role: "user"})

	clamped := clampToMax(messages, 1, 4)

	got := idsOf(clamped)
	want := []string{"summary", "B-user"}
	if !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	assertNoOrphanedToolMsgs(t, clamped)
}

// - Verify clampToMax keeps just the summary when every survivor it would keep is a tool result
func Test_Unit_ClampToMax_OnlyToolSurvivors_KeepsOnlySummary(t *testing.T) {
	messages := []model.ChatMessage{
		{ID: "summary", Role: "user", Type: "summary"},
		{ID: "call", Role: "assistant", ToolCalls: []model.ToolCall{{ID: "call"}}},
		{ID: "tool0", Role: "tool", ToolCallID: "call"},
		{ID: "tool1", Role: "tool", ToolCallID: "call"},
	}

	clamped := clampToMax(messages, 1, 3)

	if got := idsOf(clamped); !slices.Equal(got, []string{"summary"}) {
		t.Errorf("expected only the summary, got %v", got)
	}
}

// - Verify compaction end to end: when the summary pair plus the messages that arrived during
// compaction overflow the window, and the clamp cuts between a tool call and its result, the
// orphaned result and the rest of that turn are dropped rather than left after the summary pair
func Test_ContextCompaction_ClampCutsBetweenToolCallAndResult_NoOrphan(t *testing.T) {
	chatContext, err := NewChatContext(NewOffsetWindow(6), 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	state := chatContext.Snapshot() // empty: everything added next counts as "arrived during compaction"

	// two whole turns fill the window: A-user, A-call0, A-tool0, A-final, B-user, B-final
	chatContext.AddMessages(toolTurn("A", 1))
	chatContext.AddMessages(toolTurn("B", 0))

	// [request, summary, + the 6 above] clamped to 6 keeps the pair and the last 4, starting at A-tool0
	pair := []model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary"},
		{ID: "summary", Role: "assistant", Type: "summary"},
	}
	if !chatContext.Compact(state, pair) {
		t.Fatalf("expected Compact() to succeed")
	}

	got := chatContext.GetMessages()
	want := []string{"summary-request", "summary", "B-user", "B-final"}
	if !slices.Equal(idsOf(got), want) {
		t.Errorf("expected %v, got %v", want, idsOf(got))
	}
	assertNoOrphanedToolMsgs(t, got)
}

// - Verify SummarizeChatContext puts the user's instructions in the one leading system message: the
// request is exactly [system (prompt + instructions), user (transcript)], with no trailing system message
func Test_Unit_SummarizeChatContext_InstructionsGoInTheOneSystemMessage(t *testing.T) {
	provider := &modeltest.FakeProvider{Reply: "a summary"}
	const instructions = "keep only the fake test discussion"

	if _, err := SummarizeChatContext(context.Background(), provider, modeltest.Msgs(2), fakeSummarizerPrompt, instructions); err != nil {
		t.Fatalf("SummarizeChatContext() returned an error: %v", err)
	}

	if len(provider.Calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(provider.Calls))
	}
	req := provider.Calls[0]
	if len(req) != 2 {
		t.Fatalf("expected a 2-message request, got %d: %+v", len(req), req)
	}
	for i, m := range req {
		if (m.Role == "system") != (i == 0) {
			t.Errorf("expected only message 0 to be a system message, got message %d with role %q", i, m.Role)
		}
	}
	if !strings.Contains(req[0].Content, fakeSummarizerPrompt) || !strings.Contains(req[0].Content, instructions) {
		t.Errorf("expected the system message to hold both the summarize prompt and the instructions, got %q", req[0].Content)
	}
	if req[1].Role != "user" || !strings.HasPrefix(req[1].Content, "<transcript>") {
		t.Errorf("expected message 1 to be the user transcript, got %+v", req[1])
	}
}

// - Verify CompactChatContext replaces the context with a [user summary request, assistant summary] pair,
// both typed "summary" with distinct IDs, and returns the assistant message
func Test_ContextCompaction_ReplacesContextWithUserAssistantSummaryPair(t *testing.T) {
	chatContext, err := NewChatContext(NewOffsetWindow(10), 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(modeltest.Msgs(4))
	provider := &modeltest.FakeProvider{Reply: "a fake summary"}

	returned, err := CompactChatContext(context.Background(), chatContext, provider, fakeSummarizerPrompt, "")
	if err != nil {
		t.Fatalf("CompactChatContext() returned an error: %v", err)
	}

	got := chatContext.GetMessages()
	if len(got) != 2 {
		t.Fatalf("expected the context to be exactly the summary pair, got %+v", got)
	}
	request, summary := got[0], got[1]
	if request.Role != "user" || request.Content != SummaryRequest || request.Type != "summary" {
		t.Errorf("expected message 0 to be the user summary request, got %+v", request)
	}
	if summary.Role != "assistant" || summary.Content != "a fake summary" || summary.Type != "summary" {
		t.Errorf("expected message 1 to be the assistant summary, got %+v", summary)
	}
	if request.ID == "" || summary.ID == "" || request.ID == summary.ID {
		t.Errorf("expected distinct non-empty IDs, got %q and %q", request.ID, summary.ID)
	}
	if returned.ID != summary.ID || returned.Content != summary.Content || returned.Role != "assistant" {
		t.Errorf("expected the returned message to be the assistant summary %+v, got %+v", summary, returned)
	}
}

// - Verify Compact with a 2-message summary keeps the messages that arrived during compaction, after the pair
func Test_ContextCompaction_PairSummary_KeepsMessagesAddedDuringCompactionAfterThePair(t *testing.T) {
	chatContext, err := NewChatContext(NewOffsetWindow(10), 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(modeltest.Msgs(2)) // IDs "0","1"
	state := chatContext.Snapshot()
	chatContext.AddMessages([]model.ChatMessage{modeltest.Msg("user", "2", "arrived during compaction")})

	pair := []model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary"},
		{ID: "summary", Role: "assistant", Type: "summary"},
	}
	if !chatContext.Compact(state, pair) {
		t.Fatalf("expected Compact() to succeed")
	}

	want := []string{"summary-request", "summary", "2"}
	if got := idsOf(chatContext.GetMessages()); !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// - Verify clampToMax with a 2-message summary keeps both summary messages and the newest survivors
func Test_Unit_ClampToMax_TwoSummaryMessages_KeepsBothAndNewestSurvivors(t *testing.T) {
	messages := append([]model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary"},
		{ID: "summary", Role: "assistant", Type: "summary"},
	}, modeltest.Msgs(5)...) // IDs "0".."4"

	clamped := clampToMax(messages, 2, 5)

	want := []string{"summary-request", "summary", "2", "3", "4"}
	if got := idsOf(clamped); !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// - Verify clampToMax with a 2-message summary still drops a leading tool survivor, up to the next user message
func Test_Unit_ClampToMax_TwoSummaryMessages_LeadingToolSurvivor_DropsUpToNextUser(t *testing.T) {
	// [request, summary, call, tool, final, user] clamped to 5 keeps the pair and the last 3,
	// which would start at the tool result
	messages := append([]model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary"},
		{ID: "summary", Role: "assistant", Type: "summary"},
	}, toolTurn("A", 1)[1:]...)
	messages = append(messages, model.ChatMessage{ID: "B-user", Role: "user"})

	clamped := clampToMax(messages, 2, 5)

	want := []string{"summary-request", "summary", "B-user"}
	if got := idsOf(clamped); !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	assertNoOrphanedToolMsgs(t, clamped)
}

// - Verify clampToMax with a 2-message summary drops a plain assistant survivor the cut lands on: the
// result is [summary pair, next user, ...], never a pair followed by an assistant reply whose question is gone
func Test_Unit_ClampToMax_TwoSummaryMessages_LeadingAssistantSurvivor_IsDropped(t *testing.T) {
	// [request, summary, A-user, A-final, B-user, B-final] clamped to 5 keeps the pair and the last 3,
	// which would start at A-final
	messages := append(append([]model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary"},
		{ID: "summary", Role: "assistant", Type: "summary"},
	}, toolTurn("A", 0)...), toolTurn("B", 0)...)

	clamped := clampToMax(messages, 2, 5)

	want := []string{"summary-request", "summary", "B-user", "B-final"}
	if got := idsOf(clamped); !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// fakeSummarizerPrompt stands in for config's summarizerPrompt in tests that call the summarizer directly
const fakeSummarizerPrompt = "fake test summarizer prompt"

// - Verify CheckConfig accepts the defaults and each valid edge, and rejects each bad context window setting:
// a window below 4, an unknown strategy, a threshold outside (0, 1] (even with autoCompact off), an
// auto-compaction start of 2 messages or less (only when autoCompact is on), and an empty summarizer prompt
func Test_Unit_CheckConfig(t *testing.T) {
	tests := []struct {
		name    string
		set     func(*config.Config)
		wantErr string // "" = valid; otherwise a part of the error naming the bad setting
	}{
		{"defaults", func(c *config.Config) {}, ""},
		{"messages 3", func(c *config.Config) { c.ContextWindowMessages = 3 }, "contextWindowMessages"},
		{"messages 4", func(c *config.Config) { c.ContextWindowMessages = 4; c.AutoCompactThreshold = 0.75 }, ""},
		{"unknown strategy", func(c *config.Config) { c.ContextStrategy = "fake-strategy" }, "contextStrategy"},
		{"threshold 0", func(c *config.Config) { c.AutoCompactThreshold = 0 }, "autoCompactThreshold"},
		{"threshold -0.1", func(c *config.Config) { c.AutoCompactThreshold = -0.1 }, "autoCompactThreshold"},
		{"threshold 1.1", func(c *config.Config) { c.AutoCompactThreshold = 1.1 }, "autoCompactThreshold"},
		{"threshold 1.0", func(c *config.Config) { c.AutoCompactThreshold = 1.0 }, ""},
		{"autoCompact off, threshold 0", func(c *config.Config) { c.AutoCompact = false; c.AutoCompactThreshold = 0 }, "autoCompactThreshold"},
		{"autoCompact on, starts at 2", func(c *config.Config) { c.ContextWindowMessages = 4; c.AutoCompactThreshold = 0.5 }, "autoCompactThreshold"},
		{"autoCompact on, starts at 3", func(c *config.Config) { c.ContextWindowMessages = 4; c.AutoCompactThreshold = 0.75 }, ""},
		{"autoCompact off, would start at 2", func(c *config.Config) {
			c.AutoCompact = false
			c.ContextWindowMessages = 4
			c.AutoCompactThreshold = 0.5
		}, ""},
		{"summarizerPrompt empty", func(c *config.Config) { c.SummarizerPrompt = "" }, "summarizerPrompt"},
		{"summarizerPrompt whitespace", func(c *config.Config) { c.SummarizerPrompt = "  \n" }, "summarizerPrompt"},
	}
	for _, strategy := range WindowStrategies {
		tests = append(tests, struct {
			name    string
			set     func(*config.Config)
			wantErr string
		}{"strategy " + strategy, func(c *config.Config) { c.ContextStrategy = strategy }, ""})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.GetDefaultConfig()
			tt.set(&cfg)
			err := CheckConfig(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("expected no error, got %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("expected an error naming %s, got %v", tt.wantErr, err)
			}
		})
	}
}

// - Verify the window SetupChatContext builds holds exactly contextWindowMessages messages (no hidden buffer)
func Test_Unit_SetupChatContext_MaxSizeIsContextWindowMessages(t *testing.T) {
	for _, n := range []int{50, 7} {
		cfg := config.GetDefaultConfig()
		cfg.ContextWindowMessages = n
		chatContext, err := SetupChatContext(cfg)
		if err != nil {
			t.Fatalf("contextWindowMessages %d: SetupChatContext() returned an error: %v", n, err)
		}
		if got := chatContext.GetMaxSize(); got != n {
			t.Errorf("contextWindowMessages %d: expected max size %d, got %d", n, n, got)
		}
		chatContext.AddMessages(modeltest.Msgs(n + 2))
		if got := chatContext.GetSize(); got > n {
			t.Errorf("contextWindowMessages %d: expected the window to hold at most %d messages, got %d", n, n, got)
		}
	}
}

// - Verify SetupChatContext builds the window type contextStrategy names
func Test_Unit_SetupChatContext_UsesContextStrategy(t *testing.T) {
	for _, strategy := range WindowStrategies {
		cfg := config.GetDefaultConfig()
		cfg.ContextStrategy = strategy
		chatContext, err := SetupChatContext(cfg)
		if err != nil {
			t.Fatalf("strategy %s: SetupChatContext() returned an error: %v", strategy, err)
		}
		var ok bool
		switch w := chatContext.GetWindow(); strategy {
		case "offset":
			_, ok = w.(*OffsetWindow)
		case "in-place":
			_, ok = w.(*InPlaceWindow)
		case "ring-buffer":
			_, ok = w.(*RingBufferWindow)
		case "linked-list":
			_, ok = w.(*LLWindow)
		default:
			t.Fatalf("strategy %s has no type check in this test; add one", strategy)
		}
		if !ok {
			t.Errorf("strategy %s: got window type %T", strategy, chatContext.GetWindow())
		}
	}
}

// - Verify that with autoCompact off, filling the window to its max never makes auto-compaction needed
func Test_Unit_SetupChatContext_AutoCompactOff_NeverNeeded(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.ContextWindowMessages = 10
	cfg.AutoCompact = false
	chatContext, err := SetupChatContext(cfg)
	if err != nil {
		t.Fatalf("SetupChatContext() returned an error: %v", err)
	}
	for i := 1; i <= cfg.ContextWindowMessages; i++ {
		chatContext.AddMessages(modeltest.Msgs(1))
		if chatContext.IsAutoCompactionNeeded() {
			t.Fatalf("expected auto-compaction never to be needed with autoCompact off, got needed at %d messages", i)
		}
	}
}

// - Verify that with autoCompact on, a 10-message window at 0.9 needs compaction at 9 messages, not at 8
func Test_Unit_SetupChatContext_AutoCompactOn_NeededAtThreshold(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.ContextWindowMessages = 10
	cfg.AutoCompact = true
	cfg.AutoCompactThreshold = 0.9
	chatContext, err := SetupChatContext(cfg)
	if err != nil {
		t.Fatalf("SetupChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(modeltest.Msgs(8))
	if chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction not to be needed at 8 messages")
	}
	chatContext.AddMessages(modeltest.Msgs(1))
	if !chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction to be needed at 9 messages")
	}
}

// - Verify SetupChatContext returns CheckConfig's error for a bad config instead of building a context
func Test_Unit_SetupChatContext_BadConfig_ReturnsCheckConfigError(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.ContextWindowMessages = 2
	chatContext, err := SetupChatContext(cfg)
	if err == nil || err.Error() != CheckConfig(cfg).Error() {
		t.Errorf("expected CheckConfig's error %v, got %v", CheckConfig(cfg), err)
	}
	if chatContext != nil {
		t.Errorf("expected no chat context for a bad config, got %+v", chatContext)
	}
}

// - Verify NewChatContext with threshold 0 turns auto-compaction off: never needed, even with the window full
func Test_Unit_NewChatContext_ThresholdZero_NeverNeeded(t *testing.T) {
	chatContext, err := NewChatContext(NewOffsetWindow(4), 0)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(modeltest.Msgs(4))
	if chatContext.GetSize() != 4 {
		t.Fatalf("expected a full window of 4, got %d", chatContext.GetSize())
	}
	if chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction not to be needed with threshold 0")
	}
}

// - Verify SummarizeChatContext sends the systemPrompt parameter as the system message: exactly that text with
// no instructions, and starting with that text (then the instructions) when there are some
func Test_Unit_SummarizeChatContext_UsesSystemPromptParameter(t *testing.T) {
	for _, instructions := range []string{"", "keep only the fake test discussion"} {
		t.Run("instructions="+instructions, func(t *testing.T) {
			provider := &modeltest.FakeProvider{Reply: "a summary"}
			if _, err := SummarizeChatContext(context.Background(), provider, modeltest.Msgs(2), fakeSummarizerPrompt, instructions); err != nil {
				t.Fatalf("SummarizeChatContext() returned an error: %v", err)
			}
			if len(provider.Calls) != 1 || len(provider.Calls[0]) == 0 {
				t.Fatalf("expected 1 provider call with a request, got %+v", provider.Calls)
			}
			got := provider.Calls[0][0]
			if got.Role != "system" || !strings.HasPrefix(got.Content, fakeSummarizerPrompt) {
				t.Errorf("expected a system message starting with %q, got %+v", fakeSummarizerPrompt, got)
			}
			if instructions == "" && got.Content != fakeSummarizerPrompt {
				t.Errorf("expected exactly %q with no instructions, got %q", fakeSummarizerPrompt, got.Content)
			}
			if instructions != "" && !strings.HasSuffix(got.Content, instructions) {
				t.Errorf("expected the instructions after the prompt, got %q", got.Content)
			}
		})
	}
}
