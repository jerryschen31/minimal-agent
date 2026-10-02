package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
//
// For multi-step (ReAct) tests, set Script to a list of replies to play back one per Chat call,
// e.g. a tool-call reply followed by a final text reply. Once Script is set, the fake is in
// "scripted" mode: running past the end of the script is an error (so a loop that calls Chat too
// many times fails loudly instead of silently reusing Reply). With Script unset, every call
// returns Reply, exactly as before. Err, if set, wins over both. ToolDefs records the tools
// argument of each call, parallel to Calls.
type fakeProvider struct {
	mu       sync.Mutex
	Reply    string
	Err      error
	Script   []ChatMessage
	next     int // index of the next Script entry to play
	Calls    [][]ChatMessage
	ToolDefs [][]ToolDef
}

func (p *fakeProvider) Chat(ctx context.Context, chatHistory []ChatMessage, tools []ToolDef) (ChatMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, append([]ChatMessage(nil), chatHistory...))
	p.ToolDefs = append(p.ToolDefs, append([]ToolDef(nil), tools...))

	if ctx.Err() != nil {
		return ChatMessage{}, ctx.Err()
	}
	if p.Err != nil {
		return ChatMessage{}, p.Err
	}
	if p.Script != nil {
		if p.next >= len(p.Script) {
			return ChatMessage{}, fmt.Errorf("fakeProvider: script exhausted after %d replies (Chat called %d times)", len(p.Script), len(p.Calls))
		}
		reply := p.Script[p.next]
		p.next++
		return reply, nil
	}
	return ChatMessage{Role: "assistant", Content: p.Reply}, nil
}

// assistantText builds a plain assistant reply with no tool calls (the "final answer" step).
func assistantText(text string) ChatMessage {
	return ChatMessage{Role: "assistant", Content: text}
}

// assistantToolCall builds an assistant reply that asks for one tool call. args is the raw
// JSON string the model would send, e.g. `{"path":"go.mod"}`.
func assistantToolCall(callID, toolName, args string) ChatMessage {
	return ChatMessage{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:       callID,
			Type:     "function",
			Function: ToolCallFunc{Name: toolName, Arguments: args},
		}},
	}
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
	Tools    *ToolRegistry // starts empty; tests that need tools call Tools.Register
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

	tools, err := NewToolRegistry(nil)
	if err != nil {
		t.Fatalf("NewToolRegistry() returned an error: %v", err)
	}

	session, err := NewChatSession(provider, history, chatContext, tools, cfg)
	if err != nil {
		t.Fatalf("NewChatSession() returned an error: %v", err)
	}

	return &chatSessionFixture{
		Session:  session,
		History:  history,
		Context:  chatContext,
		Provider: provider,
		Tools:    tools,
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

//*************************************//
// Slash command tests
//*************************************//

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

// - Verify that /config correctly prints the current agent configuration
func Test_ConfigCommand_PrintsCurrentConfiguration(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "/config")

	// Since the output is printed to the OutBuffer, we can check if it contains the expected configuration string
	output := fx.Out.String()
	if !strings.Contains(output, "Current agent configuration:") {
		t.Errorf("expected /config to print the current agent configuration, got output: %s", output)
	}
}

//*************************************//
// Config file loading: setDefaultConfig
//*************************************//

// captureStdout swaps os.Stdout for a temp file while fn runs and returns what was written.
// getDefaultConfig reads os.Stdout when it is called, so a Config built inside fn writes its
// OutBuffer output here. Tests using this must not run in parallel (os.Stdout is process-wide).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("creating stdout capture file: %v", err)
	}
	orig := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = orig }() // restore even if fn calls t.Fatalf (runtime.Goexit still runs defers)
	fn()
	f.Close()

	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(out)
}

// - Verify setDefaultConfig applies the values in the file, keeps defaults for keys the file
// omits, and prints no warning when every key is recognized
func Test_Unit_SetDefaultConfig_PopulatesFromFile_KeepsDefaultsForMissingKeys(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"userId": "jerry",
		"model": "qwen2.5:0.5b",
		"baseUrl": "http://127.0.0.1:11434/v1",
		"builtinTools": ["read_file"]
	}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = setDefaultConfig(path) })

	if err != nil {
		t.Fatalf("setDefaultConfig() returned an error: %v", err)
	}
	if cfg.UserID != "jerry" || cfg.Model != "qwen2.5:0.5b" || cfg.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Errorf("expected file values to be applied, got UserID=%q Model=%q BaseURL=%q", cfg.UserID, cfg.Model, cfg.BaseURL)
	}
	if len(cfg.BuiltinTools) != 1 || cfg.BuiltinTools[0] != "read_file" {
		t.Errorf("expected BuiltinTools [read_file], got %v", cfg.BuiltinTools)
	}

	defaults := getDefaultConfig()
	if cfg.Provider != defaults.Provider || cfg.SystemPrompt != defaults.SystemPrompt || cfg.ChatStoreType != defaults.ChatStoreType {
		t.Errorf("expected omitted keys to keep their defaults, got Provider=%q SystemPrompt=%q ChatStoreType=%q", cfg.Provider, cfg.SystemPrompt, cfg.ChatStoreType)
	}
	if cfg.InBuffer == nil || cfg.OutBuffer == nil {
		t.Errorf("expected InBuffer and OutBuffer to stay wired, got %v and %v", cfg.InBuffer, cfg.OutBuffer)
	}
	if out != "" {
		t.Errorf("expected no output when all keys are recognized, got %q", out)
	}
}

// - Verify setDefaultConfig returns an error (that errors.Is os.ErrNotExist recognizes) when the file is missing
func Test_Unit_SetDefaultConfig_MissingFile_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")

	_, err := setDefaultConfig(path)

	if err == nil {
		t.Fatalf("expected an error for a missing config file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected the error to wrap os.ErrNotExist, got %v", err)
	}
}

