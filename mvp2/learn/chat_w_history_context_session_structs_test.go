package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Test cases for chat with context compaction functionality.
//
// Race tests
// - Run all tests with -race and make sure the program is free of race conditions.
//

// Verify all tests involving context work specifically for each of the current window types
// (OffsetWindow, InPlaceWindow, RingBufferWindow, LLWindow)
// Note that OffsetWindow should be the default type
var windowDataStructureTypes = map[string]func(maxSize int) ContextWindow{
	"offset":      func(n int) ContextWindow { return NewOffsetWindow(n) },
	"in-place":    func(n int) ContextWindow { return NewInPlaceWindow(n) },
	"ring-buffer": func(n int) ContextWindow { return NewRingBufferWindow(n) },
	"linked-list": func(n int) ContextWindow { return NewLLWindow(n) },
}

func windowTypeNames() []string {
	names := make([]string, 0, len(windowDataStructureTypes))
	for name := range windowDataStructureTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

//*************************************//
// Test fixture
//*************************************//

// fakeProvider is a scriptable Provider for tests: set Reply/Err before a call, inspect Calls
// afterward to see exactly which message list a test triggered. Calls is mutex-guarded because
// auto-compaction invokes Chat from a background goroutine — tests exercising that path need
// this safe under -race, not just in the common single-goroutine case.
type fakeProvider struct {
	mu    sync.Mutex
	Reply string
	Err   error
	Calls [][]ChatMessage
}

func (p *fakeProvider) Chat(ctx context.Context, chatHistory []ChatMessage) (string, error) {
	p.mu.Lock()
	p.Calls = append(p.Calls, append([]ChatMessage(nil), chatHistory...))
	p.mu.Unlock()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if p.Err != nil {
		return "", p.Err
	}
	return p.Reply, nil
}

// chatSessionFixture bundles a fully wired ChatSession (history + context + a fake provider)
// so tests don't have to repeat the setup, and so a signature change to how these pieces are
// wired together only needs fixing here, not in every test. Meant for tests that exercise
// ChatSession-level behavior (handleUserInput, compaction, chat request shape) — pure
// ContextWindow-mechanics tests don't need a ChatSession at all and should use forEachWindow
// (once that's built) instead of this fixture.
type chatSessionFixture struct {
	Session  *ChatSession
	History  *InMemoryChatHistory
	Context  *ChatContext
	Provider *fakeProvider
	Out      *bytes.Buffer
}

// newChatSessionFixture wires up a ChatSession backed by an OffsetWindow (the default window
// type for tests that aren't specifically about window-type mechanics) of the given maxSize,
// with auto-compaction configured to fire once the window reaches threshold messages.
func newChatSessionFixture(t *testing.T, maxSize, threshold int) *chatSessionFixture {
	t.Helper()

	history, err := NewInMemoryChatHistory()
	if err != nil {
		t.Fatalf("NewInMemoryChatHistory() returned an error: %v", err)
	}

	window := NewOffsetWindow(maxSize)
	chatContext, err := NewChatContext(window, threshold)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}

	provider := &fakeProvider{}
	out := &bytes.Buffer{}

	cfg := Config{
		UserID:       "test-user",
		SystemPrompt: "You are a helpful assistant.",
		InBuffer:     strings.NewReader(""),
		OutBuffer:    out,
	}

	session, err := NewChatSession(provider, history, chatContext, cfg)
	if err != nil {
		t.Fatalf("NewChatSession() returned an error: %v", err)
	}

	return &chatSessionFixture{
		Session:  session,
		History:  history,
		Context:  chatContext,
		Provider: provider,
		Out:      out,
	}
}

// forEachWindow runs fn once per registered ContextWindow implementation, as a subtest named
// after that implementation (e.g. TestX/ring-buffer), so a failure identifies exactly which
// window type broke without needing to read the test body. Use this for behavior that should
// hold across all four types; a type-specific edge case belongs in its own Test_Unit_* function
// instead (see the Unit tests section).
func forEachWindow(t *testing.T, maxSize int, fn func(t *testing.T, w ContextWindow)) {
	t.Helper()
	for name, factory := range windowDataStructureTypes {
		t.Run(name, func(t *testing.T) {
			fn(t, factory(maxSize))
		})
	}
}

// msg builds a ChatMessage with a distinct ID, so tests can tell messages apart after
// eviction/compaction without depending on their Content.
func msg(role, id, content string) ChatMessage {
	return ChatMessage{ID: id, Role: role, Content: content}
}

