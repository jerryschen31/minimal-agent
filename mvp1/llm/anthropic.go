package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"mvp1/agent"
)

// [agent] Anthropic adapts the Messages API via the official Go SDK. It shows how
// a provider with a *different* wire shape (system as a top-level field, tool
// results inside user turns, thinking blocks) still fits behind agent.Provider.
type Anthropic struct {
	Model     string
	MaxTokens int64
	client    anthropic.Client
}

// NewAnthropic builds a client. Empty apiKey → SDK default resolution
// (ANTHROPIC_API_KEY, `ant auth login` profile, ...). baseURL is optional.
func NewAnthropic(model, apiKey, baseURL string, maxTokens int64) *Anthropic {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Anthropic{Model: model, MaxTokens: maxTokens, client: anthropic.NewClient(opts...)}
}

func (p *Anthropic) Chat(ctx context.Context, msgs []agent.Message, tools []agent.ToolSpec) (agent.Response, error) {
	params := anthropic.MessageNewParams{Model: anthropic.Model(p.Model), MaxTokens: p.MaxTokens}

	// [agent] Anthropic wants all tool results for one assistant turn batched into a
	// single user message, so consecutive RoleTool messages are buffered then flushed.
	var pending []anthropic.ContentBlockParamUnion
	flush := func() {
		if len(pending) > 0 {
			params.Messages = append(params.Messages, anthropic.NewUserMessage(pending...))
			pending = nil
		}
	}
	for _, m := range msgs {
		if m.Role != agent.RoleTool {
			flush()
		}
		switch m.Role {
		case agent.RoleSystem:
			params.System = append(params.System, anthropic.TextBlockParam{Text: m.Content})
		case agent.RoleUser:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case agent.RoleAssistant:
			// [agent] Replay our own native turn verbatim when we have it: this preserves
			// thinking blocks/signatures, which the API requires across tool-use turns.
			var native anthropic.MessageParam
			if m.Native != nil && json.Unmarshal(m.Native, &native) == nil && native.Role == "assistant" {
				params.Messages = append(params.Messages, native)
				continue
			}
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, c := range m.ToolCalls {
				blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, orEmpty(c.Args), c.Name))
			}
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(blocks...))
		case agent.RoleTool:
			pending = append(pending, anthropic.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError))
		}
	}
	flush()

	for _, t := range tools {
		var s struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		_ = json.Unmarshal(t.Schema, &s)
		if s.Properties == nil {
			s.Properties = map[string]any{}
		}
		tp := anthropic.ToolParam{Name: t.Name, Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: s.Properties, Required: s.Required}}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tp})
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return agent.Response{}, err
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return agent.Response{}, fmt.Errorf("anthropic: request refused (%s): %s", resp.StopDetails.Category, resp.StopDetails.Explanation)
	}
	// [agent] wire → neutral; keep the native turn for faithful replay next step.
	out := agent.Message{Role: agent.RoleAssistant}
	out.Native, _ = json.Marshal(resp.ToParam())
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			out.Content += v.Text
		case anthropic.ToolUseBlock:
			out.ToolCalls = append(out.ToolCalls, agent.ToolCall{ID: v.ID, Name: v.Name, Args: json.RawMessage(v.JSON.Input.Raw())})
		}
	}
	return agent.Response{Message: out, Usage: agent.Usage{Input: int(resp.Usage.InputTokens), Output: int(resp.Usage.OutputTokens)}}, nil
}
