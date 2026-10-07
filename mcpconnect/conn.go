package mcpconnect

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type McpConnection struct {
	Name    string // server name from config
	Config  config.McpServerConfig
	Session *mcp.ClientSession
}

// connects to all MCP servers listed in input config, if possible - returns list of MCP server objects
func SetupMCPConns(ctx context.Context, cfg config.Config) ([]*McpConnection, error) {
	var mcpConns []*McpConnection
	// note sconfig is already of type McpServerConfig (we unmarshaled it earlier from the config JSON)
	for sname, sconfig := range cfg.McpServers {
		transportType, err := sconfig.TransportType()
		// if transport type is not supported or error in config, do not connect to this MCP server
		if err != nil {
			return nil, fmt.Errorf("failed to determine transport type for server %s: %w", sname, err)
		}
		switch transportType {
		case config.TransportStdio:
			mcpConn, err := connectLocalMCPServer(ctx, sname, sconfig, cfg)
			if err != nil {
				return nil, err
			}
			mcpConns = append(mcpConns, mcpConn)
		case config.TransportHTTP:
			mcpConn, err := connectRemoteMCPServer(ctx, sname, sconfig, cfg)
			if err != nil {
				return nil, err
			}
			mcpConns = append(mcpConns, mcpConn)
		default:
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

	httpClient := &http.Client{
		Transport: &headerTransport{base: http.DefaultTransport, headers: config.Headers},
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

// headerTransport adds fixed headers (e.g. Authorization) to every outgoing request.
// this is needed because the request shape in Go MCP SDK does not include a headers field, so we need to manually add it to each request
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

// adds headers to a request before sending it off
// this is needed because the request shape in Go MCP SDK does not include a headers field, so we need to manually add it to each request
func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