// msgs builds n distinct ChatMessages with IDs "0".."n-1" in order, alternating user/assistant
// roles, for tests that just need "some messages" without caring about their content.
func msgs(n int) []ChatMessage {
	out := make([]ChatMessage, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = msg(role, fmt.Sprintf("%d", i), fmt.Sprintf("message %d", i))
	}
	return out
}

//*************************************//
// Basic chat request mechanics
//*************************************//

// - Verify that a user request receives a valid response
func Test_ChatRequest_UserRequest_ValidResponse(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "hi there"

	quit := handleUserInput(context.Background(), fx.Session, "hello")
	if quit {
		t.Fatalf("handleUserInput returned quit=true for a normal message")
	}

	messages := fx.History.GetMessages()
	if len(messages) != 2 {
		t.Fatalf("expected 2 chat history entries (user + assistant), got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "hello" {
		t.Errorf("expected first entry to be the user message %q, got %+v", "hello", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "hi there" {
		t.Errorf("expected second entry to be the assistant response %q, got %+v", "hi there", messages[1])
	}
}

// - Verify the system message is always first in the message list within the request and is never evicted.
func Test_ChatRequest_SystemMessage_AlwaysFirst(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"

	handleUserInput(context.Background(), fx.Session, "hello")

	if len(fx.Provider.Calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(fx.Provider.Calls))
	}
	sent := fx.Provider.Calls[0]
	if len(sent) == 0 {
		t.Fatalf("expected a non-empty message list sent to the provider")
	}
	if sent[0].Role != "system" || sent[0].Content != fx.Session.SystemMsg.Content {
		t.Errorf("expected first message to be the system message %+v, got %+v", fx.Session.SystemMsg, sent[0])
	}
}

func Test_ChatRequest_SystemMessage_NotEvicted(t *testing.T) {
	// small window forces real eviction; threshold is unreachable so auto-compaction never fires
	fx := newChatSessionFixture(t, 4, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		handleUserInput(ctx, fx.Session, "hello")
	}

	if len(fx.Provider.Calls) != 4 {
		t.Fatalf("expected 4 provider calls, got %d", len(fx.Provider.Calls))
	}
	for i, call := range fx.Provider.Calls {
		if len(call) == 0 || call[0].Role != "system" {
			t.Errorf("call %d: expected first message to be the system message, got %+v", i, call)
		}
	}
}

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

// - Verify that recognized slash commands are not included in chat request messages.
func Test_ChatRequest_RecognizedSlashCommands_NotIncluded(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"

	handleUserInput(context.Background(), fx.Session, "/clear hello there")

	if len(fx.Provider.Calls) != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", len(fx.Provider.Calls))
	}
	sent := fx.Provider.Calls[0]
	userMsgSent := sent[len(sent)-1]
	if userMsgSent.Content != "hello there" {
		t.Errorf("expected the slash command to be stripped and only %q sent, got %q", "hello there", userMsgSent.Content)
	}
}

// - Verify that unrecognized slash commands do not send any request to the Provider
func Test_ChatRequest_UnrecognizedSlashCommands_NoRequestSent(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"

	handleUserInput(context.Background(), fx.Session, "/bogus")

	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no provider calls for an unrecognized slash command, got %d", len(fx.Provider.Calls))
	}
}

// - Verify that an empty user input does not send a request to the Provider.
func Test_ChatRequest_EmptyUserInput_NoRequestSent(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"

	handleUserInput(context.Background(), fx.Session, "   ")

	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no provider calls for empty/whitespace-only input, got %d", len(fx.Provider.Calls))
	}
}

// - Verify that an empty user input does not add an entry to the chat history.
func Test_ChatRequest_EmptyUserInput_NoChatHistoryEntry(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)

	handleUserInput(context.Background(), fx.Session, "   ")

	if len(fx.History.GetMessages()) != 0 {
		t.Errorf("expected no chat history entries for empty/whitespace-only input, got %d", len(fx.History.GetMessages()))
	}
}

//*************************************//
// Basic chat history mechanics
//*************************************//

