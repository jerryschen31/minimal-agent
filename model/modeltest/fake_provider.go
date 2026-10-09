// Package modeltest holds test doubles and message builders shared by the tests of packages that
// depend on model. It is a regular (non-_test) package so other packages' tests can import it.
package modeltest

import (
	"context"
	"fmt"
	"sync"

	"github.com/jerryschen31/minimal-agent/model"
)

// FakeProvider is a scriptable Provider for tests: set Reply/Err before a call, inspect Calls
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
//
// To pause a call partway (e.g. to interrupt a turn while the model is "thinking"), set Gate:
// Chat then blocks until Gate is closed or ctx is cancelled (returning ctx.Err()). If Waiting is
// also set, Chat sends on it once it starts blocking, so a test knows when the call is in progress.
// The mutex is not held while blocked, so other calls (e.g. a background compaction) still work.
type FakeProvider struct {
	mu       sync.Mutex
	Reply    string
	Err      error
	Script   []model.ChatMessage
	next     int // index of the next Script entry to play
	Calls    [][]model.ChatMessage
	ToolDefs [][]model.ToolDef
	Gate     chan struct{} // if set, Chat blocks until Gate is closed or ctx is cancelled
	Waiting  chan struct{} // if set, Chat sends on it once it starts blocking on Gate
}

func (p *FakeProvider) Chat(ctx context.Context, chatHistory []model.ChatMessage, tools []model.ToolDef) (model.ChatMessage, error) {
	p.mu.Lock()
	p.Calls = append(p.Calls, append([]model.ChatMessage(nil), chatHistory...))
	p.ToolDefs = append(p.ToolDefs, append([]model.ToolDef(nil), tools...))
	gate, waiting := p.Gate, p.Waiting
	p.mu.Unlock()

	// wait on the gate without holding the mutex, so a concurrent call isn't blocked behind this one
	if gate != nil {
		if waiting != nil {
			select {
			case waiting <- struct{}{}:
			case <-ctx.Done():
				return model.ChatMessage{}, ctx.Err()
			}
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return model.ChatMessage{}, ctx.Err()
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return model.ChatMessage{}, ctx.Err()
	}
	if p.Err != nil {
		return model.ChatMessage{}, p.Err
	}
	if p.Script != nil {
		if p.next >= len(p.Script) {
			return model.ChatMessage{}, fmt.Errorf("FakeProvider: script exhausted after %d replies (Chat called %d times)", len(p.Script), len(p.Calls))
		}
		reply := p.Script[p.next]
		p.next++
		return reply, nil
	}
	return model.ChatMessage{Role: "assistant", Content: p.Reply}, nil
}

// AssistantText builds a plain assistant reply with no tool calls (the "final answer" step).
func AssistantText(text string) model.ChatMessage {
	return model.ChatMessage{Role: "assistant", Content: text}
}

// AssistantToolCall builds an assistant reply that asks for one tool call. args is the raw
// JSON string the model would send, e.g. `{"path":"go.mod"}`.
func AssistantToolCall(callID, toolName, args string) model.ChatMessage {
	return model.ChatMessage{
		Role: "assistant",
		ToolCalls: []model.ToolCall{{
			ID:       callID,
			Type:     "function",
			Function: model.ToolCallFunc{Name: toolName, Arguments: args},
		}},
	}
}

// Msg builds a model.ChatMessage with a distinct ID, so tests can tell messages apart after
// eviction/compaction without depending on their Content.
func Msg(role, id, content string) model.ChatMessage {
	return model.ChatMessage{ID: id, Role: role, Content: content}
}

// Msgs builds n distinct ChatMessages with IDs "0".."n-1" in order, alternating user/assistant
// roles, for tests that just need "some messages" without caring about their content.
func Msgs(n int) []model.ChatMessage {
	out := make([]model.ChatMessage, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = Msg(role, fmt.Sprintf("%d", i), fmt.Sprintf("message %d", i))
	}
	return out
}
