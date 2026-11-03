package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newEditTool(t *testing.T, work, traceDir, configFile string) *EditFileTool {
	t.Helper()
	ws, err := NewWorkspace(work, traceDir, configFile, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewEditFileTool(ws)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func editCall(tool *EditFileTool, path, old, new string, all bool) (string, error) {
	args, _ := json.Marshal(map[string]any{"path": path, "old_string": old, "new_string": new, "replace_all": all})
	return tool.CallTool(context.Background(), args)
}

// - Verify applyEdit: one match, many matches with and without replace_all, no match, empty old, overlapping matches,
// deletion with an empty new, and the CRLF hint
func Test_Unit_ApplyEdit(t *testing.T) {
	tests := []struct {
		name, content, old, new string
		all                     bool
		want                    string
		wantN                   int
		wantErr                 string
	}{
		{"one match", "a b c", "b", "X", false, "a X c", 1, ""},
		{"delete", "a b c", " b", "", false, "a c", 1, ""},
		{"multi without all", "x x x", "x", "y", false, "", 0, "found 3 times"},
		{"multi with all", "x x x", "x", "y", true, "y y y", 3, ""},
		{"all with one match", "a b", "a", "z", true, "z b", 1, ""},
		{"no match", "abc", "zzz", "y", false, "", 0, "not found"},
		{"empty old", "abc", "", "y", false, "", 0, "must not be empty"},
		{"overlapping is non-overlapping", "aaaa", "aa", "b", true, "bb", 2, ""},
		{"multi-line", "l1\nl2\nl3\n", "l2\nl3", "L", false, "l1\nL\n", 1, ""},
		{"crlf hint", "l1\r\nl2\r\n", "l1\nl2", "L", false, "", 0, "CRLF"},
		{"no crlf hint when the file is LF", "l1\nl2\n", "l1\nl9", "L", false, "", 0, "not found"},
	}
	for _, tc := range tests {
		got, n, err := applyEdit(tc.content, tc.old, tc.new, tc.all, 1<<20)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), tc.content) && len(tc.content) > 3 {
				t.Errorf("%s: error echoes the file content: %v", tc.name, err)
			}
			continue
		}
		if err != nil || got != tc.want || n != tc.wantN {
			t.Errorf("%s: (%q, %d, %v), want (%q, %d, nil)", tc.name, got, n, err, tc.want, tc.wantN)
		}
	}
	if _, _, err := applyEdit("xxxxxxxxxx", "x", "yy", true, 15); err == nil || !strings.Contains(err.Error(), "20 bytes") {
		t.Errorf("result over the cap: err = %v", err)
	}
	if _, _, err := applyEdit("xxxxxxxxxx", "x", "yy", true, 20); err != nil {
		t.Errorf("result exactly at the cap: %v", err)
	}
}

// - Verify an edit changes only the matched text, reports the count, keeps the mode, and edits through a relative and an
// absolute path
func Test_EditFile_SingleAndReplaceAll_KeepsModeAndRest(t *testing.T) {
	work := t.TempDir()
	p := writeFile(t, work, "run.sh", []byte("echo one\necho two\necho one\n"))
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newEditTool(t, work, "", "")
	if _, err := editCall(tool, "run.sh", "echo one", "echo 1", false); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("ambiguous match: err = %v", err)
	}
	if c := readBack(t, work, "run.sh"); c != "echo one\necho two\necho one\n" {
		t.Fatalf("a refused edit changed the file: %q", c)
	}
	got, err := editCall(tool, "run.sh", "echo one", "echo 1", true)
	if err != nil || got != "edited run.sh (2 replacements, 23 bytes)" {
		t.Fatalf("(%q, %v)", got, err)
	}
	got, err = editCall(tool, filepath.Join(tool.ws.rootDir, "run.sh"), "echo two\n", "", false)
	if err != nil || got != "edited run.sh (1 replacement, 14 bytes)" {
		t.Fatalf("absolute path: (%q, %v)", got, err)
	}
	if c := readBack(t, work, "run.sh"); c != "echo 1\necho 1\n" {
		t.Errorf("file = %q", c)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

// - Verify a shrinking edit leaves no stale tail (the file is truncated before the write)
func Test_EditFile_Shrink_NoStaleTail(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("0123456789 long tail here"))
	tool := newEditTool(t, work, "", "")
	if _, err := editCall(tool, "a.txt", "0123456789 long tail here", "x", false); err != nil {
		t.Fatal(err)
	}
	if c := readBack(t, work, "a.txt"); c != "x" {
		t.Errorf("file = %q", c)
	}
}

