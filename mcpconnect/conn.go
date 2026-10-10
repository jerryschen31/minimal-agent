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

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type McpConnection struct {
	Name    string // server name from config
	Config  config.McpServerConfig
	Session *mcp.ClientSession
}

// SetupMCPConns connects to all MCP servers listed in the config, in name order, and returns one connection per server.
// If any server fails, the connections already made are closed (so no child process outlives the failed setup) and the error is returned.
func SetupMCPConns(ctx context.Context, cfg config.Config) ([]*McpConnection, error) {
	var mcpConns []*McpConnection
	closeAll := func() {
		for _, c := range mcpConns {
			c.Close()
		}
	}
	// map order is random; sorted names make which server fails first (and the error shown) repeatable
	for _, sname := range slices.Sorted(maps.Keys(cfg.McpServers)) {
		sconfig := cfg.McpServers[sname] // already of type McpServerConfig (we unmarshaled it earlier from the config JSON)
		transportType, err := sconfig.TransportType()
		// if transport type is not supported or error in config, do not connect to this MCP server
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("failed to determine transport type for server %s: %w", sname, err)
		}
		switch transportType {
		case config.TransportStdio:
			mcpConn, err := connectLocalMCPServer(ctx, sname, sconfig, cfg)
			if err != nil {
				closeAll()
				return nil, err
			}
			mcpConns = append(mcpConns, mcpConn)
		case config.TransportHTTP:
			mcpConn, err := connectRemoteMCPServer(ctx, sname, sconfig, cfg)
			if err != nil {
				closeAll()
				return nil, err
			}
			mcpConns = append(mcpConns, mcpConn)
		case config.TransportSSE:
			fmt.Fprintf(os.Stderr, "warning: skipping deprecated SSE MCP server %s\n", sname)
		default:
			closeAll()
			return nil, fmt.Errorf("unsupported transport type %q for server %s", transportType, sname)
		}
	}
	return mcpConns, nil
}

// connects to a local MCP server
func connectLocalMCPServer(ctx context.Context, name string, config config.McpServerConfig, cfg config.Config) (*McpConnection, error) {
	command := config.Command
	args := config.Args

	// build the MCP client
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.AgentName, Version: cfg.AgentVersion}, nil)

	// This is the command that is executed to start a local MCP server
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr
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
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}

	return &McpConnection{Name: name, Config: config, Session: session}, nil
}

// withDebugLogging wraps the transport so every JSON-RPC message is written to stderr, but only in debug mode.
func withDebugLogging(t mcp.Transport, debug bool) mcp.Transport {
	if !debug {
		return t
	}
	return &mcp.LoggingTransport{Transport: t, Writer: os.Stderr}
}

// connects to a remote MCP server
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

func (o origin) String() string {
	return o.scheme + "://" + net.JoinHostPort(o.host, o.port)
}

const maxRedirects = 10 // net/http's default limit; a custom CheckRedirect replaces that default, so it is restated here

// newRemoteHTTPClient builds the HTTP client for a remote MCP server. Every request gets the configured
// headers (e.g. a Bearer token), but only the headers' own server may ever receive them (no redirects).
func newRemoteHTTPClient(endpoint string, headers map[string]string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid url %q: want an absolute URL such as https://host/mcp", endpoint)
	}
	allowed := originOf(u)
	// return transport with headers included, block redirects
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

// adds headers to a request before sending it off, or refuses a request that is not for h.origin
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

// closes an MCP server connection
func (s *McpConnection) Close() error {
	return s.Session.Close()
}
