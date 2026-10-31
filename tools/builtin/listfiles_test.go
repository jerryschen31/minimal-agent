package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// testIgnore is the ignore list the list_files tests use: ".git/" only, so the other tests see every other name.
func testIgnore(t *testing.T) IgnoreList {
	t.Helper()
	l, err := NewIgnoreList([]string{".git/"})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// newListTool builds a list_files tool on a workspace for workDir with traceDir and deny, failing the test on error.
func newListTool(t *testing.T, workDir, traceDir string, deny DenyList) *ListFilesTool {
	t.Helper()
	tool, err := NewListFilesTool(newWorkspace(t, workDir, traceDir, deny), 10000, testIgnore(t))
	if err != nil {
		t.Fatalf("NewListFilesTool: %v", err)
	}
	return tool
}

// listArgs builds the JSON arguments the model would send for list_files.
func listArgs(path, pattern string, recursive bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"path": path, "pattern": pattern, "recursive": recursive})
	return b
}

// callList runs the tool the way the agent does: JSON arguments in, text or error out.
func callList(tool *ListFilesTool, path, pattern string, recursive bool) (string, error) {
	return tool.CallTool(context.Background(), listArgs(path, pattern, recursive))
}

// mustList is callList that fails the test on error.
func mustList(t *testing.T, tool *ListFilesTool, path, pattern string, recursive bool) string {
	t.Helper()
	got, err := callList(tool, path, pattern, recursive)
	if err != nil {
		t.Fatalf("list_files(path=%q, pattern=%q, recursive=%v): unexpected error: %v", path, pattern, recursive, err)
	}
	return got
}

// mkdir creates dir/rel (and its parents).
func mkdir(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
		t.Fatalf("creating directory %s: %v", rel, err)
	}
}

// lines joins expected output lines the way the tool does.
func lines(l ...string) string { return strings.Join(l, "\n") }

//*************************************//
// Constructor and definition
//*************************************//

// - Verify NewListFilesTool refuses a nil workspace with an error that says it needs one
func Test_Unit_NewListFilesTool_NilWorkspace_ReturnsError(t *testing.T) {
	tool, err := NewListFilesTool(nil, 10000, IgnoreList{})
	if err == nil || tool != nil || !strings.Contains(err.Error(), "needs a workspace") {
		t.Errorf("expected (nil, 'needs a workspace' error), got (%v, %v)", tool, err)
	}
}

// - Verify the definition is named list_files and its schema is valid JSON with path, pattern and recursive, none
// of them required
func Test_Unit_ListFiles_GetToolDefinition_NameAndSchema(t *testing.T) {
	def := newListTool(t, t.TempDir(), "", DenyList{}).GetToolDefinition()
	if def.Function.Name != "list_files" {
		t.Errorf("expected name list_files, got %q", def.Function.Name)
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(def.Function.Params, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("expected schema type object, got %q", schema.Type)
	}
	for _, p := range []string{"path", "pattern", "recursive"} {
		if _, ok := schema.Properties[p]; !ok {
			t.Errorf("schema is missing property %q", p)
		}
	}
	if len(schema.Required) != 0 {
		t.Errorf("expected no required properties, got %v", schema.Required)
	}
}

//*************************************//
// Arguments
//*************************************//

// - Verify invalid JSON arguments and a malformed pattern are errors, not a listing
func Test_Unit_ListFiles_InvalidArgs_ReturnErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("x"))
	tool := newListTool(t, dir, "", DenyList{})

	if got, err := tool.CallTool(context.Background(), json.RawMessage(`{not json`)); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("invalid JSON: expected an 'invalid arguments' error, got (%q, %v)", got, err)
	}
	for _, recursive := range []bool{false, true} {
		if got, err := callList(tool, ".", "[x", recursive); err == nil || !strings.Contains(err.Error(), "[x") {
			t.Errorf("pattern [x (recursive=%v): expected an error naming the pattern, got (%q, %v)", recursive, got, err)
		}
	}
}

// - Verify a missing or empty path lists workDir, the same as "."
func Test_Unit_ListFiles_EmptyPath_ListsWorkDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("abc"))
	tool := newListTool(t, dir, "", DenyList{})

	want := mustList(t, tool, ".", "", false)
	if want != "a.txt (3 bytes)" {
		t.Fatalf(`listing ".": expected "a.txt (3 bytes)", got %q`, want)
	}
	for _, args := range []string{`{}`, `{"path":""}`} {
		got, err := tool.CallTool(context.Background(), json.RawMessage(args))
		if err != nil || got != want {
			t.Errorf("args %s: expected %q, got (%q, %v)", args, want, got, err)
		}
	}
	// the default also names the directory in the empty-result message
	empty := newListTool(t, t.TempDir(), "", DenyList{})
	if got, err := empty.CallTool(context.Background(), json.RawMessage(`{}`)); err != nil || got != "no entries in ." {
		t.Errorf(`empty workDir, args {}: expected "no entries in .", got (%q, %v)`, got, err)
	}
}

