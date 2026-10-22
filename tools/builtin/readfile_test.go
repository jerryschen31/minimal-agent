package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testMaxBytes is the per-call cap the tests use; it is small so the tests do not need megabyte files.
const testMaxBytes = 4096

// newTool creates a read_file tool confined to dir, failing the test on error.
func newTool(t *testing.T, dir string) *ReadFileTool {
	t.Helper()
	tool, err := NewReadFileTool(dir, testMaxBytes)
	if err != nil {
		t.Fatalf("NewReadFileTool(%q): %v", dir, err)
	}
	return tool
}

// writeFile writes contents to dir/rel (creating parent directories) and returns the absolute path.
func writeFile(t *testing.T, dir, rel string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
	return path
}

// symlink creates link -> target (target is stored exactly as given, so it can be relative).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("creating directory for symlink %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
}

// readFileArgs builds the JSON arguments the model would send for read_file.
func readFileArgs(path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": path})
	return b
}

// readPath runs the tool the way the agent does: JSON arguments in, text or error out.
func readPath(tool *ReadFileTool, path string) (string, error) {
	return tool.CallTool(context.Background(), readFileArgs(path))
}

// - Verify that a small text file is returned in full, unchanged
func Test_ReadFile_SmallFile_ReturnsContents(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-small.txt", []byte("hello"))

	got, err := readPath(newTool(t, dir), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("expected %q, got %q", "hello", got)
	}
}

