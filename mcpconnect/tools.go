package mcpconnect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/textutils"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// *******************************************
// MCP tool registering
// *******************************************

var _ tools.Tool = (*mcpTool)(nil) // placeholder to check for compile errors if interface is not fully implemented

type mcpTool struct {
	mcpConn     *McpConnection // shared connection (a pointer: every tool of a server uses the same session)
	toolName    string         // the tool's name on the server (what tools/call needs); toolDef holds the prefixed name the model sees
	toolDef     model.ToolDef  // tool definition
	safeToRetry bool           // are retries okay on this tool call (i.e., idempotent?)
	maxBytes    int            // most of a result that is passed to the model (config mcpResultMaxBytes)
	hints       ToolHints      // the server's annotations, for display (-mode info)
}

// ToolHints are the server's own claims about a tool (MCP annotations). They are hints, not guarantees:
// fine for display and retry decisions, never for skipping approval.
type ToolHints struct {
	Known       bool // the server sent annotations; false means nothing is known about the tool
	ReadOnly    bool
	Destructive bool // the MCP default is true when the tool is not read-only and the server didn't say
	Idempotent  bool
}

// HintsOf returns an MCP tool's hints; ok is false when t is not an MCP tool.
//
// Notes:
//   - The hints are the server's own claims, not guarantees; never use them to skip approval.
func HintsOf(t tools.Tool) (hints ToolHints, ok bool) {
	mt, ok := t.(*mcpTool)
	if !ok {
		return ToolHints{}, false
	}
	return mt.hints, true
}

// hintsFrom converts a server's tool annotations into ToolHints.
//
// Notes:
//   - nil annotations give the zero value (Known false).
//   - Destructive follows the MCP default: true unless the tool is read-only or the server said otherwise.
func hintsFrom(a *mcp.ToolAnnotations) ToolHints {
	if a == nil {
		return ToolHints{}
	}
	return ToolHints{
		Known:       true,
		ReadOnly:    a.ReadOnlyHint,
		Destructive: !a.ReadOnlyHint && (a.DestructiveHint == nil || *a.DestructiveHint),
		Idempotent:  a.IdempotentHint,
	}
}

// GetToolDefinition returns the definition of the MCP tool as the model sees it.
//
// Notes:
//   - The name is prefixed with the server label ("<server>_<tool>"), see newMCPTool.
func (t *mcpTool) GetToolDefinition() model.ToolDef {
	return t.toolDef
}

// CallTool calls the tool on its MCP server and returns the result flattened to text.
//
// Notes:
//   - Sends the tool's name on the server (toolName), not the prefixed name the model sees.
//   - A protocol error and a tool-reported failure both come back as an error (see flattenMCPResult).
func (t *mcpTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	toolParams := mcp.CallToolParams{Name: t.toolName, Arguments: args}
	res, err := t.mcpConn.Session.CallTool(ctx, &toolParams)
	if err != nil {
		return "", fmt.Errorf("CallTool error: %w", err)
	}
	// flatten the MCP tool result response struct into plain text for the model
	return flattenMCPResult(res, t.maxBytes)
}

// flattenMCPResult turns an MCP tool result into plain text for the model, or an error if the tool reported failure.
//
// Notes:
//   - The text is cut to maxBytes (which must be positive) with a truncation note.
//   - Images and other non-text content are replaced by a placeholder; with no content blocks at all it falls back to
//     the structured output, if the server sent one.
//   - The byte cut never splits a multi-byte character (textutils.CutBytes backs up to the character before it).
//   - When the tool reported failure (IsError), the text is the error message (runToolCall prefixes "error: ").
func flattenMCPResult(res *mcp.CallToolResult, maxBytes int) (string, error) {
	if res == nil {
		return "", fmt.Errorf("mcp server returned no result")
	}

	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image omitted: %s]", c.MIMEType))
		default: // audio, resource links, embedded resources: not passed to the model yet
			parts = append(parts, "[non-text content omitted]")
		}
	}
	text := strings.Join(parts, "\n")

	// no content blocks at all: fall back to the structured output, if the server sent one
	if len(parts) == 0 && res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("encoding structured content: %w", err)
		}
		text = string(b)
	}

	// cap the size; the cut never splits a multi-byte character
	if cut, wasCut := textutils.CutBytes(text, maxBytes); wasCut {
		text = cut + fmt.Sprintf("\n[truncated: result is larger than %d KB]", maxBytes/1024)
	}

	// the tool ran but reported failure: the text is the error message (runToolCall prefixes "error: ")
	if res.IsError {
		if text == "" {
			text = "tool reported an error with no message"
		}
		return "", errors.New(text)
	}
	return text, nil
}