//*************************************//
// Listing and format
//*************************************//

// - Verify a non-recursive listing shows only direct entries, in name order: directories as "name/", files with
// their size in bytes; nothing inside a subdirectory
func Test_Unit_ListFiles_NonRecursive_DirectEntriesInNameOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "zeta.txt", []byte("12345"))
	writeFile(t, dir, "alpha.txt", []byte(""))
	writeFile(t, dir, "sub/inner.txt", []byte("hidden from a flat listing"))
	mkdir(t, dir, "empty")

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", false)
	want := lines("alpha.txt (0 bytes)", "empty/", "sub/", "zeta.txt (5 bytes)")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify listing a subdirectory shows paths relative to workDir (with the OS separator), not to the subdirectory
func Test_Unit_ListFiles_Subdirectory_PathsRelativeToWorkDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/a.txt", []byte("ab"))
	mkdir(t, dir, "sub/deeper")

	got := mustList(t, newListTool(t, dir, "", DenyList{}), "sub", "", false)
	want := lines(filepath.Join("sub", "a.txt")+" (2 bytes)", filepath.Join("sub", "deeper")+"/")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify an absolute path inside workDir is accepted and lists the same as the relative one
func Test_Unit_ListFiles_AbsolutePathInside_Allowed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/a.txt", []byte("ab"))
	tool := newListTool(t, dir, "", DenyList{})

	if got, want := mustList(t, tool, filepath.Join(dir, "sub"), "", false), mustList(t, tool, "sub", "", false); got != want {
		t.Errorf("absolute path: expected %q, got %q", want, got)
	}
}