// - Verify that a new chat message is correctly added as the most recent entry in the chat history (exists and is most recent)
func Test_ChatHistory_NewChatMessage_AddedAsMostRecent(t *testing.T) {
	// arguments are pointer to test runner object (t), the maximum size of the context window, and the threshold for triggering compaction.
	fx := newChatSessionFixture(t, 10, 9)

	first := ChatMessage{ID: "1", Role: "user", Content: "hello"}
	fx.History.Append([]ChatMessage{first})

	second := ChatMessage{ID: "2", Role: "assistant", Content: "hi there"}
	fx.History.Append([]ChatMessage{second})

	messages := fx.History.GetMessages()
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages in history, got %d", len(messages))
	}

	mostRecent := messages[len(messages)-1]
	if mostRecent.ID != second.ID {
		t.Errorf("expected most recent message to have ID %q, got %q", second.ID, mostRecent.ID)
	}
}

// - Verify that successive chat messages retain the same order in the chat history.
func Test_ChatHistory_SuccessiveChatMessages_RetainOrder(t *testing.T) {
	// t.Skip("TODO: Verify that successive chat messages retain the same order in the chat history.")
	fx := newChatSessionFixture(t, 10, 9)

	first := ChatMessage{ID: "1", Role: "user", Content: "hello"}
	fx.History.Append([]ChatMessage{first})

	second := ChatMessage{ID: "2", Role: "assistant", Content: "hi there"}
	fx.History.Append([]ChatMessage{second})

	messages := fx.History.GetMessages()
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages in history, got %d", len(messages))
	}

	if messages[0].ID != first.ID {
		t.Errorf("expected first message to have ID %q, got %q", first.ID, messages[0].ID)
	}
	if messages[1].ID != second.ID {
		t.Errorf("expected second message to have ID %q, got %q", second.ID, messages[1].ID)
	}
}

