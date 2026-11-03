package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type OpenAICompat struct {
	BaseURL string
	Model   string
	ApiKey  string        // empty for Ollama
	Timeout time.Duration // most one Chat call may take (config's responseTimeoutSeconds); 0 = no limit of its own
}

// Chat sends chatContext (and tools) to an OpenAI-compatible server's /chat/completions and returns the first choice's message.
//
// Notes:
//   - a Timeout above 0 wraps ctx in a deadline for this one call; 0 means no limit of its own
//   - sends an Authorization Bearer header only when ApiKey is set (empty for Ollama)
//   - nil tools send no "tools" key at all (omitempty)
//   - any status outside 200-299 is an error carrying only the status code, with the response body unread; a response with no
//     choices is also an error
//   - the returned message is exactly what the server sent: ID, Timestamp and Type are not filled in here
func (p OpenAICompat) Chat(ctx context.Context, chatContext []ChatMessage, tools []ToolDef) (ChatMessage, error) {
	// make a request to the OpenAI-compatible server
	// 1. build a chatRequest with p.Model and the provided chat context
	reqRaw := ChatRequest{
		Model:    p.Model,
		Messages: chatContext,
		Tools:    tools, // omitempty: nil tools send no "tools" key at all
	}

	// 2. json.Marshal it into a []byte body
	reqBody, err := json.Marshal(reqRaw)
	if err != nil {
		return ChatMessage{}, err
	}

	// 3. build an *http.Request with http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", ...)
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	reqHttp, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return ChatMessage{}, err
	}

	// 4. set header "Content-Type": "application/json"
	reqHttp.Header.Set("Content-Type", "application/json")
	if p.ApiKey != "" {
		reqHttp.Header.Set("Authorization", "Bearer "+p.ApiKey)
	}

	// 5. do the request with a http.DefaultClient.Do(req) and TIMEOUT
	resp, err := http.DefaultClient.Do(reqHttp)
	if err != nil {
		return ChatMessage{}, err
	}
	defer resp.Body.Close()

	// 5b. Check if the HTTP response status code indicates an error
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatMessage{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// 6. read the response body, json.Unmarshal into a chatResponse
	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return ChatMessage{}, err
	}

	// 7. return resp.Choices[0].Message.Content, nil
	if len(chatResp.Choices) == 0 {
		return ChatMessage{}, fmt.Errorf("no choices in response")
	}
	return chatResp.Choices[0].Message, nil
}
