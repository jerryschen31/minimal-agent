package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// - Verify run_end is recorded by ShutdownAgent after background work has drained: a compaction still running when
// RunAgent returns must land its event before run_end, and RunAgent alone must not end the run
func Test_Shutdown_RecordsRunEndAfterBackgroundCompaction(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	a := &Agent{session: fx.Session, agentMode: "chat"}

	if err := a.RunAgent(context.Background(), nil); err != nil { // empty input: the loop ends at EOF
		t.Fatal(err)
	}
	if slices.Contains(spy.eventTypes(), trace.RunEnd) {
		t.Fatalf("RunAgent recorded run_end before shutdown: %v", spy.eventTypes())
	}

	release := make(chan struct{})
	fx.Context.CompactWG.Add(1)
	go func() { // stands in for an auto-compaction that is still waiting on the model
		defer fx.Context.CompactWG.Done()
		<-release
		spy.Record(spy.RunID(), trace.Compaction, trace.CompactionData{Trigger: "auto"})
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.ShutdownAgent(context.Background())
	}()
	close(release)
	<-done

	types := spy.eventTypes()
	want := []trace.EventType{trace.RunStart, trace.Compaction, trace.RunEnd}
	if !slices.Equal(types, want) {
		t.Errorf("events = %v, want %v", types, want)
	}
	a.ShutdownAgent(context.Background()) // a second shutdown writes nothing more
	if len(spy.eventTypes()) != len(want) {
		t.Errorf("second ShutdownAgent recorded more events: %v", spy.eventTypes())
	}
}

// - Verify a non-positive mcpResultMaxBytes fails setup instead of silently dropping every MCP tool
func Test_SetupAgent_RejectsNonPositiveMCPResultCap(t *testing.T) {
	for _, n := range []int{0, -1} {
		var a Agent
		err := a.SetupAgent(context.Background(), config.Config{MaxSteps: 10, MCPResultMaxBytes: n}, nil)
		if err == nil || !strings.Contains(err.Error(), "mcpResultMaxBytes") {
			t.Errorf("MCPResultMaxBytes %d: err = %v, want an mcpResultMaxBytes error", n, err)
		}
	}
}

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
		"b-hangs":  {Command: "sh", Args: []string{"-c", "echo $$ > " + pidFile + "; exec sleep 60"}},
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
