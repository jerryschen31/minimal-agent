package agent

import (
	"io"

	"github.com/jerryschen31/minimal-agent/mvp3/config"
	"github.com/jerryschen31/minimal-agent/mvp3/contextwindow"
	"github.com/jerryschen31/minimal-agent/mvp3/memory"
	"github.com/jerryschen31/minimal-agent/mvp3/model"
	"github.com/jerryschen31/minimal-agent/mvp3/tools"
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

	// Name labels this session's output lines: "" (chat) prints "[assistant]" and "[tool]"; in autonomous
	// mode the sessions are "worker" and "director", printing "[worker]" / "[worker:tool]" and so on.
	Name string

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

// labels returns the prefixes for this session's assistant and tool output lines.
func (cs *ChatSession) labels() (assistant, tool string) {
	if cs.Name == "" {
		return "[assistant]", "[tool]"
	}
	return "[" + cs.Name + "]", "[" + cs.Name + ":tool]"
}