// - Verify keys Config has no field for produce one stdout warning each (sorted, naming the key),
// are ignored, and do not stop the recognized keys from being applied or cause an error
func Test_Unit_SetDefaultConfig_UnknownFields_WarnsAndIgnores(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"userId": "jerry",
		"zebra": 1,
		"subagents": true,
		"InBuffer": "not a reader"
	}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = setDefaultConfig(path) })

	if err != nil {
		t.Fatalf("expected unknown fields to be a warning, not an error, got: %v", err)
	}
	if cfg.UserID != "jerry" {
		t.Errorf("expected recognized key userId to still apply, got %q", cfg.UserID)
	}
	if cfg.InBuffer == nil {
		t.Errorf("expected InBuffer to be untouched by the file, got nil")
	}

	// sorted by key: "InBuffer" < "subagents" < "zebra" (uppercase sorts before lowercase)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 warning lines, got %d: %q", len(lines), out)
	}
	for i, key := range []string{"InBuffer", "subagents", "zebra"} {
		if !strings.HasPrefix(lines[i], "[warning]") || !strings.Contains(lines[i], fmt.Sprintf("%q", key)) {
			t.Errorf("line %d: expected a [warning] naming %q, got %q", i, key, lines[i])
		}
	}
}

// - Verify keys match case-insensitively like encoding/json does, so a key that was applied is not also reported as unrecognized
func Test_Unit_SetDefaultConfig_KeyCaseInsensitive_AppliedWithoutWarning(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{"UserId": "jerry"}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = setDefaultConfig(path) })

	if err != nil {
		t.Fatalf("setDefaultConfig() returned an error: %v", err)
	}
	if cfg.UserID != "jerry" {
		t.Errorf("expected UserId to apply to UserID, got %q", cfg.UserID)
	}
	if out != "" {
		t.Errorf("expected no warning for a case-variant of a known key, got %q", out)
	}
}

// - Verify config keys are camelCase: the old snake_case spellings are no longer recognized, so
// they warn and are ignored instead of silently applying
func Test_Unit_SetDefaultConfig_SnakeCaseKeys_WarnAndIgnored(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{"user_id": "jerry", "base_url": "http://x/v1", "apiKeyName": "MY_KEY"}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = setDefaultConfig(path) })

	if err != nil {
		t.Fatalf("setDefaultConfig() returned an error: %v", err)
	}
	defaults := getDefaultConfig()
	if cfg.UserID != defaults.UserID || cfg.BaseURL != defaults.BaseURL {
		t.Errorf("expected snake_case keys to be ignored (defaults kept), got UserID=%q BaseURL=%q", cfg.UserID, cfg.BaseURL)
	}
	if cfg.ApiKeyName != "MY_KEY" {
		t.Errorf("expected camelCase apiKeyName to apply, got %q", cfg.ApiKeyName)
	}
	for _, key := range []string{"user_id", "base_url"} {
		if !strings.Contains(out, fmt.Sprintf("%q", key)) {
			t.Errorf("expected a warning naming %q, got %q", key, out)
		}
	}
}

// - Verify malformed JSON (here a trailing comma) is an error that names the file
func Test_Unit_SetDefaultConfig_InvalidJSON_ReturnsError(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{"userId": "jerry",}`))

	_, err := setDefaultConfig(path)

	if err == nil {
		t.Fatalf("expected an error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("expected the error to name the file %q, got %v", path, err)
	}
}

// - Verify an empty filename reads DefaultConfigFile ("config.default.json") from the working directory
func Test_Unit_SetDefaultConfig_EmptyFilename_UsesDefaultConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultConfigFile), []byte(`{"userId": "from-default-file"}`), 0o644); err != nil {
		t.Fatalf("writing %s: %v", DefaultConfigFile, err)
	}
	t.Chdir(dir) // restored automatically when the test ends

	cfg, err := setDefaultConfig("")

	if err != nil {
		t.Fatalf("setDefaultConfig(\"\") returned an error: %v", err)
	}
	if cfg.UserID != "from-default-file" {
		t.Errorf("expected UserID from the default file, got %q", cfg.UserID)
	}
	if DefaultConfigFile != "config.default.json" {
		t.Errorf("expected DefaultConfigFile to be config.default.json, got %q", DefaultConfigFile)
	}
}

//*************************************//
// MCP server config: transportType
//*************************************//

// - Verify transportType infers the transport from the fields, honors a matching explicit type,
// and rejects ambiguous, unknown, contradicting and inapplicable-field entries (wantErr is a
// substring of the expected error; "" means success)
func Test_Unit_McpServerConfig_TransportType(t *testing.T) {
	env := map[string]string{"TOKEN": "x"}
	hdr := map[string]string{"Authorization": "Bearer x"}

	tests := []struct {
		name    string
		cfg     McpServerConfig
		want    string
		wantErr string
	}{
		// inferred from fields
		{"command only is stdio", McpServerConfig{Command: "npx"}, "stdio", ""},
		{"command, args and env is stdio", McpServerConfig{Command: "npx", Args: []string{"-y", "pkg"}, Env: env}, "stdio", ""},
		{"url only is http", McpServerConfig{URL: "https://x/mcp"}, "http", ""},
		{"url with headers is http", McpServerConfig{URL: "https://x/mcp", Headers: hdr}, "http", ""},
		// explicit type agreeing with the fields
		{"type stdio with command", McpServerConfig{Type: "stdio", Command: "npx"}, "stdio", ""},
		{"type http with url", McpServerConfig{Type: "http", URL: "https://x/mcp"}, "http", ""},
		{"type sse with url is passed through", McpServerConfig{Type: "sse", URL: "https://x/sse"}, "sse", ""},
		// shape errors
		{"both command and url", McpServerConfig{Command: "npx", URL: "https://x/mcp"}, "", "both"},
		{"neither command nor url", McpServerConfig{}, "", "set either"},
		{"only args set", McpServerConfig{Args: []string{"a"}}, "", "set either"},
		// type errors
		{"unknown type", McpServerConfig{Type: "htpp", URL: "https://x/mcp"}, "", "unknown type"},
		{"type is case-sensitive", McpServerConfig{Type: "HTTP", URL: "https://x/mcp"}, "", "unknown type"},
		{"type stdio without command", McpServerConfig{Type: "stdio", URL: "https://x/mcp"}, "", `needs "command"`},
		{"type http without url", McpServerConfig{Type: "http", Command: "npx"}, "", `needs "url"`},
		{"type sse without url", McpServerConfig{Type: "sse", Command: "npx"}, "", `needs "url"`},
		// fields that don't apply to the transport
		{"headers on stdio", McpServerConfig{Command: "npx", Headers: hdr}, "", `"headers"`},
		{"env on http", McpServerConfig{URL: "https://x/mcp", Env: env}, "", `"env"`},
		{"args on http", McpServerConfig{URL: "https://x/mcp", Args: []string{"a"}}, "", `"args"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.transportType()

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got (%q, %v)", tt.wantErr, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("expected transport %q, got %q", tt.want, got)
			}
		})
	}
}

