package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

func previewCall(args string) model.ToolCall {
	return model.ToolCall{Function: model.ToolCallFunc{Name: "write_file", Arguments: args}}
}

// - Verify a write_file call shows the path, size and the first lines only, not the whole content
func Test_Unit_ApprovalPreview_WriteFile_ShowsPathSizeAndFirstLines(t *testing.T) {
	content := strings.Repeat("line\n", 20) + "TAIL-SENTINEL"
	args, _ := json.Marshal(map[string]string{"path": "docs/a.md", "content": content})
	got := strings.Join(approvalPreview(previewCall(string(args))), "\n")
	for _, want := range []string{`Path: "docs/a.md"`, "Size: 113 bytes, 21 lines", `Content: "line"`, "16 more lines not shown"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TAIL-SENTINEL") {
		t.Errorf("preview shows content past the first lines:\n%s", got)
	}
}

// - Verify a long line is cut, and terminal escape codes in the path or content are quoted, not printed raw
func Test_Unit_ApprovalPreview_LongLineCut_EscapesQuoted(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "a\x1b[2Jb", "content": strings.Repeat("x", 500) + "\x1b[31m"})
	got := strings.Join(approvalPreview(previewCall(string(args))), "\n")
	if strings.Contains(got, "\x1b") {
		t.Errorf("raw escape code in preview: %q", got)
	}
	if strings.Contains(got, strings.Repeat("x", previewMaxLineChars+1)) || !strings.Contains(got, "…") {
		t.Errorf("long line not cut:\n%s", got)
	}
}

// - Verify empty content is a one-line, zero-byte preview, and other tools show their arguments (cut when long)
func Test_Unit_ApprovalPreview_EmptyContent_OtherTools_Truncated(t *testing.T) {
	got := approvalPreview(previewCall(`{"path":"e.txt","content":""}`))
	if !strings.Contains(strings.Join(got, "\n"), "Size: 0 bytes, 1 lines") {
		t.Errorf("empty content preview = %v", got)
	}
	short := approvalPreview(previewCall(`{"path":"x"}`))
	if len(short) != 1 || short[0] != `Arguments: "{\"path\":\"x\"}"` {
		t.Errorf("no-content call preview = %v", short)
	}
	long := approvalPreview(previewCall(`{"path":"` + strings.Repeat("p", 2000) + `"}`))
	if len(long) != 1 || len(long[0]) > previewMaxArgsChars+100 || !strings.Contains(long[0], "characters in all") {
		t.Errorf("long arguments not cut: %d chars", len(long[0]))
	}
	if bad := approvalPreview(previewCall(`not json`)); len(bad) != 1 || !strings.HasPrefix(bad[0], "Arguments:") {
		t.Errorf("bad JSON preview = %v", bad)
	}
}
