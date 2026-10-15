package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// - Verify that the system prompt is still the first message in the message list even after context compaction.
func Test_ChatRequest_SystemMessage_FirstAfterCompaction(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100) // threshold unreachable: only manual /compact runs here, no auto-compaction race
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")    // seed some context to compact
	handleUserInput(ctx, fx.Session, "/compact") // manual compaction, runs synchronously

	callsBeforeNextTurn := len(fx.Provider.Calls)
	handleUserInput(ctx, fx.Session, "another message")

	if len(fx.Provider.Calls) != callsBeforeNextTurn+1 {
		t.Fatalf("expected exactly one new provider call after compaction, got %d new calls", len(fx.Provider.Calls)-callsBeforeNextTurn)
	}
	sent := fx.Provider.Calls[callsBeforeNextTurn]
	if len(sent) == 0 || sent[0].Role != "system" || sent[0].Content != fx.Session.SystemMsg.Content {
		t.Errorf("expected first message after compaction to still be the system message, got %+v", sent)
	}
}

// - Verify that text after /summary (e.g., "/summary only the test code discussion") is passed to the summarizer
// as instructions, the summary is printed, and the context, history and chat turns are untouched.
func Test_ChatRequest_SummaryWithTrailingText_PassedAsSummarizerInstructions(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	ctx := context.Background()
	const instructions = "only the test code discussion"

	fx.Provider.Reply = "ok"
	handleUserInput(ctx, fx.Session, "hello")
	contextBefore := fx.Context.GetMessages()
	callsBefore := len(fx.Provider.Calls)
	historyBefore := len(fx.History.GetMessages())

	fx.Provider.Reply = "a focused summary"
	handleUserInput(ctx, fx.Session, "/summary "+instructions)

	// exactly one new call: the summarization request, no follow-up chat turn
	if len(fx.Provider.Calls) != callsBefore+1 {
		t.Fatalf("expected exactly 1 new provider call (summarization), got %d", len(fx.Provider.Calls)-callsBefore)
	}
	found := false
	for _, m := range fx.Provider.Calls[callsBefore] {
		if strings.Contains(m.Content, instructions) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the summarization request to contain the instructions %q, got %+v", instructions, fx.Provider.Calls[callsBefore])
	}
	// the summary is shown to the user
	if !strings.Contains(fx.Out.String(), "a focused summary") {
		t.Errorf("expected the summary to be printed, got output: %s", fx.Out.String())
	}
	// unlike /compact, /summary is read-only: context is unchanged, nothing is stored
	after := fx.Context.GetMessages()
	if len(after) != len(contextBefore) {
		t.Fatalf("expected /summary to leave the context unchanged (%d messages), got %d", len(contextBefore), len(after))
	}
	for i := range contextBefore {
		if after[i].ID != contextBefore[i].ID {
			t.Errorf("expected context message %d unchanged (ID %q), got ID %q", i, contextBefore[i].ID, after[i].ID)
		}
	}
	if len(fx.History.GetMessages()) != historyBefore {
		t.Errorf("expected chat history unchanged by /summary, got %+v", fx.History.GetMessages())
	}
}

// - Verify that context compaction correctly creates a summary message and inserts it into the context
func Test_ContextCompaction_CreatesSummaryMessageInContext(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "a summary of the conversation"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")
	handleUserInput(ctx, fx.Session, "/compact")

	found := false
	for _, m := range fx.Context.GetMessages() {
		if m.Type == "summary" && m.Content == "a summary of the conversation" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a summary message to be present in context after compaction, got %+v", fx.Context.GetMessages())
	}
}