// - Verify that a file of exactly testMaxBytes is returned in full with no truncation note
// (the +1 byte read is what tells "exactly at the cap" apart from "over the cap")
func Test_ReadFile_ExactlyMaxBytes_NotTruncated(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-exact.txt", bytes.Repeat([]byte("a"), testMaxBytes))

	got, err := readPath(newTool(t, dir), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != testMaxBytes {
		t.Errorf("expected %d bytes, got %d", testMaxBytes, len(got))
	}
	if strings.Contains(got, "[truncated") {
		t.Errorf("expected no truncation note for a file exactly at the cap")
	}
}

// - Verify that a file over testMaxBytes returns the first testMaxBytes plus a truncation
// note, so the model knows the file doesn't really end there
func Test_ReadFile_OverMaxBytes_TruncatedWithNote(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-big.txt", bytes.Repeat([]byte("a"), testMaxBytes+10))
	size := testMaxBytes + 10
	note := fmt.Sprintf("\n[truncated: showed bytes 0-%d of %d; call read_file again with offset=%d to continue]", testMaxBytes, size, testMaxBytes)

	got, err := readPath(newTool(t, dir), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, note) {
		t.Fatalf("expected output to end with the truncation note, got tail %q", got[len(got)-50:])
	}
	if len(got) != testMaxBytes+len(note) {
		t.Errorf("expected %d content bytes before the note, got %d", testMaxBytes, len(got)-len(note))
	}
}

// readPiece calls read_file with offset and limit the way the model would.
func readPiece(tool *ReadFileTool, path string, offset, limit int) (string, error) {
	args, _ := json.Marshal(map[string]any{"path": path, "offset": offset, "limit": limit})
	return tool.CallTool(context.Background(), args)
}

// - Verify that a file bigger than the cap can be read to the end by following the offsets in the truncation notes,
// and that the pieces joined together are the original file (nothing repeated, nothing lost)
func Test_ReadFile_Paging_ReassemblesTheFile(t *testing.T) {
	dir := t.TempDir()
	var want strings.Builder
	for i := 0; want.Len() < testMaxBytes*2+500; i++ {
		fmt.Fprintf(&want, "line %d héllo wörld\n", i) // multi-byte characters, so some cuts land inside one
	}
	path := writeFile(t, dir, "minagent-read-file-test-paged.txt", []byte(want.String()))
	tool := newTool(t, dir)

	var got strings.Builder
	offset, pieces := 0, 0
	for {
		piece, err := readPiece(tool, path, offset, 0)
		if err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		pieces++
		i := strings.LastIndex(piece, "\n[truncated:")
		if i == -1 {
			got.WriteString(piece)
			break
		}
		got.WriteString(piece[:i])
		if _, err := fmt.Sscanf(piece[strings.LastIndex(piece, "offset=")+len("offset="):], "%d", &offset); err != nil {
			t.Fatalf("no offset in note %q", piece[i:])
		}
		if pieces > 10 {
			t.Fatal("paging does not terminate")
		}
	}
	if got.String() != want.String() {
		t.Errorf("reassembled file differs: got %d bytes, want %d", got.Len(), want.Len())
	}
	if pieces != 3 {
		t.Errorf("expected 3 pieces, got %d", pieces)
	}
}

// - Verify an explicit offset and limit return exactly that slice, a limit above the cap is clamped to the cap,
// and a read that ends exactly at the end of the file has no truncation note
func Test_ReadFile_OffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-slice.txt", []byte("0123456789"))
	tool := newTool(t, dir)

	got, err := readPiece(tool, path, 2, 3)
	if err != nil || !strings.HasPrefix(got, "234\n[truncated: showed bytes 2-5 of 10;") || !strings.HasSuffix(got, "offset=5 to continue]") {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = readPiece(tool, path, 7, 3)
	if err != nil || got != "789" {
		t.Errorf("read up to the end: got %q, %v", got, err)
	}
	got, err = readPiece(tool, path, 0, testMaxBytes*10)
	if err != nil || got != "0123456789" {
		t.Errorf("limit above the cap: got %q, %v", got, err)
	}
}

// - Verify an offset in the middle of a multi-byte character starts at the next whole character
func Test_ReadFile_OffsetInsideCharacter(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-utf8.txt", []byte("aébc")) // é is 2 bytes: 1-2
	got, err := readPiece(newTool(t, dir), path, 2, 0)
	if err != nil || got != "bc" {
		t.Errorf("got %q, %v", got, err)
	}
}

// - Verify a limit (or a configured cap) smaller than the next character is an actionable error, never a note that
// says "continue at the offset you are already at", and that a limit big enough for the character makes progress
func Test_ReadFile_LimitSmallerThanACharacter_IsAnError(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-wide.txt", []byte("éx")) // é is 2 bytes
	tool := newTool(t, dir)

	for name, tc := range map[string]struct{ offset, limit int }{
		"limit 1 at the start":       {0, 1},
		"limit 1 inside a character": {1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readPiece(tool, path, tc.offset, tc.limit)
			if err == nil || !strings.Contains(err.Error(), "smaller than the character") || got != "" {
				t.Errorf("got %q, %v; want an error saying the limit is smaller than the character", got, err)
			}
		})
	}

	got, err := readPiece(tool, path, 0, 2)
	if err != nil || !strings.HasPrefix(got, "é\n[truncated: showed bytes 0-2 of 3;") || !strings.HasSuffix(got, "offset=2 to continue]") {
		t.Errorf("limit 2: got %q, %v", got, err)
	}

	tiny, err := NewReadFileTool(dir, 1) // the configured cap is the limit when none is given
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readPiece(tiny, path, 0, 0); err == nil || got != "" {
		t.Errorf("cap 1: got %q, %v; want an error", got, err)
	}
}

// - Verify bad offsets and limits are errors for the model to read
func Test_ReadFile_BadOffsetOrLimit(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-offsets.txt", []byte("hello"))
	tool := newTool(t, dir)
	for name, tc := range map[string]struct {
		offset, limit int
		wantErr       string
	}{
		"negative offset": {-1, 0, "offset must not be negative"},
		"negative limit":  {0, -5, "limit must not be negative"},
		"past the end":    {6, 0, "past the end"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readPiece(tool, path, tc.offset, tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Errorf("got %q, %v; want error containing %q", got, err, tc.wantErr)
			}
		})
	}
	if got, err := readPiece(tool, path, 5, 0); err != nil || got != "" { // exactly at the end is an empty read, not an error
		t.Errorf("offset == size: got %q, %v", got, err)
	}
}

