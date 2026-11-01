package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// searchArgs are the JSON arguments the model would send for search_files (query, path, pattern, regex, ignoreCase,
// skip); keys left out take the tool's defaults.
type searchArgs map[string]any

// newSearchTool builds a search_files tool on a workspace for workDir with traceDir and deny (ignore list .git/ only,
// maxVisits 10000), failing the test on error.
func newSearchTool(t *testing.T, workDir, traceDir string, deny DenyList) *SearchFilesTool {
	t.Helper()
	tool, err := NewSearchFilesTool(newWorkspace(t, workDir, traceDir, deny), 10000, testIgnore(t))
	if err != nil {
		t.Fatalf("NewSearchFilesTool: %v", err)
	}
	return tool
}

// newSearchToolWith builds a search_files tool on dir (no traceDir) with deny, maxVisits and an ignore list made from
// patterns, failing the test on error.
func newSearchToolWith(t *testing.T, dir string, maxVisits int, deny DenyList, patterns ...string) *SearchFilesTool {
	t.Helper()
	tool, err := NewSearchFilesTool(newWorkspace(t, dir, "", deny), maxVisits, newIgnoreList(t, patterns...))
	if err != nil {
		t.Fatalf("NewSearchFilesTool(maxVisits=%d, ignore %q): %v", maxVisits, patterns, err)
	}
	return tool
}

// callSearch runs the tool the way the agent does: JSON arguments in, text or error out.
func callSearch(tool *SearchFilesTool, args searchArgs) (string, error) {
	b, _ := json.Marshal(args)
	return tool.CallTool(context.Background(), b)
}

// mustSearch is callSearch that fails the test on error.
func mustSearch(t *testing.T, tool *SearchFilesTool, args searchArgs) string {
	t.Helper()
	got, err := callSearch(tool, args)
	if err != nil {
		t.Fatalf("search_files(%v): unexpected error: %v", args, err)
	}
	return got
}

// match is the output line for a match in the file at rel ("/"-separated) on line n.
func match(rel string, n int, text string) string {
	return fmt.Sprintf("%s:%d: %s", filepath.FromSlash(rel), n, text)
}

//*************************************//
// Constructor and definition
//*************************************//

// - Verify NewSearchFilesTool refuses a nil workspace ("needs a workspace") and a zero or negative maxVisits ("must be
// positive"), returning no tool; maxVisits 1 is accepted
func Test_Unit_NewSearchFilesTool_InvalidArgs_ReturnError(t *testing.T) {
	if tool, err := NewSearchFilesTool(nil, 10, IgnoreList{}); err == nil || tool != nil || !strings.Contains(err.Error(), "needs a workspace") {
		t.Errorf("nil workspace: expected (nil, 'needs a workspace' error), got (%v, %v)", tool, err)
	}
	ws := newWorkspace(t, t.TempDir(), "", DenyList{})
	for _, n := range []int{0, -1} {
		if tool, err := NewSearchFilesTool(ws, n, IgnoreList{}); err == nil || tool != nil || !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("maxVisits %d: expected (nil, 'must be positive' error), got (%v, %v)", n, tool, err)
		}
	}
	if tool, err := NewSearchFilesTool(ws, 1, IgnoreList{}); err != nil || tool == nil {
		t.Errorf("maxVisits 1: expected a tool, got (%v, %v)", tool, err)
	}
}

// - Verify the definition is named search_files, its schema is valid JSON with all six properties, and only query is
// required
func Test_Unit_SearchFiles_GetToolDefinition_NameAndSchema(t *testing.T) {
	def := newSearchTool(t, t.TempDir(), "", DenyList{}).GetToolDefinition()
	if def.Function.Name != "search_files" {
		t.Errorf("expected name search_files, got %q", def.Function.Name)
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
	for _, p := range []string{"query", "path", "pattern", "regex", "ignoreCase", "skip"} {
		if _, ok := schema.Properties[p]; !ok {
			t.Errorf("schema is missing property %q", p)
		}
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Errorf(`expected required ["query"], got %v`, schema.Required)
	}
}

// - Verify the description names the ignore list when it is non-empty (and nothing about one when empty), and tells
// the model the page size and the configured visit limit
func Test_Unit_SearchFiles_GetToolDefinition_MentionsIgnoreListAndLimits(t *testing.T) {
	desc := newSearchToolWith(t, t.TempDir(), 1234, DenyList{}, ".Git/", "*.pyc").GetToolDefinition().Function.Desc
	for _, want := range []string{
		"Paths matching the ignore list (.git/, *.pyc; .gitignore-style, relative to the working directory) are not searched",
		fmt.Sprintf("at most %d matches per call", maxSearchMatches),
		"no more than 1234 files and directories",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("expected the description to contain %q, got %q", want, desc)
		}
	}
	desc = newSearchToolWith(t, t.TempDir(), 10, DenyList{}).GetToolDefinition().Function.Desc
	if strings.Contains(desc, "ignore list") {
		t.Errorf("expected no ignore list in the description, got %q", desc)
	}
}

