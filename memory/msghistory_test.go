package memory

import (
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

// - Verify that SetupMemoryStore correctly returns an in-memory chat history.
func Test_Unit_SetupMemoryStore_InMemory_ReturnsHistory(t *testing.T) {
	history, err := SetupMemoryStore(config.Config{ChatStoreType: "in-memory"})
	if err != nil {
		t.Fatalf("SetupMemoryStore() returned an error: %v", err)
	}
	if _, ok := history.(*InMemoryChatHistory); !ok {
		t.Errorf("expected a *InMemoryChatHistory, got %T", history)
	}
}

// - Verify that SetupMemoryStore returns an error for an unsupported chat store type.
func Test_Unit_SetupMemoryStore_UnsupportedType_ReturnsError(t *testing.T) {
	history, err := SetupMemoryStore(config.Config{ChatStoreType: "not-a-real-store"})

	if err == nil {
		t.Errorf("expected an error for an unsupported chat store type, got nil")
	}
	if history != nil {
		t.Errorf("expected a nil history for an unsupported chat store type, got %v", history)
	}
}