// - Verify that every failure is returned as an error (for the model to read), never a panic
// and never an empty "success"
func Test_ReadFile_InvalidInputs_ReturnErrors(t *testing.T) {
	dir := t.TempDir()
	binary := writeFile(t, dir, "minagent-read-file-test-bin.dat", []byte{'a', 0, 'b'})
	tool := newTool(t, dir)

	cases := map[string]struct {
		args    json.RawMessage
		wantErr string // substring the error message must contain
	}{
		"bad json":     {json.RawMessage(`{"path":`), "invalid arguments"},
		"empty path":   {json.RawMessage(`{}`), "missing required argument: path"},
		"missing file": {readFileArgs(filepath.Join(dir, "nope.txt")), "no such file"},
		"directory":    {readFileArgs(dir), "is a directory"},
		"binary file":  {readFileArgs(binary), "binary file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tool.CallTool(context.Background(), tc.args)
			if err == nil {
				t.Fatalf("expected an error, got nil (output %q)", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
			if got != "" {
				t.Errorf("expected empty output on error, got %q", got)
			}
		})
	}
}

//*************************************//
// workDir confinement and the deny list
//*************************************//

// sandbox lays out a parent directory holding the allowed root ("work") next to directories the tool must
// never reach ("outside", and "work-evil", whose name merely starts with the root's), with secrets planted in
// every place an attack could try. Every secret's content contains "SECRET", so a test can check that nothing
// leaks through either the output or the error text.
type sandbox struct {
	parent string // holds work/, work-evil/ and outside/
	root   string // the allowed directory (work/)
	tool   *ReadFileTool
}

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "work")

	// allowed content, including look-alike names that must NOT be on the deny list
	writeFile(t, root, "a.txt", []byte("ALLOWED-A"))
	writeFile(t, root, "sub/b.txt", []byte("ALLOWED-B"))
	writeFile(t, root, "sub/deeper/c.txt", []byte("ALLOWED-C"))
	writeFile(t, root, ".env.example", []byte("ALLOWED-EXAMPLE"))
	writeFile(t, root, "environment.txt", []byte("ALLOWED-ENVIRONMENT"))
	writeFile(t, root, "monkey.txt", []byte("ALLOWED-MONKEY"))
	writeFile(t, root, "keyboard.txt", []byte("ALLOWED-KEYBOARD"))
	writeFile(t, root, "pem.txt", []byte("ALLOWED-PEM"))

	// secrets inside the root that the deny list must block
	for _, rel := range []string{
		".env", ".env.local", ".env.production", "nested/.env",
		"key.pem", "server.key", "id_rsa", "id_rsa.pub", "id_rsa_backup.txt",
		".ssh/config", ".ssh/id_ed25519", "sub/.ssh/known_hosts",
		"UPPER/.ENV", "UPPER/SERVER.KEY", "UPPER/KEY.PEM", // upper case: case-insensitive filesystems treat these like the lower-case names
	} {
		writeFile(t, root, rel, []byte("SECRET-"+rel))
	}

	// secrets outside the root
	writeFile(t, parent, "outside/secret.txt", []byte("SECRET-OUTSIDE"))
	writeFile(t, parent, "work-evil/secret.txt", []byte("SECRET-SIBLING"))

	// symlinks inside the root: harmless ones, and ones aimed at secrets or at the outside
	symlink(t, "a.txt", filepath.Join(root, "link_in"))                                             // inside -> inside: allowed
	symlink(t, "sub", filepath.Join(root, "link_sub"))                                              // directory link, inside -> inside: allowed
	symlink(t, ".env", filepath.Join(root, "notes.txt"))                                            // innocent name -> denied file
	symlink(t, ".ssh", filepath.Join(root, "sshlink"))                                              // directory link -> denied directory
	symlink(t, "../outside/secret.txt", filepath.Join(root, "link_out"))                            // file link -> outside
	symlink(t, "../outside", filepath.Join(root, "link_out_dir"))                                   // directory link -> outside
	symlink(t, "..", filepath.Join(root, "link_parent"))                                            // link to the parent of the root
	symlink(t, filepath.Join(parent, "outside", "secret.txt"), filepath.Join(root, "link_out_abs")) // absolute link -> outside

	return sandbox{parent: parent, root: root, tool: newTool(t, root)}
}