//*************************************//
// Arguments
//*************************************//

// - Verify invalid JSON, a missing or empty query, a negative skip and a malformed pattern are errors with no output
func Test_Unit_SearchFiles_InvalidArgs_ReturnErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("hit"))
	tool := newSearchTool(t, dir, "", DenyList{})

	cases := []struct{ args, want string }{
		{`{not json`, "invalid arguments"},
		{`{"query":5}`, "invalid arguments"},
		{`{}`, "query is required"},
		{`{"query":""}`, "query is required"},
		{`{"query":"hit","skip":-1}`, "skip must be 0 or more, got -1"},
		{`{"query":"hit","pattern":"[x"}`, `pattern "[x"`},
	}
	for _, tc := range cases {
		got, err := tool.CallTool(context.Background(), json.RawMessage(tc.args))
		if err == nil || !strings.Contains(err.Error(), tc.want) || got != "" {
			t.Errorf("args %s: expected (\"\", error containing %q), got (%q, %v)", tc.args, tc.want, got, err)
		}
	}
}

// - Verify an invalid regular expression with regex:true is an error that names the query; the same text in plain
// mode is searched literally
func Test_Unit_SearchFiles_InvalidRegex_ErrorNamesQuery(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("call a(\n"))
	tool := newSearchTool(t, dir, "", DenyList{})

	got, err := callSearch(tool, searchArgs{"query": "a(", "regex": true})
	if err == nil || !strings.Contains(err.Error(), `query "a("`) || got != "" {
		t.Errorf("regex a(: expected an error naming the query, got (%q, %v)", got, err)
	}
	if got := mustSearch(t, tool, searchArgs{"query": "a("}); got != match("a.txt", 1, "call a(") {
		t.Errorf("plain a(: expected the literal match, got %q", got)
	}
}

// - Verify plain-text mode treats regexp characters literally: "Setup(" matches only "Setup(", "a.b" doesn't match
// "axb", "foo*" doesn't match a bare "foo"
func Test_Unit_SearchFiles_PlainText_SpecialCharsLiteral(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("Setup\nSetup(cfg)\naxb\na.b\nfoo\nfoo*bar\n"))
	tool := newSearchTool(t, dir, "", DenyList{})

	for query, want := range map[string]string{
		"Setup(": match("a.txt", 2, "Setup(cfg)"),
		"a.b":    match("a.txt", 4, "a.b"),
		"foo*":   match("a.txt", 6, "foo*bar"),
	} {
		if got := mustSearch(t, tool, searchArgs{"query": query}); got != want {
			t.Errorf("query %q: expected %q, got %q", query, want, got)
		}
	}
}

// - Verify regex mode uses the query as a regular expression ("a.b" matches "axb" too; anchors work)
func Test_Unit_SearchFiles_Regex_Matches(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("axb\na.b\nfunc Main()\n  func inner()\n"))
	tool := newSearchTool(t, dir, "", DenyList{})

	if got, want := mustSearch(t, tool, searchArgs{"query": "a.b", "regex": true}), lines(match("a.txt", 1, "axb"), match("a.txt", 2, "a.b")); got != want {
		t.Errorf("a.b: expected\n%s\ngot\n%s", want, got)
	}
	if got, want := mustSearch(t, tool, searchArgs{"query": `^func \w+\(`, "regex": true}), match("a.txt", 3, "func Main()"); got != want {
		t.Errorf("anchored: expected %q, got %q", want, got)
	}
}