// - Verify recursive lists the whole tree, depth first in name order
func Test_Unit_ListFiles_Recursive_WholeTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("a"))
	writeFile(t, dir, "cmd/main.go", []byte("package main"))
	writeFile(t, dir, "cmd/util/u.go", []byte("u"))
	writeFile(t, dir, "z.txt", []byte("zz"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	want := lines(
		"a.txt (1 bytes)",
		"cmd/",
		filepath.Join("cmd", "main.go")+" (12 bytes)",
		filepath.Join("cmd", "util")+"/",
		filepath.Join("cmd", "util", "u.go")+" (1 bytes)",
		"z.txt (2 bytes)",
	)
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify with recursive a pattern filters names but non-matching directories are still walked ("*.go" finds
// cmd/main.go, and the directory cmd itself is not listed)
func Test_Unit_ListFiles_Recursive_PatternWalksNonMatchingDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", []byte("x"))
	writeFile(t, dir, "README.md", []byte("x"))
	writeFile(t, dir, "cmd/main.go", []byte("x"))
	writeFile(t, dir, "cmd/notes.txt", []byte("x"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "*.go", true)
	want := lines(filepath.Join("cmd", "main.go")+" (1 bytes)", "main.go (1 bytes)")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify without recursive a pattern filters only the direct entries (nothing below is searched), and a matching
// directory is listed as a directory
func Test_Unit_ListFiles_NonRecursive_PatternFiltersDirectEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", []byte("x"))
	writeFile(t, dir, "cmd/main.go", []byte("x"))
	mkdir(t, dir, "pkg.go")

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "*.go", false)
	if want := lines("main.go (1 bytes)", "pkg.go/"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify the pattern is matched against the name only (a pattern with a path never matches) and is case-sensitive
func Test_Unit_ListFiles_Pattern_NameOnlyAndCaseSensitive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cmd/main.go", []byte("x"))
	tool := newListTool(t, dir, "", DenyList{})

	for _, pattern := range []string{"*.GO", "cmd/*.go", "cmd/main.go"} {
		got := mustList(t, tool, ".", pattern, true)
		if want := fmt.Sprintf("no names in . match %q", pattern); got != want {
			t.Errorf("pattern %q: expected %q, got %q", pattern, want, got)
		}
	}
	if got := mustList(t, tool, ".", "m?in.[g]o", true); got != filepath.Join("cmd", "main.go")+" (1 bytes)" {
		t.Errorf("wildcard pattern: expected cmd/main.go, got %q", got)
	}
}

//*************************************//
// Hidden entries
//*************************************//

// - Verify deny-listed names never appear, even with recursive: .env, key.pem, a nested .env, and the .ssh directory
// and everything inside it; .env.example and an ordinary notes.txt are shown
func Test_Unit_ListFiles_DenyListed_Hidden(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, ".env.example", []byte("TOKEN="))
	writeFile(t, dir, "key.pem", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, ".ssh/config", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, ".ssh/plain.txt", []byte("x"))
	writeFile(t, dir, "sub/.env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "notes.txt", []byte("ok"))
	tool := newListTool(t, dir, "", DenyList{})

	for _, recursive := range []bool{false, true} {
		got := mustList(t, tool, ".", "", recursive)
		for _, hidden := range []string{".env (", "key.pem", ".ssh", "config", "plain.txt", filepath.Join("sub", ".env")} {
			if strings.Contains(got, hidden) {
				t.Errorf("recursive=%v: %q should be hidden, got\n%s", recursive, hidden, got)
			}
		}
		for _, shown := range []string{".env.example (6 bytes)", "notes.txt (2 bytes)", "sub/"} {
			if !strings.Contains(got, shown) {
				t.Errorf("recursive=%v: expected %q to be listed, got\n%s", recursive, shown, got)
			}
		}
	}
	// a pattern that matches only hidden names lists nothing
	if got := mustList(t, tool, ".", "*", true); strings.Contains(got, "config") || strings.Contains(got, "key.pem") {
		t.Errorf(`pattern "*": hidden names leaked: %q`, got)
	}
}

// - Verify a config extra (NewDenyList) hides matching files and directories; the built-ins stay hidden too
func Test_Unit_ListFiles_DenyListExtras_Hidden(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "creds.json", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "vault/inside.txt", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "notes.txt", []byte("ok"))

	got := mustList(t, newListTool(t, dir, "", newDenyList(t, "creds.json", "vault")), ".", "", true)
	if want := "notes.txt (2 bytes)"; got != want {
		t.Errorf("expected only %q, got\n%s", want, got)
	}
}

// - Verify the configured traceDir (inside workDir, any name) and its contents are hidden, while a sibling is shown
func Test_Unit_ListFiles_TraceDir_Hidden(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "logs/run.jsonl", []byte("fake-trace"))
	writeFile(t, dir, "logs2/keep.txt", []byte("k"))
	tool := newListTool(t, dir, filepath.Join(dir, "logs"), DenyList{})

	got := mustList(t, tool, ".", "", true)
	want := lines("logs2/", filepath.Join("logs2", "keep.txt")+" (1 bytes)")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

//*************************************//
// Refused paths
//*************************************//

// - Verify listing is refused (an error, no listing) outside workDir, for a deny-listed directory, for the traceDir,
// for a missing path, and for a file
func Test_Unit_ListFiles_RefusedPaths_ReturnErrors(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "work")
	writeFile(t, dir, "plain.txt", []byte("x"))
	writeFile(t, dir, ".ssh/config", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "trace/run.jsonl", []byte("fake-trace"))
	writeFile(t, parent, "outside.txt", []byte("FAKE_SECRET_TOKEN_123"))
	other := t.TempDir()
	writeFile(t, other, "elsewhere.txt", []byte("x"))
	tool := newListTool(t, dir, filepath.Join(dir, "trace"), DenyList{})

	cases := []struct{ path, want string }{
		{"..", "outside the allowed directory"},
		{other, "outside the allowed directory"},
		{".ssh", "deny list"},
		{"trace", "trace directory"},
		{"missing", ""},
		{"plain.txt", "is a file, not a directory"},
	}
	for _, tc := range cases {
		got, err := callList(tool, tc.path, "", true)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected an error containing %q, got (%q, %v)", tc.path, tc.want, got, err)
		}
		if got != "" {
			t.Errorf("%s: expected no listing on error, got %q", tc.path, got)
		}
	}
}

//*************************************//
// Empty results
//*************************************//

// - Verify an empty directory says "no entries in <path>", and a pattern that matches nothing says so with the pattern
func Test_Unit_ListFiles_EmptyResults_Messages(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "empty")
	writeFile(t, dir, "full/a.txt", []byte("x"))
	tool := newListTool(t, dir, "", DenyList{})

	if got, want := mustList(t, tool, "empty", "", true), "no entries in empty"; got != want {
		t.Errorf("empty dir: expected %q, got %q", want, got)
	}
	if got, want := mustList(t, tool, "full", "*.go", false), `no names in full match "*.go"`; got != want {
		t.Errorf("no match: expected %q, got %q", want, got)
	}
	// a directory holding only hidden names is "empty" too
	writeFile(t, dir, "secrets/.env", []byte("FAKE_SECRET_TOKEN_123"))
	if got, want := mustList(t, tool, "secrets", "", false), "no entries in secrets"; got != want {
		t.Errorf("only hidden: expected %q, got %q", want, got)
	}
}

