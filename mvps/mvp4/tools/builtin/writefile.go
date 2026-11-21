package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jerryschen31/minimal-agent/mvp4/model"
)

// Write-side built-in tools: write_file, edit_file and list_dir. They follow the same rules as read_file
// (DECISIONS.md § read_file is confined to workDir): every path stays inside workDir, deny-listed names
// are refused, symlinks are resolved before the check, and the actual open goes through os.Root.

// WriteFileMaxBytes caps what write_file and edit_file will write (and what edit_file will read).
const WriteFileMaxBytes = 1024 * 1024

// ListDirMaxEntries caps how many entries list_dir returns.
const ListDirMaxEntries = 500

// resolveTarget is resolve() for a path that may not exist yet (write_file creating a new file).
// [agent] filepath.EvalSymlinks fails on a missing path, so it resolves the deepest *existing* ancestor
// instead and re-attaches the missing tail. A symlinked directory higher up is therefore still followed
// before the "inside root?" check, just as resolve() does for existing files.
func resolveTarget(root, requested string) (string, error) {
	full := requested
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	full = filepath.Clean(full)

	existing := full
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("%s: no existing parent directory", requested)
		}
		existing = parent
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	tail, err := filepath.Rel(existing, full)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, filepath.Join(realExisting, tail))
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s is outside the allowed directory", requested)
	}
	if isDenied(rel) {
		return "", fmt.Errorf("%s is blocked: it matches the sensitive-file deny list (.env files, *.pem, *.key, id_rsa*, .ssh/)", requested)
	}
	return rel, nil
}

// ---- write_file ----

// WriteFileTool creates or overwrites a text file inside workDir, creating parent directories as needed.
type WriteFileTool struct {
	root string
}

func NewWriteFileTool(workDir string) (*WriteFileTool, error) {
	rt, err := NewReadFileTool(workDir) // same workDir checks and symlink-resolved root
	if err != nil {
		return nil, err
	}
	return &WriteFileTool{root: rt.root}, nil
}

func (*WriteFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"write_file",
		"Creates a text file, or replaces its whole contents, inside the working directory. Parent directories are created as needed. For a small change to an existing file prefer edit_file.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path of the file to write, relative to the working directory"},
				"content": {"type": "string", "description": "The complete new contents of the file"}
			},
			"required": ["path", "content"]
		}`),
	)
}

func (t *WriteFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path    string  `json:"path"`
		Content *string `json:"content"` // pointer: "" is a valid (empty) file, a missing key is not
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("missing required argument: path")
	}
	if params.Content == nil {
		return "", fmt.Errorf("missing required argument: content")
	}
	if len(*params.Content) > WriteFileMaxBytes {
		return "", fmt.Errorf("content is %d bytes; the limit is %d", len(*params.Content), WriteFileMaxBytes)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rel, err := resolveTarget(t.root, params.Path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(t.root)
	if err != nil {
		return "", err
	}
	defer root.Close()

	// only ever replace a regular file: never a directory, pipe or device
	if info, err := root.Lstat(rel); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s exists and is not a regular file", params.Path)
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := root.WriteFile(rel, []byte(*params.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(*params.Content), filepath.ToSlash(rel)), nil
}

// ---- edit_file ----

// EditFileTool replaces an exact piece of text in an existing file. The text must match exactly once
// (unless replace_all is set), so the model can't silently edit the wrong spot.
type EditFileTool struct {
	rt *ReadFileTool // reuses read_file's resolve() and root
}

func NewEditFileTool(workDir string) (*EditFileTool, error) {
	rt, err := NewReadFileTool(workDir)
	if err != nil {
		return nil, err
	}
	return &EditFileTool{rt: rt}, nil
}

func (*EditFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"edit_file",
		"Edits an existing text file inside the working directory by replacing old_string with new_string. old_string must match the file exactly (including whitespace) and occur exactly once, unless replace_all is true. Read the file first.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path of the file to edit"},
				"old_string": {"type": "string", "description": "The exact text to replace"},
				"new_string": {"type": "string", "description": "The replacement text"},
				"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of exactly one (default false)"}
			},
			"required": ["path", "old_string", "new_string"]
		}`),
	)
}