// - Verify files in the workDir and its subdirectories can be read, by relative path, absolute path, and
// harmless symlink, and that look-alike names (.env.example, environment.txt, monkey.txt, ...) are not blocked
func Test_ReadFile_InsideWorkDir_Allowed(t *testing.T) {
	sb := newSandbox(t)
	cases := map[string]string{ // requested path -> expected content
		"a.txt":                                "ALLOWED-A",
		"./a.txt":                              "ALLOWED-A",
		"sub/b.txt":                            "ALLOWED-B",
		"sub/deeper/c.txt":                     "ALLOWED-C",
		"sub/../a.txt":                         "ALLOWED-A",
		filepath.Join(sb.root, "a.txt"):        "ALLOWED-A",
		filepath.Join(sb.root, "sub", "b.txt"): "ALLOWED-B",
		".env.example":                         "ALLOWED-EXAMPLE",
		"environment.txt":                      "ALLOWED-ENVIRONMENT",
		"monkey.txt":                           "ALLOWED-MONKEY",
		"keyboard.txt":                         "ALLOWED-KEYBOARD",
		"pem.txt":                              "ALLOWED-PEM",
		"link_in":                              "ALLOWED-A",
		"link_sub/b.txt":                       "ALLOWED-B",
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			got, err := readPath(sb.tool, path)
			if err != nil {
				t.Fatalf("expected %q to be readable, got error: %v", path, err)
			}
			if got != want {
				t.Errorf("expected %q, got %q", want, got)
			}
		})
	}
}

// - Verify every deny-list file is refused, and that the refusal leaks nothing: no secret content in the
// output or the error message
func Test_ReadFile_DenyList_Blocked(t *testing.T) {
	sb := newSandbox(t)
	paths := []string{
		".env", ".env.local", ".env.production", "nested/.env",
		"key.pem", "server.key", "id_rsa", "id_rsa.pub", "id_rsa_backup.txt",
		".ssh/config", ".ssh/id_ed25519", "sub/.ssh/known_hosts",
		"UPPER/.ENV", "UPPER/SERVER.KEY", "UPPER/KEY.PEM",
		filepath.Join(sb.root, ".env"), // absolute path
		filepath.Join(sb.root, ".ssh", "config"),
		"sub/../.env",    // reaches the file through ".."
		"notes.txt",      // symlink with an innocent name pointing at .env
		"sshlink/config", // symlink to the .ssh directory
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := readPath(sb.tool, path)
			if err == nil {
				t.Fatalf("expected %q to be blocked, got output %q", path, got)
			}
			if !strings.Contains(err.Error(), "deny list") {
				t.Errorf("expected a deny-list error, got %q", err.Error())
			}
			if got != "" || strings.Contains(err.Error(), "SECRET") {
				t.Errorf("expected nothing leaked, got output %q and error %q", got, err.Error())
			}
		})
	}
}

// - Verify nothing outside the workDir can be read, whether reached with "..", an absolute path, a sibling
// directory that shares the root's name prefix, or a symlink (to a file, a directory, the parent, or via an
// absolute link), and that the refusal leaks nothing
func Test_ReadFile_OutsideWorkDir_Blocked(t *testing.T) {
	sb := newSandbox(t)
	paths := []string{
		"../outside/secret.txt",
		"sub/../../outside/secret.txt",
		filepath.Join(sb.parent, "outside", "secret.txt"),
		filepath.Join(sb.parent, "work-evil", "secret.txt"), // "work-evil" starts with the root's name "work"
		"link_out",
		"link_out_dir/secret.txt",
		"link_parent/outside/secret.txt",
		"link_out_abs",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := readPath(sb.tool, path)
			if err == nil {
				t.Fatalf("expected %q to be blocked, got output %q", path, got)
			}
			if !strings.Contains(err.Error(), "outside the allowed directory") {
				t.Errorf("expected an outside-the-directory error, got %q", err.Error())
			}
			if got != "" || strings.Contains(err.Error(), "SECRET") {
				t.Errorf("expected nothing leaked, got output %q and error %q", got, err.Error())
			}
		})
	}

	t.Run("system file", func(t *testing.T) {
		if _, err := os.Stat("/etc/hosts"); err != nil {
			t.Skip("/etc/hosts does not exist here")
		}
		got, err := readPath(sb.tool, "/etc/hosts")
		if err == nil || got != "" {
			t.Fatalf("expected /etc/hosts to be blocked, got (%q, %v)", got, err)
		}
	})
}

