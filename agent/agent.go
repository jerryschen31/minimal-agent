package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
)

// ErrMaxSteps is returned (wrapped) by reActLoop when the model is still asking for tools after cfg.MaxSteps steps.
// A "sentinel error": one shared error value that callers recognize with errors.Is(err, ErrMaxSteps)
var ErrMaxSteps = errors.New("max steps for ReAct loop exceeded")

type Agent struct {
	session      *ChatSession
	toolRegistry *tools.ToolRegistry
	mcpConns     []*mcpconnect.McpConnection
	agentMode    string
}

func (agent *Agent) SetupAgent(ctx context.Context, cfg config.Config) error {

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
	builtinTools, err := tools.SetupBuiltinTools(cfg.BuiltinTools)
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
	mcpTools, err := mcpconnect.SetupMCPTools(ctx, mcpConns)
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

	// 8. setup this chat session
	chatSession, err := setupChatSession(provider, chatHistory, chatContext, tools, cfg)
	if err != nil {
		return err
	}
	agent.session = chatSession

	return nil
}

// need to update this function to handle headless and oneshot modes, not just chat mode
func (agent *Agent) RunAgent(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if agent.agentMode == "chat" {
		runChatLoop(ctx, agent.session)
		return nil
	}
	return fmt.Errorf("unsupported agent mode: %s", agent.agentMode)
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
			m.Close()
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

func reActLoop(ctx context.Context, cs *ChatSession, priorMsgs []model.ChatMessage, initialMsg model.ChatMessage) ([]model.ChatMessage, error) {
	// implementation of the ReAct loop goes here
	// // [PLAN] ReAct Loop
	// loop up to MaxSteps:
	// reply := Chat(msgs, cs.Tools.GetToolDefs())
	// append reply to the turn's messages
	// if len(reply.ToolCalls) == 0 → done
	// for each call in reply.ToolCalls:
	//     append runToolCall(ctx, cs, call)

	requestMsgs := []model.ChatMessage{initialMsg}
	toolDefs := cs.Tools.GetToolDefs()
	for stepNum := 0; stepNum < cs.Config.MaxSteps; stepNum++ {
		// if user cancels the operation (maybe the ReAct loop is taking too long), exit the loop immediately
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// prepare the request to send to the chat provider - includes the system message, the context, the request message, and tool definition info
		request2send := model.PrepareChatRequest(priorMsgs, cs.SystemMsg, requestMsgs)

		// send the chat request to the LLM provider
		response, err := cs.Provider.Chat(ctx, request2send, toolDefs)
		if err != nil {
			fmt.Fprintln(cs.OutBuffer, "error:", err)
			return nil, err
		}

		// append the response as a ChatMessage to the request messages for the next iteration
		fmt.Fprintf(cs.OutBuffer, "[assistant] %s\n", response)
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
