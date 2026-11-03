package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
)

// maxWriteBytes is the most content one write_file call writes. [agent] write_file replaces the whole file, so this
// is also the largest file it can leave behind; it is not a limit on total disk use. Equal to the default
// readFileMaxBytes, so anything written can be read back in one call. A constant, not a config key: the model's own
// output limit is far lower, so this only stops a runaway payload.
const maxWriteBytes = 1 << 20 // 1 MB

// WriteFileTool creates a text file, or replaces one, inside the workspace (rootDir, minus deny-listed, write-denied
// and trace paths).
type WriteFileTool struct {
	ws       *Workspace // where it may write (shared with the other file tools)
	maxBytes int        // most content one call writes (maxWriteBytes; a field so tests can use a small one)
}

// NewWriteFileTool creates a write_file tool that writes only inside ws (NewWorkspace).
//
// Notes:
//   - A nil ws is an error.
func NewWriteFileTool(ws *Workspace) (*WriteFileTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("write_file needs a workspace")
	}
	return &WriteFileTool{ws: ws, maxBytes: maxWriteBytes}, nil
}

// GetToolDefinition returns the definition of the write_file tool.
//
// Notes:
//   - The description states the size cap, that the parent directory must exist, and that an existing file is replaced
//     whole, so the model doesn't expect appending.
func (t *WriteFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"write_file",
		fmt.Sprintf("Writes a UTF-8 text file inside the working directory: creates it, or replaces its whole contents if it exists. "+
			"The parent directory must already exist (use create_directory first). At most %d KB per call.", t.maxBytes/1024),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The path of the file to write, e.g. \"docs/notes.md\""
				},
				"content": {
					"type": "string",
					"description": "The complete new contents of the file"
				}
			},
			"required": ["path", "content"]
		}`),
	)
}

// CallTool writes the file and says whether it was created or overwritten.
//
// Notes:
//   - Order of defences: size and NUL checks, resolveNewPath (workspace and write-deny checks), openRoot (os.Root
//     refuses ".." and symlink escapes at the moment of the write), then the file is opened and checked on its
//     descriptor before anything is written.
//   - The parent directory must exist; a missing one is an error telling the model to call create_directory.
//   - A new file is created with O_EXCL, so a file that appears after the check is not silently replaced. An existing
//     file is opened WITHOUT O_TRUNC, checked to be a regular file and the same one the check saw (os.SameFile), and
//     only then truncated. Opening with O_NONBLOCK keeps a named pipe from blocking.
//   - Content with a NUL byte is refused, like read_file refuses a file with one: this tool writes text.
//   - Downside: the write is not atomic. A failure part-way (disk full) leaves a truncated file; writing a temp file
//     and renaming would fix that at the cost of a new inode and lost permissions on overwrite.
//   - The result is "created x (N bytes)" or "overwrote x (N bytes)"; a new file gets mode 0644, an overwritten one
//     keeps its mode.
func (t *WriteFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path    string  `json:"path"`
		Content *string `json:"content"` // a pointer so "" (an empty file) is allowed but a missing content is not
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
	content := *params.Content
	if len(content) > t.maxBytes {
		return "", fmt.Errorf("content is %d bytes; the most one write_file call writes is %d bytes", len(content), t.maxBytes)
	}
	if strings.IndexByte(content, 0) >= 0 {
		return "", fmt.Errorf("content contains a NUL byte; write_file writes text only")
	}
	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start writing
		return "", err
	}
	rel, err := t.ws.resolveNewPath(params.Path)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", fmt.Errorf("%s is the working directory, not a file", params.Path)
	}
	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()

	parent := filepath.Dir(rel)
	switch pinfo, err := root.Stat(parent); {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("the directory %s doesn't exist; create it with create_directory first", filepath.ToSlash(parent))
	case err != nil:
		return "", errMsgHidePath(params.Path, err)
	case !pinfo.IsDir():
		return "", fmt.Errorf("%s: a parent in the path is a file, not a directory", params.Path)
	}

	f, verb, err := t.openForWrite(root, params.Path, rel)
	if err != nil {
		return "", err
	}
	n, err := f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	return fmt.Sprintf("%s %s (%d bytes)", verb, filepath.ToSlash(rel), n), nil
}

// openForWrite opens rel (relative to rootDir) through root for writing and returns the file, positioned at the
// start and empty, with "created" or "overwrote".
//
// Notes:
//   - requested names the file in errors. The caller closes the file.
//   - Missing: created with O_EXCL (0644). Regular file: opened without O_TRUNC, checked on its descriptor against the
//     Lstat taken here, then truncated. Anything else (a directory, a symlink swapped in, a pipe, a socket, a device)
//     is refused.
//   - [agent] The Lstat is taken here, not in resolveNewPath, because resolveNewPath returns no identity. A swap
//     between resolveNewPath and this Lstat shows up as a symlink (refused) or as another regular file at the same
//     already-approved name (harmless); a swap after the Lstat fails the SameFile check.
func (t *WriteFileTool) openForWrite(root *os.Root, requested, rel string) (*os.File, string, error) {
	want, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return nil, "", fmt.Errorf("%s was created while it was being written; try again", requested)
		}
		if err != nil {
			return nil, "", errMsgHidePath(requested, err)
		}
		return f, "created", nil
	case err != nil:
		return nil, "", errMsgHidePath(requested, err)
	}
	f, _, err := t.ws.openExisting(root, requested, rel, want, os.O_WRONLY) // no O_TRUNC until the checks pass
	if err != nil {
		return nil, "", err // our own message, or an os.Root error that names only the relative path
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, "", errMsgHidePath(requested, err)
	}
	return f, "overwrote", nil
}
