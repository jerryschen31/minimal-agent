package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func newMoveTool(t *testing.T, work, traceDir, configFile string) *MoveFileTool {
	t.Helper()
	ws, err := NewWorkspace(work, traceDir, configFile, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewMoveFileTool(ws)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func moveCall(tool *MoveFileTool, src, dst string) (string, error) {
	return tool.CallTool(context.Background(), twoPathArgs(src, dst))
}

// - Verify a move is a rename: the source is gone, the destination has the same inode, mode and bytes, and it works
// for a rename in place, a move into a subdirectory, and a file bigger than the 1 MB the write tools allow
func Test_MoveFile_Rename_SameInodeNoSizeLimit(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 3<<20)
	p := writeFile(t, work, "a.sh", []byte("#!/bin/sh\n"))
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, work, "big.bin", big)
	before, _ := os.Stat(p)
	tool := newMoveTool(t, work, "", "")
	if got, err := moveCall(tool, "a.sh", "b.sh"); err != nil || got != "moved a.sh -> b.sh (10 bytes)" {
		t.Fatalf("in place: (%q, %v)", got, err)
	}
	if got, err := moveCall(tool, "b.sh", "sub/c.sh"); err != nil || got != "moved b.sh -> sub/c.sh (10 bytes)" {
		t.Fatalf("into a directory: (%q, %v)", got, err)
	}
	if got, err := moveCall(tool, "big.bin", "sub/big.bin"); err != nil || !strings.HasPrefix(got, "moved big.bin -> sub/big.bin (3145728 bytes") {
		t.Fatalf("big file: (%q, %v)", got, err)
	}
	after, err := os.Stat(filepath.Join(work, "sub", "c.sh"))
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o755 {
		t.Errorf("moved file: %v, same inode %v", err, err == nil && os.SameFile(before, after))
	}
	for _, gone := range []string{"a.sh", "b.sh", "big.bin"} {
		if _, err := os.Stat(filepath.Join(work, gone)); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
}

// - Verify an existing destination file is replaced and the result says so
func Test_MoveFile_Overwrite_ReplacesDestination(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "new.txt", []byte("NEW"))
	writeFile(t, work, "old.txt", []byte("OLD"))
	tool := newMoveTool(t, work, "", "")
	got, err := moveCall(tool, "new.txt", "old.txt")
	if err != nil || got != "moved new.txt -> old.txt (3 bytes, overwrote existing)" {
		t.Fatalf("(%q, %v)", got, err)
	}
	if c := readBack(t, work, "old.txt"); c != "NEW" {
		t.Errorf("destination = %q", c)
	}
	if _, err := os.Stat(filepath.Join(work, "new.txt")); err == nil {
		t.Error("source still exists")
	}
}

// - Verify bad arguments and paths are errors that change nothing and never print workDir: missing fields, missing
// source, directory source (files only), directory destination, missing destination parent, same file, workDir
func Test_MoveFile_BadArgumentsAndPaths(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("A"))
	if err := os.MkdirAll(filepath.Join(work, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := newMoveTool(t, work, "", "")
	for _, args := range []string{`{}`, `{"source":"a.txt"}`, `{"destination":"b.txt"}`, `nope`} {
		if _, err := tool.CallTool(context.Background(), []byte(args)); err == nil {
			t.Errorf("args %s: want an error", args)
		}
	}
	for _, tc := range []struct{ src, dst, want string }{
		{"missing.txt", "b.txt", "no such file"},
		{"adir", "b", "moves files only"},
		{"a.txt", "adir", "is a directory"},
		{"a.txt", "nodir/b.txt", "create_directory"},
		{"a.txt", "a.txt", "same file"},
		{"a.txt", ".", "working directory"},
	} {
		_, err := moveCall(tool, tc.src, tc.dst)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("move %q -> %q: error = %v, want one containing %q", tc.src, tc.dst, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), work) {
			t.Errorf("move %q -> %q: error leaks workDir: %v", tc.src, tc.dst, err)
		}
	}
	if c := readBack(t, work, "a.txt"); c != "A" {
		t.Errorf("a.txt = %q", c)
	}
	if _, err := NewMoveFileTool(nil); err == nil {
		t.Error("nil workspace: want an error")
	}
}

// - Verify the policy: moving a protected file away (.env, .git/config, the config file, a trace file, outside) and
// moving onto/into a protected path is refused, and every file stays where and what it was
func Test_MoveFile_ProtectedPaths_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	for _, d := range []string{work, filepath.Join(work, ".git"), filepath.Join(work, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, parent, "outside.txt", []byte("OUT"))
	writeFile(t, work, ".env", []byte("SECRET"))
	writeFile(t, work, ".git/config", []byte("GIT"))
	writeFile(t, work, "logs/run.jsonl", []byte("TRACE"))
	writeFile(t, work, "ok.txt", []byte("OK"))
	cfg := writeFile(t, work, "agent.json", []byte("CFG"))
	tool := newMoveTool(t, work, filepath.Join(work, "logs"), cfg)
	for _, tc := range []struct{ src, dst, want string }{
		{".env", "x.txt", "deny list"},
		{".git/config", "x.txt", "read-only"},
		{"agent.json", "x.txt", "config file"},
		{"logs/run.jsonl", "x.txt", "trace directory"},
		{"../outside.txt", "x.txt", "outside the allowed directory"},
		{"ok.txt", ".env", "deny list"},
		{"ok.txt", ".git/hooks", "read-only"},
		{"ok.txt", "agent.json", "config file"},
		{"ok.txt", "logs/x.jsonl", "trace directory"},
		{"ok.txt", "../escape.txt", "outside the allowed directory"},
	} {
		_, err := moveCall(tool, tc.src, tc.dst)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("move %q -> %q: error = %v, want one containing %q", tc.src, tc.dst, err, tc.want)
		}
	}
	for f, want := range map[string]string{".env": "SECRET", ".git/config": "GIT", "agent.json": "CFG", "logs/run.jsonl": "TRACE", "ok.txt": "OK"} {
		if c := readBack(t, work, f); c != want {
			t.Errorf("%s = %q, want %q", f, c, want)
		}
	}
	for _, rel := range []string{"x.txt", ".git/hooks", "logs/x.jsonl"} {
		if _, err := os.Stat(filepath.Join(work, rel)); err == nil {
			t.Errorf("%s was created", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Error("a file was moved outside workDir")
	}
}

// - Verify a rename across filesystems (EXDEV) gives a message that points at copy_file and names only the requested
// paths, and other rename errors keep their cause without the absolute paths
func Test_RenameError(t *testing.T) {
	le := &os.LinkError{Op: "renameat", Old: "/abs/work/a", New: "/mnt/x/b", Err: syscall.EXDEV}
	err := renameError("a", "mnt/b", le)
	if err == nil || !strings.Contains(err.Error(), "copy_file") || strings.Contains(err.Error(), "/abs") {
		t.Errorf("EXDEV: %v", err)
	}
	err = renameError("a", "b", &os.LinkError{Op: "renameat", Old: "/abs/work/a", New: "/abs/work/b", Err: syscall.EACCES})
	if err == nil || !errors.Is(err, syscall.EACCES) || strings.Contains(err.Error(), "/abs") {
		t.Errorf("EACCES: %v", err)
	}
}
