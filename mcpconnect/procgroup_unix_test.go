//go:build unix

package mcpconnect

import (
	"context"
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
// although closing the session only stops the server itself
func Test_Unit_McpConnectionClose_KillsTheServersGrandchildren(t *testing.T) {
	gcFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv("GO_MCP_HELPER_GRANDCHILD_PIDFILE", gcFile)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _ := startHelperServer(t, ctx)
	gc := readPid(t, gcFile)

	c.Close()

	// signal 0 checks that a process exists; ESRCH means it is gone
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(gc, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(gc, syscall.SIGKILL) // don't leave the leaked grandchild behind
			t.Fatalf("the server's grandchild (pid %d) is still running after Close", gc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
