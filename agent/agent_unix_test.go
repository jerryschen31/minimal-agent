//go:build unix

package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Unix-only: these tests start sh/sleep and check processes with syscall.Kill.

// - Verify Ctrl+C during setup aborts it gracefully (DECISIONS.md § Deferred for Later, item 15): a server that is
// still starting is stopped, SetupAgent returns context.Canceled instead of starting without it, the server that
// had already connected is kept so ShutdownAgent can close it, and nothing after the MCP steps is set up
func Test_SetupAgent_CtrlCWhileConnecting_AbortsAndKeepsConnectedServersForShutdown(t *testing.T) {
	// "a-remote" connects: an in-process MCP server over HTTP
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, nil)
	}, nil))
	defer srv.Close()
	// "b-hangs" starts (sorted after a-remote) but never answers the handshake; it records its pid first
	pidFile := filepath.Join(t.TempDir(), "hang.pid")
	cfg := config.GetDefaultConfig()
	cfg.OutBuffer, cfg.TraceEnabled, cfg.WorkDir = io.Discard, false, t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{
		"a-remote": {URL: srv.URL},
		"b-hangs":  {Command: "sh", Args: []string{"-c", `echo $$ > "$1"; exec sleep 60`, "sh", pidFile}}, // path passed as $1, not spliced into the script
	}
	interrupts := make(chan os.Signal, 1)
	go func() { // press Ctrl+C once b-hangs is running, i.e. while setup waits for its handshake
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if raw, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
				interrupts <- os.Interrupt
				return
			}
		}
	}()

	var a Agent
	err := a.SetupAgent(context.Background(), cfg, interrupts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SetupAgent err = %v, want context.Canceled", err)
	}
	if len(a.mcpConns) != 1 || a.mcpConns[0].Name != "a-remote" {
		t.Fatalf("expected a-remote kept for shutdown, got %d connections", len(a.mcpConns))
	}
	if a.session != nil {
		t.Error("setup went on after the interrupt: a chat session was created")
	}
	raw, _ := os.ReadFile(pidFile)
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("bad pid file contents %q: %v", raw, convErr)
	}
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL) // don't leave it behind
		t.Errorf("the server that was still starting (pid %d) is running after SetupAgent returned", pid)
	}

	a.ShutdownAgent(context.Background()) // closes a-remote; must not panic without a session or run trace
}

// - Verify info mode never passes a stdio server's own stderr through (a server can print tokens from its
// environment there), and says it hid it when such a server failed
func Test_Unit_PrintInfo_StdioServerStderrHidden(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	realStderr := os.Stderr
	os.Stderr = w // catches anything that would still go to the real stderr
	defer func() { os.Stderr = realStderr }()

	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{
		"leaky": {Command: "sh", Args: []string{"-c", "echo FAKE_SECRET_STDERR_456 >&2; exit 1"}},
	}
	code, out := runInfo(t, cfg, "")
	os.Stderr = realStderr
	w.Close()
	stderr, _ := io.ReadAll(r)

	if code != InfoFailed {
		t.Errorf("exit code = %d, want %d (InfoFailed)", code, InfoFailed)
	}
	if strings.Contains(out, "FAKE_SECRET_STDERR_456") || strings.Contains(string(stderr), "FAKE_SECRET_STDERR_456") {
		t.Errorf("the server's stderr leaked:\nstdout: %s\nstderr: %s", out, stderr)
	}
	if !strings.Contains(out, "error output is hidden in info mode") {
		t.Errorf("expected the hidden-stderr note in the output:\n%s", out)
	}
}
