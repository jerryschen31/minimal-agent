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

// newListTool builds a list_files tool on a workspace for workDir with traceDir and deny, failing the test on error.
func newListTool(t *testing.T, workDir, traceDir string, deny DenyList) *ListFilesTool {
	t.Helper()
	tool, err := NewListFilesTool(newWorkspace(t, workDir, traceDir, deny))
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
	tool, err := NewListFilesTool(nil)
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
