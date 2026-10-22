package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// ErrMaxSteps is returned (wrapped) by reActLoop when the model is still asking for tools after cfg.MaxSteps steps.
// A "sentinel error": one shared error value that callers recognize with errors.Is(err, ErrMaxSteps)
var ErrMaxSteps = errors.New("max steps for ReAct loop exceeded")

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
	provider := cs.getSessionProvider(taskTrace.ID()) // session provider wraps the underlying model.Provider and records requests and responses under the given scope ID
	for stepNum := 0; stepNum < cs.Config.MaxSteps; stepNum++ {
		// if user cancels the operation (maybe the ReAct loop is taking too long), exit the loop immediately
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// prepare the request to send to the chat provider - includes the system message, the context, the request message, and tool definition info
		request2send := model.PrepareChatRequest(priorMsgs, cs.SystemMsg, requestMsgs)

		// send the chat request to the LLM provider
		provider.Step, provider.TurnMsgs = stepNum+1, requestMsgs                    // the trace records the turn's messages; the system prompt and prior context are only counted
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
