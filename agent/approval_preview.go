package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
)

const (
	previewMaxLines     = 5   // lines of write_file content shown in the approval question
	previewMaxLineChars = 120 // characters of one shown line
	previewMaxArgsChars = 500 // characters of the raw arguments shown for any other tool
)

// approvalPreview returns the lines the approval question shows for a tool call, each already quoted with %q.
//
// Notes:
//   - [agent] The raw arguments of write_file hold the whole file (up to 1 MB), which would scroll the question off the
//     screen and bury the path. For a call that has a string "path" and a string "content" (write_file, and an MCP
//     server's write_file with the same shape) it shows the path, the size and the first previewMaxLines lines instead.
//   - A call with string "path", "old_string" and "new_string" (edit_file) shows a one-line summary and the first line
//     of each side (editPreview): one question per call, never per line.
//   - Any other call, or arguments that don't parse, show the raw arguments cut at previewMaxArgsChars.
//   - Everything is quoted with %q because a path or content can hold terminal escape codes.
//   - Downside: the user approves a preview, not the whole content; the lines after the shown ones are unseen.
func approvalPreview(call model.ToolCall) []string {
	var ed struct {
		Path       *string `json:"path"`
		OldString  *string `json:"old_string"`
		NewString  *string `json:"new_string"`
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &ed); err == nil && ed.Path != nil && ed.OldString != nil && ed.NewString != nil {
		return editPreview(*ed.Path, *ed.OldString, *ed.NewString, ed.ReplaceAll)
	}
	var in struct {
		Path    *string `json:"path"`
		Content *string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &in); err == nil && in.Path != nil && in.Content != nil {
		lines := strings.Split(*in.Content, "\n")
		out := []string{
			fmt.Sprintf("Path: %q", *in.Path),
			fmt.Sprintf("Size: %d bytes, %d lines", len(*in.Content), len(lines)),
		}
		for i, l := range lines {
			if i == previewMaxLines {
				out = append(out, fmt.Sprintf("... %d more lines not shown", len(lines)-previewMaxLines))
				break
			}
			if r := []rune(l); len(r) > previewMaxLineChars {
				l = string(r[:previewMaxLineChars]) + "…"
			}
			out = append(out, fmt.Sprintf("Content: %q", l))
		}
		return out
	}
	args := call.Function.Arguments
	if r := []rune(args); len(r) > previewMaxArgsChars {
		return []string{fmt.Sprintf("Arguments: %q… (%d characters in all)", string(r[:previewMaxArgsChars]), len(r))}
	}
	return []string{fmt.Sprintf("Arguments: %q", args)}
}

// editPreview returns the approval lines for an edit_file-shaped call: one summary line and the first line of each side.
//
// Notes:
//   - The summary is computed from the arguments (sizes and line counts), not written by the model, so it can't
//     describe an edit that isn't the one requested.
//   - Only the first line of old_string and new_string is shown, cut at previewMaxLineChars, quoted with %q.
func editPreview(path, oldStr, newStr string, all bool) []string {
	scope := "one match"
	if all {
		scope = "every match"
	}
	return []string{
		fmt.Sprintf("Path: %q", path),
		fmt.Sprintf("Edit: replace %s of %d lines (%d bytes) with %d lines (%d bytes)", scope,
			strings.Count(oldStr, "\n")+1, len(oldStr), strings.Count(newStr, "\n")+1, len(newStr)),
		fmt.Sprintf("Old starts: %q", previewFirstLine(oldStr)),
		fmt.Sprintf("New starts: %q", previewFirstLine(newStr)),
	}
}

// previewFirstLine returns the first line of s, cut at previewMaxLineChars characters, with "…" when anything was left out.
//
// Notes: none.
func previewFirstLine(s string) string {
	line, rest, more := strings.Cut(s, "\n")
	r := []rune(line)
	if len(r) > previewMaxLineChars {
		return string(r[:previewMaxLineChars]) + "…"
	}
	if more && rest != "" {
		return line + "…"
	}
	return line
}
