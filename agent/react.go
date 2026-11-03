package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// ErrMaxSteps is returned (wrapped) by reActLoop when the model is still asking for tools after cfg.MaxSteps steps.
// A "sentinel error": one shared error value that callers recognize with errors.Is(err, ErrMaxSteps)
var ErrMaxSteps = errors.New("max steps for ReAct loop exceeded")

// callToolSafely runs tool.CallTool and recovers from a panic inside a tool.
//
// Notes:
//   - A panic becomes a "tool panicked" error and an empty result, since we want model execution to continue on a
//     failed tool.
func callToolSafely(ctx context.Context, tool tools.Tool, args json.RawMessage) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return tool.CallTool(ctx, args)
}

// runToolCall executes a single tool call and returns the result as a tool ChatMessage, which the next iteration of the
// ReAct loop sends back to the model.
//
// Notes:
//   - It is essentially a wrapper around the actual tool call execution. This wrapper allows us to record the call
//     before and after: tool_requested before it runs, tool_finished with what the model will see after.
//   - Every failure becomes an observation the model can recover from: the content is "error: ..." and the message is
//     still returned.
func runToolCall(ctx context.Context, cs *ChatSession, taskTrace *trace.Scope, call model.ToolCall) model.ChatMessage {
	taskTrace.Record(trace.ToolRequested, trace.GetToolRequestedData(call))
	start := time.Now()

	content, err := executeToolCall(ctx, cs, taskTrace, call)
	if err != nil {
		content = fmt.Sprintf("error: %v", err) // every failure becomes an observation the model can recover from
	}

	taskTrace.Record(trace.ToolFinished, trace.GetToolFinishedData(call, content, err != nil, time.Since(start)))
	return model.CreateChatMessage("tool", "tool", content, call.ID, nil)
}

// executeToolCall validates and runs a single tool call. It returns the tool's result, or an error for an unknown tool,
// bad arguments, a hook veto, a tool error or a tool panic.
//
// Notes:
//   - Order: look up the tool by call.Function.Name, check the arguments are a JSON object, run the before-tool hooks,
//     then call the tool.
//   - Arguments is a string holding JSON; empty "" is allowed for no-arg tools. A JSON null or other non-object is
//     rejected.
//   - If Arguments is empty, json.RawMessage would be an empty byte slice, so a proper empty JSON object `{}` is
//     passed instead.
//   - The call runs under a context limited to cfg.ToolTimeoutSeconds. A tool that ignores its context cannot be
//     stopped by this (the loop waits for it), so every tool must honour ctx.
func executeToolCall(ctx context.Context, cs *ChatSession, taskTrace *trace.Scope, call model.ToolCall) (string, error) {
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
	if err := checkBeforeToolHooks(ctx, cs, taskTrace, call); err != nil {
		return "", err
	}

	// 4. Run tool.CallTool(ctx, json.RawMessage(args)).
	args := json.RawMessage(call.Function.Arguments)
	// if call.Function.Arguments is empty, json.RawMessage returns an empty byte slice - create a proper empty JSON string in this case.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	// 5. Limit the call to cfg.ToolTimeoutSeconds (built-in and MCP tools alike). 0 = no limit (only a hand-built config
	// in a test; checkConfig makes real configs positive).
	toolCtx := ctx
	if secs := cs.Config.ToolTimeoutSeconds; secs > 0 {
		var cancel context.CancelFunc
		toolCtx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
	}
	result, err := callToolSafely(toolCtx, tool, args)
	// ctx still alive and only the tool's own deadline passed: say it timed out, so the model can react (a Ctrl+C or the
	// turn timeout ends ctx itself and is reported by the caller instead)
	if err != nil && ctx.Err() == nil && errors.Is(toolCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("tool timed out after %ds (toolTimeoutSeconds): %w", cs.Config.ToolTimeoutSeconds, err)
	}
	return result, err
}

// checkBeforeToolHooks calls hooks that check the tool calls. It returns an error if any hook vetoes the call.
//
// Notes:
//   - Only a veto wrapping hooks.ErrNeedsApproval (an ask rule) can be overridden: by allow-all (cs.AllowAskRules) or
//     by the user when there is someone to ask (cs.PromptAllowDenyCall). Any other veto (a deny rule) is final.
//   - Every veto is recorded as a tool_vetoed event, with what happened next and how long the user took to decide.
func checkBeforeToolHooks(ctx context.Context, cs *ChatSession, taskTrace *trace.Scope, call model.ToolCall) error {
	for _, hook := range cs.Hooks {
		// BeforeTool returns non-nil if the tool call is denied or needs approval (e.g., matches a deny or ask rule)
		err := hook.BeforeTool(ctx, call)
		if err == nil {
			continue
		}
		decision, wait := trace.VetoDenied, time.Duration(0)
		if errors.Is(err, hooks.ErrNeedsApproval) {
			switch {
			case cs.AllowAskRules:
				decision = trace.VetoAllowedByConfig
			case cs.PromptAllowDenyCall != nil: // the user on a terminal (or a test) decides
				start := time.Now()
				decision = trace.VetoDeniedByUser
				if cs.PromptAllowDenyCall(ctx, call, err) {
					decision = trace.VetoAllowedByUser
				}
				wait = time.Since(start)
			}
		}
		taskTrace.Record(trace.ToolVetoed, trace.GetToolVetoedData(call, err, decision, wait))
		if decision != trace.VetoAllowedByUser && decision != trace.VetoAllowedByConfig {
			return err
		}
	}
	return nil
}

// reActLoop runs the ReAct loop for one user turn, up to MaxSteps steps, and returns the turn's messages.
//
// Notes:
//   - At each step, the model either returns the response if the initial prompt request has been fulfilled; otherwise
//     it calls tool functions as needed.
//   - Exits at once if ctx is cancelled (Ctrl+C), checked at the start of every step.
//   - The request is built with model.PrepareChatRequest from the prior context, system message and this turn's
//     messages. The session provider wraps the underlying model.Provider and records requests and responses under the
//     task's scope ID; the trace records the turn's messages, while the system prompt and prior context are only
//     counted.
//   - A model error is returned as is: the caller reports it (cs.Output.Error), once.
//   - Returns ErrMaxSteps (wrapped with the limit) when the model is still asking for tools after cfg.MaxSteps steps.
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
		response, err := chatWithProgress(ctx, cs, provider, request2send, toolDefs) // cs.Output shows thinking dots in chat mode
		if err != nil {
			return nil, err // the caller reports it (cs.Output.Error), once
		}

		// append the response as a ChatMessage to the request messages for the next iteration
		cs.Output.Assistant(response.Content)
		responseMsg := model.CreateChatMessage("assistant", "assistant", response.Content, "", response.ToolCalls)
		requestMsgs = append(requestMsgs, responseMsg)

		// if there are no tool calls in the response, we are done
		if len(response.ToolCalls) == 0 {
			return requestMsgs, nil
		}
		// otherwise there are tool calls to be made
		for _, tc := range response.ToolCalls {
			cs.Output.ToolCalled(tc.Function.Name)
			toolResultMsg := runToolCall(ctx, cs, taskTrace, tc)
			requestMsgs = append(requestMsgs, toolResultMsg)
		}
	}

	return nil, fmt.Errorf("%w (limit %d)", ErrMaxSteps, cs.Config.MaxSteps)
}
