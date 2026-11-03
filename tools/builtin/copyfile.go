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

	"github.com/jerryschen31/minimal-agent/model"
)

// maxTransferBytes is the largest file copy_file copies. [agent] The bytes go from one file to another and never into
// the model's context, so this is far above the 1 MB that write_file and edit_file hold in memory. It is a backstop
// against one call filling the disk, not a limit on total disk use. Downside: a larger file, such as a big binary,
// can't be copied by this tool. A constant, not a config key, to keep one number to reason about; raise it here.
const maxTransferBytes = 100 << 20 // 100 MB

// copyChunk is how many bytes copyWithContext moves between checks of the context.
const copyChunk = 256 << 10

// CopyFileTool copies one regular file to a new or existing path inside the workspace.
type CopyFileTool struct {
	ws       *Workspace // where it may read and write (shared with the other file tools)
	maxBytes int64      // largest file it copies (maxTransferBytes; a field so tests can use a small one)
	// afterResolve, if not nil, runs after both paths are approved and before anything is opened; tests use it to swap a
	// directory or file at that moment, which nothing else can stage deterministically
	afterResolve func()
	// afterOpenDest, if not nil, runs after the destination is opened (created or emptied) and before the bytes are
	// copied; tests use it to make the copy fail part-way
	afterOpenDest func()
}

// NewCopyFileTool creates a copy_file tool that works only inside ws (NewWorkspace).
//
// Notes:
//   - A nil ws is an error.
func NewCopyFileTool(ws *Workspace) (*CopyFileTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("copy_file needs a workspace")
	}
	return &CopyFileTool{ws: ws, maxBytes: maxTransferBytes}, nil
}

// GetToolDefinition returns the definition of the copy_file tool.
//
// Notes:
//   - The description states that both paths are files, that the destination's folder must exist, that an existing
//     destination is replaced, and the size limit.
func (t *CopyFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"copy_file",
		fmt.Sprintf("Copies one file to a new path inside the working directory. The destination is the full new file path (not a folder), "+
			"its folder must already exist (use create_directory first), and an existing file there is replaced. Files only, up to %d MB.", t.maxBytes>>20),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"source": {
					"type": "string",
					"description": "The path of the existing file to copy"
				},
				"destination": {
					"type": "string",
					"description": "The path of the copy, e.g. \"backup/notes.md\""
				}
			},
			"required": ["source", "destination"]
		}`),
	)
}

// copyWithContext copies src to dst in copyChunk pieces and returns the bytes copied.
//
// Notes:
//   - The context is checked before every piece, so Ctrl+C stops a long copy.
//   - It stops with an error as soon as more than limit bytes have been read, so a file that grows while it is being
//     copied can't pass the size check made before the copy.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	buf := make([]byte, copyChunk)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return total, fmt.Errorf("the file is larger than the %d-byte limit", limit)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// pathErrorOnly hides the absolute path in an *fs.PathError (errMsgHidePath) and returns any other error unchanged.
//
// Notes:
//   - For errors from os.Root calls, which name the path as given, and for our own messages and context errors,
//     which must not be turned into "can't be resolved".
func pathErrorOnly(requested string, err error) error {
	if _, ok := errors.AsType[*fs.PathError](err); ok {
		return errMsgHidePath(requested, err)
	}
	return err
}

// CallTool copies the file and reports the bytes copied and whether it replaced a file.
//
// Notes:
//   - Order of defences: resolvePath on the source (read deny list, trace directory, outside rootDir) and
//     resolveNewPath on the destination (write-deny, deny list), so copying .env to notes.txt (a way to read a secret)
//     and copying into .git are both refused. Then openRoot; the source is opened with openExisting (regular file,
//     the approved one); the destination's parent is opened with openDir (the approved directory); the destination is
//     opened by openForWrite (O_EXCL for a new file; an existing one is checked and truncated only after the checks).
//   - A source or destination with several names (hard links) is refused: the checks are by name, so a link to .env
//     under another name would pass them. This also means the source and destination can't be two names of one file.
//   - Files only (a directory or a pipe is an error); binary files are fine. The destination is the full path, its
//     parent must exist, and it must not be the source.
//   - A new file gets the source's permission bits (before the umask); an overwritten file keeps its own mode.
//   - If the copy fails after a new file was created, that partial file is removed. A failed overwrite can leave a
//     truncated destination (the same accepted downside as write_file).
//   - The result is "copied a -> b (N bytes)", with ", overwrote existing" when it replaced a file.
func (t *CopyFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
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
	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start copying
		return "", err
	}
	srcRel, srcInfo, err := t.ws.resolvePath(params.Source)
	if err != nil {
		return "", err
	}
	dst, err := t.ws.resolveNewPath(params.Destination)
	if err != nil {
		return "", err
	}
	if dst.rel == "." {
		return "", fmt.Errorf("%s is the working directory, not a file path", params.Destination)
	}
	if dst.rel == srcRel {
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

	src, info, err := t.ws.openExisting(root, params.Source, srcRel, srcInfo, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer src.Close()
	if n, known := linkCount(info); known && n > 1 {
		return "", fmt.Errorf("%s has %d names (hard links), so it may also be a protected file under another name; copy_file won't copy it", params.Source, n)
	}
	if info.Size() > t.maxBytes {
		return "", fmt.Errorf("%s is %d bytes; copy_file copies files up to %d bytes", params.Source, info.Size(), t.maxBytes)
	}
	dir, name, err := t.ws.openDir(root, params.Destination, dst) // the directory resolveNewPath approved, not whatever the path names now
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if filepath.Dir(name) != "." { // the nearest existing directory is above the destination's parent, so the parent is missing
		return "", fmt.Errorf("the directory %s doesn't exist; create it with create_directory first", filepath.ToSlash(filepath.Dir(dst.rel)))
	}

	out, verb, err := t.ws.openForWrite(dir, params.Destination, name, info.Mode().Perm())
	if err != nil {
		return "", err
	}
	if t.afterOpenDest != nil {
		t.afterOpenDest()
	}
	n, err := copyWithContext(ctx, out, src, t.maxBytes)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if verb == "created" {
			dir.Remove(name) // don't leave a partial copy behind
		}
		return "", pathErrorOnly(params.Destination, err)
	}
	extra := ""
	if verb == "overwrote" {
		extra = ", overwrote existing"
	}
	return fmt.Sprintf("copied %s -> %s (%d bytes%s)", filepath.ToSlash(srcRel), filepath.ToSlash(dst.rel), n, extra), nil
}
