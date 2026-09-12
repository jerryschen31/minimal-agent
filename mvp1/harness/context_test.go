package harness

import (
	"context"
	"testing"

	"mvp1/agent"
)

func TestWindowCutsAtUserTurn(t *testing.T) {
	h := []agent.Message{
		{Role: agent.RoleUser}, {Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "1"}}}, {Role: agent.RoleTool},
		{Role: agent.RoleAssistant}, {Role: agent.RoleUser}, {Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "2"}}}, {Role: agent.RoleTool},
	}
	out, _ := Window{MaxMessages: 2}.Build(context.Background(), "sys", h)
	// [agent] system + last user turn and everything after it (3 msgs), never a bare tool result
	if len(out) != 4 || out[1].Role != agent.RoleUser || out[0].Content == "sys" {
		t.Fatalf("got %+v", out)
	}
}
