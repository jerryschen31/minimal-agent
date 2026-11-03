package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWriteTool(t *testing.T, work, traceDir, configFile string, deny DenyList) *WriteFileTool {
	t.Helper()
	ws, err := NewWorkspace(work, traceDir, configFile, deny)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewWriteFileTool(ws)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func writeCall(tool *WriteFileTool, path, content string) (string, error) {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return tool.CallTool(context.Background(), args)
}

func readBack(t *testing.T, work, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(work, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// - Verify a new file is created, then replaced whole on the second call, with the result saying which
func Test_WriteFile_NewThenOverwrite_ResultSaysWhich(t *testing.T) {
	work := t.TempDir()
	tool := newWriteTool(t, work, "", "", DenyList{})
	got, err := writeCall(tool, "a.txt", "first version")
	if err != nil || got != "created a.txt (13 bytes)" {
		t.Fatalf("create: (%q, %v)", got, err)
	}
	got, err = writeCall(tool, "a.txt", "v2")
	if err != nil || got != "overwrote a.txt (2 bytes)" {
		t.Fatalf("overwrite: (%q, %v)", got, err)
	}
	if c := readBack(t, work, "a.txt"); c != "v2" {
		t.Errorf("file = %q, want the old content fully replaced", c)
	}
}

// - Verify empty content creates an empty file, but a missing content argument is an error
func Test_WriteFile_EmptyContent_AllowedMissingContent_Error(t *testing.T) {
	work := t.TempDir()
	tool := newWriteTool(t, work, "", "", DenyList{})
	if got, err := writeCall(tool, "empty.txt", ""); err != nil || got != "created empty.txt (0 bytes)" {
		t.Fatalf("(%q, %v)", got, err)
	}
	for _, args := range []string{`{"path":"x.txt"}`, `{"content":"hi"}`, `{"path":"","content":"hi"}`, `nope`} {
		if _, err := tool.CallTool(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("args %s: want an error", args)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "x.txt")); err == nil {
		t.Error("x.txt created without content")
	}
}

// - Verify a file in an existing subdirectory is written (relative and absolute path), and mode is 0644 for new files
func Test_WriteFile_ExistingSubdirectory_RelativeAndAbsolute(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newWriteTool(t, work, "", "", DenyList{})
	if got, err := writeCall(tool, "sub/a.txt", "x"); err != nil || got != "created sub/a.txt (1 bytes)" {
		t.Fatalf("relative: (%q, %v)", got, err)
	}
	abs := filepath.Join(tool.ws.rootDir, "sub", "b.txt")
	if got, err := writeCall(tool, abs, "y"); err != nil || got != "created sub/b.txt (1 bytes)" {
		t.Fatalf("absolute: (%q, %v)", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(work, "sub", "a.txt")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

// - Verify a missing parent directory is an error that points at create_directory, and nothing is created
func Test_WriteFile_MissingParent_ErrorMentionsCreateDirectory(t *testing.T) {
	work := t.TempDir()
	tool := newWriteTool(t, work, "", "", DenyList{})
	_, err := writeCall(tool, "new/dir/a.txt", "x")
	if err == nil || !strings.Contains(err.Error(), "create_directory") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), work) {
		t.Errorf("error leaks workDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "new")); err == nil {
		t.Error("parent directory was created")
	}
}

// - Verify refusals: a directory or a file as parent, workDir itself, outside workDir, deny-listed, .git, the config
// file (read-only for writes), and the trace directory; none writes anything, none leaks workDir
func Test_WriteFile_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	for _, d := range []string{work, filepath.Join(work, "adir"), filepath.Join(work, ".git"), filepath.Join(work, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, work, "file.txt", []byte("keep"))
	cfg := writeFile(t, work, "agent.json", []byte(`{"keep":true}`))
	tool := newWriteTool(t, work, filepath.Join(work, "logs"), cfg, DenyList{})
	for path, want := range map[string]string{
		"adir":                "is a directory",
		"file.txt/sub.txt":    "not a directory",
		".":                   "working directory",
		"../outside.txt":      "outside the allowed directory",
		".env":                "deny list",
		"key.pem":             "deny list",
		".git/hooks":          "read-only",
		".git/config":         "read-only",
		"agent.json":          "config file",
		cfg:                   "config file",
		"logs/run.jsonl":      "trace directory",
		filepath.Join(parent): "outside the allowed directory",
	} {
		_, err := writeCall(tool, path, "NEW")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("write %q: error = %v, want one containing %q", path, err, want)
			continue
		}
		if !filepath.IsAbs(path) && strings.Contains(err.Error(), work) {
			t.Errorf("write %q: error leaks workDir: %v", path, err)
		}
	}
	if c := readBack(t, work, "file.txt"); c != "keep" {
		t.Errorf("file.txt = %q", c)
	}
	if c := readBack(t, work, "agent.json"); c != `{"keep":true}` {
		t.Errorf("config file was changed: %q", c)
	}
	for _, rel := range []string{".env", "key.pem", ".git/hooks", "logs/run.jsonl"} {
		if _, err := os.Stat(filepath.Join(work, rel)); err == nil {
			t.Errorf("%s was created", rel)
		}
	}
}

// - Verify the config file can still be READ by the file tools' workspace check (it is write-protected only)
func Test_Unit_Workspace_ConfigFile_ReadAllowedWriteRefused(t *testing.T) {
	work := t.TempDir()
	cfg := writeFile(t, work, "agent.json", []byte("{}"))
	ws, err := NewWorkspace(work, "", cfg, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	if rel, _, err := ws.resolvePath("agent.json"); err != nil || rel != "agent.json" {
		t.Errorf("resolvePath = (%q, %v), want it readable", rel, err)
	}
	if _, err := ws.resolveNewPath("agent.json"); err == nil {
		t.Error("resolveNewPath(config file): want an error")
	}
	if _, err := ws.resolveNewPath("other.json"); err != nil {
		t.Errorf("another file: %v", err)
	}
	if _, err := NewWorkspace(work, "", filepath.Join(work, "missing.json"), DenyList{}); err != nil {
		t.Errorf("a config file path that doesn't exist yet should not fail: %v", err)
	}
}

// - Verify the size cap: content of exactly maxBytes is written, one byte more is refused with nothing written, and a
// NUL byte is refused
func Test_WriteFile_SizeCapAndNUL(t *testing.T) {
	work := t.TempDir()
	tool := newWriteTool(t, work, "", "", DenyList{})
	tool.maxBytes = 10
	if _, err := writeCall(tool, "ok.txt", strings.Repeat("a", 10)); err != nil {
		t.Fatalf("exactly the cap: %v", err)
	}
	_, err := writeCall(tool, "big.txt", strings.Repeat("a", 11))
	if err == nil || !strings.Contains(err.Error(), "11 bytes") {
		t.Fatalf("over the cap: error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "big.txt")); err == nil {
		t.Error("big.txt was created")
	}
	if _, err := writeCall(tool, "ok.txt", "a\x00b"); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("NUL: error = %v", err)
	}
	if c := readBack(t, work, "ok.txt"); c != strings.Repeat("a", 10) {
		t.Errorf("a refused write changed the file: %q", c)
	}
}

// - Verify the 1 MB constant is the cap a real tool starts with, and the description states it
func Test_WriteFile_DefaultCapIs1MB(t *testing.T) {
	tool := newWriteTool(t, t.TempDir(), "", "", DenyList{})
	if tool.maxBytes != 1<<20 {
		t.Errorf("maxBytes = %d", tool.maxBytes)
	}
	if _, err := writeCall(tool, "big.txt", strings.Repeat("a", 1<<20+1)); err == nil {
		t.Error("1 MB + 1 byte: want an error")
	}
	if d := tool.GetToolDefinition(); !strings.Contains(d.Function.Desc, "1024 KB") {
		t.Errorf("description = %q", d.Function.Desc)
	}
}

// - Verify a cancelled context and a nil workspace write nothing / fail
func Test_WriteFile_CancelledContext_NilWorkspace(t *testing.T) {
	work := t.TempDir()
	tool := newWriteTool(t, work, "", "", DenyList{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.CallTool(ctx, json.RawMessage(`{"path":"a.txt","content":"x"}`)); err == nil {
		t.Error("want an error")
	}
	if _, err := os.Stat(filepath.Join(work, "a.txt")); err == nil {
		t.Error("a.txt was created")
	}
	if _, err := NewWriteFileTool(nil); err == nil {
		t.Error("nil workspace: want an error")
	}
}

// - Verify an overwrite keeps the file's mode (an executable script stays executable)
func Test_WriteFile_Overwrite_KeepsMode(t *testing.T) {
	work := t.TempDir()
	p := writeFile(t, work, "run.sh", []byte("old"))
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newWriteTool(t, work, "", "", DenyList{})
	if _, err := writeCall(tool, "run.sh", "new"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
}
