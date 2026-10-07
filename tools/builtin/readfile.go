package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jerryschen31/minimal-agent/model"
)

// In the agent kernel, we define a built-in tool that can read contents of text files, capped at ReadFileMaxBytes.
const ReadFileMaxBytes = 64 * 1024

type ReadFileTool struct{}

func (ReadFileTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"read_file",
		"Reads the contents of a UTF-8 text file. Files larger than 64 KB are truncated, with a note at the end.",
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

func (ReadFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
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

	f, err := os.Open(params.Path)
	if err != nil {
		return "", err // e.g. "open go.mod: no such file or directory" (clear enough for the model)
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
