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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// testOrigin returns the origin of rawURL, failing the test if it does not parse.
func testOrigin(t *testing.T, rawURL string) origin {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	return originOf(u)
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
	ht := &headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer abc", "X-Example": "hello"}, origin: testOrigin(t, "http://example.invalid/mcp")}
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
	ht := &headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer new"}, origin: testOrigin(t, "http://example.invalid/mcp")}
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
	ht := &headerTransport{base: base, origin: testOrigin(t, "http://example.invalid/mcp")} // nil headers map
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
	// a test can ask the child to record its pid, to check later whether the process is still alive
	if pidFile := os.Getenv("GO_MCP_HELPER_PIDFILE"); pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "helper MCP server:", err)
			os.Exit(1)
		}
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

// - Verify that when a later server fails, SetupMCPConns closes the connections it already made: the
// good local server (a child process, sorted first by name) must be gone once SetupMCPConns returns,
// whether the failure is a bad config entry or a server that cannot start
func Test_Unit_SetupMCPConns_ServerFails_ClosesAlreadyConnectedServers(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cases := map[string]config.McpServerConfig{
		"cannot start": {Command: "definitely-not-a-real-command-xyz"},
		"bad config":   {Command: "x", URL: "https://example.com/mcp"}, // both command and url: rejected before connecting
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv("GO_WANT_MCP_HELPER", "1") // the children inherit our environment
			t.Setenv("GO_MCP_HELPER_PIDFILE", pidFile)
			cfg := config.Config{McpServers: map[string]config.McpServerConfig{
				"a-good": {Command: exe, Args: []string{"-test.run=^TestHelperMCPServer$"}},
				"z-bad":  bad,
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			conns, err := SetupMCPConns(ctx, cfg)

			if err == nil || conns != nil {
				t.Fatalf("expected (nil, error), got (%v, %v)", conns, err)
			}
			if !strings.Contains(err.Error(), "z-bad") {
				t.Errorf("expected the error to name the failing server, got %q", err)
			}
			raw, readErr := os.ReadFile(pidFile)
			if readErr != nil {
				t.Fatalf("expected the good server to have started (and recorded its pid): %v", readErr)
			}
			pid, convErr := strconv.Atoi(string(raw))
			if convErr != nil {
				t.Fatalf("bad pid file contents %q: %v", raw, convErr)
			}
			// signal 0 checks that a process exists; ESRCH means it is gone
			deadline := time.Now().Add(5 * time.Second)
			for syscall.Kill(pid, 0) == nil {
				if time.Now().After(deadline) {
					syscall.Kill(pid, syscall.SIGKILL) // don't leave the leaked child behind
					t.Fatalf("the already-connected server (pid %d) is still running after SetupMCPConns failed", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

//*************************************//
// Remote MCP: redirects must stay on the configured origin
//*************************************//

// receivedRequest is what a recording server saw: the headers and body of one request.
type receivedRequest struct {
	header http.Header
	body   string
}

// recordingServer starts a server that answers 200 to everything and records each request it receives.
func recordingServer(t *testing.T) (*httptest.Server, func() []receivedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []receivedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, receivedRequest{header: r.Header.Clone(), body: string(body)})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() []receivedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]receivedRequest(nil), got...)
	}
}

var testCredentials = map[string]string{"Authorization": "Bearer SECRET", "X-Api-Key": "KEY-SECRET"}

// post sends a POST with a JSON body (a 307/308 would replay it) through the client.
func post(client *http.Client, url string) (*http.Response, error) {
	return client.Post(url, "application/json", strings.NewReader(`{"tool":"args"}`))
}

// - Verify a redirect to a different origin (another port, or another host name) is refused and the
// target receives nothing: not the token, not the API key, not the request body
func Test_Unit_RemoteClient_CrossOriginRedirect_RejectedAndNothingSent(t *testing.T) {
	target, targetHits := recordingServer(t)
	targets := map[string]string{
		"other port":      target.URL,
		"other host name": strings.Replace(target.URL, "127.0.0.1", "localhost", 1),
	}
	for name, to := range targets {
		t.Run(name, func(t *testing.T) {
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, to, http.StatusTemporaryRedirect)
			}))
			defer redirector.Close()
			client, err := newRemoteHTTPClient(redirector.URL+"/mcp", testCredentials)
			if err != nil {
				t.Fatalf("newRemoteHTTPClient: %v", err)
			}

			resp, err := post(client, redirector.URL+"/mcp")

			if err == nil {
				resp.Body.Close()
				t.Fatalf("expected the cross-origin redirect to fail, got status %d", resp.StatusCode)
			}
			if !strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), redirector.URL[len("http://"):]) {
				t.Errorf("expected an error saying the redirect was blocked and naming the allowed origin, got %q", err)
			}
			if hits := targetHits(); len(hits) != 0 {
				t.Errorf("expected the redirect target to receive nothing, got %+v", hits)
			}
		})
	}
}