// - Verify bad arguments are errors and change nothing: missing/empty fields, identical strings, NUL, bad JSON
func Test_EditFile_BadArguments(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("hello"))
	tool := newEditTool(t, work, "", "")
	for name, args := range map[string]string{
		"no path":    `{"old_string":"h","new_string":"j"}`,
		"empty old":  `{"path":"a.txt","old_string":"","new_string":"j"}`,
		"no new":     `{"path":"a.txt","old_string":"h"}`,
		"same":       `{"path":"a.txt","old_string":"h","new_string":"h"}`,
		"NUL in new": `{"path":"a.txt","old_string":"h","new_string":"a\u0000b"}`,
		"NUL in old": `{"path":"a.txt","old_string":"\u0000","new_string":"b"}`,
		"bad json":   `nope`,
		"wrong type": `{"path":"a.txt","old_string":1,"new_string":"b"}`,
	} {
		if _, err := tool.CallTool(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := editCall(tool, "a.txt", "h", "", false); err != nil { // empty new_string is allowed
		t.Errorf("delete: %v", err)
	}
	if c := readBack(t, work, "a.txt"); c != "ello" {
		t.Errorf("file = %q", c)
	}
}

// - Verify a missing file points at write_file and creates nothing; a directory is refused
func Test_EditFile_MissingFile_PointsAtWriteFile(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newEditTool(t, work, "", "")
	_, err := editCall(tool, "new.txt", "a", "b", false)
	if err == nil || !strings.Contains(err.Error(), "write_file") || strings.Contains(err.Error(), work) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "new.txt")); err == nil {
		t.Error("new.txt was created")
	}
	if _, err := editCall(tool, "adir", "a", "b", false); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("directory: error = %v", err)
	}
}

// - Verify refusals: outside workDir, deny-listed, .git, the config file, the trace directory; files are unchanged and
// errors never leak workDir
func Test_EditFile_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	for _, d := range []string{work, filepath.Join(work, ".git"), filepath.Join(work, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, parent, "outside.txt", []byte("SECRET"))
	writeFile(t, work, ".env", []byte("SECRET"))
	writeFile(t, work, ".git/config", []byte("SECRET"))
	writeFile(t, work, "logs/run.jsonl", []byte("SECRET"))
	cfg := writeFile(t, work, "agent.json", []byte("SECRET"))
	tool := newEditTool(t, work, filepath.Join(work, "logs"), cfg)
	for path, want := range map[string]string{
		"../outside.txt": "outside the allowed directory",
		".env":           "deny list",
		".git/config":    "read-only",
		"agent.json":     "config file",
		"logs/run.jsonl": "trace directory",
	} {
		_, err := editCall(tool, path, "SECRET", "PWNED", false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("edit %q: error = %v, want one containing %q", path, err, want)
			continue
		}
		if strings.Contains(err.Error(), work) {
			t.Errorf("edit %q: error leaks workDir: %v", path, err)
		}
	}
	for _, p := range []string{filepath.Join(work, ".env"), filepath.Join(work, ".git", "config"), filepath.Join(work, "agent.json"), filepath.Join(parent, "outside.txt")} {
		if b, _ := os.ReadFile(p); string(b) != "SECRET" {
			t.Errorf("%s was changed: %q", p, b)
		}
	}
}

