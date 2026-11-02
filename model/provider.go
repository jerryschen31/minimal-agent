package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

type Provider interface {
	Chat(ctx context.Context, chatContext []ChatMessage, tools []ToolDef) (ChatMessage, error)
}

// ChatMessage is sent to the provider as-is, so only the fields the chat completions API defines
// (role, content, tool_calls, tool_call_id) have JSON keys. ID, Timestamp and Type are the agent's own
// bookkeeping (compaction matches messages by ID), so they are tagged `json:"-"`: never sent, and never
// read back from a response. Anything that later serializes ChatMessage for storage (e.g. a persistent
// chat store) must not rely on encoding/json to keep them; use a separate storage type.
type ChatMessage struct {
	ID         string     `json:"-"`
	Role       string     `json:"role"` // `json:"role"` is a tag indicating the JSON key for this field
	Content    string     `json:"content"`
	Timestamp  time.Time  `json:"-"`
	Type       string     `json:"-"`                      // flexible type field that indicates what kind of message this is (e.g., "user", "system", "summary")
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // records any tool invocations associated with this message
	ToolCallID string     `json:"tool_call_id,omitempty"` // ID of the associated tool call, if any
}

// shape of a tool definition that is sent as context to the LLM - this shape matches what is expected by most models (OpenAI chat completions standard)
type ToolDefFunc struct {
	Name   string          `json:"name"`
	Desc   string          `json:"description"`
	Params json.RawMessage `json:"parameters"`
	Strict bool            `json:"strict,omitempty"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolDefFunc `json:"function"`
}

// shape of the LLM response when it asks for a tool call - this shape matches what is returned by most models
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

// chat request and response structs
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ToolDef     `json:"tools,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
}

// SetupProvider sets up the provider named in cfg.
//
// Notes:
//   - only cfg.Provider "openai" (any OpenAI-compatible server) is supported; any other value is an "unsupported provider" error
//   - the API key is read from the environment variable named by cfg.ApiKeyName; an unset variable gives an empty key, which is
//     fine for Ollama
//   - ResponseTimeoutSeconds becomes the provider's Timeout
func SetupProvider(cfg config.Config) (Provider, error) {
	if cfg.Provider == "openai" {
		return &OpenAICompat{
			Model:   cfg.Model,
			BaseURL: cfg.BaseURL,
			ApiKey:  os.Getenv(cfg.ApiKeyName),
			Timeout: time.Duration(cfg.ResponseTimeoutSeconds) * time.Second,
		}, nil
	} else {
		// provider not supported
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
}

// CreateChatMessage builds a ChatMessage with a fresh ID and the current UTC time.
//
// Notes:
//   - the ID (CreateID) is what compaction uses to match messages
//   - Timestamp, ID and Type are the agent's own bookkeeping and are never sent to the provider (see ChatMessage)
func CreateChatMessage(role string, msgType string, content string, toolCallID string, toolCalls []ToolCall) ChatMessage {
	return ChatMessage{
		Role:       role,
		ToolCallID: toolCallID,
		Content:    content,
		ID:         CreateID(),
		Timestamp:  time.Now().UTC(),
		Type:       msgType,
		ToolCalls:  toolCalls,
	}
}

// PrepareChatRequest prepares the chat messages to be sent to the provider by combining the system message, the context, and
// the messages this turn into a single slice of ChatMessage.
//
// Notes:
//   - an empty (or whitespace-only) system message is left out, so a config file's "systemPrompt": "" really sends no system prompt
//   - the order is system message, context, then turn messages
//   - [agent] length 0 but room for every message; append fills it in place, and the new slice never shares the caller's arrays
func PrepareChatRequest(msgContext []ChatMessage, systemMsg ChatMessage, turnMsgs []ChatMessage) []ChatMessage {
	// [agent] length 0 but room for every message; append fills it in place, and the new slice never shares the caller's arrays
	chat2send := make([]ChatMessage, 0, len(msgContext)+len(turnMsgs)+1)
	if strings.TrimSpace(systemMsg.Content) != "" {
		chat2send = append(chat2send, systemMsg)
	}
	chat2send = append(chat2send, msgContext...)
	return append(chat2send, turnMsgs...)
}

// NewToolDef instantiates a new tool definition instance.
//
// Notes:
//   - type is always "function"
//   - empty params become an empty object schema, `{"type":"object","properties":{}}`, for a tool that takes no arguments
func NewToolDef(name string, description string, params json.RawMessage) ToolDef {
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return ToolDef{
		Type:     "function",
		Function: ToolDefFunc{Name: name, Desc: description, Params: params},
	}
}

// CreateID generates a (fairly) unique ID using 16 random bytes and encodes it as a hex string.
//
// Notes:
//   - github.com/google/uuid is better but generates a dependency. We can consider if we truly need RFC-4122 UUIDs
//   - uses crypto/rand; its error is ignored
func CreateID() string {
	b := make([]byte, 16)
	rand.Read(b) // crypto/rand
	return hex.EncodeToString(b)
}