// - Verify ignoreCase matches upper and lower case alike in plain and regex mode, and without it case matters
func Test_Unit_SearchFiles_IgnoreCase_BothModes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("Hello.World\nhello.world\nHELLOXWORLD\n"))
	tool := newSearchTool(t, dir, "", DenyList{})

	if got, want := mustSearch(t, tool, searchArgs{"query": "hello.world"}), match("a.txt", 2, "hello.world"); got != want {
		t.Errorf("case-sensitive: expected %q, got %q", want, got)
	}
	got := mustSearch(t, tool, searchArgs{"query": "hello.world", "ignoreCase": true})
	if want := lines(match("a.txt", 1, "Hello.World"), match("a.txt", 2, "hello.world")); got != want {
		t.Errorf("plain ignoreCase: expected\n%s\ngot\n%s", want, got)
	}
	got = mustSearch(t, tool, searchArgs{"query": "hello.world", "ignoreCase": true, "regex": true})
	if want := lines(match("a.txt", 1, "Hello.World"), match("a.txt", 2, "hello.world"), match("a.txt", 3, "HELLOXWORLD")); got != want {
		t.Errorf("regex ignoreCase: expected\n%s\ngot\n%s", want, got)
	}
}

//*************************************//
// Output format and paths
//*************************************//

// - Verify matches are "path:line: text" with OS-separator paths relative to workDir, files in name order (a nested
// directory between top-level files), correct line numbers, and no trailing \r from CRLF lines
func Test_Unit_SearchFiles_Output_FormatOrderAndLineNumbers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.txt", []byte("no\nhit two\n\nhit four"))
	writeFile(t, dir, "a.txt", []byte("hit one\r\nnope\r\nhit three\r\n"))
	writeFile(t, dir, "a/z.txt", []byte("x\nhit\n"))
	writeFile(t, dir, "c.txt", []byte("nothing here"))

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit"})
	want := lines(
		match("a/z.txt", 2, "hit"),
		match("a.txt", 1, "hit one"),
		match("a.txt", 3, "hit three"),
		match("b.txt", 2, "hit two"),
		match("b.txt", 4, "hit four"),
	)
	if got != want {
		t.Errorf("expected\n%s\ngot\n%q", want, got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("CRLF line kept its \\r: %q", got)
	}
}

// - Verify a missing or empty path searches workDir, the same as ".", and a search with no match names the query and
// the path
func Test_Unit_SearchFiles_DefaultPath_SearchesWorkDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/a.txt", []byte("hit"))
	tool := newSearchTool(t, dir, "", DenyList{})

	want := mustSearch(t, tool, searchArgs{"query": "hit", "path": "."})
	if want != match("sub/a.txt", 1, "hit") {
		t.Fatalf(`path ".": expected one match, got %q`, want)
	}
	for _, args := range []searchArgs{{"query": "hit"}, {"query": "hit", "path": ""}} {
		if got := mustSearch(t, tool, args); got != want {
			t.Errorf("args %v: expected %q, got %q", args, want, got)
		}
	}
	if got, want := mustSearch(t, tool, searchArgs{"query": "absent"}), `no matches for "absent" in .`; got != want {
		t.Errorf("no match: expected %q, got %q", want, got)
	}
}

// - Verify path can be a subdirectory (only its tree is searched; paths stay relative to workDir), a single file, or
// an absolute path inside workDir
func Test_Unit_SearchFiles_Path_SubdirFileAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "top.txt", []byte("hit"))
	writeFile(t, dir, "sub/a.txt", []byte("hit a"))
	writeFile(t, dir, "sub/deep/b.txt", []byte("hit b"))
	tool := newSearchTool(t, dir, "", DenyList{})

	want := lines(match("sub/a.txt", 1, "hit a"), match("sub/deep/b.txt", 1, "hit b"))
	if got := mustSearch(t, tool, searchArgs{"query": "hit", "path": "sub"}); got != want {
		t.Errorf("subdir: expected\n%s\ngot\n%s", want, got)
	}
	if got := mustSearch(t, tool, searchArgs{"query": "hit", "path": filepath.Join(dir, "sub")}); got != want {
		t.Errorf("absolute subdir: expected\n%s\ngot\n%s", want, got)
	}
	if got, want := mustSearch(t, tool, searchArgs{"query": "hit", "path": filepath.Join("sub", "deep", "b.txt")}), match("sub/deep/b.txt", 1, "hit b"); got != want {
		t.Errorf("single file: expected %q, got %q", want, got)
	}
}

