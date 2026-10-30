package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/trace"
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
		err := a.SetupAgent(context.Background(), config.Config{MaxSteps: 10, ResponseTimeoutSeconds: 300, MCPResultMaxBytes: n}, nil)
		if err == nil || !strings.Contains(err.Error(), "mcpResultMaxBytes") {
			t.Errorf("MCPResultMaxBytes %d: err = %v, want an mcpResultMaxBytes error", n, err)
		}
	}
}

// - Verify checkConfig also runs contextwindow.CheckConfig, returning its error for a bad context window setting
// (so SetupAgent and info mode both report it), and passes the defaults
func Test_Unit_CheckConfig_ReturnsContextWindowError(t *testing.T) {
	cfg := config.GetDefaultConfig()
	if err := checkConfig(cfg); err != nil {
		t.Fatalf("expected the default config to pass, got %v", err)
	}
	cfg.ContextWindowMessages = 2
	err := checkConfig(cfg)
	want := contextwindow.CheckConfig(cfg)
	if err == nil || want == nil || err.Error() != want.Error() {
		t.Errorf("expected contextwindow.CheckConfig's error %v, got %v", want, err)
	}
}

// - Verify checkConfig rejects a non-positive responseTimeoutSeconds (0 would time out every model call at once),
// naming the field, while the rest of the config is the valid default
func Test_Unit_CheckConfig_RejectsNonPositiveResponseTimeout(t *testing.T) {
	for _, n := range []int{0, -1} {
		cfg := config.GetDefaultConfig()
		cfg.ResponseTimeoutSeconds = n
		err := checkConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "responseTimeoutSeconds") {
			t.Errorf("ResponseTimeoutSeconds %d: err = %v, want a responseTimeoutSeconds error", n, err)
		}
	}
}

// - Verify SetupAgent prints a failed remote MCP server's notice without the token in its URL: the connect error
// quotes the URL, so SetupAgent must hide it (as info mode does) before writing to OutBuffer
func Test_SetupAgent_FailedRemoteMCPServer_HidesTokenInURL(t *testing.T) {
	// [agent] a closed server: connection refused, and that error quotes the URL (a 500 reply would not)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	cfg := denyWiringConfig(t)
	var out bytes.Buffer
	cfg.OutBuffer = &out
	cfg.McpServers = map[string]config.McpServerConfig{
		"remote": {URL: srv.URL + "/mcp?token=FAKE_SECRET_TOKEN_123"},
	}

	var a Agent
	err := a.SetupAgent(context.Background(), cfg, nil)
	defer a.ShutdownAgent(context.Background())

	if err != nil {
		t.Fatalf("SetupAgent: %v (a failed MCP server should only be reported)", err)
	}
	if !strings.Contains(out.String(), "MCP servers that failed to connect") {
		t.Errorf("expected the failure notice in output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "FAKE_SECRET_TOKEN_123") {
		t.Errorf("output leaked the token:\n%s", out.String())
	}
	if host := strings.TrimPrefix(srv.URL, "http://"); !strings.Contains(out.String(), "http://"+host+"/[rest hidden]") {
		t.Errorf("expected the URL shown as scheme and host only:\n%s", out.String())
	}
}