// - Verify a redirect within the configured origin (a trailing-slash 307) is followed, and the headers
// and the request body are carried to the new path, so the token keeps working
func Test_Unit_RemoteClient_SameOriginRedirect_KeepsHeadersAndBody(t *testing.T) {
	var mu sync.Mutex
	var got []receivedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/mcp/", http.StatusTemporaryRedirect)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, receivedRequest{header: r.Header.Clone(), body: string(body)})
		mu.Unlock()
	}))
	defer srv.Close()
	client, err := newRemoteHTTPClient(srv.URL+"/mcp", testCredentials)
	if err != nil {
		t.Fatalf("newRemoteHTTPClient: %v", err)
	}

	resp, err := post(client, srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("expected the same-origin redirect to be followed, got %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected the redirected path to be reached once, got %d requests", len(got))
	}
	if got[0].header.Get("Authorization") != "Bearer SECRET" || got[0].header.Get("X-Api-Key") != "KEY-SECRET" {
		t.Errorf("expected the credentials on the redirected request, got %v", got[0].header)
	}
	if got[0].body != `{"tool":"args"}` {
		t.Errorf("expected the body to be replayed, got %q", got[0].body)
	}
}

// - Verify a chain that starts same-origin and then leaves (A -> A -> B) is refused at the hop that leaves,
// so the check is against the configured origin and not just the previous hop
func Test_Unit_RemoteClient_RedirectChainLeavingOrigin_Rejected(t *testing.T) {
	target, targetHits := recordingServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/step2", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	client, err := newRemoteHTTPClient(srv.URL+"/mcp", testCredentials)
	if err != nil {
		t.Fatalf("newRemoteHTTPClient: %v", err)
	}

	resp, err := post(client, srv.URL+"/mcp")

	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected the chain to be refused, got status %d", resp.StatusCode)
	}
	if hits := targetHits(); len(hits) != 0 {
		t.Errorf("expected the final target to receive nothing, got %+v", hits)
	}
}

// - Verify the redirect limit still applies: a custom CheckRedirect replaces net/http's default of 10
// hops, so an endless same-origin redirect loop must still stop
func Test_Unit_RemoteClient_EndlessSameOriginRedirects_StopAtLimit(t *testing.T) {
	var hops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hops.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/hop%d", n), http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	client, err := newRemoteHTTPClient(srv.URL+"/mcp", nil)
	if err != nil {
		t.Fatalf("newRemoteHTTPClient: %v", err)
	}

	resp, err := post(client, srv.URL+"/mcp")

	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected an endless redirect loop to fail")
	}
	if !strings.Contains(err.Error(), "10 redirects") {
		t.Errorf("expected a 'stopped after 10 redirects' error, got %q", err)
	}
	if got := hops.Load(); got > maxRedirects+1 {
		t.Errorf("expected at most %d requests, the server saw %d", maxRedirects+1, got)
	}
}

