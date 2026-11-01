package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/jerryschen31/minimal-agent/model"
)

const (
	maxSearchMatches   = 200      // matches shown per call (the page size); the model asks for the next page with skip
	maxMatchLineLen    = 500      // bytes of a matching line shown to the model, taken around the match and cut with "…" (protects context)
	maxSearchLineBytes = 10 << 20 // longest line read (10 MiB); a file is searched only up to a longer line, and the note says so (protects memory)
	binarySniffBytes   = 8 << 10  // a NUL byte in a file's first 8 KiB means binary, and the file is skipped (roughly grep's rule)
	scanStartBytes     = 64 << 10 // the line buffer starts at 64 KiB and grows only for a longer line, up to maxSearchLineBytes
)

// SearchFilesTool finds text in the files below a directory of the workspace, grep-style: every visible text file is
// read line by line to the end, so file size doesn't matter, only line length. It sees what list_files sees: deny-listed
// names, the trace directory and ignored paths are never searched, and symlinks are not followed.
type SearchFilesTool struct {
	ws        *Workspace // where it may search (shared with the other file tools)
	maxVisits int        // most entries one call looks at (config maxSearchFiles), matching or not
	ignore    IgnoreList // paths not entered or searched (config ignorePaths)
}

// NewSearchFilesTool creates a search_files tool that searches only inside ws (NewWorkspace). maxVisits (config
// maxSearchFiles) is the most entries one call looks at and must be positive. ignore (NewIgnoreList, config
// ignorePaths) is what it leaves out; the zero value leaves out nothing.
func NewSearchFilesTool(ws *Workspace, maxVisits int, ignore IgnoreList) (*SearchFilesTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("search_files needs a workspace")
	}
	if maxVisits <= 0 {
		return nil, fmt.Errorf("search_files max search files must be positive, got %d", maxVisits)
	}
	return &SearchFilesTool{ws: ws, maxVisits: maxVisits, ignore: ignore}, nil
}

