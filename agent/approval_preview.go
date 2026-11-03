package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/textutils"
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
//   - The shapes are mutually exclusive: a call with both content and old_string/new_string fields is shown raw with a
//     warning, never as only one of the two (the tool ignores unknown keys, so either half could be the one that runs).
//   - A call with a string "command" and no path, content or old_string/new_string (run_command) shows the program, its
//     arguments one by one, and the directory (commandPreview). With a command AND any of those file fields it is shown
//     raw with a warning, like the mixed content/old_string shape above, so the command is never hidden behind a file preview.
//   - Any other call, or arguments that don't parse, show the raw arguments cut at previewMaxArgsChars.
//   - Everything is quoted with %q because a path or content can hold terminal escape codes.
//   - Downside: the user approves a preview, not the whole content; the lines after the shown ones are unseen.
func approvalPreview(call model.ToolCall) []string {
	// One struct for every shape, decoded the way the tools decode (encoding/json: case-insensitive keys, unknown keys
	// ignored), so the preview reads the arguments as the tool will. A nil pointer means the key is absent.
	var in struct {
		Path       *string   `json:"path"`
		Content    *string   `json:"content"`
		OldString  *string   `json:"old_string"`
		NewString  *string   `json:"new_string"`
		ReplaceAll bool      `json:"replace_all"`
		Command    *string   `json:"command"`
		Args       *[]string `json:"args"`
		Dir        *string   `json:"dir"`
	}
	args := call.Function.Arguments
	err := json.Unmarshal([]byte(args), &in)
	if err == nil && in.Command != nil { // run_command
		if in.Path == nil && in.Content == nil && in.OldString == nil && in.NewString == nil {
			return commandPreview(*in.Command, in.Args, in.Dir)
		}
		// [agent] run_command ignores unknown keys, so a call with a command AND file fields would run the command after
		// the user approved what looks like a file preview. Say so and show the raw arguments, never one half.
		return append([]string{"[warning] this call has a command and also path/content/old_string/new_string fields; shown as sent:"}, rawPreview(args)...)
	}
	if err == nil && in.Path != nil {
		hasEditFields := in.OldString != nil || in.NewString != nil
		switch {
		case in.Content == nil && in.OldString != nil && in.NewString != nil: // edit_file
			return editPreview(*in.Path, *in.OldString, *in.NewString, in.ReplaceAll)
		case in.Content != nil && !hasEditFields: // write_file
			return writePreview(*in.Path, *in.Content)
		case in.Content != nil && hasEditFields:
			// [agent] A write_file call may carry extra old_string/new_string keys (the tool ignores unknown keys), and an
			// edit_file call may carry a content key. Describing it as either would hide the other half, so say so and
			// show the raw arguments.
			return append([]string{"[warning] this call has both content and old_string/new_string fields; shown as sent:"}, rawPreview(args)...)
		}
	}
	return rawPreview(args)
}

// writePreview returns the approval lines for a write_file-shaped call: the path, the size and the first lines.
//
// Notes:
//   - At most previewMaxLines lines, each cut at previewMaxLineChars, quoted with %q.
func writePreview(path, content string) []string {
	lines := strings.Split(content, "\n")
	out := []string{
		fmt.Sprintf("Path: %q", path),
		fmt.Sprintf("Size: %d bytes, %d lines", len(content), len(lines)),
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

// rawPreview returns the arguments as one quoted line, cut at previewMaxArgsChars characters.
//
// Notes:
//   - A cut line says how many characters there were in all.
func rawPreview(args string) []string {
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
	if cut, wasCut := textutils.CutRunes(line, previewMaxLineChars); wasCut {
		return cut + "…"
	}
	if more && rest != "" {
		return line + "…"
	}
	return line
}

// commandPreview returns the approval lines for a run_command-shaped call: the program, the arguments and the directory.
//
// Notes:
//   - The arguments are shown as a quoted list, one element per argument, so a space inside an argument is visible
//     (the question never joins them into one string a person could misread). A long list is cut at previewMaxArgsChars
//     characters and says how many arguments there were.
//   - args and dir are nil when the call has no such key; dir is shown only when set.
func commandPreview(command string, args *[]string, dir *string) []string {
	out := []string{fmt.Sprintf("Run: %q", command)}
	if args != nil && len(*args) > 0 {
		list := fmt.Sprintf("%q", *args)
		if cut, wasCut := textutils.CutRunes(list, previewMaxArgsChars); wasCut {
			list = fmt.Sprintf("%s… (%d arguments in all)", cut, len(*args))
		}
		out = append(out, "Args: "+list)
	}
	if dir != nil && *dir != "" {
		out = append(out, fmt.Sprintf("In directory: %q", *dir))
	}
	return out
}
