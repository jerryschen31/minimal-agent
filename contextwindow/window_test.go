package contextwindow

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
)

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

// - Verify that adding a new message to a full context window does not exceed the maximum context window size.
func Test_ContextWindow_AddNewMessage_FullWindow_MaxSizeNotExceeded(t *testing.T) {
	forEachWindow(t, 3, func(t *testing.T, w ContextWindow) {
		w.AddMessages(modeltest.Msgs(5)) // one at a time via a single batch call, well past maxSize

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
		w.AddMessages(modeltest.Msgs(3)) // fills the window exactly: IDs "0","1","2"
		w.AddMessages([]model.ChatMessage{modeltest.Msg("user", "3", "newest")})

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

// assertWindowKeepsNewestOnBatchOverflow adds more messages in a single AddMessages call than
// the window can hold, and checks that exactly the newest maxSize survive, in order. This is
// deliberately different from (and more rigorous than) the generic forEachWindow eviction check
// in "Basic context window mechanics": each window type's eviction logic loops once per message
// in a batch, so a single-message steady-state test passing doesn't guarantee a large batch
// overflow (N > 1 evicted at once) is handled correctly by that same loop.
// It overflows by an even number (4) so the oldest survivor is a user message: a trim that left an
// assistant message in front would drop it too (the window must start at a user message).
func assertWindowKeepsNewestOnBatchOverflow(t *testing.T, w ContextWindow, maxSize int) {
	t.Helper()
	w.AddMessages(modeltest.Msgs(maxSize + 4)) // one batch call, overflowing by 4

	messages := w.GetMessages()
	if len(messages) != maxSize {
		t.Fatalf("expected exactly %d messages after batch overflow, got %d", maxSize, len(messages))
	}
	for i, m := range messages {
		wantID := fmt.Sprintf("%d", i+4) // IDs "0".."maxSize+3" were added; oldest 4 evicted
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
	// All user messages, so every trim already lands on a user message and only the indexing is tested.
	for i := range 8 { // IDs "0".."7"
		w.AddMessages([]model.ChatMessage{modeltest.Msg("user", fmt.Sprintf("%d", i), "fake")})
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
		// NewContextWindow holds exactly maxMessages (no hidden buffer) — confirms the size, not just "didn't error".
		if wantMaxSize := 10; cw.GetMaxSize() != wantMaxSize {
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
		messages := modeltest.Msgs(5)
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
			w.AddMessages(modeltest.Msgs(3))

			w.RemoveLast(5) // more than the 3 messages present

			if got := len(w.GetMessages()); got != c.expectedAfter {
				t.Errorf("expected %d messages to remain, got %d", c.expectedAfter, got)
			}
		})
	}
}

// - Verify that adding messages to a context window and clearing it empties the window, for all window types
func Test_Unit_ContextWindow_Clear_EmptiesWindow(t *testing.T) {
	forEachWindow(t, 10, func(t *testing.T, w ContextWindow) {
		w.AddMessages(modeltest.Msgs(3))

		w.Clear()

		if w.GetSize() != 0 {
			t.Errorf("expected size 0 after clearing, got %d", w.GetSize())
		}
		if len(w.GetMessages()) != 0 {
			t.Errorf("expected 0 messages after clearing, got %d", len(w.GetMessages()))
		}
	})
}

//*********************************************//
// Orphaned tool messages
//*********************************************//

// toolTurn builds one conversation turn: a user message, `rounds` pairs of (assistant tool call,
// tool result), then a final assistant reply (2*rounds + 2 messages). IDs are "<name>-user",
// "<name>-call<i>", "<name>-tool<i>" and "<name>-final"; each tool result's ToolCallID matches the
// ID of the assistant message that asked for it.
func toolTurn(name string, rounds int) []model.ChatMessage {
	turn := []model.ChatMessage{{ID: name + "-user", Role: "user"}}
	for i := 0; i < rounds; i++ {
		callID := fmt.Sprintf("%s-call%d", name, i)
		turn = append(turn,
			model.ChatMessage{ID: callID, Role: "assistant", ToolCalls: []model.ToolCall{{ID: callID}}},
			model.ChatMessage{ID: fmt.Sprintf("%s-tool%d", name, i), Role: "tool", ToolCallID: callID},
		)
	}
	return append(turn, model.ChatMessage{ID: name + "-final", Role: "assistant"})
}

// idsOf returns the message IDs in order, for comparing and for readable failure messages.
func idsOf(msgs []model.ChatMessage) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// assertNoOrphanedToolMsgs fails the test if any tool result has no earlier assistant tool call
// with a matching ID in msgs. Providers reject such a request, so a window must never hand one out.
func assertNoOrphanedToolMsgs(t *testing.T, msgs []model.ChatMessage) {
	t.Helper()
	seenCalls := map[string]bool{}
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			seenCalls[tc.ID] = true
		}
		if m.Role == "tool" && !seenCalls[m.ToolCallID] {
			t.Errorf("message %d (%s) is a tool result whose tool call is not in the window: %v", i, m.ID, idsOf(msgs))
		}
	}
}

// assertStartsAtUser fails the test unless msgs is empty or its first message is a user message.
// A trim must never leave the window starting mid-turn (a tool result, an assistant tool call, a
// plain assistant reply, or the assistant half of a summary pair).
func assertStartsAtUser(t *testing.T, msgs []model.ChatMessage) {
	t.Helper()
	if len(msgs) > 0 && msgs[0].Role != "user" {
		t.Errorf("expected the window to be empty or start with a user message, got role %q first: %v", msgs[0].Role, idsOf(msgs))
	}
}

// - Verify that after every add, for all window types, the window never exceeds its max size,
// never contains a tool result without its tool call, and is empty or starts with a user message,
// using turns of varying length so the trim point lands mid-turn at different places
func Test_ContextWindow_AddMessages_NeverLeavesOrphanedToolMessages(t *testing.T) {
	forEachWindow(t, 8, func(t *testing.T, w ContextWindow) {
		for i, rounds := range []int{1, 0, 2, 1, 3, 0, 1, 2, 1, 1} {
			w.AddMessages(toolTurn(fmt.Sprintf("T%d", i), rounds))

			got := w.GetMessages()
			if len(got) > w.GetMaxSize() {
				t.Fatalf("after turn T%d: expected at most %d messages, got %d: %v", i, w.GetMaxSize(), len(got), idsOf(got))
			}
			assertNoOrphanedToolMsgs(t, got)
			assertStartsAtUser(t, got)
		}
	})
}

// summaryPair is the [user summary request, assistant summary] pair that compaction puts at the
// front of the context; both halves are typed "summary".
func summaryPair() []model.ChatMessage {
	return []model.ChatMessage{
		{ID: "summary-request", Role: "user", Type: "summary", Content: SummaryRequest},
		{ID: "summary", Role: "assistant", Type: "summary", Content: "a fake summary"},
	}
}

// - Verify, for all window types, that wherever a trim lands mid-turn, everything up to the next user
// message is dropped: the window ends up empty or starting with a user message, with the exact
// expected messages. Each batch is added with its own AddMessages call, the way a chat adds turns.
func Test_ContextWindow_AddMessages_TrimLandsMidTurn_StartsAtNextUser(t *testing.T) {
	cases := []struct {
		name    string
		maxSize int
		batches [][]model.ChatMessage
		want    []string
	}{
		// 8 messages in 6: the trim lands on A-tool0
		{"tool result", 6, [][]model.ChatMessage{toolTurn("A", 1), toolTurn("B", 1)}, idsOf(toolTurn("B", 1))},
		// 8 messages in 7: the trim lands on A-call0
		{"assistant tool call", 7, [][]model.ChatMessage{toolTurn("A", 1), toolTurn("B", 1)}, idsOf(toolTurn("B", 1))},
		// 4 messages in 3: the trim lands on A-final
		{"plain assistant reply", 3, [][]model.ChatMessage{toolTurn("A", 0), toolTurn("B", 0)}, idsOf(toolTurn("B", 0))},
		// 4 messages in 3: the trim lands on the assistant summary
		{"assistant half of summary pair", 3, [][]model.ChatMessage{summaryPair(), toolTurn("B", 0)}, idsOf(toolTurn("B", 0))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forEachWindow(t, c.maxSize, func(t *testing.T, w ContextWindow) {
				for _, b := range c.batches {
					w.AddMessages(b)
				}

				got := w.GetMessages()
				assertStartsAtUser(t, got)
				assertNoOrphanedToolMsgs(t, got)
				if !slices.Equal(idsOf(got), c.want) {
					t.Errorf("expected %v, got %v", c.want, idsOf(got))
				}
			})
		})
	}
}