// - Verify what counts as the same origin: path changes, an explicit default port and host-name case are
// the same; a different scheme (https to http, AND http to https), port, host or subdomain are not
func Test_Unit_RemoteClient_CheckRedirect_OriginRules(t *testing.T) {
	cases := []struct {
		name        string
		configured  string
		redirectTo  string
		wantBlocked bool
	}{
		{"path change", "https://mcp.example.com/mcp", "https://mcp.example.com/mcp/", false},
		{"explicit default https port", "https://mcp.example.com/mcp", "https://mcp.example.com:443/other", false},
		{"explicit default http port", "http://mcp.example.com/mcp", "http://mcp.example.com:80/other", false},
		{"host name case", "https://MCP.Example.com/mcp", "https://mcp.example.com/mcp", false},
		{"IPv6 literal, same", "http://[::1]:8080/mcp", "http://[::1]:8080/other", false},
		{"https to http (downgrade)", "https://mcp.example.com/mcp", "http://mcp.example.com/mcp", true},
		{"http to https (upgrade, rejected for now)", "http://mcp.example.com/mcp", "https://mcp.example.com/mcp", true},
		{"different port", "https://mcp.example.com/mcp", "https://mcp.example.com:8443/mcp", true},
		{"different host", "https://mcp.example.com/mcp", "https://evil.example.net/mcp", true},
		{"subdomain", "https://mcp.example.com/mcp", "https://evil.mcp.example.com/mcp", true},
		{"look-alike prefix", "https://mcp.example.com/mcp", "https://mcp.example.com.evil.net/mcp", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := newRemoteHTTPClient(tc.configured, nil)
			if err != nil {
				t.Fatalf("newRemoteHTTPClient(%q): %v", tc.configured, err)
			}
			req, err := http.NewRequest(http.MethodGet, tc.redirectTo, nil)
			if err != nil {
				t.Fatalf("building request: %v", err)
			}

			err = client.CheckRedirect(req, nil)

			if tc.wantBlocked && err == nil {
				t.Errorf("expected a redirect from %s to %s to be blocked", tc.configured, tc.redirectTo)
			}
			if !tc.wantBlocked && err != nil {
				t.Errorf("expected a redirect from %s to %s to be allowed, got %v", tc.configured, tc.redirectTo, err)
			}
		})
	}
}

// - Verify the transport itself refuses a request for another origin (second line of defence) without
// calling the base transport, and still adds the headers for the configured origin
func Test_Unit_HeaderTransport_OtherOrigin_RefusedWithoutSending(t *testing.T) {
	for _, other := range []string{
		"https://other.example.com/mcp",    // other host
		"http://mcp.example.com/mcp",       // other scheme
		"https://mcp.example.com:8443/mcp", // other port
	} {
		t.Run(other, func(t *testing.T) {
			base := &stubRoundTripper{resp: &http.Response{StatusCode: 200}}
			ht := &headerTransport{base: base, headers: testCredentials, origin: testOrigin(t, "https://mcp.example.com/mcp")}
			req, _ := http.NewRequest(http.MethodPost, other, nil)

			resp, err := ht.RoundTrip(req)

			if err == nil || resp != nil {
				t.Fatalf("expected the request to be refused, got (%v, %v)", resp, err)
			}
			if base.got != nil {
				t.Errorf("expected the base transport not to be called, but it saw %v", base.got.URL)
			}
		})
	}

	base := &stubRoundTripper{resp: &http.Response{StatusCode: 200}}
	ht := &headerTransport{base: base, headers: testCredentials, origin: testOrigin(t, "https://mcp.example.com/mcp")}
	req, _ := http.NewRequest(http.MethodPost, "https://mcp.example.com:443/elsewhere", nil)
	if _, err := ht.RoundTrip(req); err != nil {
		t.Fatalf("expected a same-origin request to be sent, got %v", err)
	}
	if base.got.Header.Get("Authorization") != "Bearer SECRET" {
		t.Errorf("expected the Authorization header on a same-origin request, got %v", base.got.Header)
	}
}

// - Verify connectRemoteMCPServer is wired to this: a server that redirects to another origin fails to
// connect with an error naming the server, and the real server it pointed at sees no request at all
func Test_Unit_ConnectRemoteMCP_CrossOriginRedirect_FailsAndSendsNothing(t *testing.T) {
	targetURL, targetRequests := startFakeRemoteMCP(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := connectRemoteMCPServer(ctx, "redirected", config.McpServerConfig{URL: redirector.URL, Headers: testCredentials}, config.Config{})

	if err == nil || s != nil {
		t.Fatalf("expected (nil, error), got (%v, %v)", s, err)
	}
	if !strings.Contains(err.Error(), "redirected") {
		t.Errorf("expected the error to name the server, got %q", err)
	}
	if reqs := targetRequests(); len(reqs) != 0 {
		t.Errorf("expected the redirect target to receive no requests, got %d", len(reqs))
	}
}

// - Verify an endpoint that is not an absolute URL is rejected up front with a clear error
func Test_Unit_NewRemoteHTTPClient_InvalidEndpoint_ReturnsError(t *testing.T) {
	for _, endpoint := range []string{"", "::bad", "/relative/path", "mcp.example.com/mcp", "https:///nohost"} {
		t.Run(endpoint, func(t *testing.T) {
			if client, err := newRemoteHTTPClient(endpoint, nil); err == nil {
				t.Errorf("expected an error for %q, got a client %v", endpoint, client)
			}
		})
	}
}
