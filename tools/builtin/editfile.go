package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jerryschen31/minimal-agent/model"
)

// EditFileTool replaces text inside an existing file in the workspace (rootDir, minus deny-listed, write-denied and
// trace paths). It never creates a file.
type EditFileTool struct {
	ws       *Workspace // where it may edit (shared with the other file tools)
	maxBytes int        // largest file it edits and largest result it writes (maxWriteBytes; a field so tests can use a small one)
	// afterRead, if not nil, runs after the file is read and before the changed-since-read check; tests use it to change the
	// file at that moment, which nothing else can stage deterministically
	afterRead func()
	locks     sync.Map // resolved path (lowercased) -> *sync.Mutex, so two edits of one file in the same step run one after the other
}

// NewEditFileTool creates an edit_file tool that edits only inside ws (NewWorkspace).
//
// Notes:
//   - A nil ws is an error.
func NewEditFileTool(ws *Workspace) (*EditFileTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("edit_file needs a workspace")
	}
	return &EditFileTool{ws: ws, maxBytes: maxWriteBytes}, nil
}

// GetToolDefinition returns the definition of the edit_file tool.
//
// Notes:
//   - The description says the match must be exact and unique (or replace_all), that the file must exist, and states
//     the size cap, so the model reads first and copies exactly.
func (t *EditFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"edit_file",
		fmt.Sprintf("Replaces text in an existing UTF-8 text file inside the working directory. old_string must match the file exactly "+
			"(whitespace and line endings included) and exactly once, unless replace_all is true; include enough surrounding lines to make it unique. "+
			"Read the file first and copy the text exactly. The file must exist (use write_file to create one). Files and results up to %d KB.", t.maxBytes/1024),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The path of the existing file to edit"
				},
				"old_string": {
					"type": "string",
					"description": "The exact text to replace; must not be empty"
				},
				"new_string": {
					"type": "string",
					"description": "The text to put in its place (empty deletes old_string); must differ from old_string"
				},
				"replace_all": {
					"type": "boolean",
					"description": "Replace every occurrence of old_string, not just a single one (default false)"
				}
			},
			"required": ["path", "old_string", "new_string"]
		}`),
	)
}

// applyEdit replaces old with new in content and returns the new content and the number of replacements.
//
// Notes:
//   - old must be non-empty and appear in content: once, or any number of times if all is true. Otherwise it is an
//     error that says how many matches there were, never what the file holds.
//   - Matches are exact bytes, left to right, non-overlapping (strings.Count and strings.Replace agree on that).
//   - A zero-match error adds a hint when the file has CRLF line endings and old has LF ones, the usual reason a
//     multi-line old_string misses.
//   - The size of the result is computed before it is built and refused above maxBytes, so a replace_all with a long
//     new can't use unbounded memory.
func applyEdit(content, old, new string, all bool, maxBytes int) (string, int, error) {
	if old == "" {
		return "", 0, fmt.Errorf("old_string must not be empty")
	}
	n := strings.Count(content, old)
	switch {
	case n == 0:
		msg := "old_string was not found in the file; read the file again and copy the text exactly (whitespace included)"
		if strings.Contains(old, "\n") && !strings.Contains(old, "\r\n") && strings.Contains(content, strings.ReplaceAll(old, "\n", "\r\n")) {
			msg = "old_string was not found, but it matches with CRLF line endings; the file uses \\r\\n, so use \\r\\n between lines in old_string"
		}
		return "", 0, fmt.Errorf("%s", msg)
	case n > 1 && !all:
		return "", 0, fmt.Errorf("old_string was found %d times; add surrounding lines to make it unique, or set replace_all to replace every one", n)
	}
	if !all {
		n = 1
	}
	if size := len(content) + n*(len(new)-len(old)); size > maxBytes {
		return "", 0, fmt.Errorf("the edited file would be %d bytes; the most edit_file writes is %d bytes", size, maxBytes)
	}
	return strings.Replace(content, old, new, n), n, nil
}

// lockFor returns the mutex for a file, so the read-modify-write of two edits to the same file can't interleave.
//
// Notes:
//   - The key is the resolved path relative to rootDir, lowercased (macOS and Windows ignore case, as in isWithin).
//   - Mutexes are never removed: one small value per file edited in a run.
func (t *EditFileTool) lockFor(rel string) *sync.Mutex {
	m, _ := t.locks.LoadOrStore(strings.ToLower(rel), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// CallTool edits the file and reports how many replacements it made.
//
// Notes:
//   - Order of defences: argument and size checks, resolvePathForWrite (workspace, deny list and write-deny checks),
//     openRoot, openExisting (regular file, same file as checked, opened read-write WITHOUT O_TRUNC). Nothing is
//     changed until the new content is built in memory.
//   - The file must exist; a missing one is an error pointing at write_file. A file over maxBytes, or with a NUL byte,
//     is refused, so a binary file is never rewritten.
//   - [agent] The read-modify-write has a window: another process can change the file after it is read. Just before
//     the write the file is stat'ed again and the edit is refused if its size or modification time changed. Locks
//     can't stop another process, so a very small window remains (accepted; an atomic temp-file rename would close it
//     at the cost of a new inode and mode on every edit).
//   - Edits of the same file by this tool are serialized (lockFor); an edit and a write_file of one file are not.
//   - The write is in place (truncate, then write), like write_file, so the mode is kept. Downside: a failure part-way
//     (disk full) leaves a truncated file.
//   - The result is "edited x (N replacement(s), M bytes)". Errors name only the requested path and never print
//     file content.
func (t *EditFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path       string  `json:"path"`
		OldString  string  `json:"old_string"`
		NewString  *string `json:"new_string"` // a pointer so "" (delete the text) is allowed but a missing one is not
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	switch {
	case params.Path == "":
		return "", fmt.Errorf("missing required argument: path")
	case params.OldString == "":
		return "", fmt.Errorf("old_string must not be empty (to create a file, use write_file)")
	case params.NewString == nil:
		return "", fmt.Errorf("missing required argument: new_string")
	case params.OldString == *params.NewString:
		return "", fmt.Errorf("old_string and new_string are the same; nothing to change")
	case len(params.OldString) > t.maxBytes || len(*params.NewString) > t.maxBytes:
		return "", fmt.Errorf("old_string and new_string are limited to %d bytes each", t.maxBytes)
	case strings.IndexByte(params.OldString, 0) >= 0 || strings.IndexByte(*params.NewString, 0) >= 0:
		return "", fmt.Errorf("old_string and new_string must not contain a NUL byte; edit_file edits text only")
	}
	newString := *params.NewString
	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start editing
		return "", err
	}
	rel, want, err := t.ws.resolvePathForWrite(params.Path)
	if err != nil {
		return "", missingFileHint(err, params.Path)
	}
	mu := t.lockFor(rel)
	mu.Lock()
	defer mu.Unlock()

	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, before, err := t.ws.openExisting(root, params.Path, rel, want, os.O_RDWR)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if before.Size() > int64(t.maxBytes) {
		return "", fmt.Errorf("%s is %d bytes; edit_file edits files up to %d bytes", params.Path, before.Size(), t.maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(t.maxBytes)+1))
	if err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	if len(data) > t.maxBytes {
		return "", fmt.Errorf("%s is more than %d bytes; edit_file edits files up to that size", params.Path, t.maxBytes)
	}
	content := string(data)
	if strings.IndexByte(content, 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file (it contains a NUL byte); edit_file edits text only", params.Path)
	}
	edited, n, err := applyEdit(content, params.OldString, newString, params.ReplaceAll, t.maxBytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", params.Path, err)
	}

	if t.afterRead != nil {
		t.afterRead()
	}
	// refuse if the file changed since it was read; the new content is already built, so only I/O can fail after this
	now, err := f.Stat()
	if err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	if now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		return "", fmt.Errorf("%s changed while it was being edited; read it again and retry", params.Path)
	}
	if err := t.ws.stillAtPath(root, params.Path, rel, f); err != nil {
		return "", err
	}
	if err := f.Truncate(0); err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	if _, err := f.WriteAt([]byte(edited), 0); err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	if err := f.Close(); err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	noun := "replacements"
	if n == 1 {
		noun = "replacement"
	}
	return fmt.Sprintf("edited %s (%d %s, %d bytes)", filepath.ToSlash(rel), n, noun, len(edited)), nil
}

// missingFileHint adds "use write_file to create it" to a not-found error from resolvePathForWrite.
//
// Notes:
//   - Any other error is returned as is.
func missingFileHint(err error, requested string) error {
	if errors.Is(err, fs.ErrNotExist) { // errMsgHidePath keeps the cause wrapped
		return fmt.Errorf("%w (edit_file only edits existing files; use write_file to create %s)", err, requested)
	}
	return err
}