func (t *EditFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path       string  `json:"path"`
		OldString  string  `json:"old_string"`
		NewString  *string `json:"new_string"`
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	switch {
	case params.Path == "":
		return "", fmt.Errorf("missing required argument: path")
	case params.OldString == "":
		return "", fmt.Errorf("old_string must not be empty (use write_file to create or replace a whole file)")
	case params.NewString == nil:
		return "", fmt.Errorf("missing required argument: new_string")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rel, err := t.rt.resolve(params.Path)
	if err != nil {
		return "", err
	}
	data, perm, err := readRegularFile(t.rt.root, rel, params.Path, WriteFileMaxBytes)
	if err != nil {
		return "", err
	}
	if len(data) > WriteFileMaxBytes {
		return "", fmt.Errorf("%s is larger than %d bytes; edit_file won't change it", params.Path, WriteFileMaxBytes)
	}
	if bytes.IndexByte(data, 0) != -1 {
		return "", fmt.Errorf("%s looks like a binary file, not text", params.Path)
	}

	text := string(data)
	count := strings.Count(text, params.OldString)
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string not found in %s (it must match exactly, including whitespace)", params.Path)
	case count > 1 && !params.ReplaceAll:
		return "", fmt.Errorf("old_string occurs %d times in %s; add surrounding text to make it unique, or set replace_all", count, params.Path)
	}
	n := 1
	if params.ReplaceAll {
		n = -1
	}
	updated := strings.Replace(text, params.OldString, *params.NewString, n)
	if len(updated) > WriteFileMaxBytes {
		return "", fmt.Errorf("the edited file would be %d bytes; the limit is %d", len(updated), WriteFileMaxBytes)
	}

	root, err := os.OpenRoot(t.rt.root)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.WriteFile(rel, []byte(updated), perm); err != nil {
		return "", err
	}
	replaced := 1
	if params.ReplaceAll {
		replaced = count
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, filepath.ToSlash(rel)), nil
}

// ---- list_dir ----

// ListDirTool lists one directory (not recursive) inside workDir. Deny-listed names are left out.
type ListDirTool struct {
	rt *ReadFileTool
}

func NewListDirTool(workDir string) (*ListDirTool, error) {
	rt, err := NewReadFileTool(workDir)
	if err != nil {
		return nil, err
	}
	return &ListDirTool{rt: rt}, nil
}

func (*ListDirTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"list_dir",
		"Lists the files and subdirectories of a directory inside the working directory (not recursive). Directories end with \"/\".",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Directory to list, relative to the working directory (default \".\")"}
			}
		}`),
	)
}

func (t *ListDirTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		params.Path = "."
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rel, err := t.rt.resolve(params.Path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(t.rt.root)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0) // O_NONBLOCK: see readRegularFile
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", params.Path)
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return "", err
	}

	var lines []string
	for _, e := range entries {
		if isDenied(e.Name()) {
			continue
		}
		switch {
		case e.IsDir():
			lines = append(lines, e.Name()+"/")
		default:
			if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
				lines = append(lines, fmt.Sprintf("%s (%d bytes)", e.Name(), fi.Size()))
			} else {
				lines = append(lines, e.Name())
			}
		}
	}
	slices.Sort(lines)
	if len(lines) == 0 {
		return "(empty directory)", nil
	}
	if len(lines) > ListDirMaxEntries {
		more := len(lines) - ListDirMaxEntries
		lines = append(lines[:ListDirMaxEntries], fmt.Sprintf("[... %d more entries not shown]", more))
	}
	return strings.Join(lines, "\n"), nil
}
