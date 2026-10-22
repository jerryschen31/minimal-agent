package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
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

// recorderSpy is a trace.Recorder that keeps what it was given.
type recorderSpy struct {
	mu     sync.Mutex
	scopes []string
	types  []trace.EventType
	data   []any
}

func (s *recorderSpy) Record(scopeID string, eventType trace.EventType, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append(s.scopes, scopeID)
	s.types = append(s.types, eventType)
	s.data = append(s.data, data)
}
func (s *recorderSpy) RunID() string { return "r-test" }
func (s *recorderSpy) Close() error  { return nil }

// - Verify that a manual /compact records one compaction event inside the command's scope, and an auto compaction uses the run ID.
func Test_Compaction_RecordsAnEvent(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "a summary"
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "hello")
	handleUserInput(ctx, fx.Session, "/compact")

	var manual *trace.CompactionData
	for i, typ := range spy.types {
		if typ == trace.Compaction {
			d := spy.data[i].(trace.CompactionData)
			manual = &d
			if spy.scopes[i] == "r-test" || spy.scopes[i] == "" {
				t.Errorf("manual compaction scope = %q, want the command's task ID", spy.scopes[i])
			}
		}
	}
	if manual == nil {
		t.Fatalf("no compaction event recorded: %v", spy.types)
	}
	if manual.Trigger != "manual" || manual.Status != "ok" || manual.SummaryID == "" || manual.MessagesAfter >= manual.MessagesBefore {
		t.Errorf("compaction = %+v", *manual)
	}

	fx.Provider.Reply = "second summary"
	handleUserInput(ctx, fx.Session, "more")
	if _, err := compactAndTrace(ctx, fx.Session, fx.Session.Recorder.RunID(), "auto", ""); err != nil {
		t.Fatal(err)
	}
	last := len(spy.types) - 1
	if spy.types[last] != trace.Compaction || spy.scopes[last] != "r-test" || spy.data[last].(trace.CompactionData).Trigger != "auto" {
		t.Errorf("auto compaction event = %v %q %+v", spy.types[last], spy.scopes[last], spy.data[last])
	}
}

func (s *recorderSpy) eventTypes() []trace.EventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.types)
}

// scopedEvent is one recorded event, for asserting which scope a model call landed in.
type scopedEvent struct {
	scope string
	typ   trace.EventType
	data  any
}

func (s *recorderSpy) events() []scopedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []scopedEvent
	for i := range s.types {
		out = append(out, scopedEvent{s.scopes[i], s.types[i], s.data[i]})
	}
	return out
}

// - Verify every model call is recorded as a request followed by a response, inside the scope that made it:
// a chat turn's steps carry step numbers and the task ID, /summary and /compact carry their own command's task ID.
func Test_ModelCalls_AreRecordedInTheirScope(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.MaxSteps = 3
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	ctx := context.Background()

	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("c1", "no_such_tool", "{}"),
		modeltest.AssistantText("done"),
		modeltest.AssistantText("a summary"),
		modeltest.AssistantText("a compact summary"),
	}
	handleUserInput(ctx, fx.Session, "hello")
	handleUserInput(ctx, fx.Session, "/summary")
	handleUserInput(ctx, fx.Session, "/compact")

	// group the model events by scope, in order
	byScope := map[string][]scopedEvent{}
	var order []string
	for _, e := range spy.events() {
		if e.typ != trace.ModelRequest && e.typ != trace.ModelResponse && e.typ != trace.ModelError {
			continue
		}
		if _, ok := byScope[e.scope]; !ok {
			order = append(order, e.scope)
		}
		byScope[e.scope] = append(byScope[e.scope], e)
	}
	if len(order) != 3 {
		t.Fatalf("expected model events in 3 scopes (turn, /summary, /compact), got %d: %v", len(order), order)
	}

	turn := byScope[order[0]]
	if len(turn) != 4 || turn[0].typ != trace.ModelRequest || turn[1].typ != trace.ModelResponse || turn[2].typ != trace.ModelRequest || turn[3].typ != trace.ModelResponse {
		t.Fatalf("turn events = %v", turn)
	}
	if got := turn[0].data.(trace.ModelRequestData).Step; got != 1 {
		t.Errorf("first request step = %d, want 1", got)
	}
	if got := turn[2].data.(trace.ModelRequestData).Step; got != 2 {
		t.Errorf("second request step = %d, want 2", got)
	}
	// only the turn's own messages are recorded, so step 2 repeats step 1's user message before the reply and the tool result
	if r := turn[0].data.(trace.ModelRequestData); r.ContextMessages != 1 || len(r.Messages) != 1 || r.Messages[0].Role != "user" {
		t.Errorf("step 1 request = %+v, want 1 context message (system) and the user message", r)
	}
	if r := turn[2].data.(trace.ModelRequestData); r.ContextMessages != 1 || len(r.Messages) != 3 || r.Messages[1].Role != "assistant" || r.Messages[2].Role != "tool" {
		t.Errorf("step 2 request = %+v, want 1 context message (system) and the user message, assistant reply, tool result", r)
	}
	first := turn[1].data.(trace.ModelResponseData).Message
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].CallID != "c1" {
		t.Errorf("first response should carry the tool call: %+v", first)
	}
	if got := turn[3].data.(trace.ModelResponseData).Message.Content; got != "done" {
		t.Errorf("second response content = %q", got)
	}

	for _, id := range order[1:] {
		evs := byScope[id]
		if len(evs) != 2 || evs[0].typ != trace.ModelRequest || evs[1].typ != trace.ModelResponse {
			t.Errorf("scope %s events = %v", id, evs)
		}
		if evs[0].data.(trace.ModelRequestData).Step != 0 {
			t.Errorf("a call outside the loop should have no step")
		}
	}
}

// - Verify a failed model call records model_request then model_error (flagged canceled for a cancelled context).
func Test_ModelCalls_ErrorIsRecorded(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	fx.Provider.Err = context.Canceled

	handleUserInput(context.Background(), fx.Session, "hello")

	var got *trace.ModelErrorData
	for _, e := range spy.events() {
		if e.typ == trace.ModelError {
			d := e.data.(trace.ModelErrorData)
			got = &d
		}
	}
	if got == nil || !got.Canceled || got.Error == "" {
		t.Fatalf("model_error = %+v, events %v", got, spy.eventTypes())
	}
}

// - Verify a later turn does not repeat earlier turns: the system prompt and the prior context are only counted.
func Test_ModelRequest_SecondTurnRecordsOnlyItsOwnMessages(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	fx.Provider.Reply = "ok"
	ctx := context.Background()

	handleUserInput(ctx, fx.Session, "first") // adds user + assistant to the context
	handleUserInput(ctx, fx.Session, "second")

	var reqs []trace.ModelRequestData
	for _, e := range spy.events() {
		if e.typ == trace.ModelRequest {
			reqs = append(reqs, e.data.(trace.ModelRequestData))
		}
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 2 model requests, got %d", len(reqs))
	}
	if r := reqs[1]; r.ContextMessages != 3 || len(r.Messages) != 1 || r.Messages[0].Content != "second" {
		t.Errorf("second turn request = %+v, want 3 context messages (system, first, ok) and only %q", r, "second")
	}
}