// newMCPTool wraps one tool listed by an MCP server as a Tool the agent can register and call.
//
// Notes:
//   - maxBytes is the most of a result that is passed to the model; it must be positive.
//   - InputSchema is an `any` (a map[string]any when it comes from a server), but ToolDef wants raw JSON. A nil schema
//     is left empty so NewToolDef fills in its "no parameters" default; marshalling nil would send "null".
//   - The model sees the name "<server label>_<tool name>", which avoids tool name conflicts across multiple servers.
//   - Annotations are optional (nil) and only hints; no annotations means "not known to be safe to retry".
func newMCPTool(mcpConn *McpConnection, t *mcp.Tool, maxBytes int) (*mcpTool, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("mcp result max bytes must be positive, got %d", maxBytes)
	}
	// get tool parameters as raw JSON from the mcp.Tool input schema
	// InputSchema is an `any` (a map[string]any when it comes from a server), but ToolDef wants raw JSON.
	// A nil schema is left empty so NewToolDef fills in its "no parameters" default; marshalling nil would send "null".
	var params json.RawMessage
	if t.InputSchema != nil {
		b, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("encoding input schema of tool %q: %w", t.Name, err)
		}
		params = b
	}

	// name sent to the model - sees "<server label>_<tool name>"; avoids tool name conflicts across multiple servers
	modelName := mcpConn.Name + "_" + t.Name

	// annotations are optional (nil) and only hints; no annotations means "not known to be safe to retry"
	safeToRetry := t.Annotations != nil && (t.Annotations.ReadOnlyHint || t.Annotations.IdempotentHint)

	return &mcpTool{
		mcpConn:     mcpConn,
		toolName:    t.Name,
		toolDef:     model.NewToolDef(modelName, t.Description, params),
		safeToRetry: safeToRetry,
		maxBytes:    maxBytes,
		hints:       hintsFrom(t.Annotations),
	}, nil
}

// GetMCPTools lists the tools of one MCP server, each wrapped as a tools.Tool.
//
// Notes:
//   - The list may be incomplete: a failed tools/list request or a tool that can't be wrapped is skipped, and the
//     returned error (all such failures, joined) says so.
//   - The tools that did load are returned either way, so the caller decides whether a partial list is usable (chat
//     setup: yes, with a warning; info mode: shows the server as failed).
func GetMCPTools(ctx context.Context, mcpConn *McpConnection, maxResultBytes int) ([]tools.Tool, error) {
	mcpTools := []tools.Tool{}
	var errs []error
	// query the server for available tools - t is of type *mcp.Tool (from the MCP official Go SDK)
	for t, err := range mcpConn.Session.Tools(ctx, nil) {
		if err != nil {
			errs = append(errs, fmt.Errorf("listing tools: %w", err))
			continue
		}
		// otherwise from t of type *mcp.Tool, create a variable that satifies the Tool interface (type mcpTool)
		mt, err := newMCPTool(mcpConn, t, maxResultBytes) // t is *mcp.Tool, mt is *mcpTool
		if err != nil {
			errs = append(errs, fmt.Errorf("tool %q: %w", t.Name, err)) // skip this one tool
			continue
		}
		mcpTools = append(mcpTools, mt)
	}
	return mcpTools, errors.Join(errs...) // nil when errs is empty
}

// SetupMCPTools lists the tools of every connected MCP server and returns them all in one slice.
//
// Notes:
//   - A partial list is still usable: a server whose tools could not all be loaded gets a warning on stderr and
//     contributes the tools that did load, as one failing server doesn't stop the others.
//   - The returned error is always nil; with no connections the result is nil.
func SetupMCPTools(ctx context.Context, mcpConns []*McpConnection, maxResultBytes int) ([]tools.Tool, error) {
	mcpTools := []tools.Tool{}
	if len(mcpConns) == 0 {
		return nil, nil
	}

	// for each server, we get the list of tools and then append to our tools object
	for _, s := range mcpConns {
		sTools, err := GetMCPTools(ctx, s, maxResultBytes)
		if err != nil {
			// a partial list is still usable: warn and keep the tools that loaded, as one failing server doesn't stop the others
			fmt.Fprintf(os.Stderr, "warning: MCP server %s: some tools could not be loaded: %v\n", s.Name, err)
		}
		mcpTools = append(mcpTools, sTools...)
	}
	return mcpTools, nil
}
