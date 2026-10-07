package mcpconnect

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// - Verify newMCPTool gives the model a "<server label>_<tool name>" name, keeps the server-side
// name for the call, carries the description and schema into the ToolDef, and shares the server pointer
func Test_Unit_NewMCPTool_BuildsToolDefAndKeepsServerSideName(t *testing.T) {
	server := &McpConnection{Name: "fs"}
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []any{"path"},
	}

	tool, err := newMCPTool(server, &mcp.Tool{Name: "read_text_file", Description: "Reads a file", InputSchema: schema})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := tool.GetToolDefinition()
	if def.Function.Name != "fs_read_text_file" {
		t.Errorf("expected the model-facing name fs_read_text_file, got %q", def.Function.Name)
	}
	if tool.toolName != "read_text_file" {
		t.Errorf("expected the server-side name read_text_file, got %q", tool.toolName)
	}
	if def.Type != "function" || def.Function.Desc != "Reads a file" {
		t.Errorf("expected a function def with the description, got %+v", def)
	}
	var gotSchema map[string]any
	if err := json.Unmarshal(def.Function.Params, &gotSchema); err != nil || gotSchema["type"] != "object" || gotSchema["required"] == nil {
		t.Errorf("expected the input schema as JSON params, got %s (err %v)", def.Function.Params, err)
	}
	if tool.mcpConn != server {
		t.Errorf("expected the tool to share the server pointer, not a copy")
	}
}

// - Verify a tool with no input schema gets the default "no parameters" schema, not the JSON text "null"
func Test_Unit_NewMCPTool_NilSchema_UsesDefaultParams(t *testing.T) {
	tool, err := newMCPTool(&McpConnection{Name: "s"}, &mcp.Tool{Name: "ping"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(tool.GetToolDefinition().Function.Params); got != `{"type":"object","properties":{}}` {
		t.Errorf("expected the default empty-object schema, got %s", got)
	}
}

// - Verify a schema that can't be marshalled to JSON is an error naming the tool
func Test_Unit_NewMCPTool_UnmarshalableSchema_ReturnsError(t *testing.T) {
	_, err := newMCPTool(&McpConnection{Name: "s"}, &mcp.Tool{Name: "bad", InputSchema: make(chan int)})

	if err == nil || !strings.Contains(err.Error(), `"bad"`) {
		t.Errorf("expected an error naming the tool, got %v", err)
	}
}

// - Verify safeToRetry is true only when annotations say the tool is read-only or idempotent (nil annotations = not safe)
func Test_Unit_NewMCPTool_SafeToRetry_FromAnnotations(t *testing.T) {
	tests := []struct {
		name string
		ann  *mcp.ToolAnnotations
		want bool
	}{
		{"nil annotations", nil, false},
		{"empty annotations", &mcp.ToolAnnotations{}, false},
		{"read-only", &mcp.ToolAnnotations{ReadOnlyHint: true}, true},
		{"idempotent", &mcp.ToolAnnotations{IdempotentHint: true}, true},
		{"title only", &mcp.ToolAnnotations{Title: "Move file"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, err := newMCPTool(&McpConnection{Name: "s"}, &mcp.Tool{Name: "x", Annotations: tt.ann})

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tool.safeToRetry != tt.want {
				t.Errorf("expected safeToRetry=%v, got %v", tt.want, tool.safeToRetry)
			}
		})
	}
}

// - Verify flattenMCPResult turns each kind of result into the right text, or into an error when
// the tool reported failure (wantErr is a substring of the expected error; "" means success)
func Test_Unit_FlattenMCPResult(t *testing.T) {
	text := func(s string) mcp.Content { return &mcp.TextContent{Text: s} }

	tests := []struct {
		name    string
		res     *mcp.CallToolResult
		want    string
		wantErr string
	}{
		{"one text block", &mcp.CallToolResult{Content: []mcp.Content{text("hello")}}, "hello", ""},
		{"text blocks joined by newline", &mcp.CallToolResult{Content: []mcp.Content{text("a"), text("b")}}, "a\nb", ""},
		{"image becomes a placeholder", &mcp.CallToolResult{Content: []mcp.Content{text("see:"), &mcp.ImageContent{MIMEType: "image/png", Data: []byte("x")}}}, "see:\n[image omitted: image/png]", ""},
		{"audio becomes a generic placeholder", &mcp.CallToolResult{Content: []mcp.Content{&mcp.AudioContent{MIMEType: "audio/wav"}}}, "[non-text content omitted]", ""},
		{"no content, structured output is used", &mcp.CallToolResult{StructuredContent: map[string]any{"n": 1}}, `{"n":1}`, ""},
		{"content wins over structured output", &mcp.CallToolResult{Content: []mcp.Content{text("hi")}, StructuredContent: map[string]any{"n": 1}}, "hi", ""},
		{"nothing at all is empty text", &mcp.CallToolResult{}, "", ""},
		{"IsError returns the text as an error", &mcp.CallToolResult{IsError: true, Content: []mcp.Content{text("no such file")}}, "", "no such file"},
		{"IsError without text still errors", &mcp.CallToolResult{IsError: true}, "", "no message"},
		{"nil result is an error", nil, "", "no result"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := flattenMCPResult(tt.res)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got (%q, %v)", tt.wantErr, got, err)
				}
				if got != "" {
					t.Errorf("expected empty text alongside the error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// - Verify a result over MCPResultMaxBytes is cut at the cap with a truncation note, and a result exactly at the cap is left alone
func Test_Unit_FlattenMCPResult_Truncation(t *testing.T) {
	big := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}

	exact, err := flattenMCPResult(big(strings.Repeat("a", MCPResultMaxBytes)))
	if err != nil || len(exact) != MCPResultMaxBytes || strings.Contains(exact, "truncated") {
		t.Errorf("expected a result exactly at the cap to be untouched, got len=%d err=%v", len(exact), err)
	}

	over, err := flattenMCPResult(big(strings.Repeat("a", MCPResultMaxBytes+10)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(over, strings.Repeat("a", MCPResultMaxBytes)) || !strings.HasSuffix(over, "KB]") || !strings.Contains(over, "[truncated:") {
		t.Errorf("expected the cap's worth of text plus a truncation note, got len=%d tail=%q", len(over), over[len(over)-45:])
	}
}

// - Verify truncation never leaves half of a multi-byte character (invalid UTF-8) in the result
func Test_Unit_FlattenMCPResult_Truncation_DoesNotSplitMultiByteCharacter(t *testing.T) {
	// "a" then 2-byte "é" repeated: byte MCPResultMaxBytes-1 is the first half of an "é", so a raw byte cut would split it
	s := "a" + strings.Repeat("é", MCPResultMaxBytes)

	got, err := flattenMCPResult(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Errorf("expected valid UTF-8 after truncation")
	}
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("expected a truncation note, got tail %q", got[len(got)-45:])
	}
}