// - Verify, for all window types, that a trim never splits the compaction summary pair: a trim that
// lands on the user summary request keeps both halves, and one that lands on the assistant summary
// drops both (never the assistant summary alone)
func Test_ContextWindow_AddMessages_TrimNeverSplitsSummaryPair(t *testing.T) {
	cases := []struct {
		name    string
		maxSize int
		batches [][]model.ChatMessage
		want    []string
	}{
		// [X-user, X-final, request, summary, B-user, B-final] in 4: the trim lands on the request
		{"lands on request: keeps both", 4,
			[][]model.ChatMessage{toolTurn("X", 0), summaryPair(), toolTurn("B", 0)},
			append(idsOf(summaryPair()), idsOf(toolTurn("B", 0))...)},
		// [request, summary, B-user, B-final, C-user, C-final] in 5: the trim lands on the summary
		{"lands on summary: drops both", 5,
			[][]model.ChatMessage{summaryPair(), toolTurn("B", 0), toolTurn("C", 0)},
			append(idsOf(toolTurn("B", 0)), idsOf(toolTurn("C", 0))...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forEachWindow(t, c.maxSize, func(t *testing.T, w ContextWindow) {
				for _, b := range c.batches {
					w.AddMessages(b)
				}

				got := w.GetMessages()
				if !slices.Equal(idsOf(got), c.want) {
					t.Errorf("expected %v, got %v", c.want, idsOf(got))
				}
			})
		})
	}
}

