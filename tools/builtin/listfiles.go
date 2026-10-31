package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
)

// maxListEntries caps one list_files call, so listing a huge tree can't flood the model's context.
const maxListEntries = 1000

// ListFilesTool lists the files and directories in one directory of the workspace (optionally the whole tree below
// it). Deny-listed names and the trace directory are hidden, and symlinks are listed but never followed.
type ListFilesTool struct {
	ws *Workspace // where it may list (shared with the other file tools)
}

// NewListFilesTool creates a list_files tool that lists only inside ws (NewWorkspace).
func NewListFilesTool(ws *Workspace) (*ListFilesTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("list_files needs a workspace")
	}
	return &ListFilesTool{ws: ws}, nil
}

// GetToolDefinition returns the definition of the list_files tool
func (t *ListFilesTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"list_files",
		fmt.Sprintf("Lists the files and directories in a directory inside the working directory, as paths relative to the working directory "+
			"(directories end with /, files show their size). Set recursive to list everything below it, and pattern to keep only matching names. "+
			"Sensitive files (such as .env and keys) and the agent's trace directory are never shown, and symlinks are listed but not followed. "+
			"Returns at most %d entries per call.", maxListEntries),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The directory to list (default: the working directory)"
				},
				"pattern": {
					"type": "string",
					"description": "Only list names matching this pattern, e.g. \"*.go\" or \"*_test.go\" (* ? and [...] wildcards, matched against the name only, not the whole path; case-sensitive)"
				},
				"recursive": {
					"type": "boolean",
					"description": "List the whole tree below the directory, not only its direct entries (default false)"
				}
			}
		}`),
	)
}

// CallTool lists one directory (or, with recursive, the tree below it). Every entry passes the same workspace check as
// read_file, so what can't be read isn't listed either; a hidden directory isn't looked into.
func (t *ListFilesTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path      string `json:"path"`
		Pattern   string `json:"pattern"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		params.Path = "."
	}
	if params.Pattern != "" {
		if _, err := path.Match(params.Pattern, ""); err != nil {
			return "", fmt.Errorf("pattern %q: %w", params.Pattern, err)
		}
	}

	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start listing
		return "", err
	}
	rel, err := t.ws.resolve(params.Path) // refuses outside workDir, a deny-listed directory, the trace directory
	if err != nil {
		return "", err
	}
	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()
	// [agent] root.FS() is an fs.FS that can't leave rootDir: a second line of defence behind the check on each entry.
	// fs.FS paths always use "/", whatever the OS.
	fsys := root.FS()
	start := filepath.ToSlash(rel)
	info, err := fs.Stat(fsys, start)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file, not a directory", params.Path)
	}

	var lines []string
	truncated := false
	// fs.WalkDir visits entries in name order (so the output is the same every time) and never follows symlinks
	err = fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // Ctrl+C stops a long walk
		}
		if err != nil {
			if p == start {
				return err // the directory itself can't be read
			}
			return nil // skip an entry or subdirectory that can't be read
		}
		if p == start {
			return nil // don't list the directory itself
		}
		// The walk never follows symlinks, so p joined onto rootDir is already the real path, as check expects
		if _, err := t.ws.check(p, filepath.Join(t.ws.rootDir, filepath.FromSlash(p))); err != nil {
			if d.IsDir() {
				return fs.SkipDir // hidden directory (e.g. .ssh): its contents would be hidden anyway (their path contains it); this skips reading them
			}
			return nil
		}
		if params.Pattern == "" || matchName(params.Pattern, d.Name()) {
			if len(lines) == maxListEntries {
				truncated = true // there is at least one more entry than the cap
				return fs.SkipAll
			}
			shown := filepath.FromSlash(p)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				lines = append(lines, shown+" -> (symlink)") // the target is never shown or followed
			case d.IsDir():
				lines = append(lines, shown+"/")
			default:
				fi, err := d.Info()
				if err != nil {
					return nil // removed during the walk
				}
				lines = append(lines, fmt.Sprintf("%s (%d bytes)", shown, fi.Size()))
			}
		}
		// A directory that doesn't match the pattern is still walked when recursive ("*.go" finds cmd/main.go)
		if d.IsDir() && !params.Recursive {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil { // fs.SkipDir and fs.SkipAll never come back from WalkDir; this is a cancel or an unreadable start
		return "", err
	}

	if len(lines) == 0 {
		if params.Pattern != "" {
			return fmt.Sprintf("no names in %s match %q", params.Path, params.Pattern), nil
		}
		return fmt.Sprintf("no entries in %s", params.Path), nil
	}
	out := strings.Join(lines, "\n")
	if truncated {
		out += fmt.Sprintf("\n[truncated: showed %d entries; list a subdirectory or give a pattern]", maxListEntries)
	}
	return out, nil
}

// matchName reports whether name matches pattern (already checked to be valid).
func matchName(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)
	return ok
}
