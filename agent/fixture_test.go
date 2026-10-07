package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/tools"
)

// chatSessionFixture bundles a fully wired ChatSession (history + context + a fake provider)
// so tests don't have to repeat the setup, and so a signature change to how these pieces are
// wired together only needs fixing here, not in every test. Meant for tests that exercise
// ChatSession-level behavior (handleUserInput, compaction, chat request shape) — pure
// ContextWindow-mechanics tests don't need a ChatSession at all and should use forEachWindow
// (once that's built) instead of this fixture.
type chatSessionFixture struct {
	Session  *ChatSession
	History  *memory.InMemoryChatHistory
	Context  *contextwindow.ChatContext
	Provider *modeltest.FakeProvider
	Tools    *tools.ToolRegistry // starts empty; tests that need tools call Tools.Register
	Out      *bytes.Buffer
}

// newChatSessionFixture wires up a ChatSession backed by an OffsetWindow (the default window
// type for tests that aren't specifically about window-type mechanics) of the given maxSize,
// with auto-compaction configured to fire once the window reaches threshold messages.
func newChatSessionFixture(t *testing.T, maxSize, threshold int) *chatSessionFixture {
	t.Helper()

	history, err := memory.NewInMemoryChatHistory()
	if err != nil {
		t.Fatalf("memory.NewInMemoryChatHistory() returned an error: %v", err)
	}

	window := contextwindow.NewOffsetWindow(maxSize)
	chatContext, err := contextwindow.NewChatContext(window, threshold)
	if err != nil {
		t.Fatalf("contextwindow.NewChatContext() returned an error: %v", err)
	}

	provider := &modeltest.FakeProvider{}
	out := &bytes.Buffer{}

	cfg := config.Config{
		UserID:       "test-user",
		SystemPrompt: "You are a helpful assistant.",
		MaxSteps:     10,
		InBuffer:     strings.NewReader(""),
		OutBuffer:    out,
	}

	tools, err := tools.NewToolRegistry(nil)
	if err != nil {
		t.Fatalf("tools.NewToolRegistry() returned an error: %v", err)
	}

	session, err := NewChatSession(provider, history, chatContext, tools, cfg)
	if err != nil {
		t.Fatalf("NewChatSession() returned an error: %v", err)
	}

	return &chatSessionFixture{
		Session:  session,
		History:  history,
		Context:  chatContext,
		Provider: provider,
		Tools:    tools,
		Out:      out,
	}
}
