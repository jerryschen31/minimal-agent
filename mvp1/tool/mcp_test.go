package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// [agent] Integration test against a real MCP server (needs npx + network).
// Run with: MCP_E2E=1 go test ./tool -run MCP -v
func TestMCPFilesystemServer(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil || os.Getenv("MCP_E2E") == "" {
		t.Skip("set MCP_E2E=1 and install npx to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	_ = os.WriteFile(dir+"/hello.txt", []byte("hi from mcp"), 0o644)

	c, err := ConnectMCP(ctx, "fs", "npx", "-y", "@modelcontextprotocol/server-filesystem", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	for _, tl := range tools {
		if tl.Spec().Name == "fs_read_text_file" || tl.Spec().Name == "fs_read_file" {
			args, _ := json.Marshal(map[string]string{"path": dir + "/hello.txt"})
			out, err := tl.Call(ctx, args)
			if err != nil || !strings.Contains(out, "hi from mcp") {
				t.Fatalf("call: %q %v", out, err)
			}
			return
		}
	}
	t.Fatalf("no read tool found among %d tools", len(tools))
}

// [agent] a server that exits without answering must fail the call, not hang it.
func TestMCPCrashedServerFailsPendingCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ConnectMCP(ctx, "dead", "sh", "-c", "read line; exit 1")
	if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "closed connection") {
		t.Fatalf("want closed-connection error before the test deadline, got %v (ctx %v)", err, ctx.Err())
	}
}