// - Verify pattern limits the search to files whose name (not path) matches, in subdirectories too, case-sensitively
func Test_Unit_SearchFiles_Pattern_FiltersByName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", []byte("hit"))
	writeFile(t, dir, "a.txt", []byte("hit"))
	writeFile(t, dir, "B.GO", []byte("hit"))
	writeFile(t, dir, "pkg.go/x.txt", []byte("hit")) // a directory whose name matches: its files still filtered by name
	writeFile(t, dir, "sub/c.go", []byte("hit"))

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit", "pattern": "*.go"})
	if want := lines(match("a.go", 1, "hit"), match("sub/c.go", 1, "hit")); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

//*************************************//
// Refused paths
//*************************************//

// - Verify a search is refused (an error, no output) outside workDir, for deny-listed paths, inside the traceDir and for
// a missing path, and the error never contains the absolute workDir
func Test_Unit_SearchFiles_RefusedPaths_ReturnErrors(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "work")
	writeFile(t, dir, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, ".ssh/config", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "trace/run.jsonl", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, parent, "outside.txt", []byte("FAKE_SECRET_TOKEN_123"))
	tool := newSearchTool(t, dir, filepath.Join(dir, "trace"), DenyList{})

	cases := []struct{ path, want string }{
		{"..", "outside the allowed directory"},
		{filepath.Join("..", "outside.txt"), "outside the allowed directory"},
		{".env", "deny list"},
		{".ssh", "deny list"},
		{filepath.Join(".ssh", "config"), "deny list"},
		{"trace", "trace directory"},
		{filepath.Join("trace", "run.jsonl"), "trace directory"},
		{"missing", "missing"},
	}
	for _, tc := range cases {
		got, err := callSearch(tool, searchArgs{"query": "FAKE", "path": tc.path})
		if got != "" {
			t.Errorf("%s: expected no output on error, got %q", tc.path, got)
		}
		expectErrorHidesWorkDir(t, "search_files("+tc.path+")", err, dir, tc.want)
	}
}

//*************************************//
// Never searched: deny list, traceDir, ignore list
//*************************************//

// - Verify searching "." for a secret never returns it from .env, a nested .env, a key file, inside .ssh/, a config
// denyPaths file or directory, or the trace directory; only the allowed .env.example matches
func Test_Unit_SearchFiles_Secrets_NeverReturned(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		".env", "sub/.env", ".env.local", "key.pem", "id_rsa", ".ssh/config", "secrets/key.txt", "secrets/old/db.txt",
		"config/creds.json", "logs/run.jsonl",
	} {
		writeFile(t, dir, rel, []byte("FAKE_SECRET_TOKEN_123\n"))
	}
	writeFile(t, dir, ".env.example", []byte("FAKE_SECRET_TOKEN_123\n"))
	tool := newSearchTool(t, dir, filepath.Join(dir, "logs"), newDenyList(t, "secrets/", "config/creds.json"))

	for _, args := range []searchArgs{
		{"query": "FAKE_SECRET_TOKEN_123"},
		{"query": ".*", "regex": true},
		{"query": "fake_secret", "ignoreCase": true, "pattern": "*"},
	} {
		got := mustSearch(t, tool, args)
		if want := match(".env.example", 1, "FAKE_SECRET_TOKEN_123"); got != want {
			t.Errorf("args %v: expected only %q, got\n%s", args, want, got)
		}
	}
}

// - Verify files inside an ignored directory and ignored files (*.pyc) are not searched, at any depth, and the default
// test ignore list's .git/ is not searched either
func Test_Unit_SearchFiles_IgnoredPaths_NotSearched(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".git/HEAD", []byte("hit"))
	writeFile(t, dir, "node_modules/pkg/index.js", []byte("hit"))
	writeFile(t, dir, "sub/node_modules/x.js", []byte("hit"))
	writeFile(t, dir, "a.pyc", []byte("hit"))
	writeFile(t, dir, "sub/B.PYC", []byte("hit"))
	writeFile(t, dir, "sub/b.py", []byte("hit"))

	got := mustSearch(t, newSearchToolWith(t, dir, 10000, DenyList{}, ".git/", "node_modules/", "*.pyc"), searchArgs{"query": "hit"})
	if want := match("sub/b.py", 1, "hit"); got != want {
		t.Errorf("expected only %q, got\n%s", want, got)
	}
	// with an empty ignore list the same tree is searched everywhere
	got = mustSearch(t, newSearchToolWith(t, dir, 10000, DenyList{}), searchArgs{"query": "hit"})
	if n := len(strings.Split(got, "\n")); n != 6 {
		t.Errorf("empty ignore list: expected 6 matches, got %d:\n%s", n, got)
	}
}

