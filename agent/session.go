package agent

import (
	"context"
	"encoding/json"
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
	// ToolJournal records every tool call that runs, before and after (auto mode's run journal); nil = not recorded
	// (chat and plan mode). See executeToolCall.
	ToolJournal toolRecorder
	// Meter counts and limits the tokens of this session's model calls (auto mode: one per worker or director attempt,
	// set on a copy of the session); nil = not counted. See getSessionProvider.
	Meter *usageMeter
	// ModeDetails holds what one mode keeps on the session: *planModeDetails in plan mode, nil in chat mode. Code that
	// needs a mode's fields checks for its type (cs.ModeDetails.(*planModeDetails)), so the session itself stays
	// mode-agnostic.
	ModeDetails ModeDetails
	// Output reports to the user (a turn's progress, notices, errors); reActLoop and chat mode never print directly,
	// except what Output's doc comment lists. NewChatSession sets chat mode's chatOutput on OutBuffer; code that swaps
	// OutBuffer must set Output too.
	Output Output
}

// toolRecorder records tool calls around their run (ChatSession.ToolJournal; auto mode's runToolJournal).
type toolRecorder interface {
	// recordCall records that call (to tool, with args) is about to run, and returns the context to run it with and a
	// function to call once it returns, whose error means the end could not be recorded (the caller must not report the
	// call as done). An error from recordCall means it could not be recorded: the call must not run.
	recordCall(ctx context.Context, call model.ToolCall, tool tools.Tool, args json.RawMessage) (context.Context, func(failed bool) error, error)
}

// ModeDetails is what one mode keeps on a ChatSession (ChatSession.ModeDetails); each mode that needs any has its own
// struct. ModeName names the mode, so not just any value can be put there.
type ModeDetails interface{ ModeName() string }

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

// getSessionProvider returns the provider to use for model calls made under scopeID: the session's provider, metered
// when cs.Meter is set, then traced.
//
// Notes:
//   - The meter is under the tracer, so a call it refuses is traced as a model_error.
func (cs *ChatSession) getSessionProvider(scopeID string) *trace.TracedProvider {
	var p model.Provider = cs.Provider
	if cs.Meter != nil {
		p = meteredProvider{inner: p, meter: cs.Meter}
	}
	return trace.GetTracedProvider(p, cs.Recorder, scopeID, cs.Config)
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
