package contextwindow

import (
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
)

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
		w.AddMessages(modeltest.Msgs(20)) // fill to capacity first: measures the realistic steady-state
		// cost (every add evicts one), not empty-window growth, since a real session's window
		// is full for the vast majority of its lifetime.
		one := []model.ChatMessage{modeltest.Msg("user", "bench", "hello")}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.AddMessages(one)
		}
	})
}

// Benchmark bulk add multiple messages to different context window data structure types
func Benchmark_ContextWindow_BulkAddMessages(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(modeltest.Msgs(20))
		batch := modeltest.Msgs(10) // one AddMessages call carrying several messages at once, e.g. what
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
		w.AddMessages(modeltest.Msgs(20))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = w.GetMessages()
		}
	})
}

// Benchmark eviction operations (when context window is full)on different context window data structure types
func Benchmark_ContextWindow_Eviction(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(modeltest.Msgs(20)) // start full
		// more than 2x maxSize in one call: every iteration evicts the entire prior contents
		// in one shot, rather than one message at a time — a different traffic pattern from
		// Benchmark_ContextWindow_AddMessages, worth comparing separately.
		overflow := modeltest.Msgs(40)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.AddMessages(overflow)
		}
	})
}

// Benchmark clear operations on different context window data structure types
func Benchmark_ContextWindow_Clear(b *testing.B) {
	forEachWindowBench(b, 20, func(b *testing.B, w ContextWindow) {
		w.AddMessages(modeltest.Msgs(20))
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
		w.AddMessages(modeltest.Msgs(20))
		one := []model.ChatMessage{modeltest.Msg("user", "bench", "hello")}
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
