package memory

import (
	"fmt"
	"sync"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
)

// ChatHistory interface for managing running chat history storage and retrieval.
type ChatHistory interface {
	GetMessages() []model.ChatMessage
	Append(msgs []model.ChatMessage)
}

type InMemoryChatHistory struct {
	mu       sync.Mutex
	messages []model.ChatMessage
}

// NewInMemoryChatHistory returns an empty InMemoryChatHistory.
//
// Notes:
//   - the error result is always nil today
func NewInMemoryChatHistory() (*InMemoryChatHistory, error) {
	return &InMemoryChatHistory{
		messages: make([]model.ChatMessage, 0),
	}, nil
}

// SetupMemoryStore creates and returns a new chat history instance.
//
// Notes:
//   - only cfg.ChatStoreType "in-memory" is supported; any other value is an "unsupported chat store type" error
func SetupMemoryStore(cfg config.Config) (ChatHistory, error) {
	if cfg.ChatStoreType == "in-memory" {
		c, err := NewInMemoryChatHistory()
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, fmt.Errorf("unsupported chat store type: %s", cfg.ChatStoreType)
}

// GetMessages gets a full copy of the chat history.
//
// Notes:
//   - the copy means the caller may keep or modify the result without affecting the history
func (h *InMemoryChatHistory) GetMessages() []model.ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]model.ChatMessage(nil), h.messages...)
}

// Append appends new messages to the chat history.
//
// Notes:
//   - the ... appends all of the messages one at a time preserving order (equivalent to a for loop but cleaner syntax)
//   - never trims or evicts: the full history is kept
func (h *InMemoryChatHistory) Append(msgs []model.ChatMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// the ... appends all of the messages one at a time preserving order (equivalent to a for loop but cleaner syntax)
	h.messages = append(h.messages, msgs...)
}