// - Verify that a full successful prompt-response cycle adds both the prompt and the response to the chat history.
func Test_ChatHistory_FullPromptResponseCycle_AddsBoth(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "hi there"

	// note that the actual chat request is mocked with a fake provider Chat() function
	handleUserInput(context.Background(), fx.Session, "hello")

	messages := fx.History.GetMessages()
	if len(messages) != 2 {
		t.Fatalf("expected 2 chat history entries (user + assistant), got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "hello" {
		t.Errorf("expected first entry to be the user prompt %q, got %+v", "hello", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "hi there" {
		t.Errorf("expected second entry to be the assistant response %q, got %+v", "hi there", messages[1])
	}
}

// - Verify that a failed response (timeout or error) does not add an incomplete entry to the chat history.
func Test_ChatHistory_FailedResponse_DoesNotAddIncompleteEntry(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Err = errors.New("provider unavailable")

	// note that the actual chat request is mocked with a fake provider Chat() function
	handleUserInput(context.Background(), fx.Session, "hello")

	messages := fx.History.GetMessages()
	if len(messages) != 0 {
		t.Errorf("expected no chat history entries after a failed response, got %d: %+v", len(messages), messages)
	}
}

// - Verify that a failed response (timeout or error) does not add the corresponding user prompt to the chat history.
func Test_ChatHistory_FailedResponse_DoesNotAddUserPrompt(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Err = errors.New("provider unavailable")

	// note that the actual chat request is mocked with a fake provider Chat() function
	handleUserInput(context.Background(), fx.Session, "hello")

	for _, msg := range fx.History.GetMessages() {
		if msg.Role == "user" && msg.Content == "hello" {
			t.Errorf("expected the user prompt not to be added to chat history after a failed response, found %+v", msg)
		}
	}
}

//*************************************//
// Basic context window mechanics
//*************************************//

// - Verify that adding a new message to a full context window does not exceed the maximum context window size.
func Test_ContextWindow_AddNewMessage_FullWindow_MaxSizeNotExceeded(t *testing.T) {
	forEachWindow(t, 3, func(t *testing.T, w ContextWindow) {
		w.AddMessages(msgs(5)) // one at a time via a single batch call, well past maxSize

		if w.GetSize() > w.GetMaxSize() {
			t.Errorf("expected size to never exceed max size %d, got %d", w.GetMaxSize(), w.GetSize())
		}
		if len(w.GetMessages()) > w.GetMaxSize() {
			t.Errorf("expected GetMessages() to return at most %d messages, got %d", w.GetMaxSize(), len(w.GetMessages()))
		}
	})
}

// - Verify that adding a new message to a full context window correctly removes the oldest message from the context window.
func Test_ContextWindow_AddNewMessage_FullWindow_OldestMessageRemoved(t *testing.T) {
	forEachWindow(t, 3, func(t *testing.T, w ContextWindow) {
		w.AddMessages(msgs(3)) // fills the window exactly: IDs "0","1","2"
		w.AddMessages([]ChatMessage{msg("user", "3", "newest")})

		messages := w.GetMessages()
		for _, m := range messages {
			if m.ID == "0" {
				t.Errorf("expected the oldest message (ID %q) to have been evicted, but it's still present: %+v", "0", messages)
			}
		}
		found := false
		for _, m := range messages {
			if m.ID == "3" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected the newest message (ID %q) to be present, got %+v", "3", messages)
		}
	})
}

// - Verify that a user can successfully clear the context with an appropriate slash command.
func Test_ContextWindow_ClearContext_SlashCommand(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")
	if fx.Context.GetSize() == 0 {
		t.Fatalf("test setup problem: expected some context before clearing")
	}

	handleUserInput(ctx, fx.Session, "/clear")

	if fx.Context.GetSize() != 0 {
		t.Errorf("expected /clear to empty the context, got size %d", fx.Context.GetSize())
	}
	// /clear only resets the context; chat history is the durable record and is untouched
	if len(fx.History.GetMessages()) == 0 {
		t.Errorf("expected /clear to leave chat history untouched, but history is empty")
	}
}

//*************************************//
// Basic context compaction tests
//*************************************//

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

	chatContext.AddMessages(msgs(2)) // IDs "0","1"
	state := chatContext.Snapshot()

	// simulate a message arriving while compaction's summarization call is in flight
	chatContext.AddMessages([]ChatMessage{msg("user", "2", "arrived during compaction")})

	summaryMsg := msg("user", "summary", "a summary")
	if !chatContext.Compact(state, summaryMsg) {
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

// - Verify that auto-compaction is correctly triggered when the context footprint exceeds a certain threshold.
func Test_ContextCompaction_AutoCompactionTriggeredWhenThresholdExceeded(t *testing.T) {
	// threshold == maxSize: compaction should fire the moment the window fills.
	fx := newChatSessionFixture(t, 4, 4)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "first")  // 2 messages, size 2, below threshold
	handleUserInput(ctx, fx.Session, "second") // 2 more, size 4, hits threshold -> triggers background compaction

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

// - Verify that compacting an empty context returns an error and leaves the context unchanged.
func Test_ContextCompaction_CompactingEmptyContextReturnsError(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"

	_, err := compactChatContext(context.Background(), fx.Session, CompactionManual)
	if err == nil {
		t.Errorf("expected compacting an empty context to return an error")
	}
	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no provider call when there's nothing to summarize, got %d", len(fx.Provider.Calls))
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

//*************************************//
// Request-Response shape tests
//*************************************//

// - Verify that an error in the chat request object errors gracefull
func Test_ChatRequest_ErrorsGracefully(t *testing.T) {
	// server is closed before use, so any request against its URL fails at the transport
	// level (connection refused) — deterministic, unlike guessing an unused port.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("handler should never be reached: server was closed before the request")
	}))
	srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}})

	if err == nil {
		t.Fatalf("expected an error when the provider is unreachable, got reply %q", reply)
	}
}

// - Verify a non-2xx response code errors gracefully
func Test_ChatRequest_Non2xxResponseErrorsGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}})

	if err == nil {
		t.Fatalf("expected an error for a non-2xx response, got reply %q", reply)
	}
}

// - Verify an empty response errors gracefully
func Test_ChatRequest_EmptyResponseErrorsGracefully(t *testing.T) {
	// Two distinct ways a response can be "empty" — each exercises a different failure
	// path in Chat(), so both are worth covering rather than picking one.
	t.Run("no body at all", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// write nothing: json.Decode on the other end hits EOF
		}))
		defer srv.Close()

		p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}})

		if err == nil {
			t.Fatalf("expected an error for an empty response body, got reply %q", reply)
		}
	})

	t.Run("valid JSON but no choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResponse{Choices: nil})
		}))
		defer srv.Close()

		p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}})

		if err == nil {
			t.Fatalf("expected an error for a response with no choices, got reply %q", reply)
		}
	})
}

// - Verify a proper request-response cycle that returns a 2xx response is successful
func Test_ChatRequest_ProperRequestResponseCycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected a POST request, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected path %q, got %q", "/chat/completions", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{
			Choices: []struct {
				Message ChatMessage `json:"message"`
			}{
				{Message: ChatMessage{Role: "assistant", Content: "hi there"}},
			},
		})
	}))
	defer srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}})

	if err != nil {
		t.Fatalf("Chat() returned an unexpected error: %v", err)
	}
	if reply != "hi there" {
		t.Errorf("expected reply %q, got %q", "hi there", reply)
	}
}

