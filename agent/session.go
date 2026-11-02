package agent

import (
	"context"
	"fmt"
	"io"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// chatSession holds the state and configuration for an ongoing chat session
type ChatSession struct {
	Config              config.Config
	Provider            model.Provider
	MsgHistory          memory.ChatHistory
	MsgContext          *contextwindow.ChatContext
	Tools               *tools.ToolRegistry
	Hooks               []hooks.Hook
	SystemMsg           model.ChatMessage
	UserID              string
	InBuffer            io.Reader
	OutBuffer           io.Writer
	Recorder            trace.Recorder
	PromptAllowDenyCall func(ctx context.Context, call model.ToolCall, reason error) bool // asks the user about an ask-rule call; nil = nobody to ask (denied)
	// AllowAskRules runs ask-rule calls without asking: approval "allow-all", confirmed by the user at chat startup
	// (confirmAllowAll). It never affects deny rules.
	AllowAskRules bool
	// Output reports to the user (a turn's progress, notices, errors); reActLoop and chat mode never print directly,
	// except what Output's doc comment lists. NewChatSession sets chat mode's chatOutput on OutBuffer; code that swaps
	// OutBuffer must set Output too.
	Output Output
}

// NewChatSession builds a ChatSession from its parts, with chat mode's Output on cfg.OutBuffer.
//
// Notes:
//   - A nil recorder is an error.
//   - A session must have an Output: code that swaps OutBuffer must set Output too.
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
		Output:     chatOutput{w: cfg.OutBuffer, showProgress: shouldShowProgress(cfg)},
	}, nil
}

// getSessionProvider returns the provider to use for model calls made under scopeID: the session's provider, traced.
//
// Notes: none.
func (cs *ChatSession) getSessionProvider(scopeID string) *trace.TracedProvider {
	return trace.GetTracedProvider(cs.Provider, cs.Recorder, scopeID, cs.Config)
}

// setupChatSession creates a new chat session instance.
//
// Notes:
//   - Thin wrapper over NewChatSession.
func setupChatSession(provider model.Provider, chatHistory memory.ChatHistory, chatContext *contextwindow.ChatContext, tools *tools.ToolRegistry, recorder trace.Recorder, cfg config.Config) (*ChatSession, error) {
	cs, err := NewChatSession(provider, chatHistory, chatContext, tools, recorder, cfg)
	if err != nil {
		return nil, err
	}
	return cs, nil
}
