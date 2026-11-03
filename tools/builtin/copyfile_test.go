package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCopyTool(t *testing.T, work, traceDir, configFile string) *CopyFileTool {
	t.Helper()
	ws, err := NewWorkspace(work, traceDir, configFile, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewCopyFileTool(ws)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func twoPathArgs(src, dst string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"source": src, "destination": dst})
	return b
}

func copyCall(tool *CopyFileTool, src, dst string) (string, error) {
	return tool.CallTool(context.Background(), twoPathArgs(src, dst))
}

// - Verify a copy creates the destination with the same bytes (text, empty, and binary with NUL bytes), leaves the
// source alone, and keeps the source's permission bits on a new file
func Test_CopyFile_NewDestination_SameBytesAndMode(t *testing.T) {
	work := t.TempDir()
	bin := []byte{0, 1, 2, 0xff, 0, 'a', '\n', 0}
	writeFile(t, work, "a.txt", []byte("hello\nworld"))
	writeFile(t, work, "empty.txt", nil)
	writeFile(t, work, "img.bin", bin)
	script := writeFile(t, work, "run.sh", []byte("#!/bin/sh\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newCopyTool(t, work, "", "")
	for src, want := range map[string]string{
		"a.txt":     "copied a.txt -> out/a.txt (11 bytes)",
		"empty.txt": "copied empty.txt -> out/empty.txt (0 bytes)",
		"img.bin":   "copied img.bin -> out/img.bin (8 bytes)",
		"run.sh":    "copied run.sh -> out/run.sh (10 bytes)",
	} {
		got, err := copyCall(tool, src, "out/"+src)
		if err != nil || got != want {
			t.Errorf("%s: (%q, %v), want %q", src, got, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(work, "out", "img.bin")); !bytes.Equal(b, bin) {
		t.Errorf("binary copy differs: %v", b)
	}
	if c := readBack(t, work, "a.txt"); c != "hello\nworld" {
		t.Errorf("source changed: %q", c)
	}
	if fi, _ := os.Stat(filepath.Join(work, "out", "run.sh")); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %v, want the source's executable bit", fi.Mode().Perm())
	}
}

// - Verify an existing destination is replaced whole, the result says so, and the destination keeps its own mode
func Test_CopyFile_Overwrite_ReplacesAndKeepsDestinationMode(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "new.txt", []byte("NEW"))
	dst := writeFile(t, work, "old.txt", []byte("OLD CONTENT THAT IS LONGER"))
	if err := os.Chmod(dst, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := newCopyTool(t, work, "", "")
	got, err := copyCall(tool, "new.txt", "old.txt")
	if err != nil || got != "copied new.txt -> old.txt (3 bytes, overwrote existing)" {
		t.Fatalf("(%q, %v)", got, err)
	}
	if c := readBack(t, work, "old.txt"); c != "NEW" {
		t.Errorf("destination = %q", c)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// - Verify bad arguments and bad paths are errors that change nothing and never print workDir: missing fields, missing
// source, directory source, directory destination, missing destination parent, same file, workDir as destination
func Test_CopyFile_BadArgumentsAndPaths(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("A"))
	if err := os.MkdirAll(filepath.Join(work, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newCopyTool(t, work, "", "")
	for _, args := range []string{`{}`, `{"source":"a.txt"}`, `{"destination":"b.txt"}`, `nope`, `{"source":1,"destination":"b"}`} {
		if _, err := tool.CallTool(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("args %s: want an error", args)
		}
	}
	for _, tc := range []struct{ src, dst, want string }{
		{"missing.txt", "b.txt", "no such file"},
		{"adir", "b.txt", "is a directory"},
		{"a.txt", "adir", "is a directory"},
		{"a.txt", "nodir/b.txt", "create_directory"},
		{"a.txt", "a.txt", "same file"},
		{"a.txt", "./adir/../a.txt", "same file"},
		{"a.txt", ".", "working directory"},
	} {
		_, err := copyCall(tool, tc.src, tc.dst)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("copy %q -> %q: error = %v, want one containing %q", tc.src, tc.dst, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), work) {
			t.Errorf("copy %q -> %q: error leaks workDir: %v", tc.src, tc.dst, err)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "nodir")); err == nil {
		t.Error("the missing destination parent was created")
	}
	if _, err := NewCopyFileTool(nil); err == nil {
		t.Error("nil workspace: want an error")
	}
}

// - Verify the policy: copying out of a protected source (.env, trace dir, outside workDir) and into a protected
// destination (.env, .git, the config file, trace dir, outside) is refused, and nothing is created or changed
func Test_CopyFile_ProtectedPaths_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	for _, d := range []string{work, filepath.Join(work, ".git"), filepath.Join(work, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, parent, "outside.txt", []byte("OUT"))
	writeFile(t, work, ".env", []byte("SECRET"))
	writeFile(t, work, "logs/run.jsonl", []byte("TRACE"))
	writeFile(t, work, "ok.txt", []byte("OK"))
	cfg := writeFile(t, work, "agent.json", []byte("CFG"))
	tool := newCopyTool(t, work, filepath.Join(work, "logs"), cfg)
	for _, tc := range []struct{ src, dst, want string }{
		{".env", "notes.txt", "deny list"},
		{"logs/run.jsonl", "notes.txt", "trace directory"},
		{"../outside.txt", "notes.txt", "outside the allowed directory"},
		{"ok.txt", ".env", "deny list"},
		{"ok.txt", ".git/hooks", "read-only"},
		{"ok.txt", "agent.json", "config file"},
		{"ok.txt", "logs/copy.jsonl", "trace directory"},
		{"ok.txt", "../escape.txt", "outside the allowed directory"},
	} {
		_, err := copyCall(tool, tc.src, tc.dst)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("copy %q -> %q: error = %v, want one containing %q", tc.src, tc.dst, err, tc.want)
		}
	}
	for _, rel := range []string{"notes.txt", ".git/hooks", "logs/copy.jsonl"} {
		if _, err := os.Stat(filepath.Join(work, rel)); err == nil {
			t.Errorf("%s was created", rel)
		}
	}
	for f, want := range map[string]string{".env": "SECRET", "agent.json": "CFG"} {
		if c := readBack(t, work, f); c != want {
			t.Errorf("%s = %q", f, c)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Error("a file was created outside workDir")
	}
}

// - Verify the size limit: a file over the limit is refused up front without creating the destination, a file at the
// limit is copied, and the default limit is 100 MB with the description saying so
func Test_CopyFile_SizeLimit(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "big.bin", bytes.Repeat([]byte("x"), 11))
	writeFile(t, work, "fit.bin", bytes.Repeat([]byte("x"), 10))
	tool := newCopyTool(t, work, "", "")
	tool.maxBytes = 10
	if _, err := copyCall(tool, "big.bin", "big2.bin"); err == nil || !strings.Contains(err.Error(), "11 bytes") {
		t.Errorf("over the limit: error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "big2.bin")); err == nil {
		t.Error("the destination was created for a refused copy")
	}
	if _, err := copyCall(tool, "fit.bin", "fit2.bin"); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	def := newCopyTool(t, work, "", "")
	if def.maxBytes != 100<<20 || !strings.Contains(def.GetToolDefinition().Function.Desc, "100 MB") {
		t.Errorf("default limit %d, description %q", def.maxBytes, def.GetToolDefinition().Function.Desc)
	}
}

// - Verify a copy that fails part-way removes the partial NEW destination (the source grew past the limit after the
// size check), but a destination that already existed is not deleted
func Test_CopyFile_FailsPartWay_PartialNewFileRemoved_ExistingKept(t *testing.T) {
	work := t.TempDir()
	src := writeFile(t, work, "grow.txt", []byte("12345"))
	writeFile(t, work, "existing.txt", []byte("OLD"))
	tool := newCopyTool(t, work, "", "")
	tool.maxBytes = 8
	tool.afterOpenDest = func() { // grow the source after the size check
		if err := os.WriteFile(src, []byte("123456789012"), 0o644); err != nil {
			panic(err)
		}
	}
	_, err := copyCall(tool, "grow.txt", "partial.txt")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("new destination: error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "partial.txt")); err == nil {
		t.Error("the partial new file was left behind")
	}
	if err := os.WriteFile(src, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := copyCall(tool, "grow.txt", "existing.txt"); err == nil {
		t.Fatal("overwrite: want an error")
	}
	if _, err := os.Stat(filepath.Join(work, "existing.txt")); err != nil {
		t.Errorf("an existing destination must not be deleted: %v", err)
	}
	var sink bytes.Buffer
	if n, err := copyWithContext(context.Background(), &sink, strings.NewReader(strings.Repeat("x", 100)), 40); err == nil || n <= 40 {
		t.Errorf("copyWithContext over the limit: (%d, %v)", n, err)
	}
}

type cancelAfterFirstRead struct {
	r      io.Reader
	cancel context.CancelFunc
	reads  int
}

func (c *cancelAfterFirstRead) Read(p []byte) (int, error) {
	c.reads++
	n, err := c.r.Read(p)
	if c.reads == 1 {
		c.cancel()
	}
	return n, err
}

// - Verify a cancelled context stops copyWithContext between pieces (so Ctrl+C stops a long copy), and the cancel error
// is returned as it is
func Test_CopyWithContext_Cancelled_StopsBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &cancelAfterFirstRead{r: strings.NewReader(strings.Repeat("x", 10*copyChunk)), cancel: cancel}
	var sink bytes.Buffer
	n, err := copyWithContext(ctx, &sink, src, 1<<30)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if n > int64(copyChunk) || src.reads != 1 {
		t.Errorf("copied %d bytes in %d reads, want to stop after the first piece", n, src.reads)
	}
}

// - Verify a context cancelled before the call copies nothing
func Test_CopyFile_CancelledContext_CopiesNothing(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("A"))
	tool := newCopyTool(t, work, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.CallTool(ctx, twoPathArgs("a.txt", "b.txt")); err == nil {
		t.Error("want an error")
	}
	if _, err := os.Stat(filepath.Join(work, "b.txt")); err == nil {
		t.Error("b.txt was created")
	}
}
