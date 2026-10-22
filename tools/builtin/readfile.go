package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/jerryschen31/minimal-agent/model"
)

// deniedNames are file or directory names (matched case-insensitively against every path component)
// that read_file refuses even inside the allowed directory, because they usually hold credentials.
// ".agent-trace" is the default trace directory: the agent must not be able to read its own audit log.
// Only that default name is listed, so a traceDir set to another name inside workDir is not protected.
// ".env.example" is the one exception to the ".env.*" pattern.
var deniedNames = []string{".env", ".env.*", "*.pem", "*.key", "id_rsa*", ".ssh", ".agent-trace"}

// ReadFileTool reads text files, but only inside root (the configured workDir and its subdirectories)
// and never a file on the deny list. Absolute paths, "..", and symlinks are all resolved first, so none of
// them can reach outside root.
type ReadFileTool struct {
	root     string // absolute path of the allowed directory, with symlinks resolved
	maxBytes int    // most one call returns; this bounds how much of the model's context a call can use. A bigger file is read in pieces with offset and limit
}

// NewReadFileTool creates a read_file tool confined to workDir. workDir must be an existing directory.
// maxBytes is the most one call returns (config readFileMaxBytes) and must be positive.
func NewReadFileTool(workDir string, maxBytes int) (*ReadFileTool, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("read_file max bytes must be positive, got %d", maxBytes)
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("workDir %q: %w", workDir, err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workDir %q: %w", workDir, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("workDir %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workDir %q is not a directory", workDir)
	}
	return &ReadFileTool{root: root, maxBytes: maxBytes}, nil
}

// isDenied reports whether any component of rel (a slash- or OS-separated relative path) is on the deny list.
func isDenied(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		name := strings.ToLower(part) // case-insensitive filesystems (macOS, Windows) would otherwise let ".ENV" through
		if name == ".env.example" {
			continue
		}
		for _, pattern := range deniedNames {
			if ok, _ := path.Match(pattern, name); ok {
				return true
			}
		}
	}
	return false
}

// fileKind names a non-regular file type for the error shown to the model.
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

// resolve turns the model's path (relative to root, or absolute) into a path relative to root with every
// symlink followed, or returns an error if it ends up outside root or on the deny list. The deny list is
// checked on the resolved path, so a symlink named notes.txt that points at .env is refused too.
func (t *ReadFileTool) resolve(requested string) (string, error) {
	candidate := requested
	if !filepath.IsAbs(requested) {
		candidate = filepath.Join(t.root, requested)
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(t.root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the allowed directory", requested)
	}
	if isDenied(rel) {
		return "", fmt.Errorf("%s is blocked: it matches the sensitive-file deny list (.env files, *.pem, *.key, id_rsa*, .ssh/)", requested)
	}
	return rel, nil
}

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
	rel, err := t.resolve(params.Path)
	if err != nil {
		return "", err // e.g. "lstat go.mod: no such file or directory" or "... is outside the allowed directory"
	}
	// os.Root is a second line of defence: it refuses ".." and symlink escapes at open time, so a
	// symlink swapped in after resolve() still cannot leave root
	root, err := os.OpenRoot(t.root)
	if err != nil {
		return "", err
	}
	defer root.Close()
	// Open with O_NONBLOCK: opening a named pipe (FIFO) normally waits for a writer, which would hang the whole
	// agent turn (this call is synchronous and cannot be cancelled once inside open). With O_NONBLOCK the open
	// returns at once, and the file type is checked on the opened descriptor below, not on the path, so a
	// regular file swapped for a pipe after resolve() cannot be raced in. O_NONBLOCK does not change reads of
	// regular files.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat() // fstat on the descriptor we actually opened
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a file", params.Path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file (it is a %s)", params.Path, fileKind(info.Mode()))
	}

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
	end := start + int64(len(data))
	if !more {
		return string(bytes.ToValidUTF8(data, nil)), nil
	}
	return string(bytes.ToValidUTF8(data, nil)) +
		fmt.Sprintf("\n[truncated: showed bytes %d-%d of %d; call read_file again with offset=%d to continue]", start, end, info.Size(), end), nil
}

// trimPartialRune drops an incomplete multi-byte character from the end of b.
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
