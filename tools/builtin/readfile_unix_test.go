//go:build unix

package builtin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// makeFifo creates a named pipe at path. Its cleanup opens the pipe read-write, which releases any goroutine
// still stuck in a blocking open of it, so a failing test does not leave a goroutine hanging.
func makeFifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory for the pipe: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("cannot create a named pipe here: %v", err)
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
}

// readWithTimeout runs the tool in a goroutine and fails the test if it does not return in time, which is
// how a hang shows up (a blocked open cannot be interrupted, so the test must not wait on it directly).
func readWithTimeout(t *testing.T, tool *ReadFileTool, path string) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := readPath(tool, path)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(3 * time.Second):
		t.Fatalf("read_file did not return within 3s for %q: it is blocked", path)
		return "", nil
	}
}

// - Verify a named pipe with no writer is rejected at once instead of blocking the open forever
func Test_ReadFile_NamedPipe_NoWriter_RejectedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	makeFifo(t, filepath.Join(dir, "pipe"))

	got, err := readWithTimeout(t, newTool(t, dir), "pipe")

	if err == nil || got != "" {
		t.Fatalf("expected an error and no output, got (%q, %v)", got, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), "named pipe") {
		t.Errorf("expected a 'not a regular file ... named pipe' error, got %q", err)
	}
}

// - Verify a named pipe that has a writer with data waiting is also rejected, and that nothing is read from it
func Test_ReadFile_NamedPipe_WithWriter_RejectedAndNotRead(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	makeFifo(t, fifo)
	writer, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0) // read-write open does not wait for a reader
	if err != nil {
		t.Skipf("cannot open the pipe for writing here: %v", err)
	}
	defer writer.Close()
	if _, err := writer.Write([]byte("SECRET-PIPE-CONTENT")); err != nil {
		t.Fatalf("writing to the pipe: %v", err)
	}

	got, err := readWithTimeout(t, newTool(t, dir), "pipe")

	if err == nil {
		t.Fatalf("expected the pipe to be rejected, got output %q", got)
	}
	if got != "" || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("expected nothing read from the pipe, got output %q and error %q", got, err)
	}
	// the data must still be sitting in the pipe: a rejected file must not have been consumed
	buf := make([]byte, 64)
	writer.SetReadDeadline(time.Now().Add(time.Second))
	n, readErr := writer.Read(buf)
	if string(buf[:n]) != "SECRET-PIPE-CONTENT" {
		t.Errorf("expected the pipe to still hold its data, read %q (err %v)", buf[:n], readErr)
	}
}

// - Verify a symlink inside the workDir that points at a named pipe inside the workDir is rejected
// without blocking (resolve() follows the link, and the type check is on the file that was opened)
func Test_ReadFile_SymlinkToNamedPipe_RejectedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	makeFifo(t, filepath.Join(dir, "real_pipe"))
	symlink(t, "real_pipe", filepath.Join(dir, "link_to_pipe"))

	got, err := readWithTimeout(t, newTool(t, dir), "link_to_pipe")

	if err == nil || got != "" {
		t.Fatalf("expected an error and no output, got (%q, %v)", got, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected a 'not a regular file' error, got %q", err)
	}
}

// - Verify a Unix socket file is rejected without blocking
func Test_ReadFile_UnixSocket_Rejected(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rf") // socket paths are limited to about 100 bytes, t.TempDir() can be too long
	if err != nil {
		t.Skipf("cannot create a short temp directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	defer listener.Close()

	got, err := readWithTimeout(t, newTool(t, dir), "s")

	if err == nil || got != "" {
		t.Fatalf("expected an error and no output, got (%q, %v)", got, err)
	}
}

// - Verify a cancelled context (Ctrl+C during the turn) makes the tool return the context error without reading
func Test_ReadFile_CancelledContext_ReturnsContextError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("ALLOWED-A"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := newTool(t, dir).CallTool(ctx, readFileArgs("a.txt"))

	if !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("expected (empty, context.Canceled), got (%q, %v)", got, err)
	}
}

// - Verify a symlink inside workDir that points into the trace directory (at a file, or at the directory) is refused,
// because the check runs on the resolved path
func Test_ReadFile_TraceDir_SymlinkInto_Blocked(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "logs/trace/run.jsonl", []byte("SECRET-TRACE"))
	symlink(t, "logs/trace/run.jsonl", filepath.Join(work, "notes.txt"))
	symlink(t, filepath.Join(work, "logs", "trace"), filepath.Join(work, "history"))
	tool := newTraceTool(t, work, filepath.Join(work, "logs", "trace"))

	expectTraceBlocked(t, tool, "notes.txt")
	expectTraceBlocked(t, tool, "history/run.jsonl")
	expectTraceBlocked(t, tool, "history")
}

// - Verify the trace directory is protected when workDir and traceDir name it through different routes: one through
// a symlink to the real directory and the other by the real path (t.TempDir on macOS is itself under the /var ->
// /private/var symlink), and the contains-workDir check sees through the symlink too
func Test_ReadFile_TraceDir_SymlinkedWorkDir_Blocked(t *testing.T) {
	real := t.TempDir()
	writeFile(t, real, "logs/trace/run.jsonl", []byte("SECRET-TRACE"))
	link := filepath.Join(t.TempDir(), "worklink")
	symlink(t, real, link)

	t.Run("workDir via symlink, traceDir real", func(t *testing.T) {
		expectTraceBlocked(t, newTraceTool(t, link, filepath.Join(real, "logs", "trace")), "logs/trace/run.jsonl")
	})
	t.Run("workDir real, traceDir via symlink", func(t *testing.T) {
		expectTraceBlocked(t, newTraceTool(t, real, filepath.Join(link, "logs", "trace")), "logs/trace/run.jsonl")
	})
	t.Run("traceDir via symlink, not created yet", func(t *testing.T) {
		tool := newTraceTool(t, real, filepath.Join(link, "logs", "later"))
		writeFile(t, real, "logs/later/run.jsonl", []byte("SECRET-LATER"))
		expectTraceBlocked(t, tool, "logs/later/run.jsonl")
	})
	t.Run("traceDir is workDir via symlink", func(t *testing.T) {
		if _, err := NewReadFileTool(real, link, testMaxBytes, DenyList{}); err == nil || !strings.Contains(err.Error(), "is workDir or contains it") {
			t.Errorf("expected an 'is workDir or contains it' error, got %v", err)
		}
	})
}

// - Verify NewReadFileTool refuses a traceDir that is a dangling symlink, or that goes through one (link/trace with
// link -> missing): the recorder would write where the link points, not at the link's own path
func Test_ReadFile_NewReadFileTool_TraceDirDanglingSymlink_ReturnsError(t *testing.T) {
	work := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(other, "link")
	symlink(t, filepath.Join(work, "missing"), link)

	for name, traceDir := range map[string]string{
		"traceDir is the link": link,
		"parent is the link":   filepath.Join(link, "trace"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewReadFileTool(work, traceDir, testMaxBytes, DenyList{})
			if err == nil || !strings.Contains(err.Error(), "is a symlink to a path that doesn't exist") {
				t.Errorf("expected a dangling-symlink error for traceDir %q, got %v", traceDir, err)
			}
		})
	}
}
