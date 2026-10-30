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

// - Verify that SetupChatContext computes the expected auto-compaction threshold from the
// package constants. config.Config{} (empty) is deliberate: SetupChatContext currently ignores its cfg
// parameter entirely and builds the window from MaxContextWindow/WindowStrategy directly.
func Test_Unit_SetupChatContext_ComputesExpectedThreshold(t *testing.T) {
	chatContext, err := SetupChatContext(config.Config{})
	if err != nil {
		t.Fatalf("SetupChatContext() returned an error: %v", err)
	}

	wantMaxSize := MaxContextWindow - 2 // NewContextWindow reserves a 2-message buffer
	if chatContext.GetMaxSize() != wantMaxSize {
		t.Fatalf("expected max size %d, got %d", wantMaxSize, chatContext.GetMaxSize())
	}

	// ChatContext doesn't expose the raw threshold value directly, so exercise it indirectly
	// via IsAutoCompactionNeeded at the boundary.
	wantThreshold := int(float64(wantMaxSize) * AutoCompactThresholdFrac)
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
	messages := modeltest.Msgs(7)
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

	_, err = CompactChatContext(context.Background(), chatContext, provider, "")
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

// - Verify clampToMax drops survivors that start with a tool result whose tool call was clamped away
func Test_Unit_ClampToMax_LeadingToolSurvivor_IsDropped(t *testing.T) {
	// [summary, call, tool, final, user] clamped to 4 keeps the summary and the last 3,
	// which would start at the tool result
	messages := append([]model.ChatMessage{{ID: "summary", Role: "user", Type: "summary"}}, toolTurn("A", 1)[1:]...)
	messages = append(messages, model.ChatMessage{ID: "B-user", Role: "user"})

	clamped := clampToMax(messages, 1, 4)

	got := idsOf(clamped)
	want := []string{"summary", "A-final", "B-user"}
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

// - Verify compaction end to end: when messages that arrived during compaction start with an
// assistant tool call, and the clamp cuts between that call and its result, the orphaned result
// is dropped rather than left after the summary
func Test_ContextCompaction_ClampCutsBetweenToolCallAndResult_NoOrphan(t *testing.T) {
	chatContext, err := NewChatContext(NewOffsetWindow(5), 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	state := chatContext.Snapshot() // empty: everything added next counts as "arrived during compaction"

	// a window that legitimately starts with an assistant tool call (a previous trim landed there)
	arrived := append(toolTurn("A", 1)[1:], toolTurn("B", 0)...)[:5] // call, tool, final, B-user, B-final
	chatContext.AddMessages(arrived)

	if !chatContext.Compact(state, []model.ChatMessage{{ID: "summary", Role: "user", Type: "summary"}}) {
		t.Fatalf("expected Compact() to succeed")
	}

	got := chatContext.GetMessages()
	want := []string{"summary", "A-final", "B-user", "B-final"}
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

	if _, err := SummarizeChatContext(context.Background(), provider, modeltest.Msgs(2), instructions); err != nil {
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
	if !strings.Contains(req[0].Content, SummarizeSystemPrompt) || !strings.Contains(req[0].Content, instructions) {
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

	returned, err := CompactChatContext(context.Background(), chatContext, provider, "")
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

// - Verify clampToMax with a 2-message summary still drops a leading tool survivor
func Test_Unit_ClampToMax_TwoSummaryMessages_LeadingToolSurvivor_IsDropped(t *testing.T) {
	// [request, summary, call, tool, final, user] clamped to 5 keeps the pair and the last 3,
	// which would start at the tool result
	messages := append([]model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary"},
		{ID: "summary", Role: "assistant", Type: "summary"},
	}, toolTurn("A", 1)[1:]...)
	messages = append(messages, model.ChatMessage{ID: "B-user", Role: "user"})

	clamped := clampToMax(messages, 2, 5)

	want := []string{"summary-request", "summary", "A-final", "B-user"}
	if got := idsOf(clamped); !slices.Equal(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	assertNoOrphanedToolMsgs(t, clamped)
}
