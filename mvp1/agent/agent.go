// Package agent is the kernel: the provider-neutral message model, the plug-in
// interfaces, and the ReAct loop. Everything else in mvp1 is a plug-in that
// imports this package; this package imports nothing from mvp1.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// [agent] ---- Provider-neutral message model --------------------------------
// Every LLM adapter translates to/from these types. Owning our own model
// (instead of one vendor's) is what makes the LLM swappable.

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // [agent] an observation: the result of one tool call
)

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // [agent] assistant asking to act
	ToolCallID string     `json:"tool_call_id,omitempty"` // [agent] which call this observation answers
	IsError    bool       `json:"is_error,omitempty"`
	// [agent] Native is an opaque, provider-specific copy of an assistant turn
	// (e.g. Anthropic thinking blocks + signatures). Adapters replay it verbatim
	// when it is theirs and ignore it otherwise, so history survives a model swap.
	Native json.RawMessage `json:"native,omitempty"`
}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"` // [agent] raw JSON; each tool decodes its own args
}

// ToolSpec is what the LLM sees. Schema is a JSON Schema object.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Usage struct{ Input, Output int }

type Response struct {
	Message Message
	Usage   Usage
}

// [agent] ---- Plug-in interfaces ---------------------------------------------

// Provider is one LLM API call. Implementations: llm.OpenAI (any OpenAI-compatible
// server: Ollama, vLLM, llama.cpp, OpenAI, Groq, OpenRouter...) and llm.Anthropic.
type Provider interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec) (Response, error)
}

// Tool is an action the agent can take. Implementations: tool.Func (a Go closure),
// tools served by tool.MCP (remote MCP server), agent.Subagent (another agent).
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Memory is the durable transcript: everything that happened.
// Swap for SQLite, Postgres, a vector store, etc.
type Memory interface {
	Append(ctx context.Context, msgs ...Message) error
	Messages(ctx context.Context) ([]Message, error)
}

// ContextBuilder decides what the LLM sees *this step* given the full history:
// windowing, summarisation, retrieval, prompt-cache layout...
type ContextBuilder interface {
	Build(ctx context.Context, system string, history []Message) ([]Message, error)
}

// Hook observes and can veto. This is the single seam for observability/tracing,
// authorization, human-in-the-loop approval, evaluation, and cost accounting.
type Hook interface {
	BeforeLLM(ctx context.Context, step int, msgs []Message)
	AfterLLM(ctx context.Context, step int, resp Response, err error)
	BeforeTool(ctx context.Context, call ToolCall) error // [agent] non-nil error = denied
	AfterTool(ctx context.Context, call ToolCall, result Message)
}

// Hooks fans out to many hooks; the first BeforeTool veto wins.
type Hooks []Hook

func (h Hooks) BeforeLLM(ctx context.Context, s int, m []Message) {
	for _, x := range h {
		x.BeforeLLM(ctx, s, m)
	}
}
func (h Hooks) AfterLLM(ctx context.Context, s int, r Response, err error) {
	for _, x := range h {
		x.AfterLLM(ctx, s, r, err)
	}
}
func (h Hooks) BeforeTool(ctx context.Context, c ToolCall) error {
	for _, x := range h {
		if err := x.BeforeTool(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
func (h Hooks) AfterTool(ctx context.Context, c ToolCall, r Message) {
	for _, x := range h {
		x.AfterTool(ctx, c, r)
	}
}

// [agent] ---- The agent --------------------------------------------------------

var ErrMaxSteps = errors.New("agent: max steps reached without a final answer")

type Agent struct {
	LLM      Provider
	Tools    map[string]Tool
	Memory   Memory
	Context  ContextBuilder
	Hooks    Hooks
	System   string
	MaxSteps int
}

// Run executes one ReAct episode: reason → act → observe, repeating until the
// model replies with plain text (no tool calls) or MaxSteps is exhausted.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	if err := a.Memory.Append(ctx, Message{Role: RoleUser, Content: input}); err != nil {
		return "", err
	}
	specs := a.specs()
	for step := 1; step <= a.MaxSteps; step++ {
		// [agent] REASON / PLAN: rebuild the context from memory and ask the model
		// what to do next. Planning is the model's job; we just give it the state.
		history, err := a.Memory.Messages(ctx)
		if err != nil {
			return "", err
		}
		msgs, err := a.Context.Build(ctx, a.System, history)
		if err != nil {
			return "", err
		}
		a.Hooks.BeforeLLM(ctx, step, msgs)
		resp, err := a.LLM.Chat(ctx, msgs, specs)
		a.Hooks.AfterLLM(ctx, step, resp, err)
		if err != nil {
			return "", err
		}
		if err := a.Memory.Append(ctx, resp.Message); err != nil {
			return "", err
		}
		// [agent] No tool calls means the model is done: that is the final answer.
		if len(resp.Message.ToolCalls) == 0 {
			return resp.Message.Content, nil
		}
		// [agent] ACT + OBSERVE: run every requested tool (concurrently) and record
		// the results as observations for the next reasoning step.
		if err := a.Memory.Append(ctx, a.act(ctx, resp.Message.ToolCalls)...); err != nil {
			return "", err
		}
	}
	return "", ErrMaxSteps
}

// act runs all tool calls in parallel and returns results in call order.
func (a *Agent) act(ctx context.Context, calls []ToolCall) []Message {
	out := make([]Message, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = a.callTool(ctx, c) }()
	}
	wg.Wait()
	return out
}

func (a *Agent) callTool(ctx context.Context, c ToolCall) Message {
	var content string
	var err error
	if t, ok := a.Tools[c.Name]; !ok {
		err = fmt.Errorf("unknown tool %q", c.Name)
	} else if err = a.Hooks.BeforeTool(ctx, c); err == nil { // [agent] authorization gate
		content, err = t.Call(ctx, c.Args)
	}
	res := Message{Role: RoleTool, ToolCallID: c.ID, Content: content}
	if err != nil { // [agent] errors go back to the model as observations so it can recover
		res.Content, res.IsError = "error: "+err.Error(), true
	}
	a.Hooks.AfterTool(ctx, c, res)
	return res
}

// specs returns tool specs in a stable order (keeps the prompt prefix cacheable).
func (a *Agent) specs() []ToolSpec {
	specs := make([]ToolSpec, 0, len(a.Tools))
	for _, t := range a.Tools {
		specs = append(specs, t.Spec())
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}