//*************************************//
// Truncation
//*************************************//

// makeEmptyFiles creates n empty files f0000..f(n-1) in dir.
func makeEmptyFiles(t *testing.T, dir string, n int) {
	t.Helper()
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// - Verify more than maxListEntries entries gives exactly maxListEntries lines plus a final truncation note
func Test_Unit_ListFiles_OverCap_TruncatedWithNote(t *testing.T) {
	dir := t.TempDir()
	makeEmptyFiles(t, dir, maxListEntries+1)

	out := strings.Split(mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", false), "\n")
	if len(out) != maxListEntries+1 {
		t.Fatalf("expected %d lines (entries + note), got %d", maxListEntries+1, len(out))
	}
	if want := fmt.Sprintf("[truncated: showed %d entries;", maxListEntries); !strings.HasPrefix(out[len(out)-1], want) {
		t.Errorf("expected last line to start with %q, got %q", want, out[len(out)-1])
	}
	if out[maxListEntries-1] != fmt.Sprintf("f%04d (0 bytes)", maxListEntries-1) {
		t.Errorf("expected the last entry to be f%04d, got %q", maxListEntries-1, out[maxListEntries-1])
	}
}

// - Verify exactly maxListEntries entries are all listed with no truncation note
func Test_Unit_ListFiles_ExactlyCap_NotTruncated(t *testing.T) {
	dir := t.TempDir()
	makeEmptyFiles(t, dir, maxListEntries)

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", false)
	if strings.Contains(got, "truncated") {
		t.Errorf("expected no truncation note for exactly %d entries", maxListEntries)
	}
	if n := len(strings.Split(got, "\n")); n != maxListEntries {
		t.Errorf("expected %d lines, got %d", maxListEntries, n)
	}
}

//*************************************//
// Cancellation
//*************************************//

// - Verify an already-cancelled context returns context.Canceled and no listing
func Test_Unit_ListFiles_CancelledContext_ReturnsContextError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := newListTool(t, dir, "", DenyList{}).CallTool(ctx, listArgs(".", "", true))
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("expected (\"\", context.Canceled), got (%q, %v)", got, err)
	}
}

// cancelAfterCtx reports no error for its first `after` Err calls, then context.Canceled: a turn cancelled (Ctrl+C)
// while the walk is already running.
type cancelAfterCtx struct {
	context.Context
	after int64
	calls atomic.Int64
}

func (c *cancelAfterCtx) Err() error {
	if c.calls.Add(1) > c.after {
		return context.Canceled
	}
	return nil
}

// - Verify a context cancelled during the walk stops it with context.Canceled and no partial listing
func Test_Unit_ListFiles_CancelledDuringWalk_ReturnsContextError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("x"))
	writeFile(t, dir, "sub/b.txt", []byte("x"))
	ctx := &cancelAfterCtx{Context: context.Background(), after: 1} // the check before the walk passes

	got, err := newListTool(t, dir, "", DenyList{}).CallTool(ctx, listArgs(".", "", true))
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("expected (\"\", context.Canceled), got (%q, %v)", got, err)
	}
}

//*************************************//
// Visit limit (maxVisits, config maxSearchFiles)
//*************************************//

// newListToolMax builds a list_files tool on dir (no traceDir, built-in deny list) that looks at most maxVisits
// entries per call, failing the test on error.
func newListToolMax(t *testing.T, dir string, maxVisits int) *ListFilesTool {
	t.Helper()
	tool, err := NewListFilesTool(newWorkspace(t, dir, "", DenyList{}), maxVisits, testIgnore(t))
	if err != nil {
		t.Fatalf("NewListFilesTool(maxVisits=%d): %v", maxVisits, err)
	}
	return tool
}

// stoppedNote is the last line of a listing that hit the visit limit n.
func stoppedNote(n int) string {
	return fmt.Sprintf("[stopped after looking at %d entries (the maxSearchFiles limit); list a subdirectory to see the rest]", n)
}

// - Verify NewListFilesTool refuses a zero or negative maxVisits with a "must be positive" error and no tool
func Test_Unit_NewListFilesTool_NonPositiveMaxVisits_ReturnsError(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), "", DenyList{})
	for _, n := range []int{0, -1} {
		tool, err := NewListFilesTool(ws, n, IgnoreList{})
		if err == nil || tool != nil || !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("maxVisits %d: expected (nil, 'must be positive' error), got (%v, %v)", n, tool, err)
		}
	}
	if tool, err := NewListFilesTool(ws, 1, IgnoreList{}); err != nil || tool == nil {
		t.Errorf("maxVisits 1: expected a tool, got (%v, %v)", tool, err)
	}
}

