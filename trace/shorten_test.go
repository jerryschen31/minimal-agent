package trace

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

// - Verify small arguments, non-object arguments, other tools' long fields, and invalid JSON come back unchanged
func Test_Unit_ShortenArguments_Untouched(t *testing.T) {
	long := `{"command":"` + strings.Repeat("x", 3000) + `"}`
	for name, in := range map[string]string{
		"small":         `{"path":"a","content":"hi"}`,
		"not an object": strings.Repeat("z", 5000),
		"other field":   long,
		"invalid json":  `{"content":"` + strings.Repeat("x", 3000),
		"empty":         ``,
	} {
		if got := shortenArguments(in); got != in {
			t.Errorf("%s: arguments changed (%d -> %d bytes)", name, len(in), len(got))
		}
	}
}

// - Verify long content, old_string and new_string are cut to a prefix plus the byte count, the result is still valid
// JSON, and path and replace_all survive
func Test_Unit_ShortenArguments_CutsFileBodies(t *testing.T) {
	big := strings.Repeat("é", 5000) // multi-byte on purpose: 10000 bytes, 5000 characters
	args, _ := json.Marshal(map[string]any{"path": "a.go", "content": big, "old_string": big, "new_string": "short", "replace_all": true})
	got := shortenArguments(string(args))
	if len(got) > 2000 {
		t.Fatalf("not shortened: %d bytes", len(got))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["path"] != "a.go" || m["new_string"] != "short" || m["replace_all"] != true {
		t.Errorf("other fields changed: %v", m)
	}
	for _, k := range []string{"content", "old_string"} {
		s := m[k].(string)
		if !strings.HasPrefix(s, strings.Repeat("é", bodyKeptChars)) || !strings.Contains(s, "10000 bytes in all") {
			t.Errorf("%s = %.60q…", k, s)
		}
	}
}

// - Verify the cut is applied where arguments are copied into the trace: tool_requested data and message records
func Test_Unit_ShortenArguments_AppliedInTraceRecords(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "a.txt", "content": strings.Repeat("S", 100000)})
	call := model.ToolCall{ID: "c1", Function: model.ToolCallFunc{Name: "write_file", Arguments: string(args)}}
	if d := GetToolRequestedData(call); len(d.Arguments) > 2000 || !strings.Contains(d.Arguments, "100000 bytes in all") {
		t.Errorf("tool_requested arguments: %d bytes", len(d.Arguments))
	}
	rec := messageRecord(model.ChatMessage{Role: "assistant", ToolCalls: []model.ToolCall{call}})
	if len(rec.ToolCalls) != 1 || len(rec.ToolCalls[0].Arguments) > 2000 {
		t.Errorf("message record not shortened: %d", len(rec.ToolCalls[0].Arguments))
	}
	if len(call.Function.Arguments) < 100000 {
		t.Error("the original call was modified")
	}
}
