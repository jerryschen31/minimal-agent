package modeltest

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

// - Verify the scripted FakeProvider plays replies in order, records the messages and tools of
// every call, and fails loudly (rather than reusing Reply) once the script runs out
func Test_FakeProvider_Script_PlaysRepliesInOrderThenErrors(t *testing.T) {
	p := &FakeProvider{Reply: "unscripted", Script: []model.ChatMessage{
		AssistantToolCall("c1", "read_file", `{"path":"go.mod"}`),
		AssistantText("done"),
	}}
	defs := []model.ToolDef{model.NewToolDef("read_file", "reads a file", nil)}
	ctx := context.Background()

	first, err := p.Chat(ctx, []model.ChatMessage{Msg("user", "u1", "hi")}, defs)
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

// - Verify an unscripted FakeProvider still returns Reply on every call (the behavior the
// existing tests depend on)
func Test_FakeProvider_NoScript_ReturnsReplyEveryCall(t *testing.T) {
	p := &FakeProvider{Reply: "same"}

	for i := 0; i < 3; i++ {
		got, err := p.Chat(context.Background(), nil, nil)
		if err != nil || got.Role != "assistant" || got.Content != "same" {
			t.Fatalf("call %d: expected assistant %q, got (%+v, %v)", i+1, "same", got, err)
		}
	}
}
