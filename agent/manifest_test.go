package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// secretConfig returns a config whose MCP servers and base URL carry a token in every place a token tends to hide.
func secretConfig(token string) config.Config {
	cfg := config.GetDefaultConfig(config.ModeAuto)
	cfg.BaseURL = "https://user:" + token + "@api.example.com/v1?key=" + token
	cfg.McpServers = map[string]config.McpServerConfig{
		"gh":  {Command: "gh-mcp", Args: []string{"--token", token}, Env: map[string]string{"GH_TOKEN": token}},
		"web": {URL: "https://mcp.example.com/?t=" + token, Headers: map[string]string{"Authorization": "Bearer " + token}},
	}
	return cfg
}

//   - Verify configSnapshot gives the same bytes for the same config (map order doesn't leak in), and different bytes
//     when the model, an ask rule or the system prompt changes
func Test_Unit_ConfigSnapshot_Deterministic(t *testing.T) {
	base := secretConfig("tok_AAAA")
	first, err := configSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if again, _ := configSnapshot(secretConfig("tok_AAAA")); !bytes.Equal(again, first) {
			t.Fatalf("snapshot not deterministic:\n%s\n---\n%s", first, again)
		}
	}
	for name, change := range map[string]func(*config.Config){
		"model":         func(c *config.Config) { c.Model = "other" },
		"ask rule":      func(c *config.Config) { c.AskRules = append(slices.Clone(c.AskRules), "x*") },
		"system prompt": func(c *config.Config) { c.SystemPrompt += "!" },
	} {
		cfg := secretConfig("tok_AAAA")
		change(&cfg)
		if got, _ := configSnapshot(cfg); bytes.Equal(got, first) {
			t.Errorf("changing the %s left the snapshot the same", name)
		}
	}
	if !json.Valid(first) {
		t.Errorf("snapshot is not valid JSON:\n%s", first)
	}
}

//   - Verify no token from the MCP servers' args, env, url or headers, or from the base URL, is in the snapshot; the
//     servers keep their names and transport types; and changing only the token leaves the snapshot the same
func Test_Unit_ConfigSnapshot_NoSecrets(t *testing.T) {
	a, _ := configSnapshot(secretConfig("tok_AAAA"))
	b, _ := configSnapshot(secretConfig("tok_BBBB"))
	if strings.Contains(string(a), "tok_AAAA") {
		t.Errorf("the token is in the snapshot:\n%s", a)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("changing only the token changed the snapshot")
	}
	for _, want := range []string{`"gh": {`, `"type": "stdio"`, `"web": {`, `"type": "http"`, `api.example.com`} {
		if !strings.Contains(string(a), want) {
			t.Errorf("snapshot lacks %q:\n%s", want, a)
		}
	}
	for _, gone := range []string{"gh-mcp", "GH_TOKEN", "Authorization", "--token"} {
		if strings.Contains(string(a), gone) {
			t.Errorf("snapshot keeps %q:\n%s", gone, a)
		}
	}
}

// toolDef builds a tool definition for toolsDigest tests.
func toolDef(name, desc, params string) model.ToolDef {
	return model.NewToolDef(name, desc, json.RawMessage(params))
}

//   - Verify toolsDigest sorts the names and gives the same hash whatever the order or the schema's whitespace, and a
//     different hash when a description or a parameter changes
func Test_Unit_ToolsDigest(t *testing.T) {
	a := toolDef("read_file", "Reads a file.", `{"type":"object"}`)
	b := toolDef("list_files", "Lists files.", `{"type":"object"}`)
	names, h1 := toolsDigest([]model.ToolDef{a, b})
	if !slices.Equal(names, []string{"list_files", "read_file"}) {
		t.Errorf("names = %v", names)
	}
	if _, h := toolsDigest([]model.ToolDef{b, toolDef("read_file", "Reads a file.", `{ "type" : "object" }`)}); h != h1 {
		t.Error("order or whitespace changed the hash")
	}
	if _, h := toolsDigest([]model.ToolDef{toolDef("read_file", "Reads one file.", `{"type":"object"}`), b}); h == h1 {
		t.Error("a reworded description left the hash the same")
	}
	if _, h := toolsDigest([]model.ToolDef{toolDef("read_file", "Reads a file.", `{"type":"string"}`), b}); h == h1 {
		t.Error("a changed parameter left the hash the same")
	}
}