//*************************************//
// Window type performance tests
//*************************************//

// - Measure and compare the performance of the current window types under various chat and context window operations
//   (OffsetWindow, InPlaceWindow, RingBufferWindow, LLWindow)

// forEachWindowBench runs fn once per registered ContextWindow implementation, as a
// sub-benchmark named after that implementation (e.g. Benchmark_X/ring-buffer) — mirrors
// forEachWindow's pattern for tests, so `go test -bench=. -benchmem` prints a direct
// side-by-side comparison across all four types for each operation.
func forEachWindowBench(b *testing.B, maxSize int, fn func(b *testing.B, w ContextWindow)) {
	b.Helper()
	for name, factory := range windowDataStructureTypes {
		b.Run(name, func(b *testing.B) {
			fn(b, factory(maxSize))
		})
	}
}

// Benchmark add message operations on different context window data structure types
func Benchmark_ContextWindow_AddMessages(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(msgs(20)) // fill to capacity first: measures the realistic steady-state
		// cost (every add evicts one), not empty-window growth, since a real session's window
		// is full for the vast majority of its lifetime.
		one := []ChatMessage{msg("user", "bench", "hello")}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.AddMessages(one)
		}
	})
}

// Benchmark bulk add multiple messages to different context window data structure types
func Benchmark_ContextWindow_BulkAddMessages(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(msgs(20))
		batch := msgs(10) // one AddMessages call carrying several messages at once, e.g. what
		// a compaction's summary-plus-survivors write looks like
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.AddMessages(batch)
		}
	})
}

// Benchmark get message operations on different context window data structure types
func Benchmark_ContextWindow_GetMessages(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(msgs(20))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = w.GetMessages()
		}
	})
}

// Benchmark eviction operations (when context window is full)on different context window data structure types
func Benchmark_ContextWindow_Eviction(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(msgs(20)) // start full
		// more than 2x maxSize in one call: every iteration evicts the entire prior contents
		// in one shot, rather than one message at a time — a different traffic pattern from
		// Benchmark_ContextWindow_AddMessages, worth comparing separately.
		overflow := msgs(40)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.AddMessages(overflow)
		}
	})
}

// Benchmark clear operations on different context window data structure types
func Benchmark_ContextWindow_Clear(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(msgs(20))
		// Deliberately not refilled between iterations: unlike AddMessages/RemoveLast, none of
		// the four Clear() implementations have a cost that depends on how full the window was
		// (Offset/InPlace/LLWindow are O(1) re-slices/list resets regardless of content;
		// RingBufferWindow always zeroes its whole maxSize-length backing array regardless of
		// count). So measuring Clear() repeatedly on an already-emptied window is still a fair,
		// representative cost for all four types here.
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.Clear()
		}
	})
}

// Benchmark remove last message operations on different context window data structure types
func Benchmark_ContextWindow_RemoveLastMessage(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(msgs(20))
		one := []ChatMessage{msg("user", "bench", "hello")}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// A bounded-capacity window can't be pre-loaded with more than maxSize messages,
			// so sustaining repeated RemoveLast calls requires replenishing every iteration —
			// there's no way to isolate RemoveLast's cost alone without running out after the
			// first maxSize iterations. This benchmark's number is RemoveLast(1)+AddMessages(1)
			// combined, not RemoveLast in isolation.
			w.RemoveLast(1)
			w.AddMessages(one)
		}
	})
}

//*************************************//
// Unit tests
//*************************************//

// assertWindowKeepsNewestOnBatchOverflow adds more messages in a single AddMessages call than
// the window can hold, and checks that exactly the newest maxSize survive, in order. This is
// deliberately different from (and more rigorous than) the generic forEachWindow eviction check
// in "Basic context window mechanics": each window type's eviction logic loops once per message
// in a batch, so a single-message steady-state test passing doesn't guarantee a large batch
// overflow (N > 1 evicted at once) is handled correctly by that same loop.
func assertWindowKeepsNewestOnBatchOverflow(t *testing.T, w ContextWindow, maxSize int) {
	t.Helper()
	w.AddMessages(msgs(maxSize + 3)) // one batch call, overflowing by 3

	messages := w.GetMessages()
	if len(messages) != maxSize {
		t.Fatalf("expected exactly %d messages after batch overflow, got %d", maxSize, len(messages))
	}
	for i, m := range messages {
		wantID := fmt.Sprintf("%d", i+3) // IDs "0".."maxSize+2" were added; oldest 3 evicted
		if m.ID != wantID {
			t.Errorf("expected message at index %d to have ID %q, got %q", i, wantID, m.ID)
		}
	}
}