// - Verify searching a path that is itself ignored (a directory, a directory inside it, or an ignored file) is refused
// with "cannot search <path> (ignored path ...)" and no output
func Test_Unit_SearchFiles_IgnoredPathAsStart_Refused(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "node_modules/pkg/index.js", []byte("hit"))
	writeFile(t, dir, "a.pyc", []byte("hit"))
	tool := newSearchToolWith(t, dir, 10000, DenyList{}, "node_modules/", "*.pyc")

	for _, p := range []string{"node_modules", filepath.Join("node_modules", "pkg"), filepath.Join("node_modules", "pkg", "index.js"), "a.pyc"} {
		got, err := callSearch(tool, searchArgs{"query": "hit", "path": p})
		want := "cannot search " + p + " (ignored path: it matches ignorePaths)"
		if err == nil || got != "" || err.Error() != want {
			t.Errorf("path %q: expected the error %q, got (%q, %v)", p, want, got, err)
		}
	}
}

//*************************************//
// Binary files and long lines
//*************************************//

// - Verify a file with a NUL byte in its first 8 KiB is skipped and counted in the "not searched" note, while a file
// whose first NUL comes after 8 KiB is searched
func Test_Unit_SearchFiles_BinaryFiles_SkippedAndCounted(t *testing.T) {
	dir := t.TempDir()
	early := []byte("hit early\n")
	early = append(early, bytes.Repeat([]byte("x"), binarySniffBytes-1-len(early))...)
	early = append(early, 0) // the NUL is the last byte of the first 8 KiB
	writeFile(t, dir, "a.bin", early)
	late := []byte("hit late\n")
	late = append(late, bytes.Repeat([]byte("x"), binarySniffBytes-len(late))...)
	late = append(late, 0) // the NUL is the first byte after 8 KiB
	writeFile(t, dir, "b.dat", late)

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit"})
	if want := lines(match("b.dat", 1, "hit late"), "[not searched: 1 binary file(s)]"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
	// "no matches" still carries the note, so the model knows a file was left out
	got = mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit early"})
	if want := lines(`no matches for "hit early" in .`, "[not searched: 1 binary file(s)]"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify a line longer than 10 MiB stops that file: the match before it is returned, the match after it is not,
// later files are still searched, and the note says one file was searched only up to a long line
func Test_Unit_SearchFiles_LineOver10MiB_StopsThatFile(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	b.WriteString("hit before\n")
	b.Write(bytes.Repeat([]byte("y"), maxSearchLineBytes+1))
	b.WriteString("\nhit after\n")
	writeFile(t, dir, "a.txt", b.Bytes())
	writeFile(t, dir, "b.txt", []byte("hit next file\n"))

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit"})
	want := lines(match("a.txt", 1, "hit before"), match("b.txt", 1, "hit next file"), "[not searched: 1 file(s) after a line longer than 10 MiB]")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify a file of more than 1 MiB of ordinary lines is searched to the end (a match on its last line is found), and
// a 2 MiB line (longer than the 64 KiB starting buffer) is searched with its match shown
func Test_Unit_SearchFiles_BigFileAndLongLine_SearchedToEnd(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	n := 0
	for b.Len() < 2<<20 {
		b.WriteString(strings.Repeat("filler ", 14) + "\n")
		n++
	}
	b.WriteString("needle at the end\n")
	writeFile(t, dir, "big.txt", b.Bytes())
	writeFile(t, dir, "wide.txt", append(bytes.Repeat([]byte("w"), 2<<20), []byte("needle")...))

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "needle"})
	out := strings.Split(got, "\n")
	if len(out) != 2 || out[0] != match("big.txt", n+1, "needle at the end") {
		t.Fatalf("expected the big file's last line and the wide line, got\n%.300s", got)
	}
	if !strings.HasPrefix(out[1], filepath.FromSlash("wide.txt")+":1: …") || !strings.HasSuffix(out[1], "needle") {
		t.Errorf("wide line: expected a cut line ending in the match, got %.200q", out[1])
	}
}

//*************************************//
// cutLine
//*************************************//

// - Verify a line of at most maxMatchLineLen bytes is returned unchanged, wherever the match is
func Test_Unit_SearchFiles_CutLine_ShortLineUnchanged(t *testing.T) {
	for _, line := range []string{"", "abc", strings.Repeat("a", maxMatchLineLen)} {
		if got := cutLine([]byte(line), len(line)/2); got != line {
			t.Errorf("line of %d bytes: expected it unchanged, got %q", len(line), got)
		}
	}
}

// - Verify a long line is cut to maxMatchLineLen bytes around the match, with "…" on each cut side: a match at the
// start (cut at the end only), in the middle (both sides) and at the end (start only)
func Test_Unit_SearchFiles_CutLine_LongLineAroundMatch(t *testing.T) {
	line := []byte(strings.Repeat("a", 1000) + "MATCH" + strings.Repeat("b", 1000))
	got := cutLine(line, 1000)
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "MATCH") {
		t.Errorf("middle: expected …text with MATCH…, got %q", got)
	}
	if n := len(strings.TrimSuffix(strings.TrimPrefix(got, "…"), "…")); n != maxMatchLineLen {
		t.Errorf("middle: expected %d bytes between the marks, got %d", maxMatchLineLen, n)
	}
	// some text is shown before the match, not only after it
	if i := strings.Index(got, "MATCH"); i < 50 {
		t.Errorf("middle: expected some context before the match, it starts at byte %d: %q", i, got)
	}

	start := []byte("MATCH" + strings.Repeat("b", 1000))
	if got := cutLine(start, 0); !strings.HasPrefix(got, "MATCH") || !strings.HasSuffix(got, "…") || strings.HasPrefix(got, "…") {
		t.Errorf("start: expected MATCH… with no leading mark, got %q", got)
	}
	end := []byte(strings.Repeat("a", 1000) + "MATCH")
	if got := cutLine(end, 1000); !strings.HasSuffix(got, "MATCH") || !strings.HasPrefix(got, "…") || strings.HasSuffix(got, "…") {
		t.Errorf("end: expected …MATCH with no trailing mark, got %q", got)
	}
}

// - Verify a match deep inside a long (minified-style) line is visible in the tool's output
func Test_Unit_SearchFiles_CutLine_DeepMatchVisibleInOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "min.js", []byte(strings.Repeat("var a=1;", 5000)+"secretFunctionName();"+strings.Repeat("b=2;", 100)))

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "secretFunctionName"})
	if !strings.HasPrefix(got, filepath.FromSlash("min.js")+":1: …") || !strings.Contains(got, "secretFunctionName();") || !strings.HasSuffix(got, "…") {
		t.Errorf("expected a cut line showing the match, got %q", got)
	}
	if len(got) > maxMatchLineLen+100 {
		t.Errorf("expected the line cut to about %d bytes, got %d", maxMatchLineLen, len(got))
	}
}

