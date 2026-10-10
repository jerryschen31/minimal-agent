package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/model/modeltest"
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

// ***********************************************************//
// Chat loop: Ctrl+C (interrupt) handling
// ***********************************************************//

// syncBuffer is a mutex-guarded output writer. The chat loop and a background auto-compaction write
// output from different goroutines; a plain bytes.Buffer would be a data race under -race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startChatLoop runs runChatLoop in the background on the given input lines (read by the plain
// reader, since a strings.Reader is not a terminal). It returns the loop's output and a channel
// that is closed when the loop returns.
func startChatLoop(fx *chatSessionFixture, input string, interrupts <-chan os.Signal) (*syncBuffer, <-chan struct{}) {
	out := &syncBuffer{}
	fx.Session.InBuffer = strings.NewReader(input)
	fx.Session.OutBuffer = out
	done := make(chan struct{})
	go func() {
		defer close(done)
		runChatLoop(context.Background(), interrupts, fx.Session)
	}()
	return out, done
}

// receiveOrFail waits for a value on ch, failing the test instead of hanging forever
func receiveOrFail(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitForOutput polls until the output contains want, failing the test after a timeout
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in output:\n%s", want, out.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// - Verify Ctrl+C during a turn prints [interrupted], adds nothing to memory for that turn, and the
// loop carries on with the next input
func Test_ChatLoop_InterruptMidTurn_PrintsInterruptedAndContinues(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	interrupts := make(chan os.Signal, 1)

	out, done := startChatLoop(fx, "first\nsecond\n", interrupts)

	receiveOrFail(t, fx.Provider.Waiting, "turn 1 to reach the model")
	interrupts <- os.Interrupt // Ctrl+C while turn 1 is in progress
	receiveOrFail(t, fx.Provider.Waiting, "turn 2 to reach the model (loop should continue after the interrupt)")
	close(fx.Provider.Gate) // let turn 2 finish
	receiveOrFail(t, done, "the loop to return at end of input")

	if got := strings.Count(out.String(), "[interrupted]"); got != 1 {
		t.Errorf("expected [interrupted] exactly once, got %d times; output:\n%s", got, out.String())
	}
	if strings.Contains(out.String(), "error") {
		t.Errorf("an interrupt should not be reported as an error; output:\n%s", out.String())
	}
	msgs := fx.History.GetMessages()
	if len(msgs) != 2 || msgs[0].Content != "second" || msgs[1].Content != "ok" {
		t.Fatalf("expected only turn 2 (user %q + assistant %q) in history, got %+v", "second", "ok", msgs)
	}
}

// - Verify a Ctrl+C left over from before a turn (e.g. an extra press after the previous turn was
// cancelled) is discarded and does not cancel the next turn
func Test_ChatLoop_StaleInterrupt_IsDiscardedBeforeTurn(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	interrupts := make(chan os.Signal, 1)
	interrupts <- os.Interrupt // already waiting in the 1-slot buffer before the turn starts

	out, done := startChatLoop(fx, "hello\n", interrupts)
	// hold the turn at the model for a moment: if the stale interrupt had not been discarded, the
	// turn's watcher would take it now and cancel the turn
	receiveOrFail(t, fx.Provider.Waiting, "the turn to reach the model")
	time.Sleep(20 * time.Millisecond)
	close(fx.Provider.Gate)
	receiveOrFail(t, done, "the loop to return at end of input")

	if strings.Contains(out.String(), "[interrupted]") {
		t.Errorf("a stale interrupt must not cancel the turn; output:\n%s", out.String())
	}
	if len(fx.History.GetMessages()) != 2 {
		t.Errorf("expected the turn to complete (2 history entries), got %d", len(fx.History.GetMessages()))
	}
	if len(interrupts) != 0 {
		t.Errorf("expected the stale interrupt to have been discarded, %d still buffered", len(interrupts))
	}
}

// - Verify Ctrl+C cancels only the foreground turn: a background auto-compaction running at the
// same time keeps going and completes
func Test_ChatLoop_Interrupt_DoesNotCancelAutoCompaction(t *testing.T) {
	fx := newChatSessionFixture(t, 4, 4)
	fx.Provider.Reply = "ok"
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	fx.Context.AddMessages(modeltest.Msgs(4)) // at the threshold: the loop starts auto-compaction after the next input
	interrupts := make(chan os.Signal, 1)

	// "/config" makes no model call, but the loop checks auto-compaction after it; "hello" is the turn to interrupt
	out, done := startChatLoop(fx, "/config\nhello\n", interrupts)

	// two calls are now blocked on the gate, in either order: the compaction's and the turn's
	receiveOrFail(t, fx.Provider.Waiting, "the first gated model call")
	receiveOrFail(t, fx.Provider.Waiting, "the second gated model call")
	interrupts <- os.Interrupt // cancels the turn only
	// open the gate only once the turn has really been cancelled; opening it earlier could let the
	// turn's model call return normally before the watcher gets to cancel it
	waitForOutput(t, out, "[interrupted]")
	close(fx.Provider.Gate) // lets the compaction finish
	receiveOrFail(t, done, "the loop to return at end of input")
	fx.Context.WaitForCompaction()

	if !strings.Contains(out.String(), "[interrupted]") {
		t.Errorf("expected the turn to be interrupted; output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Auto-compaction error") {
		t.Errorf("the interrupt must not reach background compaction; output:\n%s", out.String())
	}
	if fx.Context.GetSize() != 1 {
		t.Errorf("expected compaction to have replaced the context with 1 summary message, got size %d", fx.Context.GetSize())
	}
}

// - Verify /quit and /exit both end the loop, and nothing after them is sent to the model
func Test_ChatLoop_QuitCommands_EndLoop(t *testing.T) {
	for _, cmd := range []string{"/quit", "/exit"} {
		t.Run(cmd, func(t *testing.T) {
			fx := newChatSessionFixture(t, 10, 100)
			fx.Provider.Reply = "ok"

			_, done := startChatLoop(fx, cmd+"\nhello\n", make(chan os.Signal, 1))
			receiveOrFail(t, done, "the loop to return after "+cmd)

			if len(fx.Provider.Calls) != 0 {
				t.Errorf("expected no model calls after %s, got %d", cmd, len(fx.Provider.Calls))
			}
		})
	}
}

// - Verify the loop returns at end of input (Ctrl+D on a terminal, EOF on a pipe)
func Test_ChatLoop_EOF_EndsLoop(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	_, done := startChatLoop(fx, "", make(chan os.Signal, 1))
	receiveOrFail(t, done, "the loop to return on EOF")
}

// - Verify the turn watcher cancels the turn's context when an interrupt arrives
func Test_CreateTurnWatcher_InterruptCancelsContext(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	turnCtx, stop := createTurnWatcher(context.Background(), interrupts)
	defer stop()

	interrupts <- os.Interrupt
	receiveOrFail(t, turnCtx.Done(), "the turn context to be cancelled")
	if !errors.Is(turnCtx.Err(), context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", turnCtx.Err())
	}
}

// - Verify that after stop() the watcher is gone: a later interrupt stays in the channel (for the
// next turn's discard) instead of being taken by an old watcher
func Test_CreateTurnWatcher_AfterStop_InterruptStaysInChannel(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	_, stop := createTurnWatcher(context.Background(), interrupts)
	stop()

	interrupts <- os.Interrupt
	time.Sleep(10 * time.Millisecond) // give a (wrongly) still-running watcher a chance to take it
	if len(interrupts) != 1 {
		t.Fatalf("expected the interrupt to remain in the channel after stop(), found %d", len(interrupts))
	}
}

// - Verify the root context (SIGTERM / quit) still cancels the turn context through the parent
func Test_CreateTurnWatcher_ParentCancelCancelsTurn(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	turnCtx, stop := createTurnWatcher(root, make(chan os.Signal, 1))
	defer stop()

	cancelRoot()
	receiveOrFail(t, turnCtx.Done(), "the turn context to follow its parent")
}

// - Verify the per-turn context-window dump is printed only in debug mode, and goes to the session's
// output (not process stdout)
func Test_HandleUserInput_ContextDump_OnlyInDebugMode(t *testing.T) {
	for _, debug := range []bool{false, true} {
		fx := newChatSessionFixture(t, 10, 100)
		fx.Provider.Reply = "ok"
		fx.Session.Config.Debug = debug

		handleUserInput(context.Background(), fx.Session, "hello")

		if got := strings.Contains(fx.Out.String(), "[debug] --- context window ---"); got != debug {
			t.Errorf("debug=%v: expected context dump printed=%v, output:\n%s", debug, debug, fx.Out.String())
		}
	}
}