// - Verify the definition's description tells the model the configured visit limit
func Test_Unit_ListFiles_GetToolDefinition_MentionsVisitLimit(t *testing.T) {
	desc := newListToolMax(t, t.TempDir(), 1234).GetToolDefinition().Function.Desc
	if !strings.Contains(desc, "looks at no more than 1234") {
		t.Errorf("expected the description to mention the limit 1234, got %q", desc)
	}
}

// makeVisitTree creates a/1.txt, a/2.txt, b/3.txt in dir: five entries in walk order (a/, a/1.txt, a/2.txt, b/,
// b/3.txt), directories included.
func makeVisitTree(t *testing.T, dir string) {
	t.Helper()
	for _, rel := range []string{"a/1.txt", "a/2.txt", "b/3.txt"} {
		writeFile(t, dir, rel, []byte("x"))
	}
}

// - Verify a recursive walk over more entries than maxVisits stops after maxVisits entries (directories count),
// listing only those, and ends with the stopped note
func Test_Unit_ListFiles_OverVisitLimit_StoppedWithNote(t *testing.T) {
	dir := t.TempDir()
	makeVisitTree(t, dir)
	writeFile(t, dir, "c.txt", []byte("x")) // the sixth entry

	got := mustList(t, newListToolMax(t, dir, 5), ".", "", true)
	want := lines("a/", filepath.Join("a", "1.txt")+" (1 bytes)", filepath.Join("a", "2.txt")+" (1 bytes)", "b/",
		filepath.Join("b", "3.txt")+" (1 bytes)", stoppedNote(5))
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify a walk over exactly maxVisits entries lists them all with no note (the limit is reached, not exceeded)
func Test_Unit_ListFiles_ExactlyVisitLimit_NoNote(t *testing.T) {
	dir := t.TempDir()
	makeVisitTree(t, dir)

	got := mustList(t, newListToolMax(t, dir, 5), ".", "", true)
	if strings.Contains(got, "[stopped") || strings.Contains(got, "[truncated") {
		t.Errorf("expected no note for exactly 5 entries, got\n%s", got)
	}
	if n := len(strings.Split(got, "\n")); n != 5 {
		t.Errorf("expected 5 lines, got %d:\n%s", n, got)
	}
}

// - Verify a pattern that matches nothing within the limit says "no names ... match", that only the first N entries
// were looked at, and then the stopped note, even
// though a matching name exists past the limit (so the model knows the search was cut short, not complete)
func Test_Unit_ListFiles_RarePatternPastVisitLimit_NoMatchThenStopped(t *testing.T) {
	dir := t.TempDir()
	makeEmptyFiles(t, dir, 8)              // f0000..f0007, looked at first
	writeFile(t, dir, "z.go", []byte("x")) // matches, but is the ninth entry

	got := mustList(t, newListToolMax(t, dir, 5), ".", "*.go", true)
	if want := lines(`no names in . match "*.go" (only the first 5 entries were looked at)`, stoppedNote(5)); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify hidden entries count as visits: five deny-listed names looked at first use up a limit of 5, so the
// visible file after them is not listed; a hidden directory counts once (its contents are never walked)
func Test_Unit_ListFiles_HiddenEntriesCountAsVisits(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".env", ".env.local", ".env.prod", "a.pem", "b.key"} {
		writeFile(t, dir, name, []byte("FAKE_SECRET_TOKEN_123"))
	}
	writeFile(t, dir, "z.txt", []byte("x"))

	got := mustList(t, newListToolMax(t, dir, 5), ".", "", true)
	if want := lines("no entries in . (only the first 5 entries were looked at)", stoppedNote(5)); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}

	dir = t.TempDir()
	mkdir(t, dir, ".ssh")
	makeEmptyFiles(t, filepath.Join(dir, ".ssh"), 10) // ten entries inside, never walked
	writeFile(t, dir, "a.txt", []byte("x"))
	got = mustList(t, newListToolMax(t, dir, 2), ".", "", true)
	if want := "a.txt (1 bytes)"; got != want {
		t.Errorf(".ssh should count as one visit: expected %q, got\n%s", want, got)
	}
}

