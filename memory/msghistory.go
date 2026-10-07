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

func NewInMemoryChatHistory() (*InMemoryChatHistory, error) {
	return &InMemoryChatHistory{
		messages: make([]model.ChatMessage, 0),
	}, nil
}

// SetupMemoryStore creates and returns a new chat history instance
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

// gets a full copy of the chat history
func (h *InMemoryChatHistory) GetMessages() []model.ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]model.ChatMessage(nil), h.messages...)
}

// appends new messages to the chat history
func (h *InMemoryChatHistory) Append(msgs []model.ChatMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// the ... appends all of the messages one at a time preserving order (equivalent to a for loop but cleaner syntax)
	h.messages = append(h.messages, msgs...)
}