// - Verify that text after /compact (e.g., "/compact keep only the test code discussion") is passed to the
// summarizer as instructions, and is not sent or stored as a chat turn.
func Test_ContextCompaction_CompactWithTrailingText_PassedAsSummarizerInstructions(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	ctx := context.Background()
	const instructions = "keep only the test code discussion"

	fx.Provider.Reply = "ok"
	handleUserInput(ctx, fx.Session, "hello")
	callsBefore := len(fx.Provider.Calls)
	historyBefore := len(fx.History.GetMessages())

	fx.Provider.Reply = "a summary of the conversation"
	handleUserInput(ctx, fx.Session, "/compact "+instructions)

	// exactly one new call: the summarization request, no follow-up chat turn
	if len(fx.Provider.Calls) != callsBefore+1 {
		t.Fatalf("expected exactly 1 new provider call (summarization), got %d", len(fx.Provider.Calls)-callsBefore)
	}
	// the instructions reach the summarizer (checked by content, not position, so either message layout passes)
	found := false
	for _, m := range fx.Provider.Calls[callsBefore] {
		if strings.Contains(m.Content, instructions) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the summarization request to contain the instructions %q, got %+v", instructions, fx.Provider.Calls[callsBefore])
	}
	// the context is just the summary, and the instructions never became a message
	messages := fx.Context.GetMessages()
	if len(messages) != 1 || messages[0].Type != "summary" {
		t.Errorf("expected context to be exactly [summary] after /compact, got %+v", messages)
	}
	if len(fx.History.GetMessages()) != historyBefore {
		t.Errorf("expected chat history unchanged by /compact with instructions, got %+v", fx.History.GetMessages())
	}
}

// - Verify that a bare /summary or /compact adds no extra instructions message to the summarization request.
func Test_ContextCompaction_NoInstructions_NoExtraMessageInSummarizationRequest(t *testing.T) {
	for _, cmd := range []string{"/summary", "/compact"} {
		t.Run(cmd, func(t *testing.T) {
			fx := newChatSessionFixture(t, 10, 100)
			ctx := context.Background()

			fx.Provider.Reply = "ok"
			handleUserInput(ctx, fx.Session, "hello")
			callsBefore := len(fx.Provider.Calls)

			fx.Provider.Reply = "a summary"
			handleUserInput(ctx, fx.Session, cmd)

			if len(fx.Provider.Calls) != callsBefore+1 {
				t.Fatalf("expected exactly 1 new provider call (summarization), got %d", len(fx.Provider.Calls)-callsBefore)
			}
			// system prompt + transcript only; an empty instructions message would make it 3
			req := fx.Provider.Calls[callsBefore]
			if len(req) != 2 {
				t.Errorf("expected a 2-message summarization request with no instructions, got %d: %+v", len(req), req)
			}
		})
	}
}

// - Verify that unsuccessful context compaction (timeout, error or cancelled) does not alter the context
func Test_ContextCompaction_UnsuccessfulDoesNotAlterContext(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")
	before := fx.Context.GetMessages()

	fx.Provider.Err = errors.New("summarization failed")
	handleUserInput(ctx, fx.Session, "/compact")

	after := fx.Context.GetMessages()
	if len(before) != len(after) {
		t.Fatalf("expected context to be unchanged after a failed compaction, had %d messages before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Errorf("expected message at index %d to be unchanged (ID %q), got ID %q", i, before[i].ID, after[i].ID)
		}
	}
}

// - Verify that multiple context compactions cannot occur simultaneously (only the first compaction is executed).
func Test_ContextCompaction_MultipleCompactionsCannotOccurSimultaneously(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)

	// exercise the guard directly first: this is the actual mechanism /compact relies on
	if !fx.Context.ShouldStartCompaction() {
		t.Fatalf("expected the first ShouldStartCompaction() call to succeed")
	}
	if fx.Context.ShouldStartCompaction() {
		t.Errorf("expected a second ShouldStartCompaction() call to fail while a compaction is already in progress")
	}

	// now verify handleUserInput's /compact actually respects the guard instead of bypassing it
	callsBefore := len(fx.Provider.Calls)
	handleUserInput(context.Background(), fx.Session, "/compact")
	if len(fx.Provider.Calls) != callsBefore {
		t.Errorf("expected /compact to skip when a compaction is already in progress, but it made a provider call")
	}

	fx.Context.EndCompaction()
	if !fx.Context.ShouldStartCompaction() {
		t.Errorf("expected ShouldStartCompaction() to succeed again after EndCompaction()")
	}
}

