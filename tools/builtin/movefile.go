package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jerryschen31/minimal-agent/model"
)

// MoveFileTool moves (renames) one regular file to a new or existing path inside the workspace.
type MoveFileTool struct {
	ws *Workspace // where it may move files (shared with the other file tools)
	// afterResolve, if not nil, runs after both paths are approved and before anything is opened; tests use it to swap a
	// directory or file at that moment, which nothing else can stage deterministically
	afterResolve func()
}

// NewMoveFileTool creates a move_file tool that works only inside ws (NewWorkspace).
//
// Notes:
//   - A nil ws is an error.
func NewMoveFileTool(ws *Workspace) (*MoveFileTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("move_file needs a workspace")
	}
	return &MoveFileTool{ws: ws}, nil
}

// GetToolDefinition returns the definition of the move_file tool.
//
// Notes:
//   - The description states that it moves or renames a file, that the destination is the full new path whose folder
//     must exist, and that an existing destination file is replaced.
func (t *MoveFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"move_file",
		"Moves or renames one file inside the working directory. The destination is the full new file path (not a folder), "+
			"its folder must already exist (use create_directory first), and an existing file there is replaced. Files only, not folders.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"source": {
					"type": "string",
					"description": "The path of the existing file to move"
				},
				"destination": {
					"type": "string",
					"description": "The new path of the file, e.g. \"archive/notes.md\""
				}
			},
			"required": ["source", "destination"]
		}`),
	)
}

// renameError turns an error from os.Root.Rename into one for the model.
//
// Notes:
//   - EXDEV (the destination is on another filesystem, e.g. a mount inside the working directory) says to copy then
//     delete instead; a rename can't cross filesystems and this tool does not fall back to a copy.
//   - Rename's errors are *os.LinkError, which names both paths; only the cause is kept, with the requested names.
func renameError(source, destination string, err error) error {
	if errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("%s and %s are on different filesystems, so the file can't be moved; use copy_file and then remove the original yourself", source, destination)
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return fmt.Errorf("moving %s to %s: %w", source, destination, le.Err)
	}
	return pathErrorOnly(destination, err)
}

// CallTool renames the file and reports whether it replaced a file.
//
// Notes:
//   - Order of defences: resolvePathForWrite on the source (it must exist and may be changed: not in .git, not the
//     config file, not deny-listed) and resolveNewPath on the destination (write-deny, deny list), so a move onto .env
//     or into .git is refused. Then openRoot, and openDir on BOTH parents, which refuses a directory swapped since
//     the approval (for example for a relative symlink to .git), then a last check that the source is still the file
//     that was approved, then os.Root.Rename.
//   - [agent] It is a rename (rename(2)): the same inode under a new name, atomic, any size, mode and mtime kept. The
//     rename itself goes through the paths, since os.Root can't rename between two directory Roots; the openDir checks
//     just before it leave a very small window (accepted, as for the other tools).
//   - A source with more than one name (a hard link) is refused, checked on the file just before the rename: a rename
//     would keep a protected file (.env, .git/config) under the new, readable name. Skipped where the system gives no
//     link count (Windows; DECISIONS.md § PR #38 review).
//   - Files only: a directory is refused, because moving one could take files out from under an anchored deny pattern
//     (renaming `secrets` to `pub` would expose `secrets/*.env`).
//   - An existing destination file is replaced (the ask prompt covers it); a destination that is a directory, not a
//     regular file, or the same file (another name of the source's inode, where rename does nothing) is an error.
//     Renaming over a hard link replaces only that name, so it can't change a protected file.
//   - The destination's parent must exist. A move across filesystems fails with a message pointing at copy_file.
//   - The result is "moved a -> b (N bytes)", with ", overwrote existing" when it replaced a file.
func (t *MoveFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Source == "" {
		return "", fmt.Errorf("missing required argument: source")
	}
	if params.Destination == "" {
		return "", fmt.Errorf("missing required argument: destination")
	}
	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start moving
		return "", err
	}
	src, srcInfo, err := t.ws.resolvePathForWrite(params.Source)
	if err != nil {
		return "", err
	}
	switch {
	case srcInfo.IsDir():
		return "", fmt.Errorf("%s is a directory; move_file moves files only", params.Source)
	case !srcInfo.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file (it is a %s)", params.Source, fileKind(srcInfo.Mode()))
	}
	dst, err := t.ws.resolveNewPath(params.Destination)
	if err != nil {
		return "", err
	}
	if dst.rel == "." {
		return "", fmt.Errorf("%s is the working directory, not a file path", params.Destination)
	}
	if dst.rel == src.rel {
		return "", fmt.Errorf("source and destination are the same file")
	}
	if t.afterResolve != nil {
		t.afterResolve()
	}
	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()

	srcDir, _, err := t.ws.openDir(root, params.Source, src) // only to check that the source's directory is the approved one
	if err != nil {
		return "", err
	}
	srcDir.Close()
	dstDir, name, err := t.ws.openDir(root, params.Destination, dst)
	if err != nil {
		return "", err
	}
	defer dstDir.Close()
	if filepath.Dir(name) != "." { // the nearest existing directory is above the destination's parent, so the parent is missing
		return "", fmt.Errorf("the directory %s doesn't exist; create it with create_directory first", filepath.ToSlash(filepath.Dir(dst.rel)))
	}

	overwrote := false
	switch di, err := dstDir.Lstat(name); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", errMsgHidePath(params.Destination, err)
	case di.IsDir():
		return "", fmt.Errorf("%s is a directory, not a file path", params.Destination)
	case !di.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file (it is a %s)", params.Destination, fileKind(di.Mode()))
	case os.SameFile(di, srcInfo):
		return "", fmt.Errorf("source and destination are the same file")
	default:
		overwrote = true
	}
	cur, err := root.Lstat(src.rel)
	if err != nil || !os.SameFile(cur, srcInfo) {
		return "", fmt.Errorf("%s was replaced or moved while it was being moved; read it again and retry", params.Source)
	}
	// [agent] A rename keeps the file, so a source that is a hard link to a protected file (.env, .git/config) would
	// reappear under the new, readable name. Checked on the file as it is now, just before the rename, like the other
	// write tools. Where the system gives no link count (Windows) this is skipped; see DECISIONS.md § PR #38 review.
	if n, known := linkCount(cur); known && n > 1 {
		return "", fmt.Errorf("%s has %d names (hard links), so it may also be a protected file under another name; move_file won't move it", params.Source, n)
	}
	if err := root.Rename(src.rel, dst.rel); err != nil {
		return "", renameError(params.Source, params.Destination, err)
	}
	extra := ""
	if overwrote {
		extra = ", overwrote existing"
	}
	return fmt.Sprintf("moved %s -> %s (%d bytes%s)", filepath.ToSlash(src.rel), filepath.ToSlash(dst.rel), srcInfo.Size(), extra), nil
}
