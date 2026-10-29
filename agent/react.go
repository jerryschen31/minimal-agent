package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// runToolCall executes a single tool call and returns the result as a ChatMessage. In the next iteration of the ReAct loop, this chat message is sent back to the model.
// This is essentially a wrapper around the actual tool call execution. This wrapper allows us to record the call before and after:
// tool_requested before it runs, tool_finished with what the model will see after.
func runToolCall(ctx context.Context, cs *ChatSession, taskTrace *trace.Scope, call model.ToolCall) model.ChatMessage {
	taskTrace.Record(trace.ToolRequested, trace.GetToolRequestedData(call))
	start := time.Now()

	content, err := executeToolCall(ctx, cs, call)
	if err != nil {
		content = fmt.Sprintf("error: %v", err) // every failure becomes an observation the model can recover from
	}

	taskTrace.Record(trace.ToolFinished, trace.GetToolFinishedData(call, content, err != nil, time.Since(start)))
	return model.CreateChatMessage("tool", "tool", content, call.ID, nil)
}

// executeToolCall validates and runs a single tool call. It returns the tool's result, or an error for an unknown tool, bad arguments, a tool error or a tool panic.
func executeToolCall(ctx context.Context, cs *ChatSession, call model.ToolCall) (string, error) {
	// 1. Look up call.Function.Name with cs.Tools.Lookup. If the tool is unknown, that is an error.
	tool, ok := cs.Tools.Lookup(call.Function.Name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
	// 2. Check that call.Function.Arguments is a JSON object. Arguments is a string holding JSON. Empty "" for no-arg tools.
	if call.Function.Arguments != "" {
		var argsMap map[string]interface{}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &argsMap); err != nil {
			return "", fmt.Errorf("invalid JSON arguments: %v", err)
		}
		if argsMap == nil {
			return "", errors.New("arguments must be a JSON object")
		}
	}

	// 3. Call any registered hooks before running the tool
	if err := checkBeforeToolHooks(ctx, cs, call); err != nil {
		return "", err
	}

	// 4. Run tool.CallTool(ctx, json.RawMessage(args)).
	args := json.RawMessage(call.Function.Arguments)
	// if call.Function.Arguments is empty, json.RawMessage returns an empty byte slice - create a proper empty JSON string in this case.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return callToolSafely(ctx, tool, args)
}

// checkBeforeToolHooks calls hooks that check the tool calls. It returns an error if any hook returns a failure (e.g., a tool call deny).
func checkBeforeToolHooks(ctx context.Context, cs *ChatSession, call model.ToolCall) error {
	for _, hook := range cs.Hooks {
		// BeforeTool returns non-nil if the tool call is denied or vetoed by the hook (e.g., matches a deny rule)
		if err := hook.BeforeTool(ctx, call); err != nil {
			// We give the user on a terminal (or a test) a chance to override the denial if desired
			if cs.PromptAllowDenyCall != nil && cs.PromptAllowDenyCall(ctx, call, err) {
				continue
			}
			return err
		}
	}
	return nil
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
			toolResultMsg := runToolCall(ctx, cs, taskTrace, tc)
			requestMsgs = append(requestMsgs, toolResultMsg)
		}
	}

	return nil, fmt.Errorf("%w (limit %d)", ErrMaxSteps, cs.Config.MaxSteps)
}
