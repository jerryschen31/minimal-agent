package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/jerryschen31/minimal-agent/model"
)

// CreateDirectoryTool creates directories inside the workspace (rootDir, minus deny-listed, write-denied and trace
// paths).
type CreateDirectoryTool struct {
	ws *Workspace // where it may create (shared with the other file tools)
}

// NewCreateDirectoryTool creates a create_directory tool that writes only inside ws (NewWorkspace).
//
// Notes:
//   - A nil ws is an error.
func NewCreateDirectoryTool(ws *Workspace) (*CreateDirectoryTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("create_directory needs a workspace")
	}
	return &CreateDirectoryTool{ws: ws}, nil
}

// GetToolDefinition returns the definition of the create_directory tool.
//
// Notes:
//   - The description says missing parents are created and that an existing directory is not an error, so the model
//     doesn't check first.
func (t *CreateDirectoryTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"create_directory",
		"Creates a directory inside the working directory, including any missing parent directories. "+
			"Succeeds without changing anything if the directory already exists. Use it before write_file when the folder is new.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The directory to create, e.g. \"docs/notes\""
				}
			},
			"required": ["path"]
		}`),
	)
}

// CallTool creates the directory (and any missing parents) and says whether it was new.
//
// Notes:
//   - Order of defences: resolveNewPath (workspace and write-deny checks) then openRoot, whose MkdirAll refuses ".."
//     and symlink escapes at the moment of the write.
//   - An existing directory is "already exists", not an error (idempotent); an existing file at the path, or as a
//     parent, is an error.
//   - Errors name only the requested path, never the absolute one.
func (t *CreateDirectoryTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("missing required argument: path")
	}
	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start writing
		return "", err
	}
	rel, err := t.ws.resolveNewPath(params.Path)
	if err != nil {
		return "", err
	}
	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()

	shown := filepath.ToSlash(rel)
	switch info, err := root.Stat(rel); {
	case err == nil && info.IsDir():
		return fmt.Sprintf("directory %s already exists", shown), nil
	case err == nil:
		return "", fmt.Errorf("%s already exists and is a file, not a directory", params.Path)
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return "", errMsgHidePath(params.Path, err)
	}
	return fmt.Sprintf("created directory %s", shown), nil
}
