package agent

import (
	"io"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
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

	// ShowProgress prints "thinking" dots while waiting for the model (see chatWithProgress).
	// Set by SetupAgent only for chat mode on a terminal; tests leave it off.
	ShowProgress bool
}

func NewChatSession(provider model.Provider, chatHistory memory.ChatHistory, chatContext *contextwindow.ChatContext, tools *tools.ToolRegistry, cfg config.Config) (*ChatSession, error) {
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
	}, nil
}

// creates a new chat session instance
func setupChatSession(provider model.Provider, chatHistory memory.ChatHistory, chatContext *contextwindow.ChatContext, tools *tools.ToolRegistry, cfg config.Config) (*ChatSession, error) {
	cs, err := NewChatSession(provider, chatHistory, chatContext, tools, cfg)
	if err != nil {
		return nil, err
	}
	return cs, nil
}
