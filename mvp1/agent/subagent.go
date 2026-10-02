package agent

import (
	"context"
	"encoding/json"
)

// [agent] Subagent exposes "spawn a fresh agent and run it to completion" as a
// Tool, so a parent agent can delegate. Spawn is a factory: each call gets its
// own Memory (and possibly a different model, tool set, or hooks), so sub-task
// chatter never pollutes the parent's context; the parent sees only the answer.
type Subagent struct {
	Name, Description string
	Spawn             func() *Agent
}

func (s Subagent) Spec() ToolSpec {
	return ToolSpec{Name: s.Name, Description: s.Description, Schema: json.RawMessage(
		`{"type":"object","properties":{"task":{"type":"string","description":"A complete, self-contained task description"}},"required":["task"]}`)}
}

func (s Subagent) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	return s.Spawn().Run(ctx, in.Task)
}
