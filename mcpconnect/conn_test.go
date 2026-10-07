package mcpconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newFakeMCPServer builds an in-process MCP server with one tool, "echo", that always replies "hi".
func newFakeMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "replies hi"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}, nil, nil
		})
	return server
}

type recordedRequest struct {
	method string
	header http.Header
}

// startFakeRemoteMCP serves newFakeMCPServer over Streamable HTTP and records every HTTP request it
// receives (method + headers). It speaks only the classic initialize handshake. It returns the server URL and a function that returns the recording so far.
func startFakeRemoteMCP(t *testing.T) (string, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var recorded []recordedRequest

	sdkHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newFakeMCPServer() }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = append(recorded, recordedRequest{method: r.Method, header: r.Header.Clone()})
		mu.Unlock()

		// Reject the new-spec "server/discover" handshake like DeepWiki does, so the client falls back to the
		// classic "initialize" handshake (protocol 2025-11-25). Only on that path does the client try to open the
		// standalone GET stream (it is removed in 2026-07-28+), which is what DisableStandaloneSSE must prevent.
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"server/discover"`)) {
				http.Error(w, "Bad Request: Unsupported protocol version: 2026-07-28", http.StatusBadRequest)
				return
			}
		}
		sdkHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), recorded...)
	}
}

// checkEchoRoundTrip lists the fake server's tools and calls echo, proving the connection really works.
func checkEchoRoundTrip(ctx context.Context, t *testing.T, s *McpConnection) {
	t.Helper()
	tools, err := GetMCPTools(ctx, s)
	if err != nil {
		t.Fatalf("GetMCPTools: %v", err)
	}
	if len(tools) != 1 || tools[0].GetToolDefinition().Function.Name != s.Name+"_echo" {
		t.Fatalf("expected one tool named %s_echo, got %+v", s.Name, tools)
	}
	got, err := tools[0].CallTool(ctx, json.RawMessage(`{}`))
	if err != nil || got != "hi" {
		t.Fatalf("expected echo to return %q, got (%q, %v)", "hi", got, err)
	}
}

// stubRoundTripper records the request it receives and returns a canned response or error.
type stubRoundTripper struct {
	got  *http.Request
	resp *http.Response
	err  error
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.got = req
	return s.resp, s.err
}

// - Verify a remote server with no configured headers connects, lists and calls tools, and sends no
// Authorization header, and (DisableStandaloneSSE) never opens the long-lived GET stream.
func Test_Unit_ConnectRemoteMCP_NoHeaders_WorksAndSendsNoAuthorization(t *testing.T) {
	url, requests := startFakeRemoteMCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectRemoteMCPServer(ctx, "remote", config.McpServerConfig{URL: url}, config.Config{})
	if err != nil {
		t.Fatalf("connectRemoteMCPServer: %v", err)
	}
	checkEchoRoundTrip(ctx, t, s)
	if err := s.Close(); err != nil {
		t.Logf("Close: %v", err)
	}

	got := requests()
	if len(got) == 0 {
		t.Fatal("expected the fake server to receive requests")
	}
	for i, r := range got {
		if r.method == http.MethodGet {
			t.Errorf("request %d was a GET; expected DisableStandaloneSSE to prevent the standalone stream", i)
		}
		if a := r.header.Get("Authorization"); a != "" {
			t.Errorf("request %d (%s) carried Authorization %q though none was configured", i, r.method, a)
		}
	}
}

// - Verify configured headers are sent on every HTTP request (initialize, tools/list, tools/call, close),
// and still no GET.
func Test_Unit_ConnectRemoteMCP_WithHeaders_SentOnEveryRequest(t *testing.T) {
	url, requests := startFakeRemoteMCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := config.McpServerConfig{URL: url, Headers: map[string]string{
		"Authorization": "Bearer test-token",
		"X-Example":     "hello",
	}}

	s, err := connectRemoteMCPServer(ctx, "remote", cfg, config.Config{})
	if err != nil {
		t.Fatalf("connectRemoteMCPServer: %v", err)
	}
	checkEchoRoundTrip(ctx, t, s)
	if err := s.Close(); err != nil {
		t.Logf("Close: %v", err)
	}

	got := requests()
	// initialize + initialized + tools/list + tools/call is at least 4 POSTs
	if len(got) < 4 {
		t.Fatalf("expected at least 4 requests, got %d", len(got))
	}
	for i, r := range got {
		if r.method == http.MethodGet {
			t.Errorf("request %d was a GET; expected DisableStandaloneSSE to prevent the standalone stream", i)
		}
		for k, want := range cfg.Headers {
			if v := r.header.Get(k); v != want {
				t.Errorf("request %d (%s): header %s = %q, want %q", i, r.method, k, v, want)
			}
		}
	}
}

// - Verify connectRemoteMCPServer wraps a connection failure with the server name instead of panicking.
func Test_Unit_ConnectRemoteMCP_Unreachable_ReturnsErrorNamingServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening now
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectRemoteMCPServer(ctx, "gone", config.McpServerConfig{URL: url}, config.Config{})

	if err == nil || s != nil {
		t.Fatalf("expected (nil, error), got (%v, %v)", s, err)
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("expected the error to name the server, got %q", err)
	}
}

// - Verify headerTransport adds every header to the forwarded request, leaves the caller's request
// untouched (the RoundTripper contract), and returns the base transport's response.
func Test_Unit_HeaderTransport_AddsHeaders_WithoutMutatingOriginal(t *testing.T) {
	want := &http.Response{StatusCode: http.StatusTeapot}
	base := &stubRoundTripper{resp: want}
	ht := &headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer abc", "X-Example": "hello"}}
	orig, _ := http.NewRequest(http.MethodPost, "http://example.invalid/mcp", nil)
	orig.Header.Set("Content-Type", "application/json")

	resp, err := ht.RoundTrip(orig)

	if err != nil || resp != want {
		t.Fatalf("expected the base response and no error, got (%v, %v)", resp, err)
	}
	if base.got == nil {
		t.Fatal("base transport was never called")
	}
	if base.got.Header.Get("Authorization") != "Bearer abc" || base.got.Header.Get("X-Example") != "hello" {
		t.Errorf("forwarded request is missing headers: %v", base.got.Header)
	}
	if base.got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("existing headers should be kept, got %v", base.got.Header)
	}
	if orig.Header.Get("Authorization") != "" || orig.Header.Get("X-Example") != "" {
		t.Errorf("caller's request was mutated: %v", orig.Header)
	}
}

// - Verify configured headers replace (Set, not Add) a same-named header already on the request.
func Test_Unit_HeaderTransport_ReplacesExistingHeader(t *testing.T) {
	base := &stubRoundTripper{resp: &http.Response{StatusCode: 200}}
	ht := &headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer new"}}
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/mcp", nil)
	req.Header.Set("Authorization", "Bearer old")

	if _, err := ht.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if vals := base.got.Header.Values("Authorization"); len(vals) != 1 || vals[0] != "Bearer new" {
		t.Errorf("expected exactly [Bearer new], got %v", vals)
	}
}

// - Verify nil headers pass the request through, and a base error is returned unchanged.
func Test_Unit_HeaderTransport_NilHeaders_PassesThrough_AndPropagatesError(t *testing.T) {
	boom := errors.New("network down")
	base := &stubRoundTripper{err: boom}
	ht := &headerTransport{base: base} // nil headers map
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/mcp", nil)

	_, err := ht.RoundTrip(req)

	if !errors.Is(err, boom) {
		t.Errorf("expected the base error, got %v", err)
	}
	if base.got == nil {
		t.Error("base transport was never called")
	}
}

// TestHelperMCPServer is not a real test: connectLocalMCPServer's test re-runs this test binary as the child
// process and this function turns that child into a stdio MCP server. It is skipped in normal runs.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		t.Skip("helper process for the connectLocalMCPServer test, not a real test")
	}
	if err := newFakeMCPServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "helper MCP server:", err)
		os.Exit(1)
	}
	os.Exit(0) // exit here so the test framework does not print PASS onto the protocol stream (stdout)
}

// - Verify a local stdio server (this test binary re-run as the helper above) connects, lists and calls tools.
func Test_Unit_ConnectLocalMCP_StdioServer_ListsAndCallsTools(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// the child inherits our environment (connectLocalMCPServer does not apply config.Env), so set it here
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := connectLocalMCPServer(ctx, "local", config.McpServerConfig{Command: exe, Args: []string{"-test.run=^TestHelperMCPServer$"}}, config.Config{})
	if err != nil {
		t.Fatalf("connectLocalMCPServer: %v", err)
	}
	defer s.Close()

	checkEchoRoundTrip(ctx, t, s)
}

// - Verify a command that cannot start returns an error naming the server.
func Test_Unit_ConnectLocalMCP_BadCommand_ReturnsErrorNamingServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectLocalMCPServer(ctx, "broken", config.McpServerConfig{Command: "definitely-not-a-real-command-xyz"}, config.Config{})

	if err == nil || s != nil {
		t.Fatalf("expected (nil, error), got (%v, %v)", s, err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("expected the error to name the server, got %q", err)
	}
}