// - Verify LLWindow eviction behavior within the data structure (new message when context window is full evicts the oldest message)
func Test_Unit_LLWindowEvictionBehavior(t *testing.T) {
	assertWindowKeepsNewestOnBatchOverflow(t, NewLLWindow(5), 5)
}

func Test_Unit_InPlaceWindowEvictionBehavior(t *testing.T) {
	assertWindowKeepsNewestOnBatchOverflow(t, NewInPlaceWindow(5), 5)
}
func Test_Unit_RingBufferWindowEvictionBehavior(t *testing.T) {
	assertWindowKeepsNewestOnBatchOverflow(t, NewRingBufferWindow(5), 5)
}
func Test_Unit_OffsetWindowEvictionBehavior(t *testing.T) {
	assertWindowKeepsNewestOnBatchOverflow(t, NewOffsetWindow(5), 5)
}

func Test_Unit_RingBufferWindowWraparoundBehavior(t *testing.T) {
	w := NewRingBufferWindow(3)

	// add one at a time, well past 2x maxSize, so head wraps around the backing array
	// multiple times — this is what previously caught a real indexing bug (see DECISIONS.md).
	for _, m := range msgs(8) { // IDs "0".."7"
		w.AddMessages([]ChatMessage{m})
	}

	messages := w.GetMessages()
	wantIDs := []string{"5", "6", "7"} // the 3 newest, oldest-to-newest
	if len(messages) != len(wantIDs) {
		t.Fatalf("expected %d messages after wraparound, got %d", len(wantIDs), len(messages))
	}
	for i, want := range wantIDs {
		if messages[i].ID != want {
			t.Errorf("expected message at index %d to have ID %q after wraparound, got %q", i, want, messages[i].ID)
		}
	}
}

// - An operation on messages returned by GetMessages() method for ChatContext does not modify the current context
// (renamed from "...ChatHistory": GetMessages() here is ChatContext's, not ChatHistory's — same
// naming fix already applied to the compaction tests, see DECISIONS.md.)
func Test_Unit_OpOnGetMessagesDoesNotModifyContext(t *testing.T) {
	window := NewOffsetWindow(10)
	chatContext, err := NewChatContext(window, 100)
	if err != nil {
		t.Fatalf("NewChatContext() returned an error: %v", err)
	}
	chatContext.AddMessages(msgs(2))

	messages := chatContext.GetMessages()
	messages[0].Content = "mutated"
	messages = append(messages, msg("user", "extra", "should not leak back"))

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
	chatContext.AddMessages(msgs(2))

	state := chatContext.Snapshot()
	state.Messages[0].Content = "mutated"
	state.Messages = append(state.Messages, msg("user", "extra", "should not leak back"))

	after := chatContext.GetMessages()
	if len(after) != 2 {
		t.Fatalf("expected context to still have 2 messages, got %d", len(after))
	}
	if after[0].Content == "mutated" {
		t.Errorf("expected mutating the snapshot's Messages slice not to affect the context's internal state")
	}
}

// - Verify that ClampToMax does not modify the message slice if it is under the maximum capacity
func Test_Unit_ClampToMax_UnderCapacity_ReturnsUnchanged(t *testing.T) {
	messages := msgs(3)
	max := 5
	clamped := clampToMax(messages, max)
	if len(clamped) != len(messages) {
		t.Errorf("expected clamped slice to have length %d, got %d", len(messages), len(clamped))
	}
	for i := range clamped {
		if clamped[i] != messages[i] {
			t.Errorf("expected message at index %d to be unchanged", i)
		}
	}
}

// - Verify that ClampToMax does not modify the message slice if it is exactly at the maximum capacity
func Test_Unit_ClampToMax_ExactlyAtCapacity_ReturnsUnchanged(t *testing.T) {
	messages := msgs(5)
	max := 5
	clamped := clampToMax(messages, max)
	if len(clamped) != len(messages) {
		t.Errorf("expected clamped slice to have length %d, got %d", len(messages), len(clamped))
	}
	for i := range clamped {
		if clamped[i] != messages[i] {
			t.Errorf("expected message at index %d to be unchanged", i)
		}
	}
}

