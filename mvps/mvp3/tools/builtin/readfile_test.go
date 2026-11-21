package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTool creates a read_file tool confined to dir, failing the test on error.
func newTool(t *testing.T, dir string) *ReadFileTool {
	t.Helper()
	tool, err := NewReadFileTool(dir)
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

// - Verify that a file of exactly ReadFileMaxBytes is returned in full with no truncation note
// (the +1 byte read is what tells "exactly at the cap" apart from "over the cap")
func Test_ReadFile_ExactlyMaxBytes_NotTruncated(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-exact.txt", bytes.Repeat([]byte("a"), ReadFileMaxBytes))

	got, err := readPath(newTool(t, dir), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != ReadFileMaxBytes {
		t.Errorf("expected %d bytes, got %d", ReadFileMaxBytes, len(got))
	}
	if strings.Contains(got, "[truncated") {
		t.Errorf("expected no truncation note for a file exactly at the cap")
	}
}

// - Verify that a file over ReadFileMaxBytes returns the first ReadFileMaxBytes plus a truncation
// note, so the model knows the file doesn't really end there
func Test_ReadFile_OverMaxBytes_TruncatedWithNote(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "minagent-read-file-test-big.txt", bytes.Repeat([]byte("a"), ReadFileMaxBytes+10))
	const note = "\n[truncated: file is larger than 64 KB]"

	got, err := readPath(newTool(t, dir), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, note) {
		t.Fatalf("expected output to end with the truncation note, got tail %q", got[len(got)-50:])
	}
	if len(got) != ReadFileMaxBytes+len(note) {
		t.Errorf("expected %d content bytes before the note, got %d", ReadFileMaxBytes, len(got)-len(note))
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

	if _, err := NewReadFileTool(filepath.Join(dir, "missing")); err == nil {
		t.Errorf("expected an error for a workDir that does not exist")
	}
	if _, err := NewReadFileTool(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
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