// - Verify cutLine never splits a multi-byte character, whatever the match offset (2-, 3- and 4-byte characters), so the
// result is always valid UTF-8
func Test_Unit_SearchFiles_CutLine_NeverSplitsRune(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} {
		line := []byte(strings.Repeat(r, 1200/len(r)))
		for start := 0; start < len(line); start++ {
			if got := cutLine(line, start); !utf8.ValidString(got) {
				t.Fatalf("%s, start %d: invalid UTF-8 %q", r, start, got)
			}
		}
	}
}

// - Verify invalid UTF-8 in a line is dropped, short or long, so the result is valid UTF-8
func Test_Unit_SearchFiles_CutLine_InvalidUTF8Dropped(t *testing.T) {
	if got := cutLine([]byte{0xff, 'a', 0xfe, 'b'}, 1); got != "ab" {
		t.Errorf(`short: expected "ab", got %q`, got)
	}
	long := append(bytes.Repeat([]byte{0x80, 'x'}, 600), []byte("MATCH")...)
	if got := cutLine(long, 1200); !utf8.ValidString(got) || !strings.Contains(got, "MATCH") {
		t.Errorf("long: expected valid UTF-8 with MATCH, got %q", got)
	}
}

//*************************************//
// Paging (maxSearchMatches, skip)
//*************************************//