// - Verify only one note is ever printed: with maxVisits == maxListEntries and one entry more, the visit limit is
// checked first, so the listing shows maxListEntries lines and the stopped note, not the truncation note; with a
// limit one higher, the line cap is hit first and only the truncation note is printed
func Test_Unit_ListFiles_LineCapAndVisitLimit_OnlyOneNote(t *testing.T) {
	dir := t.TempDir()
	makeEmptyFiles(t, dir, maxListEntries+1)

	for _, c := range []struct {
		maxVisits int
		want      string
	}{
		{maxListEntries, stoppedNote(maxListEntries)},
		{maxListEntries + 1, fmt.Sprintf("[truncated: showed %d entries; list a subdirectory or give a pattern]", maxListEntries)},
	} {
		out := strings.Split(mustList(t, newListToolMax(t, dir, c.maxVisits), ".", "", false), "\n")
		if len(out) != maxListEntries+1 {
			t.Fatalf("maxVisits %d: expected %d lines (entries + one note), got %d", c.maxVisits, maxListEntries+1, len(out))
		}
		if out[len(out)-1] != c.want {
			t.Errorf("maxVisits %d: expected last line %q, got %q", c.maxVisits, c.want, out[len(out)-1])
		}
		if strings.HasPrefix(out[len(out)-2], "[") {
			t.Errorf("maxVisits %d: expected a single note, got two: %q, %q", c.maxVisits, out[len(out)-2], out[len(out)-1])
		}
	}
}

// - Verify a walk that finishes without being stopped and finds nothing gets no "(only the first ...)" suffix and no
// note, even when it looked at exactly maxVisits entries (the limit was reached, not exceeded)
func Test_Unit_ListFiles_EmptyAtExactlyVisitLimit_NoSuffix(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".env", ".env.local", "a.pem"} {
		writeFile(t, dir, name, []byte("FAKE_SECRET_TOKEN_123"))
	}
	tool := newListToolMax(t, dir, 3)

	if got, want := mustList(t, tool, ".", "", true), "no entries in ."; got != want {
		t.Errorf("no pattern: expected %q, got %q", want, got)
	}
	if got, want := mustList(t, tool, ".", "*.go", true), `no names in . match "*.go"`; got != want {
		t.Errorf("pattern: expected %q, got %q", want, got)
	}
}

//*************************************//
// Not walked (.git)
//*************************************//

// makeGitTree creates a.txt, .git/HEAD, .git/hooks/x.go and z.txt in dir.
func makeGitTree(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, dir, "a.txt", []byte("x"))
	writeFile(t, dir, ".git/HEAD", []byte("ref: refs/heads/fake"))
	writeFile(t, dir, ".git/hooks/x.go", []byte("x"))
	writeFile(t, dir, "z.txt", []byte("x"))
}

// gitSkipped is the line a recursive listing shows for a .git directory at rel.
func gitSkipped(rel string) string {
	return filepath.FromSlash(rel) + "/ (contents not listed; list it directly to see them)"
}