// GetToolDefinition returns the definition of the search_files tool
func (t *SearchFilesTool) GetToolDefinition() model.ToolDef {
	ignored := ""
	if s := t.ignore.String(); s != "" {
		ignored = "Paths matching the ignore list (" + s + "; .gitignore-style, relative to the working directory) are not searched. "
	}
	return model.NewToolDef(
		"search_files",
		fmt.Sprintf("Searches the text files in a directory inside the working directory (and everything below it), or one file, "+
			"for lines containing query, like grep. Returns one line per match as path:line: text, with paths relative to the working directory. "+
			"Sensitive files (such as .env and keys), the agent's trace directory, binary files and symlinks are never searched. "+
			"%sReturns at most %d matches per call; when there are more, the output says which skip value gets the next ones. "+
			"Looks at no more than %d files and directories per call.", ignored, maxSearchMatches, t.maxVisits),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "The text to find. Plain text by default (characters like ( . * have no special meaning); a regular expression (Go RE2 syntax) when regex is true. Matched within one line"
				},
				"path": {
					"type": "string",
					"description": "The directory to search, with everything below it, or a single file (default: the working directory)"
				},
				"pattern": {
					"type": "string",
					"description": "Only search files whose name matches this pattern, e.g. \"*.go\" (* ? and [...] wildcards, matched against the name only, not the whole path; case-sensitive)"
				},
				"regex": {
					"type": "boolean",
					"description": "Treat query as a regular expression (default false)"
				},
				"ignoreCase": {
					"type": "boolean",
					"description": "Match upper and lower case alike (default false)"
				},
				"skip": {
					"type": "integer",
					"description": "Leave out this many matches first, to get the next page of results (default 0)"
				}
			},
			"required": ["query"]
		}`),
	)
}

// searchResult collects one call's output across files.
type searchResult struct {
	lines      []string // match lines shown on this page
	found      int      // matches found so far, including the ones skipped
	more       bool     // a match after this page exists
	binary     int      // files skipped as binary
	longLine   int      // files searched only up to a line over maxSearchLineBytes
	unreadable int      // files or directories that couldn't be opened or read
}

// errPageFull stops the walk once one match more than the page holds has been found.
var errPageFull = errors.New("page full")

// CallTool searches the tree below path (or the one file at path). Every entry passes the same workspace check as
// read_file, so what can't be read isn't searched either; a hidden or ignored directory isn't looked into.
func (t *SearchFilesTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Query      string `json:"query"`
		Path       string `json:"path"`
		Pattern    string `json:"pattern"`
		Regex      bool   `json:"regex"`
		IgnoreCase bool   `json:"ignoreCase"`
		Skip       int    `json:"skip"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	if params.Path == "" {
		params.Path = "."
	}
	if params.Skip < 0 {
		return "", fmt.Errorf("skip must be 0 or more, got %d", params.Skip)
	}
	if params.Pattern != "" {
		if _, err := path.Match(params.Pattern, ""); err != nil {
			return "", fmt.Errorf("pattern %q: %w", params.Pattern, err)
		}
	}
	// [agent] One matcher for both modes: plain text is a regexp with every special character escaped. Go's regexp is
	// RE2, which runs in time linear in the input, so a regex from the model can't hang the search.
	expr := params.Query
	if !params.Regex {
		expr = regexp.QuoteMeta(expr)
	}
	if params.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("query %q: %w", params.Query, err) // e.g. "error parsing regexp: missing closing )"
	}

	if err := ctx.Err(); err != nil { // the turn was cancelled (e.g. Ctrl+C): don't start searching
		return "", err
	}
	rel, err := t.ws.resolve(params.Path) // refuses outside workDir, a deny-listed path, the trace directory
	if err != nil {
		return "", err
	}
	root, err := t.ws.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()
	fsys := root.FS() // can't leave rootDir; paths use "/" whatever the OS
	start := filepath.ToSlash(rel)
	info, err := fs.Stat(fsys, start)
	if err != nil {
		return "", err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file or directory (it is a %s)", params.Path, fileKind(info.Mode()))
	}
	if t.ignore.Ignores(rel, info.IsDir()) {
		return "", fmt.Errorf("cannot search %s (ignored path: it matches ignorePaths)", params.Path)
	}

	var r searchResult
	note, visits := "", 0 // note says why the walk stopped early, if the visit limit stopped it
	// fs.WalkDir visits entries in name order (so pages are the same every time) and never follows symlinks
	err = fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // Ctrl+C stops a long walk
		}
		if err != nil {
			if p == start {
				return err // the path itself can't be read
			}
			r.unreadable++
			return nil // skip an entry or subdirectory that can't be read
		}
		if p != start { // the start path itself isn't counted, as in list_files
			if visits == t.maxVisits {
				note = fmt.Sprintf("[stopped after looking at %d entries (the maxSearchFiles limit); search a subdirectory to see the rest]", t.maxVisits)
				return fs.SkipAll
			}
			visits++
		}
		// The walk never follows symlinks, so p joined onto rootDir is already the real path, as check expects
		if _, err := t.ws.check(p, filepath.Join(t.ws.rootDir, filepath.FromSlash(p))); err != nil {
			if d.IsDir() {
				return fs.SkipDir // hidden directory (e.g. .ssh): not looked into
			}
			return nil
		}
		if t.ignore.Ignores(p, d.IsDir()) { // p is relative to workDir, as ignore patterns are
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil // keep walking into directories; symlinks, pipes and devices are never opened
		}
		if params.Pattern != "" && !matchName(params.Pattern, d.Name()) {
			return nil
		}
		return t.searchFile(ctx, root, p, re, params.Skip, &r)
	})
	if err != nil && !errors.Is(err, errPageFull) { // a cancel or an unreadable start
		return "", err
	}

	out := strings.Join(r.lines, "\n")
	if len(r.lines) == 0 {
		out = fmt.Sprintf("no matches for %q in %s", params.Query, params.Path)
		if r.found > 0 && note == "" && r.binary == 0 && r.longLine == 0 && r.unreadable == 0 { // all matches were counted
			out = fmt.Sprintf("only %d match(es) for %q in %s; skip=%d is past the end", r.found, params.Query, params.Path, params.Skip)
		}
		if note != "" {
			out += fmt.Sprintf(" (only the first %d entries were looked at)", t.maxVisits)
		}
	}
	if r.more {
		next := params.Skip + len(r.lines)
		out += fmt.Sprintf("\n[showed matches %d-%d; call search_files again with skip=%d for more]", params.Skip+1, next, next)
	}
	// [agent] Say what wasn't searched, so "no matches" is only claimed for what was actually read
	var notSearched []string
	if r.binary > 0 {
		notSearched = append(notSearched, fmt.Sprintf("%d binary file(s)", r.binary))
	}
	if r.longLine > 0 {
		notSearched = append(notSearched, fmt.Sprintf("%d file(s) after a line longer than %d MiB", r.longLine, maxSearchLineBytes>>20))
	}
	if r.unreadable > 0 {
		notSearched = append(notSearched, fmt.Sprintf("%d file(s) or directories that couldn't be read", r.unreadable))
	}
	if len(notSearched) > 0 {
		out += "\n[not searched: " + strings.Join(notSearched, "; ") + "]"
	}
	if note != "" {
		out += "\n" + note
	}
	return out, nil
}

