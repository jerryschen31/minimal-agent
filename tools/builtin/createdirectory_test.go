package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCreateDirTool(t *testing.T, work string) *CreateDirectoryTool {
	t.Helper()
	tool, err := NewCreateDirectoryTool(newWorkspace(t, work, "", DenyList{}))
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func createDir(tool *CreateDirectoryTool, path string) (string, error) {
	args, _ := json.Marshal(map[string]string{"path": path})
	return tool.CallTool(context.Background(), args)
}

// - Verify a new nested directory is created with its parents, and a second call says it already exists
func Test_CreateDirectory_NewNested_CreatedThenIdempotent(t *testing.T) {
	work := t.TempDir()
	tool := newCreateDirTool(t, work)
	got, err := createDir(tool, "a/b/c")
	if err != nil || got != "created directory a/b/c" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	if fi, err := os.Stat(filepath.Join(work, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	got, err = createDir(tool, "a/b/c")
	if err != nil || got != "directory a/b/c already exists" {
		t.Fatalf("second call got (%q, %v)", got, err)
	}
}

// - Verify refusals: a file in the way, outside workDir, deny-listed, write-denied (.git), and bad arguments
func Test_CreateDirectory_Refused(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "file.txt", []byte("x"))
	tool := newCreateDirTool(t, work)
	for path, want := range map[string]string{
		"file.txt":                     "is a file",
		"file.txt/sub":                 "not a directory",
		"../escape":                    "outside the allowed directory",
		".env":                         "deny list",
		".ssh/keys":                    "deny list",
		".git":                         "read-only",
		"sub/.git/hooks":               "read-only",
		filepath.Join(work, "..", "x"): "outside the allowed directory",
	} {
		_, err := createDir(tool, path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("createDir(%q) error = %v, want one containing %q", path, err, want)
			continue
		}
		if strings.Contains(err.Error(), work) && !filepath.IsAbs(path) {
			t.Errorf("createDir(%q) error leaks workDir: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "sub")); err == nil {
		t.Error("sub/ was created although sub/.git/hooks is refused")
	}
	if _, err := tool.CallTool(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("missing path: want an error")
	}
	if _, err := tool.CallTool(context.Background(), json.RawMessage(`nope`)); err == nil {
		t.Error("bad JSON: want an error")
	}
}

// - Verify a cancelled context creates nothing
func Test_CreateDirectory_CancelledContext_CreatesNothing(t *testing.T) {
	work := t.TempDir()
	tool := newCreateDirTool(t, work)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.CallTool(ctx, json.RawMessage(`{"path":"d"}`)); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(filepath.Join(work, "d")); err == nil {
		t.Error("d was created")
	}
}

// - Verify resolveNewPath: a missing file under an existing directory resolves; the tail is not required to exist
func Test_Unit_Workspace_ResolveNewPath_MissingTail_Resolves(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "d/old.txt", []byte("x"))
	ws := newWorkspace(t, work, "", DenyList{})
	for _, p := range []string{"d/new.txt", "x/y/z.txt", filepath.Join(work, "d", "new.txt")} {
		rel, err := ws.resolveNewPath(p)
		if err != nil || rel != filepath.Clean(strings.TrimPrefix(p, work+string(filepath.Separator))) {
			t.Errorf("resolveNewPath(%q) = (%q, %v)", p, rel, err)
		}
	}
}
