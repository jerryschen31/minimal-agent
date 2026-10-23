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
