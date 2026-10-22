package agent

import (
	"fmt"
	"io"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// chatSession holds the state and configuration for an ongoing chat session
type ChatSession struct {
	Config     config.Config
	Provider   model.Provider
	MsgHistory memory.ChatHistory
	MsgContext *contextwindow.ChatContext
	Tools      *tools.ToolRegistry
	SystemMsg  model.ChatMessage
	UserID     string
	InBuffer   io.Reader
	OutBuffer  io.Writer
	Recorder   trace.Recorder

	// ShowProgress prints "thinking" dots while waiting for the model (see chatWithProgress).
	// Set by SetupAgent only for chat mode on a terminal; tests leave it off.
	ShowProgress bool
}

func NewChatSession(provider model.Provider, chatHistory memory.ChatHistory, chatContext *contextwindow.ChatContext, tools *tools.ToolRegistry, recorder trace.Recorder, cfg config.Config) (*ChatSession, error) {
	if recorder == nil {
		return nil, fmt.Errorf("no recorder provided")
	}
	return &ChatSession{
		Config:     cfg,
		Provider:   provider,
		MsgHistory: chatHistory,
		MsgContext: chatContext,
		Tools:      tools,
		SystemMsg:  createSystemMessage(cfg.SystemPrompt),
		UserID:     cfg.UserID,
		InBuffer:   cfg.InBuffer,
		OutBuffer:  cfg.OutBuffer,
		Recorder:   recorder,
	}, nil
}

// tracedProvider returns the session's provider wrapped so its model calls are recorded under scopeID.
func (cs *ChatSession) tracedProvider(scopeID string) *trace.TracedProvider {
	return trace.WrapProvider(cs.Provider, cs.Recorder, scopeID, cs.Config)
}

// creates a new chat session instance
func setupChatSession(provider model.Provider, chatHistory memory.ChatHistory, chatContext *contextwindow.ChatContext, tools *tools.ToolRegistry, recorder trace.Recorder, cfg config.Config) (*ChatSession, error) {
	cs, err := NewChatSession(provider, chatHistory, chatContext, tools, recorder, cfg)
	if err != nil {
		return nil, err
	}
	return cs, nil
}
