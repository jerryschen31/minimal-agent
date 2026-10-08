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
	// transport := &mcp.CommandTransport{Command: cmd}
	// log all transport messages - just for debugging
	transport := &mcp.LoggingTransport{
		Transport: &mcp.CommandTransport{Command: cmd},
		Writer:    os.Stderr,
	}

	// Create client-server connection (session)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}

	return &McpConnection{Name: name, Config: config, Session: session}, nil
}

// connects to a remote MCP server
func connectRemoteMCPServer(ctx context.Context, name string, config config.McpServerConfig, cfg config.Config) (*McpConnection, error) {
	// Implementation for connecting to a remote MCP server
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.AgentName, Version: cfg.AgentVersion}, nil)

	httpClient, err := newRemoteHTTPClient(config.URL, config.Headers)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}

	transport := &mcp.LoggingTransport{
		Transport: &mcp.StreamableClientTransport{
			Endpoint:             config.URL,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true, // [agent] see DECISIONS.md: no server push needed
		},
		Writer: os.Stderr,
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server %s: %w", name, err)
	}
	return &McpConnection{Name: name, Config: config, Session: session}, nil
}

// origin identifies where a URL points: scheme, lower-cased host and port (default ports filled in).
// Two URLs are the same origin only if all three match, which is stricter than net/http's own redirect
// rule (it compares host names only, so a redirect to another port, or from https to http, keeps the token).
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
// headers (e.g. a Bearer token), but only the headers' own server may ever receive them: a redirect to a
// different origin is refused outright, because following it would replay the credentials, the session
// id and (on a 307/308) the request body, which holds tool arguments, to whoever the server named.
// Redirects within the configured origin (a trailing-slash or path change) are followed normally.
func newRemoteHTTPClient(endpoint string, headers map[string]string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid url %q: want an absolute URL such as https://host/mcp", endpoint)
	}
	allowed := originOf(u)
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
// It runs on every hop of a redirect chain, so it also refuses any request to a different origin: that is
// the second line of defence behind the client's CheckRedirect, and keeps the credentials safe even if the
// redirect policy is later changed or the transport is reused.
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