// - Verify that ClampToMax truncates the message slice if it exceeds the maximum capacity, keeping the summary and newest survivors
func Test_Unit_ClampToMax_OverCapacity_KeepsSummaryAndNewestSurvivors(t *testing.T) {
	messages := msgs(7)
	max := 5
	clamped := clampToMax(messages, max)
	if len(clamped) != max {
		t.Errorf("expected clamped slice to have length %d, got %d", max, len(clamped))
	}
	// assuming the first message is the summary and the last (max-1) messages are the newest survivors
	if clamped[0] != messages[0] {
		t.Errorf("expected the first message (summary) to be unchanged")
	}
	for i := 1; i < max; i++ {
		if clamped[i] != messages[len(messages)-max+i] {
			t.Errorf("expected message at index %d to be one of the newest survivors", i)
		}
	}
}

// - Verify that NewContextWindow returns the correct type for valid strategies
func Test_Unit_NewContextWindow_ValidStrategies_ReturnsCorrectType(t *testing.T) {
	for _, strategy := range windowTypeNames() {
		cw, err := NewContextWindow(10, strategy)
		if err != nil {
			t.Errorf("expected no error for strategy %s, got %v", strategy, err)
			continue
		}
		if cw == nil {
			t.Errorf("expected a valid context window for strategy %s, got nil", strategy)
			continue
		}
		// NewContextWindow reserves a 2-message buffer internally (headroom for the system
		// prompt + pending user message) — confirms that arithmetic, not just "didn't error".
		if wantMaxSize := 10 - 2; cw.GetMaxSize() != wantMaxSize {
			t.Errorf("strategy %s: expected max size %d, got %d", strategy, wantMaxSize, cw.GetMaxSize())
		}
	}
}

// - Verify that NewContextWindow returns an error and a nil context window for unsupported strategies
func Test_Unit_NewContextWindow_UnsupportedStrategy_ReturnsError(t *testing.T) {
	cw, err := NewContextWindow(10, "unsupported-strategy")
	if err == nil {
		t.Errorf("expected an error for an unsupported strategy, got nil")
	}
	if cw != nil {
		t.Errorf("expected context window to be nil for an unsupported strategy, got %v", cw)
	}
}

// - Verify that ContextWindow.RemoveLast correctly removes the newest N messages from the context window
func Test_Unit_ContextWindow_RemoveLast_RemovesNewestN(t *testing.T) {
	forEachWindow(t, 10, func(t *testing.T, w ContextWindow) {
		messages := msgs(5)
		w.AddMessages(messages)

		w.RemoveLast(2)

		expected := messages[:len(messages)-2]
		actual := w.GetMessages()
		if len(actual) != len(expected) {
			t.Fatalf("expected %d messages after removal, got %d", len(expected), len(actual))
		}
		for i := range expected {
			if actual[i] != expected[i] {
				t.Errorf("expected message at index %d to be %+v, got %+v", i, expected[i], actual[i])
			}
		}
	})
}

// Test_Unit_RemoveLast_MoreThanAvailable_PerType — deliberately not via forEachWindow, since the
// behavior is known to differ (see DECISIONS.md): Offset/InPlace/RingBuffer no-op when n exceeds
// the current size, LLWindow removes whatever it has. A shared assertion would be wrong for 3 of
// the 4 types, so this pins down each type's actual current behavior individually — making the
// inconsistency visible and tracked in the suite rather than only documented in prose.
func Test_Unit_RemoveLast_MoreThanAvailable_PerType(t *testing.T) {
	cases := []struct {
		name          string
		factory       func(maxSize int) ContextWindow
		expectedAfter int // messages remaining after RemoveLast(5) starting from 3
	}{
		{"offset", func(n int) ContextWindow { return NewOffsetWindow(n) }, 3},
		{"in-place", func(n int) ContextWindow { return NewInPlaceWindow(n) }, 3},
		{"ring-buffer", func(n int) ContextWindow { return NewRingBufferWindow(n) }, 3},
		{"linked-list", func(n int) ContextWindow { return NewLLWindow(n) }, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := c.factory(10)
			w.AddMessages(msgs(3))

			w.RemoveLast(5) // more than the 3 messages present

			if got := len(w.GetMessages()); got != c.expectedAfter {
				t.Errorf("expected %d messages to remain, got %d", c.expectedAfter, got)
			}
		})
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

		messages := msgs(3)
		chatContext.AddMessages(messages)

		chatContext.RemoveLast(1)

		expected := messages[:len(messages)-1]
		actual := chatContext.GetMessages()
		if len(actual) != len(expected) {
			t.Fatalf("expected %d messages after removal, got %d", len(expected), len(actual))
		}
		for i := range expected {
			if actual[i] != expected[i] {
				t.Errorf("expected message at index %d to be %+v, got %+v", i, expected[i], actual[i])
			}
		}
	})
}

