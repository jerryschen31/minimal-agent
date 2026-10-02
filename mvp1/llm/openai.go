// Package llm holds agent.Provider adapters. Each one is a pure translation
// layer: neutral messages in, neutral messages out, no agent logic.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"mvp1/agent"
)

// [agent] OpenAI speaks the OpenAI Chat Completions wire format, the de-facto
// standard implemented by local servers (Ollama, vLLM, llama.cpp, LM Studio) and
// most hosted APIs (OpenAI, Groq, Together, OpenRouter, Mistral, Gemini-compat).
// One adapter therefore covers "any local or remote model" for most users.
type OpenAI struct {
	BaseURL   string // e.g. http://localhost:11434/v1 or https://api.openai.com/v1
	APIKey    string // may be empty for local servers
	Model     string
	MaxTokens int64        // 0 → server default
	HTTP      *http.Client // nil → http.DefaultClient
}

type oaCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // [agent] JSON encoded as a string on the wire
	} `json:"function"`
}

type oaMsg struct {
	Role       string   `json:"role"`
	Content    string   `json:"content"`
	ToolCalls  []oaCall `json:"tool_calls,omitempty"`
	ToolCallID string   `json:"tool_call_id,omitempty"`
}

func (p *OpenAI) Chat(ctx context.Context, msgs []agent.Message, tools []agent.ToolSpec) (agent.Response, error) {
	// [agent] neutral → wire
	wire := make([]oaMsg, 0, len(msgs))
	for _, m := range msgs {
		w := oaMsg{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			var oc oaCall
			oc.ID, oc.Type, oc.Function.Name, oc.Function.Arguments = c.ID, "function", c.Name, string(orEmpty(c.Args))
			w.ToolCalls = append(w.ToolCalls, oc)
		}
		wire = append(wire, w)
	}
	body := map[string]any{"model": p.Model, "messages": wire}
	if p.MaxTokens > 0 {
		body["max_tokens"] = p.MaxTokens
	}
	if len(tools) > 0 {
		ts := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": orEmpty(t.Schema)}})
		}
		body["tools"] = ts
	}
	raw, err := p.post(ctx, "/chat/completions", body)
	if err != nil {
		return agent.Response{}, err
	}
	// [agent] wire → neutral
	var out struct {
		Choices []struct {
			Message oaMsg `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return agent.Response{}, fmt.Errorf("openai: bad response: %v %s", err, raw)
	}
	m := out.Choices[0].Message
	res := agent.Message{Role: agent.RoleAssistant, Content: m.Content}
	for _, c := range m.ToolCalls {
		args := json.RawMessage(c.Function.Arguments)
		if !json.Valid(args) {
			// [agent] some local models emit sloppy JSON. Keep the raw text as a JSON
			// *string* so the transcript still marshals and replays verbatim; the
			// kernel rejects any non-object Args before the tool runs.
			args, _ = json.Marshal(c.Function.Arguments)
		}
		res.ToolCalls = append(res.ToolCalls, agent.ToolCall{ID: c.ID, Name: c.Function.Name, Args: orEmpty(args)})
	}
	return agent.Response{Message: res, Usage: agent.Usage{Input: out.Usage.Prompt, Output: out.Usage.Completion}}, nil
}

func (p *OpenAI) post(ctx context.Context, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	hc := p.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, raw)
	}
	return raw, nil
}

// orEmpty normalises a missing JSON object to {} (providers reject null).
func orEmpty(r json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(r)) == 0 || string(r) == "null" {
		return json.RawMessage(`{}`)
	}
	return r
}
