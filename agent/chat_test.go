package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