// searchFile reads one file line by line and adds its matches to r. It returns errPageFull when the page is full,
// ctx.Err() if cancelled, and nil otherwise; problems with the file itself are counted in r, never returned.
func (t *SearchFilesTool) searchFile(ctx context.Context, root *os.Root, p string, re *regexp.Regexp, skip int, r *searchResult) error {
	// O_NONBLOCK and the Stat on the opened file, as in read_file: the walk saw a regular file, but it may have been
	// swapped for a named pipe since, and opening a pipe would otherwise block forever
	f, err := root.OpenFile(filepath.FromSlash(p), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		r.unreadable++
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		r.unreadable++
		return nil
	}

	br := bufio.NewReaderSize(f, binarySniffBytes)
	head, err := br.Peek(binarySniffBytes)
	if err != nil && !errors.Is(err, io.EOF) { // EOF only means the file is shorter than binarySniffBytes
		r.unreadable++
		return nil
	}
	if bytes.IndexByte(head, 0) != -1 {
		r.binary++
		return nil
	}

	// [agent] A Scanner holds one line at a time, so memory is bounded by the longest line, not the file size.
	// It drops the "\n" (and a "\r" before it) from each line.
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, scanStartBytes), maxSearchLineBytes+1)
	for n := 1; sc.Scan(); n++ {
		if err := ctx.Err(); err != nil {
			return err // Ctrl+C stops even inside one huge file
		}
		line := sc.Bytes()
		loc := re.FindIndex(line)
		if loc == nil {
			continue
		}
		r.found++
		if r.found <= skip {
			continue // on an earlier page
		}
		if len(r.lines) == maxSearchMatches {
			r.more = true
			return errPageFull
		}
		r.lines = append(r.lines, fmt.Sprintf("%s:%d: %s", filepath.FromSlash(p), n, cutLine(line, loc[0])))
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			r.longLine++
		} else {
			r.unreadable++
		}
	}
	return nil
}

// cutLine returns line, or for a line longer than maxMatchLineLen bytes, the part around the match that begins at
// line[start] (a little before it, the rest after), with "…" where it was cut. Invalid UTF-8 is dropped, which also
// drops a character cut in half at either end.
func cutLine(line []byte, start int) string {
	from, to := 0, len(line)
	if len(line) > maxMatchLineLen {
		// [agent] Show the match even when it is deep inside a long (e.g. minified) line, with some text before it
		from = max(0, min(start-maxMatchLineLen/4, len(line)-maxMatchLineLen))
		to = from + maxMatchLineLen
	}
	s := string(bytes.ToValidUTF8(line[from:to], nil))
	if from > 0 {
		s = "…" + s
	}
	if to < len(line) {
		s += "…"
	}
	return s
}