// - Verify that adding messages to a context window and clearing it empties the window, for all window types
func Test_Unit_ContextWindow_Clear_EmptiesWindow(t *testing.T) {
	forEachWindow(t, 10, func(t *testing.T, w ContextWindow) {
		w.AddMessages(msgs(3))

		w.Clear()

		if w.GetSize() != 0 {
			t.Errorf("expected size 0 after clearing, got %d", w.GetSize())
		}
		if len(w.GetMessages()) != 0 {
			t.Errorf("expected 0 messages after clearing, got %d", len(w.GetMessages()))
		}
	})
}

// - Verify that setupProvider correctly returns a configured OpenAI provider.
func Test_Unit_SetupProvider_OpenAI_ReturnsConfiguredProvider(t *testing.T) {
	cfg := Config{
		Provider: "openai",
		Model:    "gpt-4o-mini",
		BaseURL:  "https://example.test/v1",
	}

	provider, err := setupProvider(cfg)
	if err != nil {
		t.Fatalf("setupProvider() returned an error: %v", err)
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

// - Verify that setupProvider returns an error for an unsupported provider.
func Test_Unit_SetupProvider_UnsupportedProvider_ReturnsError(t *testing.T) {
	provider, err := setupProvider(Config{Provider: "not-a-real-provider"})

	if err == nil {
		t.Errorf("expected an error for an unsupported provider, got nil")
	}
	if provider != nil {
		t.Errorf("expected a nil provider for an unsupported provider, got %v", provider)
	}
}

// - Verify that setupMemoryStore correctly returns an in-memory chat history.
func Test_Unit_SetupMemoryStore_InMemory_ReturnsHistory(t *testing.T) {
	history, err := setupMemoryStore(Config{ChatStoreType: "in-memory"})
	if err != nil {
		t.Fatalf("setupMemoryStore() returned an error: %v", err)
	}
	if _, ok := history.(*InMemoryChatHistory); !ok {
		t.Errorf("expected a *InMemoryChatHistory, got %T", history)
	}
}

// - Verify that setupMemoryStore returns an error for an unsupported chat store type.
func Test_Unit_SetupMemoryStore_UnsupportedType_ReturnsError(t *testing.T) {
	history, err := setupMemoryStore(Config{ChatStoreType: "not-a-real-store"})

	if err == nil {
		t.Errorf("expected an error for an unsupported chat store type, got nil")
	}
	if history != nil {
		t.Errorf("expected a nil history for an unsupported chat store type, got %v", history)
	}
}

// - Verify that setupChatContext computes the expected auto-compaction threshold from the
// package constants. Config{} (empty) is deliberate: setupChatContext currently ignores its cfg
// parameter entirely and builds the window from MaxContextWindow/WindowStrategy directly.
func Test_Unit_SetupChatContext_ComputesExpectedThreshold(t *testing.T) {
	chatContext, err := setupChatContext(Config{})
	if err != nil {
		t.Fatalf("setupChatContext() returned an error: %v", err)
	}

	wantMaxSize := MaxContextWindow - 2 // NewContextWindow reserves a 2-message buffer
	if chatContext.GetMaxSize() != wantMaxSize {
		t.Fatalf("expected max size %d, got %d", wantMaxSize, chatContext.GetMaxSize())
	}

	// ChatContext doesn't expose the raw threshold value directly, so exercise it indirectly
	// via IsAutoCompactionNeeded at the boundary.
	wantThreshold := int(float64(wantMaxSize) * AutoCompactThresholdFrac)
	chatContext.AddMessages(msgs(wantThreshold - 1))
	if chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction not to be needed yet at %d messages (threshold %d)", wantThreshold-1, wantThreshold)
	}
	chatContext.AddMessages(msgs(1))
	if !chatContext.IsAutoCompactionNeeded() {
		t.Errorf("expected auto-compaction to be needed at %d messages (threshold %d)", wantThreshold, wantThreshold)
	}
}
