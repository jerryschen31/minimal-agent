package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// ErrMaxSteps is returned (wrapped) by reActLoop when the model is still asking for tools after cfg.MaxSteps steps.
// A "sentinel error": one shared error value that callers recognize with errors.Is(err, ErrMaxSteps)
var ErrMaxSteps = errors.New("max steps for ReAct loop exceeded")

type Agent struct {
	session      *ChatSession
	toolRegistry *tools.ToolRegistry
	mcpConns     []*mcpconnect.McpConnection
	agentMode    string
	runTrace     *trace.Scope // the run's trace scope: started by RunAgent, ended by ShutdownAgent
}

func (agent *Agent) SetupAgent(ctx context.Context, cfg config.Config) error {
	if cfg.MaxSteps <= 0 {
		return fmt.Errorf("maxSteps must be a positive integer, got %d", cfg.MaxSteps)
	}
	// checked here, not per tool: MCP tool setup only warns and skips a tool that fails, so a bad cap would silently drop every MCP tool
	if cfg.MCPResultMaxBytes <= 0 {
		return fmt.Errorf("mcpResultMaxBytes must be a positive integer, got %d", cfg.MCPResultMaxBytes)
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

	// 4. setup built-in tools -> builtinTools satisfies Tool interface
	builtinTools, err := tools.SetupBuiltinTools(cfg.BuiltinTools, cfg.WorkDir, cfg.ReadFileMaxBytes)
	if err != nil {
		return err
	}

	// 5. connect to MCP servers (if applicable)
	mcpConns, err := mcpconnect.SetupMCPConns(ctx, cfg)
	if err != nil {
		// print errors on MCP server connect fails - but don't exit
		fmt.Fprintf(cfg.OutBuffer, "error connecting to MCP servers: %v\n", err)
	}
	agent.mcpConns = mcpConns

	// 6. setup MCP tools (if applicable) - mcpTools satisfies Tool interface
	mcpTools, err := mcpconnect.SetupMCPTools(ctx, mcpConns, cfg.MCPResultMaxBytes)
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

func createSystemMessage(systemPrompt string) model.ChatMessage {
	return model.ChatMessage{
		Role:      "system",
		Content:   systemPrompt,
		ID:        model.CreateID(),
		Timestamp: time.Now().UTC(),
	}
}

// callToolSafely runs tool.CallTool and recovers from a panic inside a tool (since we want model execution to continue on a failed tool)
func callToolSafely(ctx context.Context, tool tools.Tool, args json.RawMessage) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return tool.CallTool(ctx, args)
}

// executes a single tool call and returns the result as a ChatMessage. In the next iteration of the ReAct loop, this chat message is sent back to the model
func runToolCall(ctx context.Context, cs *ChatSession, call model.ToolCall) model.ChatMessage {
	// 1. Look up call.Function.Name with cs.Tools.Lookup. If the tool is unknown, the result is an error message.
	tool, ok := cs.Tools.Lookup(call.Function.Name)
	if !ok {
		return model.CreateChatMessage("tool", "tool", fmt.Sprintf("error: unknown tool %q", call.Function.Name), call.ID, nil)
	}
	// 2. Check that call.Function.Arguments is a JSON object. Arguments is a string holding JSON. If it isn't an object, the result is an error message. Empty "" for no-arg tools.
	if call.Function.Arguments != "" {
		var argsMap map[string]interface{}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &argsMap); err != nil {
			return model.CreateChatMessage("tool", "tool", fmt.Sprintf("error: invalid JSON arguments: %v", err), call.ID, nil)
		}
		if argsMap == nil {
			return model.CreateChatMessage("tool", "tool", "error: arguments must be a JSON object", call.ID, nil)
		}
	}

	// 3. Run tool.CallTool(ctx, json.RawMessage(args)). If it returns an error, put "error: ..." in Content.
	args := json.RawMessage(call.Function.Arguments)
	// if call.Function.Arguments is empty, json.RawMessage returns an empty byte slice - create a proper empty JSON string in this case.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := callToolSafely(ctx, tool, args)
	if err != nil {
		return model.CreateChatMessage("tool", "tool", fmt.Sprintf("error: %v", err), call.ID, nil)
	}

	return model.CreateChatMessage("tool", "tool", result, call.ID, nil)
}

// reActLoop loops up to MaxSteps. At each step, the model either returns the response if the initial prompt request has been fulfilled; otherwise it calls tool functions as needed.
func reActLoop(ctx context.Context, cs *ChatSession, taskTrace *trace.Scope, priorMsgs []model.ChatMessage, initialMsg model.ChatMessage) ([]model.ChatMessage, error) {
	requestMsgs := []model.ChatMessage{initialMsg}
	toolDefs := cs.Tools.GetToolDefs()
	provider := cs.tracedProvider(taskTrace.ID()) // records model_request / model_response / model_error for each step
	for stepNum := 0; stepNum < cs.Config.MaxSteps; stepNum++ {
		// if user cancels the operation (maybe the ReAct loop is taking too long), exit the loop immediately
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// prepare the request to send to the chat provider - includes the system message, the context, the request message, and tool definition info
		request2send := model.PrepareChatRequest(priorMsgs, cs.SystemMsg, requestMsgs)

		// send the chat request to the LLM provider
		provider.Step = stepNum + 1
		response, err := chatWithProgress(ctx, cs, provider, request2send, toolDefs) // prints thinking dots in chat mode
		if err != nil {
			if !errors.Is(err, context.Canceled) { // [agent] a Ctrl+C cancel is reported once, as [interrupted], by the caller
				fmt.Fprintln(cs.OutBuffer, "error:", err)
			}
			return nil, err
		}

		// append the response as a ChatMessage to the request messages for the next iteration
		fmt.Fprintf(cs.OutBuffer, "[assistant] %s\n", response.Content)
		responseMsg := model.CreateChatMessage("assistant", "assistant", response.Content, "", response.ToolCalls)
		requestMsgs = append(requestMsgs, responseMsg)

		// if there are no tool calls in the response, we are done
		if len(response.ToolCalls) == 0 {
			return requestMsgs, nil
		}
		// otherwise there are tool calls to be made
		for _, tc := range response.ToolCalls {
			fmt.Fprintln(cs.OutBuffer, "[tool] Tool called:", tc.Function.Name)
			toolResultMsg := runToolCall(ctx, cs, tc)
			requestMsgs = append(requestMsgs, toolResultMsg)
		}
	}

	return nil, fmt.Errorf("%w (limit %d)", ErrMaxSteps, cs.Config.MaxSteps)
}
