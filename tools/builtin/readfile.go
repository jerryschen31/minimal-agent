package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/jerryschen31/minimal-agent/model"
)

// ReadFileTool reads text files only within the workspace (i.e., within rootDir, excluding deny-listed files and the trace directory if it's within rootDir's path)
type ReadFileTool struct {
	ws       *Workspace // where it may read (shared with the other file tools)
	maxBytes int        // most one call returns; this bounds how much of the model's context a call can use. A bigger file is read in pieces with offset and limit
}

// NewReadFileTool creates a read_file tool that reads only inside ws (NewWorkspace).
//
// Notes:
//   - maxBytes is the most one call returns (config readFileMaxBytes) and must be positive.
//   - A nil ws is an error.
func NewReadFileTool(ws *Workspace, maxBytes int) (*ReadFileTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("read_file needs a workspace")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("read_file max bytes must be positive, got %d", maxBytes)
	}
	return &ReadFileTool{ws: ws, maxBytes: maxBytes}, nil
}

// fileKind names a non-regular file type for the error shown to the model.
//
// Notes:
//   - Anything that is not a pipe, socket or device is reported as "special file".
func fileKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeNamedPipe != 0:
		return "named pipe"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeCharDevice != 0:
		return "character device"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}

// GetToolDefinition returns the definition of the read_file tool.
//
// Notes:
//   - The description states the per-call cap (maxBytes in KB), so the model knows to read large files in pieces.
func (t *ReadFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"read_file",
		fmt.Sprintf("Reads the contents of a UTF-8 text file inside the working directory or its subdirectories. Returns at most %d KB per call. "+
			"If the file is longer, the output ends with a note giving the offset to pass to the next call, so a large file can be read in pieces.", t.maxBytes/1024),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The path to the file to read"
				},
				"offset": {
					"type": "integer",
					"minimum": 0,
					"description": "Byte position to start reading from (default 0). Use the offset given in a previous truncation note to continue."
				},
				"limit": {
					"type": "integer",
					"minimum": 1,
					"description": "Maximum number of bytes to return (default and maximum are the per-call cap)"
				}
			},
			"required": ["path"]
		}`),
	)
}

// CallTool executes the read_file tool with the given arguments and context. It returns the file contents, or an error.
//
// Notes:
//   - Order of defences: resolvePath (workspace check) then openRoot (os.Root, a second line of defence) then openFile
//     (regular file, same file as checked).
//   - A limit of 0 means "not given" and a limit above maxBytes is lowered to it. A file with a NUL byte is refused as
//     binary.
//   - The piece starts and ends on character boundaries, so the next piece (at the offset in the truncation note)
//     neither repeats nor loses bytes; a limit smaller than one character is an error rather than an endless
//     "continue at the same offset".
//   - A truncated result ends with a note giving the offset to pass to the next call.
func (t *ReadFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	// decode args into a small struct with a Path field; bad JSON returns an error. Empty path also returns an error
	var params struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
	}
	// json.Marshal = json.dumps() in Python => dumps JSON var into byte string; json.Unmarshal = json.loads() in Python => loads JSON byte string into JSON var
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("missing required argument: path")
	}
	if params.Offset < 0 {
		return "", fmt.Errorf("offset must not be negative, got %d", params.Offset)
	}
	if params.Limit < 0 {
		return "", fmt.Errorf("limit must not be negative, got %d", params.Limit)
	}
	limit := params.Limit
	if limit == 0 || limit > int64(t.maxBytes) { // 0 = not given
		limit = int64(t.maxBytes)
	}

	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start reading
		return "", err
	}
	rel, want, err := t.ws.resolvePath(params.Path)
	if err != nil {
		return "", err // e.g. "lstat go.mod: no such file or directory" or "... is outside the allowed directory"
	}
	// os.Root is a second line of defence: it refuses ".." and symlink escapes at open time, so a
	// symlink swapped in after resolve() still cannot leave the workspace's rootDir
	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()
	// openFile refuses a directory, a named pipe (without blocking on it) and a file swapped in after the check
	f, info, err := t.ws.openFile(root, params.Path, rel, want)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if params.Offset > info.Size() {
		return "", fmt.Errorf("offset %d is past the end of the file (%d bytes)", params.Offset, info.Size())
	}
	if _, err := f.Seek(params.Offset, io.SeekStart); err != nil {
		return "", err
	}

	// LimitReader is used to cap how much of the file is read into memory, preventing huge files from being fully loaded.
	// Reading one byte past the limit is how we can tell that there is more file after this piece.
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	// a NUL byte means binary; returning it would just be noise to the model
	if bytes.IndexByte(data, 0) != -1 {
		return "", fmt.Errorf("%s looks like a binary file, not text", params.Path)
	}

	start := params.Offset
	more := int64(len(data)) > limit
	if more {
		data = data[:limit]
	}
	// A piece is cut at a byte position, which can fall in the middle of a multi-byte character. Move the start
	// forward past a half character and the end back before one, so the next piece (which begins at the end offset
	// reported in the note) neither repeats nor loses bytes.
	for len(data) > 0 && !utf8.RuneStart(data[0]) {
		data, start = data[1:], start+1
	}
	if more {
		data = trimPartialRune(data)
	}
	// Nothing left after trimming means the limit is smaller than one character. Saying "continue at offset=N" would hand
	// back the same offset forever (and burn the ReAct step budget), so fail with something the model can act on.
	if more && len(data) == 0 {
		return "", fmt.Errorf("limit %d bytes is smaller than the character at offset %d; call read_file again with a larger limit (at least %d)", limit, params.Offset, utf8.UTFMax)
	}
	end := start + int64(len(data))
	if !more {
		return string(bytes.ToValidUTF8(data, nil)), nil
	}
	return string(bytes.ToValidUTF8(data, nil)) +
		fmt.Sprintf("\n[truncated: showed bytes %d-%d of %d; call read_file again with offset=%d to continue]", start, end, info.Size(), end), nil
}

// trimPartialRune drops an incomplete multi-byte character from the end of b.
//
// Notes:
//   - Looks back at most utf8.UTFMax bytes; a complete final character, or no start byte in that range, leaves b
//     unchanged.
func trimPartialRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			return b
		}
	}
	return b
}
