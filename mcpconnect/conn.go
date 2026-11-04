package mcpconnect

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/procutils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type McpConnection struct {
	Name    string // server name from config
	Config  config.McpServerConfig
	Session *mcp.ClientSession
	cmd     *exec.Cmd // the server process for a local (stdio) server, nil for a remote one; Close kills its process group
}

// SetupMCPConns connects to every MCP server listed in the config, all at once (one goroutine per server), and returns
// one connection per server that connected, in name order.
//
// Notes:
//   - A server that fails is skipped: its error (naming the server) is collected, and the other servers are not
//     affected, so one bad server does not take the others' tools away. The returned error joins every failure (nil if
//     all connected).
//   - Each server gets cfg.McpConnectTimeoutSeconds to connect (0 = no limit, only in a hand-built test config;
//     checkConfig makes real configs positive). A server that takes longer is stopped and skipped like any other
//     failure; the parent ctx is not cancelled, so the caller can tell a timeout (setup continues) from Ctrl+C (abort).
//   - Waits for every goroutine before returning, also when ctx is cancelled: a connection that finishes late is still
//     returned, so the caller can close it and its process does not leak.
//   - The connections are returned even when the error is non-nil, and the caller must close them.
//   - Results are stored by the server's position in the sorted names, so the order of connections and errors is
//     repeatable whichever server finishes first.
//   - A ctx cancelled before setup starts launches no process (exec.Command ignores the context, so a process would
//     start only to be torn down). A cancel during setup (Ctrl+C) fails the handshakes in flight instead.
//   - cfg.McpServerStderr is shared by every local server and is not locked; see DECISIONS.md § PR #40 review.
func SetupMCPConns(ctx context.Context, cfg config.Config) ([]*McpConnection, error) {
	serverNames := slices.Sorted(maps.Keys(cfg.McpServers))
	mcpConns := make([]*McpConnection, len(serverNames))
	errs := make([]error, len(serverNames))
	timeout := time.Duration(cfg.McpConnectTimeoutSeconds) * time.Second

	var wg sync.WaitGroup
	for i, sname := range serverNames {
		wg.Add(1)
		go func(i int, sname string) {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				errs[i] = fmt.Errorf("setup cancelled for server %s before startup: %w", sname, err)
				return
			}
			// [agent] each server gets its own deadline, a child of ctx: a Ctrl+C still cancels them all, while one
			// slow server running out of time affects only itself
			connCtx := ctx
			if timeout > 0 {
				var cancel context.CancelFunc
				connCtx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel() // safe once connected: the session does not keep the connect context
			}
			mcpConns[i], errs[i] = ConnectMCPServer(connCtx, sname, cfg)
			// name the setting, so the user knows what to raise; only when it was our deadline, not a Ctrl+C
			if errs[i] != nil && ctx.Err() == nil && errors.Is(connCtx.Err(), context.DeadlineExceeded) {
				errs[i] = fmt.Errorf("MCP server %s did not connect within %s (mcpConnectTimeoutSeconds): %w", sname, timeout, errs[i])
			}
		}(i, sname)
	}
	wg.Wait()
	mcpConns = slices.DeleteFunc(mcpConns, func(c *McpConnection) bool { return c == nil })
	return mcpConns, errors.Join(errs...)
}

// ConnectMCPServer connects to the one MCP server named in cfg.McpServers, choosing the transport from its config.
//
// Notes:
//   - Every error names the server.
//   - stdio goes to connectLocalMCPServer and http to connectRemoteMCPServer; sse is skipped with an error because the
//     transport is deprecated and not supported.
func ConnectMCPServer(ctx context.Context, sname string, cfg config.Config) (*McpConnection, error) {
	sconfig, ok := cfg.McpServers[sname] // already of type McpServerConfig (we unmarshaled it earlier from the config JSON)
	if !ok {
		return nil, fmt.Errorf("no MCP server named %s in the config", sname)
	}
	transportType, err := sconfig.TransportType()
	if err != nil {
		return nil, fmt.Errorf("failed to determine transport type for server %s: %w", sname, err)
	}
	switch transportType {
	case config.TransportStdio:
		return connectLocalMCPServer(ctx, sname, sconfig, cfg)
	case config.TransportHTTP:
		return connectRemoteMCPServer(ctx, sname, sconfig, cfg)
	case config.TransportSSE:
		return nil, fmt.Errorf("skipping server %s: the SSE transport is deprecated and not supported", sname)
	default:
		return nil, fmt.Errorf("unsupported transport type %q for server %s", transportType, sname)
	}
}

// connectLocalMCPServer connects to a local MCP server by starting it as a child process and talking over stdio.
//
// Notes:
//   - The server runs in its own process group (procutils.StartInOwnGroup), so a Ctrl+C that cancels a chat turn does not also
//     kill it; Close kills the group.
//   - Its stderr goes to cfg.McpServerStderr, or os.Stderr when that is nil.
//   - The configured "env" is added after our own environment (npx/node need PATH, HOME, ...); os/exec uses the last
//     value of a duplicate key, so config wins. Keys are sorted so the child's environment is repeatable.
//   - On a failed handshake the SDK stops the server; procutils.KillGroup also stops anything it started.
func connectLocalMCPServer(ctx context.Context, name string, config config.McpServerConfig, cfg config.Config) (*McpConnection, error) {
	command := config.Command
	args := config.Args

	// build the MCP client
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.AgentName, Version: cfg.AgentVersion}, nil)

	// This is the command that is executed to start a local MCP server
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr
	if cfg.McpServerStderr != nil {
		cmd.Stderr = cfg.McpServerStderr
	}
	procutils.StartInOwnGroup(cmd) // so a Ctrl+C that cancels a chat turn does not also kill the server
	// Apply the configured "env": start from our own environment (npx/node need PATH, HOME, ...) and add
	// the configured entries after it. os/exec uses the last value of a duplicate key, so config wins.
	if len(config.Env) > 0 {
		cmd.Env = os.Environ()
		for _, key := range slices.Sorted(maps.Keys(config.Env)) { // sorted so the child's environment is repeatable
			cmd.Env = append(cmd.Env, key+"="+config.Env[key])
		}
	}
	transport := withDebugLogging(&mcp.CommandTransport{Command: cmd}, cfg.Debug)

	// Create client-server connection (session)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		procutils.KillGroup(cmd) // the SDK stopped the server on a failed handshake; this also stops anything it started
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}

	return &McpConnection{Name: name, Config: config, Session: session, cmd: cmd}, nil
}