// makeMatchFiles creates a.txt with na lines and b.txt with nb lines, each line "m".
func makeMatchFiles(t *testing.T, dir string, na, nb int) {
	t.Helper()
	writeFile(t, dir, "a.txt", []byte(strings.Repeat("m\n", na)))
	writeFile(t, dir, "b.txt", []byte(strings.Repeat("m\n", nb)))
}

// - Verify more than maxSearchMatches matches gives exactly maxSearchMatches lines plus the paging note, and skip=200
// returns the rest, continuing across the file boundary exactly where the first page stopped
func Test_Unit_SearchFiles_Paging_OverPageSize(t *testing.T) {
	dir := t.TempDir()
	makeMatchFiles(t, dir, 150, 100) // 250 matches
	tool := newSearchTool(t, dir, "", DenyList{})

	out := strings.Split(mustSearch(t, tool, searchArgs{"query": "m"}), "\n")
	if len(out) != maxSearchMatches+1 {
		t.Fatalf("expected %d lines (matches + note), got %d", maxSearchMatches+1, len(out))
	}
	if want := "[showed matches 1-200; call search_files again with skip=200 for more]"; out[maxSearchMatches] != want {
		t.Errorf("expected the note %q, got %q", want, out[maxSearchMatches])
	}
	if out[0] != match("a.txt", 1, "m") || out[149] != match("a.txt", 150, "m") || out[199] != match("b.txt", 50, "m") {
		t.Errorf("unexpected first page boundaries: %q, %q, %q", out[0], out[149], out[199])
	}

	out = strings.Split(mustSearch(t, tool, searchArgs{"query": "m", "skip": 200}), "\n")
	if len(out) != 50 || out[0] != match("b.txt", 51, "m") || out[49] != match("b.txt", 100, "m") {
		t.Errorf("second page: expected b.txt:51..100 with no note, got %d lines starting %q", len(out), out[0])
	}
}

// - Verify a skip in the middle of a page shows the following matches and a note with the right range and next skip
func Test_Unit_SearchFiles_Paging_SkipMidway(t *testing.T) {
	dir := t.TempDir()
	makeMatchFiles(t, dir, 300, 0)

	out := strings.Split(mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "m", "skip": 1}), "\n")
	if len(out) != maxSearchMatches+1 || out[0] != match("a.txt", 2, "m") {
		t.Fatalf("expected 200 matches from line 2 and a note, got %d lines starting %q", len(out), out[0])
	}
	if want := "[showed matches 2-201; call search_files again with skip=201 for more]"; out[maxSearchMatches] != want {
		t.Errorf("expected %q, got %q", want, out[maxSearchMatches])
	}
}

// - Verify exactly maxSearchMatches matches are all shown with no paging note
func Test_Unit_SearchFiles_Paging_ExactlyPageSize_NoNote(t *testing.T) {
	dir := t.TempDir()
	makeMatchFiles(t, dir, 120, 80)

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "m"})
	if strings.Contains(got, "[showed") {
		t.Errorf("expected no paging note for exactly %d matches", maxSearchMatches)
	}
	if n := len(strings.Split(got, "\n")); n != maxSearchMatches {
		t.Errorf("expected %d lines, got %d", maxSearchMatches, n)
	}
}

// - Verify a skip at or past the number of matches says how many matches there were and that skip is past the end
func Test_Unit_SearchFiles_Paging_SkipPastAll_NoMatchesAfter(t *testing.T) {
	dir := t.TempDir()
	makeMatchFiles(t, dir, 3, 2)
	tool := newSearchTool(t, dir, "", DenyList{})

	for _, skip := range []int{5, 1000} {
		got := mustSearch(t, tool, searchArgs{"query": "m", "skip": skip})
		if want := fmt.Sprintf(`only 5 match(es) for "m" in .; skip=%d is past the end`, skip); got != want {
			t.Errorf("skip %d: expected %q, got %q", skip, want, got)
		}
	}
	if got, want := mustSearch(t, tool, searchArgs{"query": "m", "skip": 4}), match("b.txt", 2, "m"); got != want {
		t.Errorf("skip 4: expected only the last match %q, got %q", want, got)
	}
}