// - Verify the confinement still holds when the root itself is reached through a symlink (the root is
// resolved once, up front, so paths are compared against the real location)
func Test_ReadFile_RootGivenAsSymlink_StillConfined(t *testing.T) {
	sb := newSandbox(t)
	rootLink := filepath.Join(t.TempDir(), "rootlink")
	symlink(t, sb.root, rootLink)
	tool := newTool(t, rootLink)

	if got, err := readPath(tool, "a.txt"); err != nil || got != "ALLOWED-A" {
		t.Errorf("expected a.txt to be readable through the symlinked root, got (%q, %v)", got, err)
	}
	if got, err := readPath(tool, "../outside/secret.txt"); err == nil || got != "" {
		t.Errorf("expected ../outside/secret.txt to be blocked, got (%q, %v)", got, err)
	}
	if got, err := readPath(tool, ".env"); err == nil || got != "" {
		t.Errorf("expected .env to be blocked, got (%q, %v)", got, err)
	}
}

// - Verify the default workDir "." means the process's current directory
func Test_ReadFile_DotWorkDir_IsCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "here.txt", []byte("ALLOWED-HERE"))
	writeFile(t, filepath.Dir(dir), "SECRET-above.txt", []byte("SECRET-ABOVE"))
	t.Chdir(dir)
	tool := newTool(t, ".")

	if got, err := readPath(tool, "here.txt"); err != nil || got != "ALLOWED-HERE" {
		t.Errorf("expected here.txt to be readable, got (%q, %v)", got, err)
	}
	if got, err := readPath(tool, "../SECRET-above.txt"); err == nil || got != "" {
		t.Errorf("expected a file above the current directory to be blocked, got (%q, %v)", got, err)
	}
}

// - Verify NewReadFileTool rejects a workDir that does not exist or is not a directory
func Test_ReadFile_NewReadFileTool_InvalidWorkDir_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, dir, "plain.txt", []byte("x"))

	if _, err := NewReadFileTool(filepath.Join(dir, "missing"), testMaxBytes); err == nil {
		t.Errorf("expected an error for a workDir that does not exist")
	}
	if _, err := NewReadFileTool(file, testMaxBytes); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected a 'not a directory' error for a file, got %v", err)
	}
}

// - Verify the deny-list matcher on its own: sensitive names match at any depth and in any case,
// and the .env.example exception and look-alike names do not
func Test_Unit_IsDenied(t *testing.T) {
	cases := map[string]bool{
		".env": true, ".env.local": true, ".ENV": true, "a/b/.env": true,
		"key.pem": true, "MyKey.PEM": true, "server.key": true,
		"id_rsa": true, "id_rsa.pub": true, "sub/id_rsa_old": true,
		".ssh": true, ".ssh/config": true, "a/.ssh/b/c": true, ".SSH/config": true,
		".agent-trace": true, ".agent-trace/20261021T101500Z-9f3a41c7.jsonl": true, "sub/.AGENT-TRACE/x.jsonl": true, "agent-trace.txt": false,
		".env.example": false, ".envrc": false, "environment.txt": false,
		"monkey.txt": false, "keyboard.txt": false, "pem.txt": false,
		"a.txt": false, "sub/b.txt": false, "ssh/config": false, "src/id_ed25519_notes.txt": false,
	}
	for rel, want := range cases {
		if got := isDenied(rel); got != want {
			t.Errorf("isDenied(%q) = %v, want %v", rel, got, want)
		}
	}
}

// - Verify NewReadFileTool rejects a cap that is zero or negative (a negative cap would panic when slicing)
func Test_ReadFile_NewReadFileTool_NonPositiveCap_ReturnsError(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := NewReadFileTool(t.TempDir(), n); err == nil || !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("cap %d: expected a must-be-positive error, got %v", n, err)
		}
	}
}

// - Verify the cap is the one the tool was built with, not a package constant
func Test_ReadFile_UsesConfiguredCap(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-cap.txt", bytes.Repeat([]byte("a"), 100))
	tool, err := NewReadFileTool(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPath(tool, path)
	if err != nil || !strings.HasPrefix(got, "aaaaaaaaaa\n[truncated: showed bytes 0-10 of 100;") {
		t.Errorf("got %q, %v", got, err)
	}
}
