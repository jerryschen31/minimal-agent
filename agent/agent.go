package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

type Agent struct {
	session      *ChatSession
	toolRegistry *tools.ToolRegistry
	mcpConns     []*mcpconnect.McpConnection
	agentMode    string
	runTrace     *trace.Scope // the run's trace scope: started by RunAgent, ended by ShutdownAgent
}

// SetupAgent builds the agent from cfg. A Ctrl+C on interrupts (or SIGTERM, which cancels ctx) aborts it: SetupAgent
// then returns context.Canceled, and the MCP servers that had already connected are kept in agent.mcpConns, so
// ShutdownAgent closes them. A nil interrupts channel means setup cannot be interrupted by Ctrl+C (tests).
func (agent *Agent) SetupAgent(ctx context.Context, cfg config.Config, interrupts <-chan os.Signal) error {
	if err := checkConfig(cfg); err != nil {
		return err
	}
	// agent mode
	agent.agentMode = cfg.AgentMode

	// 1. setup LLM provider
	provider, err := model.SetupProvider(cfg)
	if err != nil {
		return err
	}

	// 2. setup memory store for chat history
	chatHistory, err := memory.SetupMemoryStore(cfg)
	if err != nil {
		return err
	}

	// 3. setup context struct for managing active conversation context
	chatContext, err := contextwindow.SetupChatContext(cfg)
	if err != nil {
		return err
	}

	// 3.5 setup deny rules list / approval gates
	denyRules, err := hooks.SetupDenyRules(cfg.DenyRules)
	if err != nil {
		return err
	}

	// 4. setup built-in tools -> builtinTools satisfies Tool interface
	builtinTools, err := tools.SetupBuiltinTools(cfg.BuiltinTools, cfg.WorkDir, cfg.ReadFileMaxBytes)
	if err != nil {
		return err
	}

	// Ctrl+C during the slow part of setup (connecting MCP servers, listing their tools) cancels setupCtx.
	// Only the MCP steps get setupCtx: stop() cancels it when setup returns, so nothing that lives on may keep it
	// (MCP sessions detach from it; see DECISIONS.md § Deferred for Later, item 15).
	setupCtx, stop := createInterruptWatcher(ctx, interrupts)
	defer stop()

	// 5. connect to MCP servers (if applicable)
	// a server that fails is skipped, the others still connect; the error lists every failed server, one per line.
	// mcpConns is kept even on error: ShutdownAgent closes it, also when a later setup step fails
	mcpConns, err := mcpconnect.SetupMCPConns(setupCtx, cfg)
	agent.mcpConns = mcpConns
	if err := setupCtx.Err(); err != nil { // interrupted: abort instead of starting without the remaining servers
		return err
	}
	if err != nil {
		// print errors on MCP server connect fails - but don't exit
		fmt.Fprintf(cfg.OutBuffer, "MCP servers that failed to connect (continuing without their tools):\n%v\n", err)
	}

	// 6. setup MCP tools (if applicable) - mcpTools satisfies Tool interface
	mcpTools, err := mcpconnect.SetupMCPTools(setupCtx, mcpConns, cfg.MCPResultMaxBytes)
	if err := setupCtx.Err(); err != nil {
		return err
	}
	if err != nil {
		// print errors on MCP tools setup fails - but don't exit
		fmt.Fprintf(cfg.OutBuffer, "error setting up MCP tools: %v\n", err)
	}
	// agent.mcpTools = mcpTools

	// 7. setup tool registry
	tools, err := tools.SetupToolRegistry(builtinTools, mcpTools)
	if err != nil {
		return err
	}

	// 8. setup trace recorder
	recorder, err := trace.SetupRecorder(ctx, cfg)
	if err != nil {
		return err
	}

	// 9. setup this chat session
	chatSession, err := setupChatSession(provider, chatHistory, chatContext, tools, recorder, cfg)
	if err != nil {
		return err
	}
	chatSession.ShowProgress = shouldShowProgress(cfg)
	chatSession.Hooks = []hooks.Hook{denyRules}
	agent.session = chatSession

	return nil
}

// need to update this function to handle headless and oneshot modes, not just chat mode
// `intsigChRecv` of type `<-chan os.Signal` only receives OS interrupt signals (e.g., Ctrl+C)
func (agent *Agent) RunAgent(ctx context.Context, intsigChRecv <-chan os.Signal) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// trace the run: run_start now. run_end is recorded by ShutdownAgent, not here: background work such as
	// auto-compaction can still be running when this function returns, and its events must land before run_end.
	agent.runTrace = trace.StartRun(agent.session.Recorder, agent.session.Config)
	// select agent mode
	var err error
	if agent.agentMode == "chat" {
		err = runChatLoop(ctx, intsigChRecv, agent.session)
	} else {
		err = fmt.Errorf("unsupported agent mode: %s", agent.agentMode)
	}
	agent.runTrace.Fail(err)
	return err
}

// gracefully shuts down the agent, ensuring any in-flight agent processes are completed and MCP servers are closed
func (agent *Agent) ShutdownAgent(ctx context.Context) {
	if agent.session != nil {
		// wait for any in-flight background compaction to finish
		agent.session.MsgContext.WaitForCompaction()
	}
	if len(agent.mcpConns) > 0 {
		// close MCP servers
		for _, m := range agent.mcpConns {
			if err := m.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "error closing MCP server: %v\n", err)
			}
		}
	}
	// end the run now that background work is cancelled and drained, and before the recorder stops accepting events
	if agent.runTrace != nil {
		agent.runTrace.End()
		agent.runTrace = nil // a second ShutdownAgent must not write a second run_end
	}
	// close (flush) the event trace recorder
	if agent.session != nil {
		if err := agent.session.Recorder.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "trace: %v\n", err)
		}
	}
}

// checkConfig rejects config values that would make the agent fail later, quietly or on every turn. Shared by
// SetupAgent and PrintInfo, so info mode doesn't report a config as fine that chat mode refuses.
func checkConfig(cfg config.Config) error {
	if cfg.MaxSteps <= 0 {
		return fmt.Errorf("maxSteps must be a positive integer, got %d", cfg.MaxSteps)
	}
	// checked here, not per tool: MCP tool setup only warns and skips a tool that fails, so a bad cap would silently drop every MCP tool
	if cfg.MCPResultMaxBytes <= 0 {
		return fmt.Errorf("mcpResultMaxBytes must be a positive integer, got %d", cfg.MCPResultMaxBytes)
	}
	return nil
}

func createSystemMessage(systemPrompt string) model.ChatMessage {
	return model.ChatMessage{
		Role:      "system",
		Content:   systemPrompt,
		ID:        model.CreateID(),
		Timestamp: time.Now().UTC(),
	}
}