// - Verify transportType does not modify the config: Type stays what the user wrote (empty here)
func Test_Unit_McpServerConfig_TransportType_DoesNotMutate(t *testing.T) {
	cfg := McpServerConfig{Command: "npx"}

	if _, err := cfg.transportType(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Type != "" {
		t.Errorf("expected Type to stay empty, got %q", cfg.Type)
	}
}

// - Verify an mcpServers block decodes from a config file in the usual MCP shape and each entry's transport is inferred
func Test_Unit_SetDefaultConfig_McpServers_DecodeAndInferTransport(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"mcpServers": {
			"fs":     {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"], "env": {"A": "b"}},
			"remote": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer x"}},
			"legacy": {"type": "sse", "url": "https://example.com/sse"}
		}
	}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = setDefaultConfig(path) })

	if err != nil {
		t.Fatalf("setDefaultConfig() returned an error: %v", err)
	}
	if out != "" {
		t.Errorf("expected no warnings (mcpServers and its inner keys are known), got %q", out)
	}
	want := map[string]string{"fs": "stdio", "remote": "http", "legacy": "sse"}
	if len(cfg.McpServers) != len(want) {
		t.Fatalf("expected %d servers, got %d: %+v", len(want), len(cfg.McpServers), cfg.McpServers)
	}
	for label, wantKind := range want {
		got, err := cfg.McpServers[label].transportType()
		if err != nil || got != wantKind {
			t.Errorf("server %q: expected %q, got (%q, %v)", label, wantKind, got, err)
		}
	}
	if fs := cfg.McpServers["fs"]; len(fs.Args) != 3 || fs.Env["A"] != "b" {
		t.Errorf("expected fs args and env to decode, got %+v", fs)
	}
}

//*************************************//
// MCP tools: newMCPTool
//*************************************//

// - Verify newMCPTool gives the model a "<server label>_<tool name>" name, keeps the server-side
// name for the call, carries the description and schema into the ToolDef, and shares the server pointer
func Test_Unit_NewMCPTool_BuildsToolDefAndKeepsServerSideName(t *testing.T) {
	server := &mcpServer{name: "fs"}
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []any{"path"},
	}

	tool, err := newMCPTool(server, &mcp.Tool{Name: "read_text_file", Description: "Reads a file", InputSchema: schema})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := tool.GetToolDefinition()
	if def.Function.Name != "fs_read_text_file" {
		t.Errorf("expected the model-facing name fs_read_text_file, got %q", def.Function.Name)
	}
	if tool.toolName != "read_text_file" {
		t.Errorf("expected the server-side name read_text_file, got %q", tool.toolName)
	}
	if def.Type != "function" || def.Function.Desc != "Reads a file" {
		t.Errorf("expected a function def with the description, got %+v", def)
	}
	var gotSchema map[string]any
	if err := json.Unmarshal(def.Function.Params, &gotSchema); err != nil || gotSchema["type"] != "object" || gotSchema["required"] == nil {
		t.Errorf("expected the input schema as JSON params, got %s (err %v)", def.Function.Params, err)
	}
	if tool.server != server {
		t.Errorf("expected the tool to share the server pointer, not a copy")
	}
}

