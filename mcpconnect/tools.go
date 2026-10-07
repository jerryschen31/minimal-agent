package mcpconnect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPResultMaxBytes caps how much of an MCP tool result is returned to the model (same idea as ReadFileMaxBytes).
const MCPResultMaxBytes = 64 * 1024

// *******************************************
// MCP tool registering
// *******************************************

var _ tools.Tool = (*mcpTool)(nil) // placeholder to check for compile errors if interface is not fully implemented

type mcpTool struct {
	mcpConn     *McpConnection // shared connection (a pointer: every tool of a server uses the same session)
	toolName    string         // the tool's name on the server (what tools/call needs); toolDef holds the prefixed name the model sees
	toolDef     model.ToolDef  // tool definition
	safeToRetry bool           // are retries okay on this tool call (i.e., idempotent?)
}

func (t *mcpTool) GetToolDefinition() model.ToolDef {
	return t.toolDef
}

func (t *mcpTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	toolParams := mcp.CallToolParams{Name: t.toolName, Arguments: args}
	res, err := t.mcpConn.Session.CallTool(ctx, &toolParams)
	if err != nil {
		return "", fmt.Errorf("CallTool error: %w", err)
	}
	// flatten the MCP tool result response struct into plain text for the model
	return flattenMCPResult(res)
}

// flattenMCPResult turns an MCP tool result into plain text for the model, or an error if the tool reported failure.
func flattenMCPResult(res *mcp.CallToolResult) (string, error) {
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

	// cap the size, then drop any multi-byte character that the byte cut split in half
	if len(text) > MCPResultMaxBytes {
		text = strings.ToValidUTF8(text[:MCPResultMaxBytes], "") +
			fmt.Sprintf("\n[truncated: result is larger than %d KB]", MCPResultMaxBytes/1024)
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
func newMCPTool(mcpConn *McpConnection, t *mcp.Tool) (*mcpTool, error) {
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
	}, nil
}

func GetMCPTools(ctx context.Context, mcpConn *McpConnection) ([]tools.Tool, error) {
	mcpTools := []tools.Tool{}
	// query the server for available tools - t is of type *mcp.Tool (from the MCP official Go SDK)
	for t, err := range mcpConn.Session.Tools(ctx, nil) {
		// if there is an issue adding a tool, just continue with a warning
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to get tool from server %s: %v\n", mcpConn.Name, err)
			continue
		}
		// otherwise from t of type *mcp.Tool, create a variable that satifies the Tool interface (type mcpTool)
		mt, err := newMCPTool(mcpConn, t) // t is *mcp.Tool, mt is *mcpTool
		if err != nil {
			// warn and skip this one tool
			fmt.Printf("warning: failed to create MCP tool from server %s: %v\n", mcpConn.Name, err)
			continue
		}
		mcpTools = append(mcpTools, mt)
	}
	return mcpTools, nil
}

func SetupMCPTools(ctx context.Context, mcpConns []*McpConnection) ([]tools.Tool, error) {
	mcpTools := []tools.Tool{}
	if len(mcpConns) == 0 {
		return nil, nil
	}

	// for each server, we get the list of tools and then append to our tools object
	for _, s := range mcpConns {
		sTools, err := GetMCPTools(ctx, s)
		if err != nil {
			return nil, err
		}
		mcpTools = append(mcpTools, sTools...)
	}
	return mcpTools, nil
}