// withDebugLogging wraps the transport so every JSON-RPC message is written to stderr, but only in debug mode.
//
// Notes:
//   - With debug false the transport is returned unchanged.
func withDebugLogging(t mcp.Transport, debug bool) mcp.Transport {
	if !debug {
		return t
	}
	return &mcp.LoggingTransport{Transport: t, Writer: os.Stderr}
}

// connectRemoteMCPServer connects to a remote MCP server over streamable HTTP.
//
// Notes:
//   - The HTTP client comes from newRemoteHTTPClient, so the configured headers only ever go to the configured origin.
//   - The standalone SSE GET request is disabled, since it has been deprecated.
//   - There is no process to kill, so the returned connection has no cmd.
func connectRemoteMCPServer(ctx context.Context, name string, config config.McpServerConfig, cfg config.Config) (*McpConnection, error) {
	// Implementation for connecting to a remote MCP server
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.AgentName, Version: cfg.AgentVersion}, nil)

	httpClient, err := newRemoteHTTPClient(config.URL, config.Headers)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}

	transport := withDebugLogging(&mcp.StreamableClientTransport{
		Endpoint:             config.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true, // standalone SSE via initial GET request to MCP server has been deprecated
	}, cfg.Debug)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}
	return &McpConnection{Name: name, Config: config, Session: session}, nil
}

// origin identifies where a URL points: (1) scheme, (2) lower-cased host and (3) port (default ports filled in).
// Two URLs are the same origin only if all three match, which is stricter than net/http's own redirect
// rule, where same host name but different port or scheme (https<->http) is considered same origin)
type origin struct{ scheme, host, port string }

// originOf returns the origin (scheme, lower-cased host, port) of u.
//
// Notes:
//   - A missing port is filled in for https (443) and http (80); other schemes keep an empty port.
func originOf(u *url.URL) origin {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return origin{scheme: u.Scheme, host: strings.ToLower(u.Hostname()), port: port}
}

// String formats the origin as scheme://host:port, for error messages.
//
// Notes:
//   - IPv6 hosts are bracketed (net.JoinHostPort).
func (o origin) String() string {
	return o.scheme + "://" + net.JoinHostPort(o.host, o.port)
}

const maxRedirects = 10 // net/http's default limit; a custom CheckRedirect replaces that default, so it is restated here

// newRemoteHTTPClient builds the HTTP client for a remote MCP server.
//
// Notes:
//   - Every request gets the configured headers (e.g. a Bearer token), and only the configured origin (scheme + host +
//     port) may ever receive them: redirects within that origin are followed (up to maxRedirects, keeping headers
//     and, for 307/308, the body); a redirect to any other origin, including http -> https, is refused.
//   - headerTransport enforces the same origin as a second layer.
//   - A URL that does not parse, or is not absolute, is an error.
func newRemoteHTTPClient(endpoint string, headers map[string]string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid url %q: want an absolute URL such as https://host/mcp", endpoint)
	}
	allowed := originOf(u)
	// return transport with headers included; follow same-origin redirects only, refuse cross-origin ones
	return &http.Client{
		Transport: &headerTransport{base: http.DefaultTransport, headers: headers, origin: allowed},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if got := originOf(req.URL); got != allowed {
				return fmt.Errorf("redirect to %s blocked: MCP server redirects must stay on %s", got, allowed)
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}, nil
}

// headerTransport adds fixed headers (e.g. Authorization) to every outgoing request to its origin.
// this is needed because the request shape in Go MCP SDK does not include a headers field, so we need to manually add it to each request
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	origin  origin // the only origin these headers may be sent to
}

// RoundTrip adds headers to a request before sending it off, or refuses a request that is not for h.origin.
//
// Notes:
//   - The request is cloned first, because RoundTrippers must not mutate the caller's request.
//   - It runs on every redirect hop too, which is why the origin check lives here as well as in CheckRedirect.
func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if got := originOf(req.URL); got != h.origin {
		return nil, errors.New("refusing to send configured headers to " + got.String() + ": it is not the MCP server's origin " + h.origin.String())
	}
	req = req.Clone(req.Context()) // RoundTrippers must not mutate the caller's request
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// Close closes an MCP server connection.
//
// Notes:
//   - Close ends the session (for a local server, the SDK closes its stdin, waits, then signals it) and then kills
//     whatever is left in a local server's process group, so a grandchild such as node under npx cannot outlive it.
//   - The returned error is the session's; the group kill's result is ignored.
func (s *McpConnection) Close() error {
	err := s.Session.Close()
	if s.cmd != nil {
		procutils.KillGroup(s.cmd)
	}
	return err
}