// - Verify a tool with no input schema gets the default "no parameters" schema, not the JSON text "null"
func Test_Unit_NewMCPTool_NilSchema_UsesDefaultParams(t *testing.T) {
	tool, err := newMCPTool(&mcpServer{name: "s"}, &mcp.Tool{Name: "ping"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(tool.GetToolDefinition().Function.Params); got != `{"type":"object","properties":{}}` {
		t.Errorf("expected the default empty-object schema, got %s", got)
	}
}

// - Verify a schema that can't be marshalled to JSON is an error naming the tool
func Test_Unit_NewMCPTool_UnmarshalableSchema_ReturnsError(t *testing.T) {
	_, err := newMCPTool(&mcpServer{name: "s"}, &mcp.Tool{Name: "bad", InputSchema: make(chan int)})

	if err == nil || !strings.Contains(err.Error(), `"bad"`) {
		t.Errorf("expected an error naming the tool, got %v", err)
	}
}

// - Verify safeToRetry is true only when annotations say the tool is read-only or idempotent (nil annotations = not safe)
func Test_Unit_NewMCPTool_SafeToRetry_FromAnnotations(t *testing.T) {
	tests := []struct {
		name string
		ann  *mcp.ToolAnnotations
		want bool
	}{
		{"nil annotations", nil, false},
		{"empty annotations", &mcp.ToolAnnotations{}, false},
		{"read-only", &mcp.ToolAnnotations{ReadOnlyHint: true}, true},
		{"idempotent", &mcp.ToolAnnotations{IdempotentHint: true}, true},
		{"title only", &mcp.ToolAnnotations{Title: "Move file"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, err := newMCPTool(&mcpServer{name: "s"}, &mcp.Tool{Name: "x", Annotations: tt.ann})

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tool.safeToRetry != tt.want {
				t.Errorf("expected safeToRetry=%v, got %v", tt.want, tool.safeToRetry)
			}
		})
	}
}

//*************************************//
// MCP tool results: flattenMCPResult
//*************************************//

// - Verify flattenMCPResult turns each kind of result into the right text, or into an error when
// the tool reported failure (wantErr is a substring of the expected error; "" means success)
func Test_Unit_FlattenMCPResult(t *testing.T) {
	text := func(s string) mcp.Content { return &mcp.TextContent{Text: s} }

	tests := []struct {
		name    string
		res     *mcp.CallToolResult
		want    string
		wantErr string
	}{
		{"one text block", &mcp.CallToolResult{Content: []mcp.Content{text("hello")}}, "hello", ""},
		{"text blocks joined by newline", &mcp.CallToolResult{Content: []mcp.Content{text("a"), text("b")}}, "a\nb", ""},
		{"image becomes a placeholder", &mcp.CallToolResult{Content: []mcp.Content{text("see:"), &mcp.ImageContent{MIMEType: "image/png", Data: []byte("x")}}}, "see:\n[image omitted: image/png]", ""},
		{"audio becomes a generic placeholder", &mcp.CallToolResult{Content: []mcp.Content{&mcp.AudioContent{MIMEType: "audio/wav"}}}, "[non-text content omitted]", ""},
		{"no content, structured output is used", &mcp.CallToolResult{StructuredContent: map[string]any{"n": 1}}, `{"n":1}`, ""},
		{"content wins over structured output", &mcp.CallToolResult{Content: []mcp.Content{text("hi")}, StructuredContent: map[string]any{"n": 1}}, "hi", ""},
		{"nothing at all is empty text", &mcp.CallToolResult{}, "", ""},
		{"IsError returns the text as an error", &mcp.CallToolResult{IsError: true, Content: []mcp.Content{text("no such file")}}, "", "no such file"},
		{"IsError without text still errors", &mcp.CallToolResult{IsError: true}, "", "no message"},
		{"nil result is an error", nil, "", "no result"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := flattenMCPResult(tt.res)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got (%q, %v)", tt.wantErr, got, err)
				}
				if got != "" {
					t.Errorf("expected empty text alongside the error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// - Verify a result over MCPResultMaxBytes is cut at the cap with a truncation note, and a result exactly at the cap is left alone
func Test_Unit_FlattenMCPResult_Truncation(t *testing.T) {
	big := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}

	exact, err := flattenMCPResult(big(strings.Repeat("a", MCPResultMaxBytes)))
	if err != nil || len(exact) != MCPResultMaxBytes || strings.Contains(exact, "truncated") {
		t.Errorf("expected a result exactly at the cap to be untouched, got len=%d err=%v", len(exact), err)
	}

	over, err := flattenMCPResult(big(strings.Repeat("a", MCPResultMaxBytes+10)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(over, strings.Repeat("a", MCPResultMaxBytes)) || !strings.HasSuffix(over, "KB]") || !strings.Contains(over, "[truncated:") {
		t.Errorf("expected the cap's worth of text plus a truncation note, got len=%d tail=%q", len(over), over[len(over)-45:])
	}
}

// - Verify truncation never leaves half of a multi-byte character (invalid UTF-8) in the result
func Test_Unit_FlattenMCPResult_Truncation_DoesNotSplitMultiByteCharacter(t *testing.T) {
	// "a" then 2-byte "é" repeated: byte MCPResultMaxBytes-1 is the first half of an "é", so a raw byte cut would split it
	s := "a" + strings.Repeat("é", MCPResultMaxBytes)

	got, err := flattenMCPResult(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Errorf("expected valid UTF-8 after truncation")
	}
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("expected a truncation note, got tail %q", got[len(got)-45:])
	}
}

// - Verify that text after /clear (e.g., "/clear what is 3 + 2?") is ignored: the context is still cleared, and the text is never sent or stored.
func Test_ContextWindow_ClearWithTrailingText_TextIgnored(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")
	if fx.Context.GetSize() == 0 {
		t.Fatalf("test setup problem: expected some context before clearing")
	}
	callsBefore := len(fx.Provider.Calls)
	historyBefore := len(fx.History.GetMessages())

	handleUserInput(ctx, fx.Session, "/clear what is 3 + 2?")

	// the command itself still runs
	if fx.Context.GetSize() != 0 {
		t.Errorf("expected /clear with trailing text to still clear the context, got %+v", fx.Context.GetMessages())
	}
	// the trailing text is not sent as a chat turn...
	if len(fx.Provider.Calls) != callsBefore {
		t.Errorf("expected no provider call for text after /clear, got %d new calls", len(fx.Provider.Calls)-callsBefore)
	}
	// ...and not recorded anywhere
	if len(fx.History.GetMessages()) != historyBefore {
		t.Errorf("expected chat history unchanged by /clear with trailing text, got %+v", fx.History.GetMessages())
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

//*************************************//
// Basic context compaction tests
//*************************************//

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

	_, err := compactChatContext(context.Background(), fx.Session, CompactionManual, "")
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
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err == nil {
		t.Fatalf("expected an error when the provider is unreachable, got reply %q", reply.Content)
	}
}

// - Verify a non-2xx response code errors gracefully
func Test_ChatRequest_Non2xxResponseErrorsGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err == nil {
		t.Fatalf("expected an error for a non-2xx response, got reply %q", reply.Content)
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
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

		if err == nil {
			t.Fatalf("expected an error for an empty response body, got reply %q", reply.Content)
		}
	})

	t.Run("valid JSON but no choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResponse{Choices: nil})
		}))
		defer srv.Close()

		p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

		if err == nil {
			t.Fatalf("expected an error for a response with no choices, got reply %q", reply.Content)
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
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err != nil {
		t.Fatalf("Chat() returned an unexpected error: %v", err)
	}
	if reply.Content != "hi there" {
		t.Errorf("expected reply %q, got %q", "hi there", reply.Content)
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
		if clamped[i].ID != messages[i].ID {
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
		if clamped[i].ID != messages[i].ID {
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
	if clamped[0].ID != messages[0].ID {
		t.Errorf("expected the first message (summary) to be unchanged")
	}
	for i := 1; i < max; i++ {
		if clamped[i].ID != messages[len(messages)-max+i].ID {
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
			if actual[i].ID != expected[i].ID {
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
			if actual[i].ID != expected[i].ID {
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

//*************************************//
// Built-in tools: read_file
//*************************************//

// writeTempFile creates a file with the given contents in a per-test temp dir (deleted by Go
// after the test) and returns its path.
func writeTempFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}

// readFileArgs builds the JSON arguments the model would send for read_file.
func readFileArgs(path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": path})
	return b
}

// - Verify that a small text file is returned in full, unchanged
func Test_ReadFile_SmallFile_ReturnsContents(t *testing.T) {
	path := writeTempFile(t, "minagent-read-file-test-small.txt", []byte("hello"))

	got, err := ReadFileTool{}.CallTool(context.Background(), readFileArgs(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("expected %q, got %q", "hello", got)
	}
}

// - Verify that a file of exactly ReadFileMaxBytes is returned in full with no truncation note
// (the +1 byte read is what tells "exactly at the cap" apart from "over the cap")
func Test_ReadFile_ExactlyMaxBytes_NotTruncated(t *testing.T) {
	path := writeTempFile(t, "minagent-read-file-test-exact.txt", bytes.Repeat([]byte("a"), ReadFileMaxBytes))

	got, err := ReadFileTool{}.CallTool(context.Background(), readFileArgs(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != ReadFileMaxBytes {
		t.Errorf("expected %d bytes, got %d", ReadFileMaxBytes, len(got))
	}
	if strings.Contains(got, "[truncated") {
		t.Errorf("expected no truncation note for a file exactly at the cap")
	}
}

// - Verify that a file over ReadFileMaxBytes returns the first ReadFileMaxBytes plus a truncation
// note, so the model knows the file doesn't really end there
func Test_ReadFile_OverMaxBytes_TruncatedWithNote(t *testing.T) {
	path := writeTempFile(t, "minagent-read-file-test-big.txt", bytes.Repeat([]byte("a"), ReadFileMaxBytes+10))
	const note = "\n[truncated: file is larger than 64 KB]"

	got, err := ReadFileTool{}.CallTool(context.Background(), readFileArgs(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, note) {
		t.Fatalf("expected output to end with the truncation note, got tail %q", got[len(got)-50:])
	}
	if len(got) != ReadFileMaxBytes+len(note) {
		t.Errorf("expected %d content bytes before the note, got %d", ReadFileMaxBytes, len(got)-len(note))
	}
}

// - Verify that every failure is returned as an error (for the model to read), never a panic
// and never an empty "success"
func Test_ReadFile_InvalidInputs_ReturnErrors(t *testing.T) {
	dir := t.TempDir()
	binary := writeTempFile(t, "minagent-read-file-test-bin.dat", []byte{'a', 0, 'b'})

	cases := map[string]struct {
		args    json.RawMessage
		wantErr string // substring the error message must contain
	}{
		"bad json":     {json.RawMessage(`{"path":`), "invalid arguments"},
		"empty path":   {json.RawMessage(`{}`), "missing required argument: path"},
		"missing file": {readFileArgs(filepath.Join(dir, "nope.txt")), "no such file"},
		"directory":    {readFileArgs(dir), "is a directory"},
		"binary file":  {readFileArgs(binary), "binary file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ReadFileTool{}.CallTool(context.Background(), tc.args)
			if err == nil {
				t.Fatalf("expected an error, got nil (output %q)", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
			if got != "" {
				t.Errorf("expected empty output on error, got %q", got)
			}
		})
	}
}

//*************************************//
// Tool registry
//*************************************//

// stubTool is a minimal Tool for registry tests. Tag lets a test tell two tools with the same
// name apart (e.g. to check which one survived a duplicate registration).
type stubTool struct {
	name string
	Tag  string
}

func (s stubTool) GetToolDefinition() ToolDef { return NewToolDef(s.name, "stub tool", nil) }
func (s stubTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return s.Tag, nil
}

// defNames returns the tool names from a slice of definitions, in order.
func defNames(defs []ToolDef) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Function.Name
	}
	return names
}

// newRegistry builds a registry from tools, failing the test on error.
func newRegistry(t *testing.T, tools ...Tool) *ToolRegistry {
	t.Helper()
	reg, err := NewToolRegistry(tools)
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	return reg
}

// - Verify that tools registered out of order come back from GetToolDefs sorted by name
// (the order must not depend on registration order or map iteration order)
func Test_ToolRegistry_Register_OutOfOrder_DefsSortedByName(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "zeta"}, stubTool{name: "alpha"}, stubTool{name: "mid"})

	got := strings.Join(defNames(reg.GetToolDefs()), ",")
	if want := "alpha,mid,zeta"; got != want {
		t.Errorf("expected defs %q, got %q", want, got)
	}
}

// - Verify that the defs stay in the same order across repeated calls and rebuilds
// (map iteration is random, so a missing sort would show up as flakiness here)
func Test_ToolRegistry_GetToolDefs_OrderStableAcrossRebuilds(t *testing.T) {
	reg := newRegistry(t)
	for _, n := range []string{"e", "b", "d", "a", "c"} {
		if err := reg.Register(stubTool{name: n}); err != nil {
			t.Fatalf("Register(%q): %v", n, err)
		}
		if names := defNames(reg.GetToolDefs()); !sort.StringsAreSorted(names) {
			t.Fatalf("defs not sorted after registering %q: %v", n, names)
		}
	}
}

// - Verify that registering a duplicate name is an error and the original tool is kept
func Test_ToolRegistry_Register_DuplicateName_ErrorsAndKeepsOriginal(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "dup", Tag: "first"})

	err := reg.Register(stubTool{name: "dup", Tag: "second"})
	if err == nil {
		t.Fatalf("expected an error registering a duplicate name, got nil")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected error to mention 'already registered', got %q", err.Error())
	}
	if n := len(reg.GetToolDefs()); n != 1 {
		t.Errorf("expected 1 def after a rejected duplicate, got %d", n)
	}
	tool, ok := reg.Lookup("dup")
	if !ok {
		t.Fatalf("expected the original tool to still be registered")
	}
	if got, _ := tool.CallTool(context.Background(), nil); got != "first" {
		t.Errorf("expected original tool (tag %q) to be kept, got tag %q", "first", got)
	}
}

// - Verify that NewToolRegistry surfaces a duplicate among its arguments as an error
func Test_ToolRegistry_NewToolRegistry_DuplicateInArgs_ReturnsError(t *testing.T) {
	reg, err := NewToolRegistry([]Tool{stubTool{name: "x"}, stubTool{name: "x"}})
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if reg != nil {
		t.Errorf("expected a nil registry on error, got %+v", reg)
	}
}

// - Verify that a tool with an empty name is rejected and leaves the registry unchanged
func Test_ToolRegistry_Register_EmptyName_Errors(t *testing.T) {
	reg := newRegistry(t)

	err := reg.Register(stubTool{name: ""})
	if err == nil {
		t.Fatalf("expected an error registering an empty name, got nil")
	}
	if n := len(reg.GetToolDefs()); n != 0 {
		t.Errorf("expected no defs after a rejected registration, got %d", n)
	}
}

// - Verify that a nil Tool is rejected with an error, not a panic
// (a typed nil pointer is deliberately not handled; see the note on Register)
func Test_ToolRegistry_Register_NilTool_Errors(t *testing.T) {
	reg := newRegistry(t)

	if err := reg.Register(nil); err == nil {
		t.Errorf("expected an error registering a nil tool, got nil")
	}
	if n := len(reg.GetToolDefs()); n != 0 {
		t.Errorf("expected no defs after a rejected registration, got %d", n)
	}
}

// - Verify that Remove drops both the tool and its definition, keeping the rest in sorted order
func Test_ToolRegistry_Remove_DropsToolAndDef(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "c"}, stubTool{name: "a"}, stubTool{name: "b"})

	if !reg.Remove("b") {
		t.Fatalf("expected Remove(%q) to return true", "b")
	}
	if _, ok := reg.Lookup("b"); ok {
		t.Errorf("expected %q to be gone from Lookup", "b")
	}
	if got, want := strings.Join(defNames(reg.GetToolDefs()), ","), "a,c"; got != want {
		t.Errorf("expected defs %q after Remove, got %q", want, got)
	}
}

// - Verify that Remove on an unknown name returns false and changes nothing
func Test_ToolRegistry_Remove_UnknownName_ReturnsFalse(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "a"})

	if reg.Remove("nope") {
		t.Errorf("expected Remove of an unknown name to return false")
	}
	if n := len(reg.GetToolDefs()); n != 1 {
		t.Errorf("expected registry unchanged (1 def), got %d", n)
	}
}

// - Verify that a removed name can be registered again
func Test_ToolRegistry_Remove_ThenRegisterAgain_Succeeds(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "a", Tag: "old"})
	reg.Remove("a")

	if err := reg.Register(stubTool{name: "a", Tag: "new"}); err != nil {
		t.Fatalf("re-registering a removed name: %v", err)
	}
	tool, _ := reg.Lookup("a")
	if got, _ := tool.CallTool(context.Background(), nil); got != "new" {
		t.Errorf("expected the new tool (tag %q), got tag %q", "new", got)
	}
}

// - Verify Lookup finds registered tools and reports (nil, false) for unknown names
func Test_ToolRegistry_Lookup_KnownAndUnknown(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "known"})

	if tool, ok := reg.Lookup("known"); !ok || tool == nil {
		t.Errorf("expected Lookup(%q) to find the tool, got (%v, %v)", "known", tool, ok)
	}
	if tool, ok := reg.Lookup("unknown"); ok || tool != nil {
		t.Errorf("expected Lookup(%q) to return (nil, false), got (%v, %v)", "unknown", tool, ok)
	}
}

// - Verify an empty registry has no defs (so the request's "tools" key is omitted)
func Test_ToolRegistry_GetToolDefs_EmptyRegistry_NoDefs(t *testing.T) {
	reg := newRegistry(t)

	if n := len(reg.GetToolDefs()); n != 0 {
		t.Errorf("expected no defs, got %d", n)
	}
}

// - Verify copy-on-write: a slice handed out earlier is not changed by later Register/Remove calls
// (this is what lets GetToolDefs return its slice without copying it)
func Test_ToolRegistry_GetToolDefs_EarlierSliceUnaffectedByLaterChanges(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "a"}, stubTool{name: "c"})
	before := reg.GetToolDefs()

	if err := reg.Register(stubTool{name: "b"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.Remove("a")

	if got, want := strings.Join(defNames(before), ","), "a,c"; got != want {
		t.Errorf("earlier slice changed: expected %q, got %q", want, got)
	}
	if got, want := strings.Join(defNames(reg.GetToolDefs()), ","), "b,c"; got != want {
		t.Errorf("expected current defs %q, got %q", want, got)
	}
}

// - Verify concurrent Register, GetToolDefs and Lookup are safe (run with -race) and that
// every registration lands
func Test_ToolRegistry_ConcurrentRegisterAndRead_RaceFree(t *testing.T) {
	const writers = 20
	reg := newRegistry(t)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(2)
		name := fmt.Sprintf("tool_%02d", i)
		go func() {
			defer wg.Done()
			if err := reg.Register(stubTool{name: name}); err != nil {
				t.Errorf("Register(%q): %v", name, err)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reg.GetToolDefs()
				reg.Lookup(name)
			}
		}()
	}
	wg.Wait()

	names := defNames(reg.GetToolDefs())
	if len(names) != writers {
		t.Errorf("expected %d defs, got %d", writers, len(names))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("expected defs sorted by name, got %v", names)
	}
}

//*************************************//
// Running tool calls
//*************************************//

// panicTool is a Tool whose CallTool always panics with the given behavior.
type panicTool struct{ boom func() }

func (panicTool) GetToolDefinition() ToolDef { return NewToolDef("boom", "always panics", nil) }
func (p panicTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	p.boom()
	return "unreachable", nil
}

// - Verify that a tool that panics (explicitly, or via a runtime error) becomes an error
// observation for the model instead of crashing the agent
func Test_RunToolCall_ToolPanics_RecoveredAsErrorMessage(t *testing.T) {
	cases := map[string]struct {
		boom    func()
		wantErr string // substring the message content must contain
	}{
		"explicit panic": {func() { panic("kaboom") }, "kaboom"},
		"runtime error": {func() {
			var m map[string]int
			m["a"] = 1 // write to a nil map panics
		}, "nil map"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newChatSessionFixture(t, 10, 9)
			if err := fx.Tools.Register(panicTool{boom: tc.boom}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			call := ToolCall{ID: "call_1", Function: ToolCallFunc{Name: "boom", Arguments: "{}"}}

			got := runToolCall(context.Background(), fx.Session, call)

			if got.Role != "tool" {
				t.Errorf("expected role %q, got %q", "tool", got.Role)
			}
			if got.ToolCallID != "call_1" {
				t.Errorf("expected ToolCallID %q, got %q", "call_1", got.ToolCallID)
			}
			if !strings.HasPrefix(got.Content, "error:") || !strings.Contains(got.Content, "tool panicked") ||
				!strings.Contains(got.Content, tc.wantErr) {
				t.Errorf("expected content like %q mentioning %q, got %q", "error: tool panicked: …", tc.wantErr, got.Content)
			}
		})
	}
}

//*************************************//
// Fixture self-tests
//*************************************//

// - Verify the scripted fakeProvider plays replies in order, records the messages and tools of
// every call, and fails loudly (rather than reusing Reply) once the script runs out
func Test_FakeProvider_Script_PlaysRepliesInOrderThenErrors(t *testing.T) {
	p := &fakeProvider{Reply: "unscripted", Script: []ChatMessage{
		assistantToolCall("c1", "read_file", `{"path":"go.mod"}`),
		assistantText("done"),
	}}
	defs := []ToolDef{ReadFileTool{}.GetToolDefinition()}
	ctx := context.Background()

	first, err := p.Chat(ctx, []ChatMessage{msg("user", "u1", "hi")}, defs)
	if err != nil {
		t.Fatalf("call 1: unexpected error: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != "c1" || first.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("call 1: expected the read_file tool call c1, got %+v", first.ToolCalls)
	}

	second, err := p.Chat(ctx, nil, nil)
	if err != nil {
		t.Fatalf("call 2: unexpected error: %v", err)
	}
	if second.Content != "done" || len(second.ToolCalls) != 0 {
		t.Errorf("call 2: expected final text %q with no tool calls, got %+v", "done", second)
	}

	if _, err := p.Chat(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "script exhausted") {
		t.Errorf("call 3: expected a 'script exhausted' error, got %v", err)
	}

	if len(p.Calls) != 3 || len(p.ToolDefs) != 3 {
		t.Fatalf("expected 3 recorded calls and 3 recorded tool lists, got %d and %d", len(p.Calls), len(p.ToolDefs))
	}
	if len(p.Calls[0]) != 1 || p.Calls[0][0].ID != "u1" {
		t.Errorf("expected call 1 to record the user message, got %+v", p.Calls[0])
	}
	if len(p.ToolDefs[0]) != 1 || p.ToolDefs[0][0].Function.Name != "read_file" {
		t.Errorf("expected call 1 to record the read_file def, got %+v", p.ToolDefs[0])
	}
	if len(p.ToolDefs[1]) != 0 {
		t.Errorf("expected call 2 to record no tools, got %+v", p.ToolDefs[1])
	}
}

// - Verify an unscripted fakeProvider still returns Reply on every call (the behavior the
// existing tests depend on)
func Test_FakeProvider_NoScript_ReturnsReplyEveryCall(t *testing.T) {
	p := &fakeProvider{Reply: "same"}

	for i := 0; i < 3; i++ {
		got, err := p.Chat(context.Background(), nil, nil)
		if err != nil || got.Role != "assistant" || got.Content != "same" {
			t.Fatalf("call %d: expected assistant %q, got (%+v, %v)", i+1, "same", got, err)
		}
	}
}

// ****************************************
// Connecting to MCP servers (remote HTTP + local stdio)
// ****************************************

// newFakeMCPServer builds an in-process MCP server with one tool, "echo", that always replies "hi".
func newFakeMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "replies hi"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}, nil, nil
		})
	return server
}