//*************************************//
// Visit limit (maxVisits, config maxSearchFiles)
//*************************************//

// searchStoppedNote is the last line of a search that hit the visit limit n.
func searchStoppedNote(n int) string {
	return fmt.Sprintf("[stopped after looking at %d entries (the maxSearchFiles limit); search a subdirectory to see the rest]", n)
}

// - Verify a walk over more entries than maxVisits stops there with the stopped note; the start directory counts as
// one entry, so a limit of 3 looks at ".", a.txt and b.txt only
func Test_Unit_SearchFiles_OverVisitLimit_StoppedWithNote(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, dir, name, []byte("hit"))
	}

	got := mustSearch(t, newSearchToolWith(t, dir, 2, DenyList{}), searchArgs{"query": "hit"})
	if want := lines(match("a.txt", 1, "hit"), match("b.txt", 1, "hit"), searchStoppedNote(2)); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
	got = mustSearch(t, newSearchToolWith(t, dir, 3, DenyList{}), searchArgs{"query": "hit"})
	if strings.Contains(got, "[stopped") || len(strings.Split(got, "\n")) != 3 {
		t.Errorf("limit 3 (exactly the entries): expected 3 matches and no note, got\n%s", got)
	}
}

// - Verify a search with no match within the visit limit says only the first N entries were looked at, then the note,
// even though a match exists past the limit
func Test_Unit_SearchFiles_NoMatchWithinVisitLimit_SaysSo(t *testing.T) {
	dir := t.TempDir()
	makeEmptyFiles(t, dir, 4)
	writeFile(t, dir, "z.txt", []byte("hit"))

	got := mustSearch(t, newSearchToolWith(t, dir, 3, DenyList{}), searchArgs{"query": "hit"})
	if want := lines(`no matches for "hit" in . (only the first 3 entries were looked at)`, searchStoppedNote(3)); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

//*************************************//
// Cancellation and concurrency
//*************************************//

// - Verify an already-cancelled context returns context.Canceled and no output
func Test_Unit_SearchFiles_CancelledContext_ReturnsContextError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("hit"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := newSearchTool(t, dir, "", DenyList{}).CallTool(ctx, json.RawMessage(`{"query":"hit"}`))
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("expected (\"\", context.Canceled), got (%q, %v)", got, err)
	}
}

// - Verify a context cancelled while one big file is being read stops the search inside that file with
// context.Canceled and no partial output (the checks before the walk, for ".", and for the file pass; a later per-line
// check fails)
func Test_Unit_SearchFiles_CancelledInsideFile_ReturnsContextError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.txt", []byte(strings.Repeat("hit\n", 1000)))
	ctx := &cancelAfterCtx{Context: context.Background(), after: 10}

	got, err := newSearchTool(t, dir, "", DenyList{}).CallTool(ctx, json.RawMessage(`{"query":"hit"}`))
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("expected (\"\", context.Canceled), got (%.100q, %v)", got, err)
	}
}

// - Verify one tool shared by concurrent calls gives every caller the same result (run with -race)
func Test_Unit_SearchFiles_ConcurrentCalls_SameResult(t *testing.T) {
	dir := t.TempDir()
	makeMatchFiles(t, dir, 30, 30)
	tool := newSearchTool(t, dir, "", DenyList{})
	want := mustSearch(t, tool, searchArgs{"query": "m"})

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			if got, err := callSearch(tool, searchArgs{"query": "m"}); err != nil || got != want {
				errs <- fmt.Sprintf("(%.60q, %v)", got, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent call differed: %s", e)
	}
}

// - Verify searchFile on a file that is already gone counts it as unreadable instead of failing (the walk saw it, then
// it was deleted)
func Test_Unit_SearchFiles_SearchFile_MissingFile_CountedUnreadable(t *testing.T) {
	dir := t.TempDir()
	tool := newSearchTool(t, dir, "", DenyList{})
	root, err := tool.ws.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var r searchResult
	if err := tool.searchFile(context.Background(), root, "gone.txt", nil, 0, &r); err != nil || r.unreadable != 1 {
		t.Errorf("expected (nil, unreadable 1), got (%v, unreadable %d)", err, r.unreadable)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test setup: gone.txt should not exist")
	}
}
