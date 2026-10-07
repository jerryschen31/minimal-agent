package contextwindow

import (
	"fmt"
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
func assertWindowKeepsNewestOnBatchOverflow(t *testing.T, w ContextWindow, maxSize int) {
	t.Helper()
	w.AddMessages(modeltest.Msgs(maxSize + 3)) // one batch call, overflowing by 3

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
	for _, m := range modeltest.Msgs(8) { // IDs "0".."7"
		w.AddMessages([]model.ChatMessage{m})
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
