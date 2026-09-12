package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// [agent] scripted fake LLM: replays canned responses, records what it was shown.
type fakeLLM struct {
	script []Message
	seen   [][]Message
}

func (f *fakeLLM) Chat(_ context.Context, msgs []Message, _ []ToolSpec) (Response, error) {
	f.seen = append(f.seen, msgs)
	m := f.script[0]
	f.script = f.script[1:]
	return Response{Message: m}, nil
}

type memStore struct{ msgs []Message }

func (m *memStore) Append(_ context.Context, ms ...Message) error {
	m.msgs = append(m.msgs, ms...)
	return nil
}
func (m *memStore) Messages(_ context.Context) ([]Message, error) { return m.msgs, nil }

type passthrough struct{}

func (passthrough) Build(_ context.Context, sys string, h []Message) ([]Message, error) {
	return append([]Message{{Role: RoleSystem, Content: sys}}, h...), nil
}

type echo struct{}

func (echo) Spec() ToolSpec {
	return ToolSpec{Name: "echo", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (echo) Call(_ context.Context, a json.RawMessage) (string, error) {
	var in struct{ S string }
	_ = json.Unmarshal(a, &in)
	return in.S, nil
}

type deny struct{ Hooks }

func (deny) BeforeTool(context.Context, ToolCall) error { return errors.New("nope") }

func newAgent(llm Provider, hooks ...Hook) (*Agent, *memStore) {
	mem := &memStore{}
	return &Agent{LLM: llm, Tools: map[string]Tool{"echo": echo{}}, Memory: mem,
		Context: passthrough{}, Hooks: hooks, System: "sys", MaxSteps: 5}, mem
}

func TestReActLoop(t *testing.T) {
	llm := &fakeLLM{script: []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "echo", Args: json.RawMessage(`{"S":"hi"}`)}}},
		{Role: RoleAssistant, Content: "done"},
	}}
	a, mem := newAgent(llm)
	out, err := a.Run(context.Background(), "go")
	if err != nil || out != "done" {
		t.Fatalf("got %q, %v", out, err)
	}
	// [agent] transcript: user, assistant(tool call), tool(observation), assistant(answer)
	if len(mem.msgs) != 4 || mem.msgs[2].Role != RoleTool || mem.msgs[2].Content != "hi" || mem.msgs[2].ToolCallID != "1" {
		t.Fatalf("bad transcript: %+v", mem.msgs)
	}
	if len(llm.seen) != 2 || llm.seen[1][0].Role != RoleSystem || len(llm.seen[1]) != 4 {
		t.Fatalf("model saw wrong context: %+v", llm.seen)
	}
}

func TestDeniedToolBecomesObservation(t *testing.T) {
	llm := &fakeLLM{script: []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "echo"}, {ID: "2", Name: "missing"}}},
		{Role: RoleAssistant, Content: "ok"},
	}}
	a, mem := newAgent(llm, deny{})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !mem.msgs[2].IsError || !mem.msgs[3].IsError || mem.msgs[3].ToolCallID != "2" {
		t.Fatalf("expected two error observations in order: %+v", mem.msgs[2:4])
	}
}

func TestMaxSteps(t *testing.T) {
	loop := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "echo"}}}
	a, _ := newAgent(&fakeLLM{script: []Message{loop, loop, loop, loop, loop}})
	if _, err := a.Run(context.Background(), "go"); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("want ErrMaxSteps, got %v", err)
	}
}
