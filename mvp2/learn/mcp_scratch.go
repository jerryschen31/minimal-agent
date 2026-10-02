package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpServer struct {
	name    string
	session *mcp.ClientSession
}

func connectMCP(ctx context.Context, name, command string, args ...string) (*mcpServer, error) {

	// build the MCP client
	client := mcp.NewClient(&mcp.Implementation{Name: "minagent", Version: "0.1.0"}, nil)

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

	return &mcpServer{name: name, session: session}, nil
}

func (s *mcpServer) Close() error {
	return s.session.Close()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	lctx, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()

	server, err := connectMCP(lctx, "test-mcp-server", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer server.Close()

	// sends tools/list to get list of tools from MCP server
	res, err := server.session.ListTools(lctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing tools: %v\n", err)
		return
	}

	fmt.Printf("res:")
	b, _ := json.MarshalIndent(res, "", " ")
	fmt.Println(string(b))

	// sends tools/call to call a particular tool
	out, err := server.session.CallTool(lctx, &mcp.CallToolParams{
		Name:      "list_directory",
		Arguments: map[string]any{"path": "/tmp"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error calling tool: %v\n", err)
		return
	}

	fmt.Printf("out:")
	b2, _ := json.MarshalIndent(out, "", " ")
	fmt.Println(string(b2))

}
