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

// - Verify an edit_file call shows one summary line and only the first line of each side, however many lines it
// changes, and never the later lines
func Test_Unit_ApprovalPreview_EditFile_SummaryNotWholeChunks(t *testing.T) {
	old := "func a() {\n" + strings.Repeat("\told body\n", 40) + "OLD-TAIL}"
	new := "func b() {\n" + strings.Repeat("\tnew body\n", 80) + "NEW-TAIL}"
	args, _ := json.Marshal(map[string]any{"path": "main.go", "old_string": old, "new_string": new, "replace_all": true})
	lines := approvalPreview(previewCall(string(args)))
	got := strings.Join(lines, "\n")
	if len(lines) != 4 {
		t.Errorf("want 4 lines, got %d:\n%s", len(lines), got)
	}
	for _, want := range []string{`Path: "main.go"`, "replace every match of 42 lines", "with 82 lines", `Old starts: "func a() {…"`, `New starts: "func b() {…"`} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TAIL") || strings.Contains(got, "body") {
		t.Errorf("preview shows more than the first lines:\n%s", got)
	}
}

// - Verify an edit preview of single-line strings has no ellipsis, a long first line is cut, escapes are quoted, and
// replace_all false says "one match"
func Test_Unit_ApprovalPreview_EditFile_ShortLongAndEscapes(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"path": "a\x1b[2J", "old_string": "foo", "new_string": strings.Repeat("y", 300)})
	got := strings.Join(approvalPreview(previewCall(string(args))), "\n")
	for _, want := range []string{"replace one match of 1 lines (3 bytes) with 1 lines (300 bytes)", `Old starts: "foo"`, "…"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, strings.Repeat("y", previewMaxLineChars+1)) {
		t.Errorf("raw escape or uncut line:\n%s", got)
	}
}