// - Verify size limits: a file over the cap and a file with a NUL byte are refused unchanged, old/new over the cap are
// refused, and an edit whose result would pass the cap is refused unchanged
func Test_EditFile_SizeLimitsAndBinary(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "big.txt", []byte(strings.Repeat("a", 20)))
	writeFile(t, work, "bin.dat", []byte("abc\x00def"))
	writeFile(t, work, "grow.txt", []byte("ab ab ab"))
	tool := newEditTool(t, work, "", "")
	tool.maxBytes = 10
	if _, err := editCall(tool, "big.txt", "a", "b", true); err == nil || !strings.Contains(err.Error(), "20 bytes") {
		t.Errorf("big file: error = %v", err)
	}
	if _, err := editCall(tool, "bin.dat", "abc", "x", false); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary: error = %v", err)
	}
	if _, err := editCall(tool, "grow.txt", "ab", strings.Repeat("z", 11), false); err == nil || !strings.Contains(err.Error(), "limited to 10") {
		t.Errorf("long new_string: error = %v", err)
	}
	if _, err := editCall(tool, "grow.txt", "ab", "zzzz", true); err == nil || !strings.Contains(err.Error(), "would be 14 bytes") {
		t.Errorf("result over the cap: error = %v", err)
	}
	for f, want := range map[string]string{"big.txt": strings.Repeat("a", 20), "bin.dat": "abc\x00def", "grow.txt": "ab ab ab"} {
		if c := readBack(t, work, f); c != want {
			t.Errorf("%s changed: %q", f, c)
		}
	}
}

// - Verify the default cap is 1 MB, and a file of exactly 1 MB is editable when the result stays within it
func Test_EditFile_DefaultCapIs1MB(t *testing.T) {
	work := t.TempDir()
	tool := newEditTool(t, work, "", "")
	if tool.maxBytes != 1<<20 {
		t.Fatalf("maxBytes = %d", tool.maxBytes)
	}
	writeFile(t, work, "m.txt", []byte("X"+strings.Repeat("a", 1<<20-1)))
	if _, err := editCall(tool, "m.txt", "X", "Y", false); err != nil {
		t.Errorf("1 MB file: %v", err)
	}
	if _, err := editCall(tool, "m.txt", "Y", "YY", false); err == nil {
		t.Error("result of 1 MB + 1 byte: want an error")
	}
	if d := tool.GetToolDefinition(); !strings.Contains(d.Function.Desc, "1024 KB") {
		t.Errorf("description = %q", d.Function.Desc)
	}
}

// - Verify a cancelled context and a nil workspace change nothing / fail
func Test_EditFile_CancelledContext_NilWorkspace(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("abc"))
	tool := newEditTool(t, work, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.CallTool(ctx, json.RawMessage(`{"path":"a.txt","old_string":"a","new_string":"b"}`)); err == nil {
		t.Error("want an error")
	}
	if c := readBack(t, work, "a.txt"); c != "abc" {
		t.Errorf("file = %q", c)
	}
	if _, err := NewEditFileTool(nil); err == nil {
		t.Error("nil workspace: want an error")
	}
}

// - Verify concurrent edits of one file (as when the model sends several calls in one step) all apply: none is lost
func Test_EditFile_ConcurrentEditsSameFile_NoneLost(t *testing.T) {
	work := t.TempDir()
	const n = 20
	var lines []string
	for i := range n {
		lines = append(lines, "line"+string(rune('A'+i))+"=0")
	}
	writeFile(t, work, "c.txt", []byte(strings.Join(lines, "\n")))
	tool := newEditTool(t, work, "", "")
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "line" + string(rune('A'+i))
			_, err := editCall(tool, "c.txt", name+"=0", name+"=1", false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("edit failed: %v", err)
		}
	}
	if c := readBack(t, work, "c.txt"); strings.Contains(c, "=0") {
		t.Errorf("an edit was lost:\n%s", c)
	}
}

// - Verify the changed-while-editing guard: if another writer changes the file (size, or only the mtime) after it was
// read, the edit is refused and the other writer's content is kept
func Test_EditFile_ChangedAfterRead_Refused(t *testing.T) {
	for name, change := range map[string]func(p string){
		"size": func(p string) {
			if err := os.WriteFile(p, []byte("abc plus another writer"), 0o644); err != nil {
				panic(err)
			}
		},
		"mtime only": func(p string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(p, later, later); err != nil {
				panic(err)
			}
		},
	} {
		work := t.TempDir()
		p := writeFile(t, work, "a.txt", []byte("abc"))
		tool := newEditTool(t, work, "", "")
		tool.afterRead = func() { change(p) }
		_, err := editCall(tool, "a.txt", "abc", "XYZ", false)
		if err == nil || !strings.Contains(err.Error(), "changed while it was being edited") {
			t.Errorf("%s: error = %v", name, err)
		}
		if c := readBack(t, work, "a.txt"); strings.Contains(c, "XYZ") {
			t.Errorf("%s: the edit overwrote the other writer: %q", name, c)
		}
	}
}
