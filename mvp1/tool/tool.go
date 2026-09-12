// Package tool holds agent.Tool implementations and adapters.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"mvp1/agent"
)

// [agent] Func adapts a Go closure into an agent.Tool. This is the simplest
// extension path: wrap any library call (HTTP client, DB query, SDK) in a Func
// and register it. Schema is a JSON Schema string describing the arguments.
type Func struct {
	Name, Description, Schema string
	Fn                        func(ctx context.Context, args json.RawMessage) (string, error)
}

func (f Func) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: f.Name, Description: f.Description, Schema: json.RawMessage(f.Schema)}
}
func (f Func) Call(ctx context.Context, args json.RawMessage) (string, error) { return f.Fn(ctx, args) }

// Args decodes tool arguments into T with a readable error for the model.
func Args[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments %s: %w", raw, err)
	}
	return v, nil
}

// Truncate caps tool output so one observation can't blow the context window.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-max)
}