type recordedRequest struct {
	method string
	header http.Header
}

// startFakeRemoteMCP serves newFakeMCPServer over Streamable HTTP and records every HTTP request it
// receives (method + headers). It speaks only the classic initialize handshake. It returns the server URL and a function that returns the recording so far.
func startFakeRemoteMCP(t *testing.T) (string, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var recorded []recordedRequest

	sdkHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newFakeMCPServer() }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = append(recorded, recordedRequest{method: r.Method, header: r.Header.Clone()})
		mu.Unlock()

		// Reject the new-spec "server/discover" handshake like DeepWiki does, so the client falls back to the
		// classic "initialize" handshake (protocol 2025-11-25). Only on that path does the client try to open the
		// standalone GET stream (it is removed in 2026-07-28+), which is what DisableStandaloneSSE must prevent.
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"server/discover"`)) {
				http.Error(w, "Bad Request: Unsupported protocol version: 2026-07-28", http.StatusBadRequest)
				return
			}
		}
		sdkHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), recorded...)
	}
}

// checkEchoRoundTrip lists the fake server's tools and calls echo, proving the connection really works.
func checkEchoRoundTrip(ctx context.Context, t *testing.T, s *mcpServer) {
	t.Helper()
	tools, err := getMCPTools(ctx, s)
	if err != nil {
		t.Fatalf("getMCPTools: %v", err)
	}
	if len(tools) != 1 || tools[0].GetToolDefinition().Function.Name != s.name+"_echo" {
		t.Fatalf("expected one tool named %s_echo, got %+v", s.name, tools)
	}
	got, err := tools[0].CallTool(ctx, json.RawMessage(`{}`))
	if err != nil || got != "hi" {
		t.Fatalf("expected echo to return %q, got (%q, %v)", "hi", got, err)
	}
}

// - Verify a remote server with no configured headers connects, lists and calls tools, and sends no
// Authorization header, and (DisableStandaloneSSE) never opens the long-lived GET stream.
func Test_Unit_ConnectRemoteMCP_NoHeaders_WorksAndSendsNoAuthorization(t *testing.T) {
	url, requests := startFakeRemoteMCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectRemoteMCP(ctx, "remote", McpServerConfig{URL: url})
	if err != nil {
		t.Fatalf("connectRemoteMCP: %v", err)
	}
	checkEchoRoundTrip(ctx, t, s)
	if err := s.Close(); err != nil {
		t.Logf("Close: %v", err)
	}

	got := requests()
	if len(got) == 0 {
		t.Fatal("expected the fake server to receive requests")
	}
	for i, r := range got {
		if r.method == http.MethodGet {
			t.Errorf("request %d was a GET; expected DisableStandaloneSSE to prevent the standalone stream", i)
		}
		if a := r.header.Get("Authorization"); a != "" {
			t.Errorf("request %d (%s) carried Authorization %q though none was configured", i, r.method, a)
		}
	}
}

// - Verify configured headers are sent on every HTTP request (initialize, tools/list, tools/call, close),
// and still no GET.
func Test_Unit_ConnectRemoteMCP_WithHeaders_SentOnEveryRequest(t *testing.T) {
	url, requests := startFakeRemoteMCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := McpServerConfig{URL: url, Headers: map[string]string{
		"Authorization": "Bearer test-token",
		"X-Example":     "hello",
	}}

	s, err := connectRemoteMCP(ctx, "remote", cfg)
	if err != nil {
		t.Fatalf("connectRemoteMCP: %v", err)
	}
	checkEchoRoundTrip(ctx, t, s)
	if err := s.Close(); err != nil {
		t.Logf("Close: %v", err)
	}

	got := requests()
	// initialize + initialized + tools/list + tools/call is at least 4 POSTs
	if len(got) < 4 {
		t.Fatalf("expected at least 4 requests, got %d", len(got))
	}
	for i, r := range got {
		if r.method == http.MethodGet {
			t.Errorf("request %d was a GET; expected DisableStandaloneSSE to prevent the standalone stream", i)
		}
		for k, want := range cfg.Headers {
			if v := r.header.Get(k); v != want {
				t.Errorf("request %d (%s): header %s = %q, want %q", i, r.method, k, v, want)
			}
		}
	}
}

// - Verify connectRemoteMCP wraps a connection failure with the server name instead of panicking.
func Test_Unit_ConnectRemoteMCP_Unreachable_ReturnsErrorNamingServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening now
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectRemoteMCP(ctx, "gone", McpServerConfig{URL: url})

	if err == nil || s != nil {
		t.Fatalf("expected (nil, error), got (%v, %v)", s, err)
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("expected the error to name the server, got %q", err)
	}
}

// stubRoundTripper records the request it receives and returns a canned response or error.
type stubRoundTripper struct {
	got  *http.Request
	resp *http.Response
	err  error
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.got = req
	return s.resp, s.err
}

// - Verify headerTransport adds every header to the forwarded request, leaves the caller's request
// untouched (the RoundTripper contract), and returns the base transport's response.
func Test_Unit_HeaderTransport_AddsHeaders_WithoutMutatingOriginal(t *testing.T) {
	want := &http.Response{StatusCode: http.StatusTeapot}
	base := &stubRoundTripper{resp: want}
	ht := &headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer abc", "X-Example": "hello"}}
	orig, _ := http.NewRequest(http.MethodPost, "http://example.invalid/mcp", nil)
	orig.Header.Set("Content-Type", "application/json")

	resp, err := ht.RoundTrip(orig)

	if err != nil || resp != want {
		t.Fatalf("expected the base response and no error, got (%v, %v)", resp, err)
	}
	if base.got == nil {
		t.Fatal("base transport was never called")
	}
	if base.got.Header.Get("Authorization") != "Bearer abc" || base.got.Header.Get("X-Example") != "hello" {
		t.Errorf("forwarded request is missing headers: %v", base.got.Header)
	}
	if base.got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("existing headers should be kept, got %v", base.got.Header)
	}
	if orig.Header.Get("Authorization") != "" || orig.Header.Get("X-Example") != "" {
		t.Errorf("caller's request was mutated: %v", orig.Header)
	}
}

// - Verify configured headers replace (Set, not Add) a same-named header already on the request.
func Test_Unit_HeaderTransport_ReplacesExistingHeader(t *testing.T) {
	base := &stubRoundTripper{resp: &http.Response{StatusCode: 200}}
	ht := &headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer new"}}
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/mcp", nil)
	req.Header.Set("Authorization", "Bearer old")

	if _, err := ht.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if vals := base.got.Header.Values("Authorization"); len(vals) != 1 || vals[0] != "Bearer new" {
		t.Errorf("expected exactly [Bearer new], got %v", vals)
	}
}

// - Verify nil headers pass the request through, and a base error is returned unchanged.
func Test_Unit_HeaderTransport_NilHeaders_PassesThrough_AndPropagatesError(t *testing.T) {
	boom := errors.New("network down")
	base := &stubRoundTripper{err: boom}
	ht := &headerTransport{base: base} // nil headers map
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/mcp", nil)

	_, err := ht.RoundTrip(req)

	if !errors.Is(err, boom) {
		t.Errorf("expected the base error, got %v", err)
	}
	if base.got == nil {
		t.Error("base transport was never called")
	}
}

// TestHelperMCPServer is not a real test: connectLocalMCP's test re-runs this test binary as the child
// process and this function turns that child into a stdio MCP server. It is skipped in normal runs.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		t.Skip("helper process for the connectLocalMCP test, not a real test")
	}
	if err := newFakeMCPServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "helper MCP server:", err)
		os.Exit(1)
	}
	os.Exit(0) // exit here so the test framework does not print PASS onto the protocol stream (stdout)
}

// - Verify a local stdio server (this test binary re-run as the helper above) connects, lists and calls tools.
func Test_Unit_ConnectLocalMCP_StdioServer_ListsAndCallsTools(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// the child inherits our environment (connectLocalMCP does not apply config.Env), so set it here
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := connectLocalMCP(ctx, "local", McpServerConfig{Command: exe, Args: []string{"-test.run=^TestHelperMCPServer$"}})
	if err != nil {
		t.Fatalf("connectLocalMCP: %v", err)
	}
	defer s.Close()

	checkEchoRoundTrip(ctx, t, s)
}

// - Verify a command that cannot start returns an error naming the server.
func Test_Unit_ConnectLocalMCP_BadCommand_ReturnsErrorNamingServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectLocalMCP(ctx, "broken", McpServerConfig{Command: "definitely-not-a-real-command-xyz"})

	if err == nil || s != nil {
		t.Fatalf("expected (nil, error), got (%v, %v)", s, err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("expected the error to name the server, got %q", err)
	}
}