// - Verify a recursive listing shows .git with a "contents not listed" mark but doesn't walk it, while the entries
// around it are listed as usual
func Test_Unit_ListFiles_Recursive_GitDirShownNotWalked(t *testing.T) {
	dir := t.TempDir()
	makeGitTree(t, dir)

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	if want := lines(gitSkipped(".git"), "a.txt (1 bytes)", "z.txt (1 bytes)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify the .git name is matched case-insensitively (.Git is not walked either)
func Test_Unit_ListFiles_Recursive_GitDirCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".Git/HEAD", []byte("ref: refs/heads/fake"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	if want := gitSkipped(".Git"); got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// - Verify a non-recursive listing shows .git as a plain "name/" line, with no mark (it is never walked anyway)
func Test_Unit_ListFiles_NonRecursive_GitDirPlainLine(t *testing.T) {
	dir := t.TempDir()
	makeGitTree(t, dir)

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", false)
	if want := lines(".git/", "a.txt (1 bytes)", "z.txt (1 bytes)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify listing .git directly lists its contents, recursive or not (the start directory is not subject to
// the ignore list); a .git nested below the start directory is still not walked
func Test_Unit_ListFiles_GitDirAsPath_ListsContents(t *testing.T) {
	dir := t.TempDir()
	makeGitTree(t, dir)
	writeFile(t, dir, ".git/modules/.git/inner", []byte("x"))
	tool := newListTool(t, dir, "", DenyList{})

	got := mustList(t, tool, ".git", "", false)
	want := lines(filepath.Join(".git", "HEAD")+" (20 bytes)", filepath.Join(".git", "hooks")+"/", filepath.Join(".git", "modules")+"/")
	if got != want {
		t.Errorf("non-recursive: expected\n%s\ngot\n%s", want, got)
	}
	got = mustList(t, tool, ".git", "", true)
	want = lines(
		filepath.Join(".git", "HEAD")+" (20 bytes)",
		filepath.Join(".git", "hooks")+"/",
		filepath.Join(".git", "hooks", "x.go")+" (1 bytes)",
		filepath.Join(".git", "modules")+"/",
		gitSkipped(".git/modules/.git"),
	)
	if got != want {
		t.Errorf("recursive: expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify a nested sub/.git is also not walked in a recursive listing
func Test_Unit_ListFiles_Recursive_NestedGitDirNotWalked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/.git/HEAD", []byte("ref: refs/heads/fake"))
	writeFile(t, dir, "sub/main.go", []byte("x"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	if want := lines("sub/", gitSkipped("sub/.git"), filepath.Join("sub", "main.go")+" (1 bytes)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify with a pattern that doesn't match .git, neither .git nor anything inside it (.git/hooks/x.go) is listed
func Test_Unit_ListFiles_Recursive_PatternSkipsGitContents(t *testing.T) {
	dir := t.TempDir()
	makeGitTree(t, dir)
	writeFile(t, dir, "cmd/main.go", []byte("x"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "*.go", true)
	if want := filepath.Join("cmd", "main.go") + " (1 bytes)"; got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// - Verify a file (not a directory) named .git, as in a git worktree or submodule, is listed as a file and the entries
// after it are still listed
func Test_Unit_ListFiles_Recursive_GitFileListedNormally(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".git", []byte("gitdir: ../fake"))
	writeFile(t, dir, "a.txt", []byte("x"))
	writeFile(t, dir, "sub/b.txt", []byte("x"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	want := lines(".git (15 bytes)", "a.txt (1 bytes)", "sub/", filepath.Join("sub", "b.txt")+" (1 bytes)")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify entries inside .git don't count as visits: .git holding many files counts once, so a small maxVisits
// still lists the real files after it, with no stopped note
func Test_Unit_ListFiles_Recursive_GitContentsNotVisits(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, ".git")
	makeEmptyFiles(t, filepath.Join(dir, ".git"), 20)
	writeFile(t, dir, "a.txt", []byte("x"))
	writeFile(t, dir, "b.txt", []byte("x"))

	got := mustList(t, newListToolMax(t, dir, 3), ".", "", true)
	if want := lines(gitSkipped(".git"), "a.txt (1 bytes)", "b.txt (1 bytes)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

//*************************************//
// Ignore list (config ignorePaths)
//*************************************//

// newListToolIgnore builds a list_files tool on dir (no traceDir) with deny, maxVisits and an ignore list made from
// patterns, failing the test on error.
func newListToolIgnore(t *testing.T, dir string, maxVisits int, deny DenyList, patterns ...string) *ListFilesTool {
	t.Helper()
	tool, err := NewListFilesTool(newWorkspace(t, dir, "", deny), maxVisits, newIgnoreList(t, patterns...))
	if err != nil {
		t.Fatalf("NewListFilesTool(ignore %q): %v", patterns, err)
	}
	return tool
}

// - Verify an ignored file is left out, at the top and nested, in both a recursive and a non-recursive listing, and
// the other files are still listed
func Test_Unit_ListFiles_IgnoredFile_LeftOut(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.pyc", []byte("x"))
	writeFile(t, dir, "a.py", []byte("x"))
	writeFile(t, dir, "sub/B.PYC", []byte("x"))
	writeFile(t, dir, "sub/b.py", []byte("x"))
	tool := newListToolIgnore(t, dir, 10000, DenyList{}, "*.pyc")

	if got, want := mustList(t, tool, ".", "", false), lines("a.py (1 bytes)", "sub/"); got != want {
		t.Errorf("non-recursive: expected\n%s\ngot\n%s", want, got)
	}
	got := mustList(t, tool, ".", "", true)
	if want := lines("a.py (1 bytes)", "sub/", filepath.Join("sub", "b.py")+" (1 bytes)"); got != want {
		t.Errorf("recursive: expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify an ignored directory is shown as a plain "name/" line when not recursive, and with the "contents not
// listed" mark and not walked when recursive; a file with the same name as a directory-only pattern is still listed
func Test_Unit_ListFiles_IgnoredDir_ShownNotWalked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "node_modules/pkg/index.js", []byte("x"))
	writeFile(t, dir, "sub/node_modules", []byte("x")) // a file, so "node_modules/" doesn't match it
	writeFile(t, dir, "z.js", []byte("x"))
	tool := newListToolIgnore(t, dir, 10000, DenyList{}, "node_modules/")

	if got, want := mustList(t, tool, ".", "", false), lines("node_modules/", "sub/", "z.js (1 bytes)"); got != want {
		t.Errorf("non-recursive: expected\n%s\ngot\n%s", want, got)
	}
	got := mustList(t, tool, ".", "", true)
	want := lines(gitSkipped("node_modules"), "sub/", filepath.Join("sub", "node_modules")+" (1 bytes)", "z.js (1 bytes)")
	if got != want {
		t.Errorf("recursive: expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify with a pattern, an ignored directory is marked only when its own name matches the pattern, and its
// contents are never listed either way
func Test_Unit_ListFiles_IgnoredDir_WithPattern(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "node_modules/x.js", []byte("x"))
	writeFile(t, dir, "src/node_main.js", []byte("x"))
	tool := newListToolIgnore(t, dir, 10000, DenyList{}, "node_modules/")

	got := mustList(t, tool, ".", "node_*", true)
	if want := lines(gitSkipped("node_modules"), filepath.Join("src", "node_main.js")+" (1 bytes)"); got != want {
		t.Errorf("matching pattern: expected\n%s\ngot\n%s", want, got)
	}
	got = mustList(t, tool, ".", "*.js", true)
	if want := filepath.Join("src", "node_main.js") + " (1 bytes)"; got != want {
		t.Errorf("non-matching pattern: expected %q, got %q", want, got)
	}
}

// - Verify listing an ignored directory directly lists its contents (the start directory is never ignored), while
// ignored files inside it are still left out and an ignored directory nested in it is still not walked
func Test_Unit_ListFiles_IgnoredDirAsPath_ListsContentsMinusIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "node_modules/index.js", []byte("x"))
	writeFile(t, dir, "node_modules/cache.pyc", []byte("x"))
	writeFile(t, dir, "node_modules/dep/node_modules/inner.js", []byte("x"))
	tool := newListToolIgnore(t, dir, 10000, DenyList{}, "node_modules/", "*.pyc")

	got := mustList(t, tool, "node_modules", "", true)
	want := lines(
		filepath.Join("node_modules", "dep")+"/",
		gitSkipped("node_modules/dep/node_modules"),
		filepath.Join("node_modules", "index.js")+" (1 bytes)",
	)
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify ignored entries each count as one visit toward maxVisits: three ignored files use up a limit of 3, so the
// file after them is not looked at and the stopped note is printed
func Test_Unit_ListFiles_IgnoredEntriesCountAsVisits(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.pyc", "b.pyc", "c.pyc"} {
		writeFile(t, dir, name, []byte("x"))
	}
	writeFile(t, dir, "z.txt", []byte("x"))

	got := mustList(t, newListToolIgnore(t, dir, 3, DenyList{}, "*.pyc"), ".", "", true)
	if want := lines("no entries in . (only the first 3 entries were looked at)", stoppedNote(3)); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify the deny list wins over the ignore list: a directory that is both deny-listed and ignored is hidden
// entirely, not shown with the "contents not listed" mark
func Test_Unit_ListFiles_IgnoredAndDenied_Hidden(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ssh/id_ed25519", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "build/out.bin", []byte("x"))
	writeFile(t, dir, "a.txt", []byte("x"))
	tool := newListToolIgnore(t, dir, 10000, newDenyList(t, "build"), ".ssh/", "build/")

	for _, recursive := range []bool{false, true} {
		got := mustList(t, tool, ".", "", recursive)
		if got != "a.txt (1 bytes)" {
			t.Errorf("recursive=%v: expected only a.txt, got\n%s", recursive, got)
		}
	}
}

// - Verify an empty ignore list ignores nothing: .git and *.pyc are listed and walked like any other names
func Test_Unit_ListFiles_EmptyIgnoreList_WalksEverything(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".git/HEAD", []byte("x"))
	writeFile(t, dir, "a.pyc", []byte("x"))

	got := mustList(t, newListToolIgnore(t, dir, 10000, DenyList{}), ".", "", true)
	if want := lines(".git/", filepath.Join(".git", "HEAD")+" (1 bytes)", "a.pyc (1 bytes)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify the description names the ignore list (as String() shows it) when it is non-empty, and says nothing about
// an ignore list when it is empty
func Test_Unit_ListFiles_GetToolDefinition_MentionsIgnoreListOnlyWhenSet(t *testing.T) {
	desc := newListToolIgnore(t, t.TempDir(), 10000, DenyList{}, ".Git/", "*.pyc").GetToolDefinition().Function.Desc
	if !strings.Contains(desc, "Names matching the ignore list (.git/, *.pyc;") {
		t.Errorf("expected the description to name the ignore list, got %q", desc)
	}
	desc = newListToolIgnore(t, t.TempDir(), 10000, DenyList{}).GetToolDefinition().Function.Desc
	if strings.Contains(desc, "ignore list") || strings.Contains(desc, "ignored") {
		t.Errorf("expected no ignore list in the description, got %q", desc)
	}
}