// - Verify that context compaction correctly reduces the context footprint (measured by the number of messages in the chat history, or tokens in the future).
func Test_ContextCompaction_ReducesContextFootprint(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		handleUserInput(ctx, fx.Session, "hello")
	}
	sizeBefore := fx.Context.GetSize()

	fx.Provider.Reply = "a short summary"
	handleUserInput(ctx, fx.Session, "/compact")

	sizeAfter := fx.Context.GetSize()
	if sizeAfter >= sizeBefore {
		t.Errorf("expected compaction to reduce context size (was %d), got %d", sizeBefore, sizeAfter)
	}
}

// - Verify that auto-compaction is correctly triggered when the context footprint exceeds a certain threshold.
func Test_ContextCompaction_AutoCompactionTriggeredWhenThresholdExceeded(t *testing.T) {
	// threshold == maxSize: compaction should fire the moment the window fills.
	fx := newChatSessionFixture(t, 4, 4)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	// [agent] the chat loop calls maybeStartAutoCompaction after each input; the test does the same
	handleUserInput(ctx, fx.Session, "first") // 2 messages, size 2, below threshold
	maybeStartAutoCompaction(ctx, fx.Session)
	if fx.Context.GetSize() != 2 {
		t.Fatalf("expected no compaction below the threshold, got size %d", fx.Context.GetSize())
	}
	handleUserInput(ctx, fx.Session, "second") // 2 more, size 4, hits threshold
	maybeStartAutoCompaction(ctx, fx.Session)  // -> triggers background compaction

	fx.Context.WaitForCompaction() // deterministic: blocks until the background compaction actually finishes

	if fx.Context.GetSize() != 1 {
		t.Fatalf("expected auto-compaction to have reduced context to 1 summary message, got size %d", fx.Context.GetSize())
	}
}

// - Verify that a user can manually trigger context compaction with an appropriate slash command.
func Test_ContextCompaction_ManualTrigger(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")

	fx.Provider.Reply = "manual summary"
	handleUserInput(ctx, fx.Session, "/compact")

	found := false
	for _, m := range fx.Context.GetMessages() {
		if m.Type == "summary" && m.Content == "manual summary" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a manual /compact to insert a summary message, got %+v", fx.Context.GetMessages())
	}
}

// - Verify that an empty or whitespace-only summary is treated as a failure and leaves the context unchanged
func Test_ContextCompaction_EmptyOrWhitespaceSummaryTreatedAsFailure(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")
	before := fx.Context.GetMessages()

	fx.Provider.Reply = "   " // whitespace-only summary
	handleUserInput(ctx, fx.Session, "/compact")

	after := fx.Context.GetMessages()
	if len(before) != len(after) {
		t.Fatalf("expected context to be unchanged after a whitespace-only summary, had %d before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Errorf("expected message at index %d to be unchanged, got a different message", i)
		}
	}
}

// - Verify that after a failed compaction, new messages can still be added to the context.
func Test_ContextCompaction_FailedCompactionAllowsNewMessages(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	ctx := context.Background()

	fx.Provider.Reply = "ok"
	handleUserInput(ctx, fx.Session, "hello")

	fx.Provider.Err = errors.New("summarization failed")
	handleUserInput(ctx, fx.Session, "/compact")

	fx.Provider.Err = nil
	fx.Provider.Reply = "hi again"
	handleUserInput(ctx, fx.Session, "still working?")

	messages := fx.History.GetMessages()
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "hi again" {
		t.Errorf("expected the session to keep working after a failed compaction, got last message %+v", last)
	}
}

// - Verify that after compaction, the next chat request does not contain old compacted messages.
func Test_ContextCompaction_NextChatRequestDoesNotContainOldCompactedMessages(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	ctx := context.Background()

	fx.Provider.Reply = "ok"
	handleUserInput(ctx, fx.Session, "hello")
	oldUserMsgID := fx.History.GetMessages()[0].ID

	fx.Provider.Reply = "a summary"
	handleUserInput(ctx, fx.Session, "/compact")

	fx.Provider.Reply = "sure"
	handleUserInput(ctx, fx.Session, "another message")

	lastCall := fx.Provider.Calls[len(fx.Provider.Calls)-1]
	for _, m := range lastCall {
		if m.ID == oldUserMsgID {
			t.Errorf("expected the next request not to contain the pre-compaction message (ID %q), but it did", oldUserMsgID)
		}
	}
}
