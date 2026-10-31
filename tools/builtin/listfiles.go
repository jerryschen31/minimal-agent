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

// maxListEntries caps the lines one list_files call returns, so listing a huge tree can't flood the model's context.
// (maxVisits, from config maxSearchFiles, separately caps how many entries a call looks at.)
const maxListEntries = 1000

// ListFilesTool lists the files and directories in one directory of the workspace (optionally the whole tree below
// it). Deny-listed names and the trace directory are hidden, and symlinks are listed but never followed.
type ListFilesTool struct {
	ws        *Workspace // where it may list (shared with the other file tools)
	maxVisits int        // most entries one call looks at (config maxSearchFiles), shown or not; bounds a recursive walk whose pattern rarely matches
	ignore    IgnoreList // paths left out (config ignorePaths); an ignored directory is shown, marked, but not walked or listed
}

// NewListFilesTool creates a list_files tool that lists only inside ws (NewWorkspace). maxVisits (config
// maxSearchFiles) is the most entries one call looks at and must be positive. ignore (NewIgnoreList, config
// ignorePaths) is what it leaves out; the zero value leaves out nothing.
func NewListFilesTool(ws *Workspace, maxVisits int, ignore IgnoreList) (*ListFilesTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("list_files needs a workspace")
	}
	if maxVisits <= 0 {
		return nil, fmt.Errorf("list_files max search files must be positive, got %d", maxVisits)
	}
	return &ListFilesTool{ws: ws, maxVisits: maxVisits, ignore: ignore}, nil
}

// GetToolDefinition returns the definition of the list_files tool
func (t *ListFilesTool) GetToolDefinition() model.ToolDef {
	ignored := ""
	if s := t.ignore.String(); s != "" {
		ignored = "Paths matching the ignore list (" + s + "; .gitignore-style, relative to the working directory) are off limits: ignored files are left out, and an ignored directory is shown as \"dir/ (ignored)\" but its contents can't be listed. "
	}
	return model.NewToolDef(
		"list_files",
		fmt.Sprintf("Lists the files and directories in a directory inside the working directory, as paths relative to the working directory "+
			"(directories end with /, files show their size). Set recursive to list everything below it, and pattern to keep only matching names. "+
			"Sensitive files (such as .env and keys) and the agent's trace directory are never shown, and symlinks are listed but not followed. "+
			"%sReturns at most %d entries per call, and looks at no more than %d.", ignored, maxListEntries, t.maxVisits),
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
	if t.ignore.Ignores(rel, true) {
		return "", fmt.Errorf("cannot list %s (ignored path: it matches ignorePaths)", params.Path)
	}

	var lines []string
	note, visits := "", 0 // note says why the walk stopped early, if it did (only one limit can stop it)
	// fs.WalkDir visits entries in name order (so the output is the same every time) and never follows symlinks
	err = fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // Ctrl+C stops a long walk
		}
		if err != nil {
			if p == start {
				return err // the directory itself can't be read
			}
			// WalkDir calls again with err when a directory's contents can't be read; its line, if shown, was the last
			// one added, so mark it rather than let it look empty
			if d != nil && d.IsDir() && len(lines) > 0 && lines[len(lines)-1] == filepath.FromSlash(p)+"/" {
				lines[len(lines)-1] += " (not readable)"
			}
			return nil // skip an entry or subdirectory that can't be read
		}
		if p == start {
			return nil // don't list the directory itself
		}
		if visits == t.maxVisits {
			note = fmt.Sprintf("[stopped after looking at %d entries (the maxSearchFiles limit); list a subdirectory to see the rest]", t.maxVisits)
			return fs.SkipAll
		}
		visits++
		// The walk never follows symlinks, so p joined onto rootDir is already the real path, as check expects
		if _, err := t.ws.check(p, filepath.Join(t.ws.rootDir, filepath.FromSlash(p))); err != nil {
			if d.IsDir() {
				return fs.SkipDir // hidden directory (e.g. .ssh): its contents would be hidden anyway (their path contains it); this skips reading them
			}
			return nil
		}
		ignored := t.ignore.Ignores(p, d.IsDir()) // p is relative to workDir, as ignore patterns are
		if ignored && !d.IsDir() {
			return nil // an ignored file (e.g. *.pyc) is left out
		}
		if params.Pattern == "" || matchName(params.Pattern, d.Name()) {
			if len(lines) == maxListEntries { // there is at least one more entry than the cap
				note = fmt.Sprintf("[truncated: showed %d entries; list a subdirectory or give a pattern]", maxListEntries)
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
		if ignored { // an ignored directory (e.g. .git/) is shown, marked, but never walked
			if len(lines) > 0 && lines[len(lines)-1] == filepath.FromSlash(p)+"/" {
				lines[len(lines)-1] += " (ignored)"
			}
			return fs.SkipDir
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

	out := strings.Join(lines, "\n")
	if len(lines) == 0 {
		// [agent] when the visit limit cut the walk short, "no match" is only true of what was looked at
		out = fmt.Sprintf("no entries in %s", params.Path)
		if params.Pattern != "" {
			out = fmt.Sprintf("no names in %s match %q", params.Path, params.Pattern)
		}
		if note != "" {
			out += fmt.Sprintf(" (only the first %d entries were looked at)", t.maxVisits)
		}
	}
	if note != "" {
		out += "\n" + note
	}
	return out, nil
}

// matchName reports whether name matches pattern (already checked to be valid).
func matchName(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)
	return ok
}
