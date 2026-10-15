package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call runs any built-in tool with args given as a Go map, the way the agent does (JSON in, text or error out).
func call(t *testing.T, tool interface {
	CallTool(context.Context, json.RawMessage) (string, error)
}, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.CallTool(context.Background(), b)
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	return string(b)
}

// - Verify write_file creates a new file (and its parent directories), overwrites an existing one,
// and accepts empty content
func Test_WriteFile_CreatesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	tool, err := NewWriteFileTool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := call(t, tool, map[string]any{"path": "notes/a/b.md", "content": "hello"}); err != nil || !strings.Contains(out, "wrote 5 bytes to notes/a/b.md") {
		t.Fatalf("create: %q, %v", out, err)
	}
	if got := readBack(t, filepath.Join(dir, "notes/a/b.md")); got != "hello" {
		t.Errorf("contents = %q", got)
	}
	if _, err := call(t, tool, map[string]any{"path": "notes/a/b.md", "content": ""}); err != nil {
		t.Fatalf("overwrite with empty content: %v", err)
	}
	if got := readBack(t, filepath.Join(dir, "notes/a/b.md")); got != "" {
		t.Errorf("after overwrite contents = %q, want empty", got)
	}
}

// - Verify write_file refuses paths outside workDir, deny-listed names, a missing content key,
// a directory target, and writes through a symlink that points outside
func Test_WriteFile_Refusals(t *testing.T) {
	outside := t.TempDir()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	symlink(t, outside, filepath.Join(dir, "escape"))
	tool, _ := NewWriteFileTool(dir)

	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"dotdot":          {map[string]any{"path": "../x.txt", "content": "x"}, "outside"},
		"absolute":        {map[string]any{"path": filepath.Join(outside, "x.txt"), "content": "x"}, "outside"},
		"symlinked dir":   {map[string]any{"path": "escape/x.txt", "content": "x"}, "outside"},
		".env":            {map[string]any{"path": ".env", "content": "x"}, "deny list"},
		"key in subdir":   {map[string]any{"path": "sub/server.key", "content": "x"}, "deny list"},
		"missing content": {map[string]any{"path": "x.txt"}, "content"},
		"missing path":    {map[string]any{"content": "x"}, "path"},
		"directory":       {map[string]any{"path": "sub", "content": "x"}, "not a regular file"},
		"too large":       {map[string]any{"path": "big.txt", "content": strings.Repeat("x", WriteFileMaxBytes+1)}, "limit"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := call(t, tool, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was written outside workDir: %v", entries)
	}
}

// - Verify edit_file replaces exactly one match, refuses zero or several matches unless replace_all,
// and keeps the file's permissions
func Test_EditFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "main.go", []byte("a := 1\nb := 1\nc := 2\n"))
	os.Chmod(path, 0o600)
	tool, _ := NewEditFileTool(dir)

	if _, err := call(t, tool, map[string]any{"path": "main.go", "old_string": "c := 2", "new_string": "c := 3"}); err != nil {
		t.Fatalf("unique edit: %v", err)
	}
	if got := readBack(t, path); got != "a := 1\nb := 1\nc := 3\n" {
		t.Errorf("after unique edit: %q", got)
	}
	if _, err := call(t, tool, map[string]any{"path": "main.go", "old_string": ":= 1", "new_string": ":= 9"}); err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Errorf("ambiguous edit err = %v", err)
	}
	if out, err := call(t, tool, map[string]any{"path": "main.go", "old_string": ":= 1", "new_string": ":= 9", "replace_all": true}); err != nil || !strings.Contains(out, "replaced 2") {
		t.Errorf("replace_all: %q, %v", out, err)
	}
	if got := readBack(t, path); got != "a := 9\nb := 9\nc := 3\n" {
		t.Errorf("after replace_all: %q", got)
	}
	if _, err := call(t, tool, map[string]any{"path": "main.go", "old_string": "nope", "new_string": "x"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing old_string err = %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("permissions changed to %v", info.Mode().Perm())
	}

	for name, args := range map[string]map[string]any{
		"empty old_string":   {"path": "main.go", "old_string": "", "new_string": "x"},
		"missing new_string": {"path": "main.go", "old_string": "a"},
		"missing file":       {"path": "nope.go", "old_string": "a", "new_string": "b"},
		"outside":            {"path": "../x", "old_string": "a", "new_string": "b"},
	} {
		if _, err := call(t, tool, args); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// - Verify list_dir lists entries sorted, marks directories, hides deny-listed names, and stays inside workDir
func Test_ListDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.txt", []byte("12345"))
	writeFile(t, dir, "a/x.go", nil)
	writeFile(t, dir, ".env", []byte("SECRET=1"))
	tool, _ := NewListDirTool(dir)

	out, err := call(t, tool, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "a/\nb.txt (5 bytes)" {
		t.Errorf("list . = %q", out)
	}
	if out, _ := call(t, tool, map[string]any{"path": "a"}); out != "x.go (0 bytes)" {
		t.Errorf("list a = %q", out)
	}
	if _, err := call(t, tool, map[string]any{"path": ".."}); err == nil {
		t.Error("list .. : want an error")
	}
	if _, err := call(t, tool, map[string]any{"path": "b.txt"}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("list a file: err = %v", err)
	}
}
