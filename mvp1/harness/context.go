// Package harness holds agent.ContextBuilder implementations: what the model
// sees each step, derived from memory. Memory is the truth; context is a view.
package harness

import (
	"context"
	"fmt"

	"mvp1/agent"
)

// [agent] Full sends the whole history. Simplest and best for short tasks or
// large-window models.
type Full struct{}

func (Full) Build(_ context.Context, system string, history []agent.Message) ([]agent.Message, error) {
	return withSystem(system, history), nil
}

// [agent] Window keeps roughly the last MaxMessages, always cutting at a user
// turn so an assistant tool call is never separated from its tool results
// (providers reject orphaned results). The dropped prefix becomes a one-line
// notice in the system prompt. A summarising builder would call the LLM here
// instead of dropping; a retrieval builder would inject relevant memories.
type Window struct{ MaxMessages int } // <= 0 means no limit

func (w Window) Build(_ context.Context, system string, history []agent.Message) ([]agent.Message, error) {
	start := 0
	if w.MaxMessages > 0 && len(history) > w.MaxMessages {
		start = len(history) - w.MaxMessages
		for start > 0 && history[start].Role != agent.RoleUser {
			start-- // [agent] widen until we land on a user turn
		}
	}
	if start > 0 {
		system += fmt.Sprintf("\n\n[%d earlier messages omitted from context]", start)
	}
	return withSystem(system, history[start:]), nil
}

func withSystem(system string, history []agent.Message) []agent.Message {
	out := make([]agent.Message, 0, len(history)+1)
	if system != "" {
		out = append(out, agent.Message{Role: agent.RoleSystem, Content: system})
	}
	return append(out, history...)
}
