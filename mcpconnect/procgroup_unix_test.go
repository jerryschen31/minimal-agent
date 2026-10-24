//go:build unix

package mcpconnect

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

// startHelperServer connects to the helper MCP server (this test binary re-run as a child) and returns the
// connection and the server's pid. Extra environment variables for the child are set with t.Setenv first.
func startHelperServer(t *testing.T, ctx context.Context) (*McpConnection, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "server.pid")
	t.Setenv("GO_WANT_MCP_HELPER", "1") // the child inherits our environment
	t.Setenv("GO_MCP_HELPER_PIDFILE", pidFile)
	c, err := connectLocalMCPServer(ctx, "local", config.McpServerConfig{Command: exe, Args: []string{"-test.run=^TestHelperMCPServer$"}}, config.Config{})
	if err != nil {
		t.Fatalf("connectLocalMCPServer: %v", err)
	}
	return c, readPid(t, pidFile)
}

func readPid(t *testing.T, file string) int {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading pid file: %v", err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatalf("bad pid file contents %q: %v", raw, err)
	}
	return pid
}

// - Verify a local server runs in its own process group (one group per server, led by the server), not in ours.
// The terminal sends Ctrl+C's SIGINT to its foreground process group only, which is ours, so this is what keeps
// a Ctrl+C that cancels a chat turn from also killing the server (DECISIONS.md § Deferred for Later, item 14).
func Test_Unit_ConnectLocalMCP_ServerRunsInItsOwnProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, pid := startHelperServer(t, ctx)
	defer c.Close()

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", pid, err)
	}
	if pgid != pid {
		t.Errorf("server pid %d is in group %d, want its own group %d", pid, pgid, pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("server is in the agent's process group %d, so the terminal's Ctrl+C would reach it", pgid)
	}
	checkEchoRoundTrip(ctx, t, c)
}

// - Verify Close also stops what the server started (as npx starts node): the grandchild must be gone after Close,
// although closing the session only stops the server itself. Liveness is checked with a FIFO the grandchild holds
// open as its stdout, not kill(pid, 0): the orphaned grandchild may stay a zombie where PID 1 does not reap it
// (some containers), and a zombie still answers signal 0. Its open files are closed when it exits, reaped or not.
func Test_Unit_McpConnectionClose_KillsTheServersGrandchildren(t *testing.T) {
	dir := t.TempDir()
	gcFile, fifo := filepath.Join(dir, "grandchild.pid"), filepath.Join(dir, "grandchild.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	t.Setenv("GO_MCP_HELPER_GRANDCHILD_PIDFILE", gcFile)
	t.Setenv("GO_MCP_HELPER_GRANDCHILD_FIFO", fifo)
	gone := make(chan struct{})
	go func() { // opening blocks until the helper opens the write end; reading ends at EOF, when the grandchild exits
		defer close(gone)
		f, err := os.Open(fifo)
		if err != nil {
			return
		}
		defer f.Close()
		io.Copy(io.Discard, f)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _ := startHelperServer(t, ctx)
	gc := readPid(t, gcFile)

	c.Close()

	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		syscall.Kill(gc, syscall.SIGKILL) // don't leave the leaked grandchild behind
		t.Fatalf("the server's grandchild (pid %d) is still running after Close", gc)
	}
}
