package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

type OpenAICompat struct {
	BaseURL string
	Model   string
	ApiKey  string        // empty for Ollama
	Timeout time.Duration // most one Chat call may take (config's responseTimeoutSeconds); 0 = no limit of its own
	// MaxTokens is the most tokens one reply may use; 0 = not sent, the server's own limit applies
	MaxTokens int
	// MaxTokensField is the request key MaxTokens goes in (config.MaxTokensFieldMaxTokens or
	// config.MaxTokensFieldMaxCompletionTokens); SetupProvider resolves it with maxTokensFieldFor
	MaxTokensField string
}

// openAIRequest is the body of POST /chat/completions, in OpenAI's wire format. Only this adapter uses it.
type openAIRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ToolDef     `json:"tools,omitempty"`
	// [agent] Go struct tags are fixed at compile time, so a key chosen at run time needs two fields: Chat sets one,
	// and omitempty leaves the other (and both, when MaxTokens is 0) out of the JSON.
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

// maxTokensFieldFor returns the request key the output limit is sent as for a server at baseURL.
//
// Notes:
//   - a non-empty override (config maxTokensField) wins
//   - OpenAI (api.openai.com) and Azure OpenAI (*.openai.azure.com) get max_completion_tokens: their reasoning models
//     reject max_tokens. Every other host gets max_tokens, which most OpenAI-compatible servers take
//   - there is no way to ask a server which key it accepts, so the host is the signal; a gateway in front of OpenAI
//     (e.g. LiteLLM) looks like "other" and needs the override
//   - a baseURL that does not parse gets max_tokens
func maxTokensFieldFor(baseURL, override string) string {
	if override != "" {
		return override
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return config.MaxTokensFieldMaxTokens
	}
	host := strings.ToLower(u.Hostname())
	if host == "api.openai.com" || strings.HasSuffix(host, ".openai.azure.com") {
		return config.MaxTokensFieldMaxCompletionTokens
	}
	return config.MaxTokensFieldMaxTokens
}

// openAIResponse is the JSON an OpenAI-compatible server returns. Only this adapter uses it; Chat converts it into a
// Response.
// [agent] "choices" is a list because a request may ask for several alternative answers ("n"); we never set n, so it
// holds one.
type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage"` // [agent] a pointer: nil means the key was missing or null, which is not the same as zero tokens
}

// openAIChoice is one answer in an openAIResponse.
type openAIChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"` // "stop" | "tool_calls" | "length" | "content_filter" | ...
}

// openAIUsage is the "usage" object of an openAIResponse.
// [agent] completion_tokens_details.reasoning_tokens is not read: those tokens are already inside completion_tokens.
type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`     // the whole input, cached tokens included
	CompletionTokens    int `json:"completion_tokens"` // the whole output, reasoning tokens included
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// toUsage converts the server's usage object into a Usage.
//
// Notes:
//   - nil (no "usage" key, or "usage": null) gives a zero Usage with Reported false: unknown, not zero
//   - a missing prompt_tokens_details gives CacheReadTokens 0; OpenAI reports no cache writes, so CacheWriteTokens is always 0
//   - the numbers are taken as the server sends them; some servers (e.g. Ollama reusing a cached prompt prefix) may count
//     less input than was sent
func (u *openAIUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	usage := Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, Reported: true}
	if u.PromptTokensDetails != nil {
		usage.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	return usage
}

// openAIStopReason maps an OpenAI finish_reason to the shared stop reasons.
//
// Notes:
//   - "stop" -> StopEnd, "tool_calls" -> StopToolCalls, "length" -> StopMaxTokens; anything else (e.g. "content_filter")
//     is returned unchanged, and "" (not reported) stays ""
//   - don't use the result to decide whether the reply has tool calls: some servers send "stop" together with tool calls
func openAIStopReason(finishReason string) string {
	switch finishReason {
	case "stop":
		return StopEnd
	case "tool_calls":
		return StopToolCalls
	case "length":
		return StopMaxTokens
	default:
		return finishReason
	}
}

// Chat sends chatContext (and tools) to an OpenAI-compatible server's /chat/completions and returns the first choice's message as a Response.
//
// Notes:
//   - a Timeout above 0 wraps ctx in a deadline for this one call; 0 means no limit of its own
//   - sends an Authorization Bearer header only when ApiKey is set (empty for Ollama)
//   - nil tools send no "tools" key at all (omitempty)
//   - MaxTokens above 0 is sent as MaxTokensField (max_completion_tokens, else max_tokens); 0 sends neither key
//   - any status outside 200-299 is an error carrying only the status code, with the response body unread; a response with no
//     choices is also an error
//   - the returned message is exactly what the server sent: ID, Timestamp and Type are not filled in here
//   - Usage comes from the response's "usage" (toUsage) and StopReason from the first choice's finish_reason
//     (openAIStopReason); a server that sends no usage gives Usage.Reported false
func (p OpenAICompat) Chat(ctx context.Context, chatContext []ChatMessage, tools []ToolDef) (Response, error) {
	// make a request to the OpenAI-compatible server
	// 1. build a chatRequest with p.Model and the provided chat context
	reqRaw := openAIRequest{
		Model:    p.Model,
		Messages: chatContext,
		Tools:    tools, // omitempty: nil tools send no "tools" key at all
	}
	if p.MaxTokens > 0 {
		if p.MaxTokensField == config.MaxTokensFieldMaxCompletionTokens {
			reqRaw.MaxCompletionTokens = p.MaxTokens
		} else {
			reqRaw.MaxTokens = p.MaxTokens
		}
	}

	// 2. json.Marshal it into a []byte body
	reqBody, err := json.Marshal(reqRaw)
	if err != nil {
		return Response{}, err
	}

	// 3. build an *http.Request with http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", ...)
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	reqHttp, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return Response{}, err
	}

	// 4. set header "Content-Type": "application/json"
	reqHttp.Header.Set("Content-Type", "application/json")
	if p.ApiKey != "" {
		reqHttp.Header.Set("Authorization", "Bearer "+p.ApiKey)
	}

	// 5. do the request with a http.DefaultClient.Do(req) and TIMEOUT
	resp, err := http.DefaultClient.Do(reqHttp)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	// 5b. Check if the HTTP response status code indicates an error
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// 6. read the response body, json.Unmarshal into a chatResponse
	var chatResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return Response{}, err
	}

	// 7. return the first choice's message as a Response
	if len(chatResp.Choices) == 0 {
		return Response{}, fmt.Errorf("no choices in response")
	}
	choice := chatResp.Choices[0]
	return Response{
		Message:    choice.Message,
		Usage:      chatResp.Usage.toUsage(),
		StopReason: openAIStopReason(choice.FinishReason),
	}, nil
}