// - Verify, for all window types, that when the messages a trim keeps contain no user message at all,
// the window ends up empty. This is intended: a window of only assistant/tool messages is a turn
// fragment with no question, which providers reject or misread, and the next turn (which always
// starts with a user message) refills it. The last 4 of [user, call0, tool0, call1, tool1, call2,
// tool2, final] are [tool1, call2, tool2, final].
func Test_ContextWindow_AddMessages_TrimKeepsNoUserMessage_EmptiesWindow(t *testing.T) {
	forEachWindow(t, 4, func(t *testing.T, w ContextWindow) {
		w.AddMessages(toolTurn("A", 3))

		if got := w.GetMessages(); len(got) != 0 {
			t.Fatalf("expected an empty window, got %v", idsOf(got))
		}

		// the next turn starts with a user message and fills the window again
		w.AddMessages(toolTurn("B", 0))
		if got, want := idsOf(w.GetMessages()), idsOf(toolTurn("B", 0)); !slices.Equal(got, want) {
			t.Errorf("expected %v after the next turn, got %v", want, got)
		}
	})
}

// - Verify the exact result when the trim point lands on a tool result: two 4-message tool turns
// in a window of 6 trim to [tool A, final A, B...], and everything up to the next user message is
// dropped, leaving just turn B
func Test_ContextWindow_AddMessages_TrimLandsOnToolMessage_DropsUpToNextUser(t *testing.T) {
	forEachWindow(t, 6, func(t *testing.T, w ContextWindow) {
		w.AddMessages(toolTurn("A", 1))
		w.AddMessages(toolTurn("B", 1))

		got := idsOf(w.GetMessages())
		want := idsOf(toolTurn("B", 1))
		if !slices.Equal(got, want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})
}

// - Verify a single batch bigger than the window still ends with a valid window: the last 4 of
// [A-user, call0, tool0, call1, tool1, call2, tool2, A-final, B-user, B-final] start at tool2, and
// everything up to B-user is dropped
func Test_ContextWindow_AddMessages_OversizedBatch_DropsUpToNextUser(t *testing.T) {
	forEachWindow(t, 4, func(t *testing.T, w ContextWindow) {
		w.AddMessages(append(toolTurn("A", 3), toolTurn("B", 0)...))

		got := idsOf(w.GetMessages())
		want := idsOf(toolTurn("B", 0))
		if !slices.Equal(got, want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})
}

// - Verify nothing extra is dropped when the trim point already falls on a turn boundary: a second
// 4-message turn in a window of 4 replaces the first turn exactly
func Test_ContextWindow_AddMessages_TrimOnTurnBoundary_DropsNothingExtra(t *testing.T) {
	forEachWindow(t, 4, func(t *testing.T, w ContextWindow) {
		w.AddMessages(toolTurn("A", 1))
		w.AddMessages(toolTurn("B", 1))

		got := idsOf(w.GetMessages())
		want := idsOf(toolTurn("B", 1))
		if !slices.Equal(got, want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})
}