// connectInMemoryMCP connects to an in-process MCP server that reports name and version, and returns the connection.
func connectInMemoryMCP(t *testing.T, label, name, version string) *mcpconnect.McpConnection {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); ss.Close() })
	return &mcpconnect.McpConnection{Name: label, Config: config.McpServerConfig{Command: "x"}, Session: session}
}

//   - Verify buildManifest fills the plan identity, harness, pid, absolute work dir, provider, model, the base URL as
//     DisplayURL shows it, the worker prompt's hash (the system prompt the session sends) and the summarizer's, the session's tools, and each MCP server's transport
//     with the name and version it reported (sorted by name); and returns the config snapshot
//   - Verify the manifest written by NewRun with that snapshot carries the config's hash, and Load finds the run
func Test_Unit_BuildManifest(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	cfg := secretConfig("tok_AAAA")
	cfg.WorkDir = t.TempDir()
	fx.Session.Config = cfg
	fx.Session.SystemMsg = createSystemMessage(workerPrompt) // as setupAutoMode sets it
	conns := []*mcpconnect.McpConnection{
		connectInMemoryMCP(t, "web", "web-server", "2.0.0"),
		connectInMemoryMCP(t, "fs", "secure-filesystem-server", "0.2.0"),
	}
	planPath := filepath.Join(t.TempDir(), "setup.md")
	os.WriteFile(planPath, []byte("# Plan: x\n"), 0o644)
	ref, err := state.NewPlanRef("7f3a9c1e5b2d8046", planPath)
	if err != nil {
		t.Fatal(err)
	}

	m, snapshot, err := buildManifest(fx.Session, conns, ref)
	if err != nil {
		t.Fatal(err)
	}
	if m.PlanID != ref.ID || m.PlanPath != ref.Path || m.Harness.Name != cfg.AgentName || m.Harness.Version != cfg.AgentVersion {
		t.Errorf("plan/harness = %q %q %+v", m.PlanID, m.PlanPath, m.Harness)
	}
	if m.PID != os.Getpid() || !filepath.IsAbs(m.WorkDir) || m.Provider != cfg.Provider || m.Model != cfg.Model {
		t.Errorf("pid/workdir/provider/model = %d %q %q %q", m.PID, m.WorkDir, m.Provider, m.Model)
	}
	if m.BaseURL != config.DisplayURL(cfg.BaseURL) || strings.Contains(m.BaseURL, "tok_AAAA") {
		t.Errorf("base URL = %q", m.BaseURL)
	}
	if m.Prompts["worker"] != state.SHA256([]byte(workerPrompt)) || m.Prompts["summarizer"] != state.SHA256([]byte(cfg.SummarizerPrompt)) {
		t.Errorf("prompts = %v", m.Prompts)
	}
	if names, hash := toolsDigest(fx.Session.Tools.GetToolDefs()); !slices.Equal(m.Tools, names) || m.ToolsSHA256 != hash {
		t.Errorf("tools = %v %s", m.Tools, m.ToolsSHA256)
	}
	want := []state.MCPServer{
		{Name: "fs", Transport: "stdio", Server: "secure-filesystem-server", Version: "0.2.0"},
		{Name: "web", Transport: "stdio", Server: "web-server", Version: "2.0.0"},
	}
	if !slices.Equal(m.MCPServers, want) {
		t.Errorf("mcp servers = %+v, want %+v", m.MCPServers, want)
	}
	if again, _ := configSnapshot(cfg); !bytes.Equal(snapshot, again) {
		t.Error("buildManifest's snapshot differs from configSnapshot's")
	}

	home, _ := state.OpenHome(t.TempDir())
	j, r, err := home.NewRun(m, []byte("# Plan: x\n"), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	written, _ := os.ReadFile(filepath.Join(r.Dir, "config.json"))
	if !bytes.Equal(written, snapshot) || strings.Contains(string(written), "tok_AAAA") {
		t.Errorf("config.json = %s", written)
	}
	s, err := home.Load(ref)
	if err != nil || len(s.Runs) != 1 {
		t.Fatalf("Load: %+v, %v", s.Runs, err)
	}
	data, _ := os.ReadFile(filepath.Join(r.Dir, "journal.jsonl"))
	if !strings.Contains(string(data), `"config_sha256":"`+state.SHA256(snapshot)+`"`) || strings.Contains(string(data), "tok_AAAA") {
		t.Errorf("journal's manifest:\n%s", data)
	}
}
