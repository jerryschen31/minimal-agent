//go:build unix

package mcpconnect

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

// Unix-only: these tests check that a server process is gone with syscall.Kill.

// - Verify a failing server does not take the others down: SetupMCPConns skips each bad server (a command that cannot
// start, a bad config entry, the unsupported SSE transport), names every one in the error, and still returns the good
// local server (a child process) connected. Closing the returned connections must leave no child process behind.
func Test_Unit_SetupMCPConns_ServerFails_SkipsItAndKeepsTheOthers(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("GO_WANT_MCP_HELPER", "1") // the children inherit our environment
	t.Setenv("GO_MCP_HELPER_PIDFILE", pidFile)
	cfg := config.Config{McpServers: map[string]config.McpServerConfig{
		"a-cannot-start": {Command: "definitely-not-a-real-command-xyz"},
		"b-good":         {Command: exe, Args: []string{"-test.run=^TestHelperMCPServer$"}},
		"c-bad-config":   {Command: "x", URL: "https://example.com/mcp"}, // both command and url: rejected before connecting
		"d-sse":          {URL: "https://example.com/sse", Type: config.TransportSSE},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conns, err := SetupMCPConns(ctx, cfg)

	if len(conns) != 1 || conns[0].Name != "b-good" {
		t.Fatalf("expected only b-good to connect, got %d connections (err %v)", len(conns), err)
	}
	if err == nil {
		t.Fatal("expected an error listing the failed servers")
	}
	for _, name := range []string{"a-cannot-start", "c-bad-config", "d-sse"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected the error to name %s, got %q", name, err)
		}
	}
	checkEchoRoundTrip(ctx, t, conns[0]) // the good server works, not just connected

	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("expected the good server to have recorded its pid: %v", readErr)
	}
	pid, convErr := strconv.Atoi(string(raw))
	if convErr != nil {
		t.Fatalf("bad pid file contents %q: %v", raw, convErr)
	}
	for _, c := range conns {
		c.Close()
	}
	// signal 0 checks that a process exists; ESRCH means it is gone
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL) // don't leave the leaked child behind
			t.Fatalf("the good server (pid %d) is still running after its connection was closed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// - Verify a server that never finishes its handshake is stopped at mcpConnectTimeoutSeconds (PR #40 review): setup
// returns, the other server is connected, the error names the slow server and the setting, and the slow server's
// process is gone. The parent context is not cancelled, so the caller treats it as one failed server, not a Ctrl+C.
func Test_Unit_SetupMCPConns_HungServer_TimesOutAndIsStopped(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "hung.pid")
	helper := []string{"-test.run=^TestHelperMCPServer$"}
	cfg := config.Config{
		McpConnectTimeoutSeconds: 1,
		McpServers: map[string]config.McpServerConfig{
			// waits for a file that is never created, so it never answers the handshake
			"a-hung": {Command: exe, Args: helper, Env: map[string]string{
				"GO_MCP_HELPER_WAITFOR": filepath.Join(dir, "never"), "GO_MCP_HELPER_PIDFILE": pidFile}},
			"b-good": {Command: exe, Args: helper},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conns, err := SetupMCPConns(ctx, cfg)
	for _, c := range conns {
		defer c.Close()
	}

	if len(conns) != 1 || conns[0].Name != "b-good" {
		t.Fatalf("expected only b-good connected, got %d connections (err %v)", len(conns), err)
	}
	if err == nil || !strings.Contains(err.Error(), "a-hung") || !strings.Contains(err.Error(), "mcpConnectTimeoutSeconds") {
		t.Errorf("expected an error naming a-hung and mcpConnectTimeoutSeconds, got %v", err)
	}
	if ctx.Err() != nil {
		t.Errorf("expected the parent context still live, got %v", ctx.Err())
	}
	pid := readPid(t, pidFile)
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil { // signal 0 checks that a process exists
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL) // don't leave the leaked child behind
			t.Fatalf("the hung server (pid %d) is still running after its connect timed out", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
