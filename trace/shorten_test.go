package trace

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

// - Verify small arguments, an empty string, and another tool's long field (a shell command) come back unchanged
func Test_Unit_ShortenArguments_Untouched(t *testing.T) {
	long := `{"command":"` + strings.Repeat("x", 3000) + `"}`
	for name, in := range map[string]string{
		"small":       `{"path":"a","content":"hi"}`,
		"other field": long,
		"empty":       ``,
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

// - Verify arguments that are too long and are not a readable JSON object (truncated or invalid JSON, a JSON array or
// string, null padded with spaces) are cut as a whole, so a body in a malformed call never reaches the trace
func Test_Unit_ShortenArguments_UnreadableLongArguments_CutWhole(t *testing.T) {
	body := strings.Repeat("SECRET-BODY ", 5000) // 60000 bytes
	for name, in := range map[string]string{
		"truncated json": `{"path":"a.txt","content":"` + body,
		"missing brace":  `{"path":"a.txt","content":"` + body + `"`,
		"trailing comma": `{"path":"a.txt","content":"` + body + `",}`,
		"json array":     `["` + body + `"]`,
		"json string":    `"` + body + `"`,
		"null padded":    `null` + strings.Repeat(" ", 5000),
		"plain text":     body,
	} {
		got := shortenArguments(in)
		if len(got) > shortenAbove+100 || !strings.Contains(got, "bytes in all, cut for the trace") {
			t.Errorf("%s: %d -> %d bytes, %.80q", name, len(in), len(got), got)
		}
	}
}

// - Verify body keys are matched case-insensitively, like the tools' own JSON decode: Content, OLD_STRING and New_String
// are cut, and an unrelated key that merely contains a body name is not
func Test_Unit_ShortenArguments_CaseVariantKeys(t *testing.T) {
	big := strings.Repeat("B", 5000)
	args, _ := json.Marshal(map[string]string{"Path": "a.txt", "Content": big, "OLD_STRING": big, "New_String": big, "content_type": big})
	var m map[string]string
	if err := json.Unmarshal([]byte(shortenArguments(string(args))), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Content", "OLD_STRING", "New_String"} {
		if len(m[k]) > bodyKeptChars+100 || !strings.Contains(m[k], "5000 bytes in all") {
			t.Errorf("%s not cut: %d bytes", k, len(m[k]))
		}
	}
	if len(m["content_type"]) != 5000 || m["Path"] != "a.txt" {
		t.Errorf("an unrelated key was changed: content_type %d bytes, Path %q", len(m["content_type"]), m["Path"])
	}
}

// - Verify the cut applies to a malformed call in the trace records too (tool_requested data and message records)
func Test_Unit_ShortenArguments_MalformedCallInTraceRecords(t *testing.T) {
	bad := `{"path":"a.txt","content":"` + strings.Repeat("S", 100000) // never closed
	call := model.ToolCall{ID: "c1", Function: model.ToolCallFunc{Name: "write_file", Arguments: bad}}
	if d := GetToolRequestedData(call); len(d.Arguments) > shortenAbove+100 {
		t.Errorf("tool_requested: %d bytes", len(d.Arguments))
	}
	rec := messageRecord(model.ChatMessage{Role: "assistant", ToolCalls: []model.ToolCall{call}})
	if len(rec.ToolCalls[0].Arguments) > shortenAbove+100 {
		t.Errorf("message record: %d bytes", len(rec.ToolCalls[0].Arguments))
	}
}
