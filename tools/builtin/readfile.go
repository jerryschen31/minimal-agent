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

	"github.com/jerryschen31/minimal-agent/model"
)

// In the agent kernel, we define a built-in tool that can read contents of text files, capped at ReadFileMaxBytes.
const ReadFileMaxBytes = 64 * 1024

// deniedNames are file or directory names (matched case-insensitively against every path component)
// that read_file refuses even inside the allowed directory, because they usually hold credentials.
// ".env.example" is the one exception to the ".env.*" pattern.
var deniedNames = []string{".env", ".env.*", "*.pem", "*.key", "id_rsa*", ".ssh"}

// ReadFileTool reads text files, but only inside root (the configured workDir and its subdirectories)
// and never a file on the deny list. Absolute paths, "..", and symlinks are all resolved first, so none of
// them can reach outside root.
type ReadFileTool struct {
	root string // absolute path of the allowed directory, with symlinks resolved
}

// NewReadFileTool creates a read_file tool confined to workDir. workDir must be an existing directory.
func NewReadFileTool(workDir string) (*ReadFileTool, error) {
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
	return &ReadFileTool{root: root}, nil
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

func (*ReadFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"read_file",
		"Reads the contents of a UTF-8 text file inside the working directory or its subdirectories. Files larger than 64 KB are truncated, with a note at the end.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The path to the file to read"
				}
			},
			"required": ["path"]
		}`),
	)
}

func (t *ReadFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	// decode args into a small struct with a Path field; bad JSON returns an error. Empty path also returns an error
	var params struct {
		Path string `json:"path"`
	}
	// json.Marshal = json.dumps() in Python => dumps JSON var into byte string; json.Unmarshal = json.loads() in Python => loads JSON byte string into JSON var
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("missing required argument: path")
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
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// LimitReader is used to cap how much of the file is read into memory, preventing huge files from being fully loaded.
	// Reading one byte past the cap is how we can tell that the file was cut off with the comparison below.
	// A directory opens fine but fails here with "is a directory".
	data, err := io.ReadAll(io.LimitReader(f, ReadFileMaxBytes+1))
	if err != nil {
		return "", err
	}
	// a NUL byte means binary; returning it would just be noise to the model
	if bytes.IndexByte(data, 0) != -1 {
		return "", fmt.Errorf("%s looks like a binary file, not text", params.Path)
	}
	if len(data) > ReadFileMaxBytes {
		// add a note clearly indicating the file output was truncated since the file exceeded the maximum allowed size
		return string(data[:ReadFileMaxBytes]) + "\n[truncated: file is larger than 64 KB]", nil
	}
	return string(data), nil
}
