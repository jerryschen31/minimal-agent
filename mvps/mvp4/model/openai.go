package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type OpenAICompat struct {
	BaseURL string
	Model   string
	ApiKey  string // empty for Ollama
}

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
	ctx, cancel := context.WithTimeout(ctx, ResponseTimeout)
	defer cancel()
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
		// include the start of the error body: it usually says why (unknown model, bad key, bad request)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ChatMessage{}, fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, strings.Join(strings.Fields(string(body)), " "))
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
