package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mvp1/agent"
)

// [agent] MCP is a minimal Model Context Protocol client over stdio (JSON-RPC
// 2.0, newline-delimited). It exposes every tool the server advertises as an
// agent.Tool, so any of the hundreds of existing MCP servers (any language)
// plugs in with one config line. Kept dependency-free on purpose; swap in
// github.com/modelcontextprotocol/go-sdk when you need HTTP transports,
// resources, prompts, or sampling.
type MCP struct {
	Name    string
	cmd     *exec.Cmd
	in      io.WriteCloser
	mu      sync.Mutex // serialises writes; tools may be called concurrently
	seq     atomic.Int64
	pending sync.Map      // id → chan rpcMsg
	done    chan struct{} // closed when the server's stdout ends (exit/crash)
	readErr error         // why, valid after done
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ConnectMCP starts the server process and completes the initialize handshake.
func ConnectMCP(ctx context.Context, name, command string, args ...string) (*MCP, error) {
	cmd := exec.CommandContext(ctx, command, args...) // [agent] a cancelled agent never orphans its server
	cmd.WaitDelay = time.Second
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	c := &MCP{Name: name, cmd: cmd, in: in, done: make(chan struct{})}
	go c.readLoop(out)
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "mvp1-agent", "version": "0.1.0"}}); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.send(rpcMsg{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Tools lists the server's tools, namespaced as <server>_<tool>.
func (c *MCP) Tools(ctx context.Context) ([]agent.Tool, error) {
	res, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []struct {
			Name, Description string
			InputSchema       json.RawMessage `json:"inputSchema"`
		}
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	tools := make([]agent.Tool, 0, len(out.Tools))
	for _, t := range out.Tools {
		tools = append(tools, mcpTool{c: c, remote: t.Name, spec: agent.ToolSpec{
			Name: c.Name + "_" + t.Name, Description: t.Description, Schema: t.InputSchema}})
	}
	return tools, nil
}

func (c *MCP) Close() error {
	_ = c.in.Close()
	return c.cmd.Wait()
}

func (c *MCP) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	defer func() {
		// [agent] stdout closed: the server exited or crashed. Fail every in-flight
		// and future call instead of leaving them blocked forever.
		c.readErr = fmt.Errorf("mcp %s: server closed connection (%v)", c.Name, orEOF(sc.Err()))
		close(c.done)
	}()
	for sc.Scan() {
		var m rpcMsg
		// [agent] only responses to our requests matter here; notifications and
		// server→client requests (sampling, roots) are out of scope for the MVP.
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil || m.Method != "" {
			continue
		}
		if ch, ok := c.pending.LoadAndDelete(*m.ID); ok {
			ch.(chan rpcMsg) <- m
		}
	}
}

func (c *MCP) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.seq.Add(1)
	ch := make(chan rpcMsg, 1)
	c.pending.Store(id, ch)
	if err := c.send(rpcMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.pending.Delete(id)
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, fmt.Errorf("mcp %s %s: %s (%d)", c.Name, method, m.Error.Message, m.Error.Code)
		}
		return m.Result, nil
	case <-c.done:
		c.pending.Delete(id)
		return nil, c.readErr
	case <-ctx.Done():
		c.pending.Delete(id)
		return nil, ctx.Err()
	}
}

func orEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

func (c *MCP) send(m rpcMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.in.Write(append(b, '\n'))
	return err
}

type mcpTool struct {
	c      *MCP
	remote string
	spec   agent.ToolSpec
}

func (t mcpTool) Spec() agent.ToolSpec { return t.spec }

func (t mcpTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := t.c.call(ctx, "tools/call", map[string]any{"name": t.remote, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct{ Type, Text string }
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	var parts []string
	for _, p := range out.Content {
		if p.Type == "text" { // [agent] images/resources dropped in the MVP; add blocks to Message to carry them
			parts = append(parts, p.Text)
		}
	}
	text := Truncate(strings.Join(parts, "\n"), maxOutput)
	if out.IsError {
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}
