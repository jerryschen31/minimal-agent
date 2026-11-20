package agent

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/jerryschen31/minimal-agent/agent/director"
	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/jerryschen31/minimal-agent/trace"
)

// buildManifest returns the manifest for a new run of the plan ref on session cs (the "agent lockfile": which plan,
// which minagent, model, settings, prompts and tools), and the run's config.json bytes (configSnapshot).
//
// Notes:
//   - PlanSHA256 and ConfigSHA256 are left for state.Home.NewRun, which hashes the bytes it writes. TraceRunID is set
//     by the caller.
//   - Prompts: "worker" (the system prompt the session sends: workerPrompt in auto mode), "director" (director.Prompt)
//     and "summarizer" (compaction), when not empty. The map form means a new role doesn't change the journal format.
//   - conns are the MCP servers that connected; each is recorded with the name and version it reported.
//   - A hostname error leaves Host empty; a manifest must not fail a run over it.
func buildManifest(cs *ChatSession, conns []*mcpconnect.McpConnection, ref state.PlanRef) (state.Manifest, []byte, error) {
	cfg := cs.Config
	snapshot, err := configSnapshot(cfg)
	if err != nil {
		return state.Manifest{}, nil, err
	}
	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return state.Manifest{}, nil, fmt.Errorf("workDir: %w", err)
	}
	m := state.Manifest{
		PlanID:   ref.ID,
		PlanPath: ref.Path,
		Harness:  state.Harness{Name: cfg.AgentName, Version: cfg.AgentVersion, Commit: trace.GitCommit()},
		PID:      os.Getpid(),
		WorkDir:  workDir,
		Provider: cfg.Provider,
		Model:    cfg.Model,
		BaseURL:  config.DisplayURL(cfg.BaseURL),
		Prompts:  map[string]string{},
	}
	if host, err := os.Hostname(); err == nil {
		m.Host = host
	}
	if cs.SystemMsg.Content != "" {
		m.Prompts["worker"] = state.SHA256([]byte(cs.SystemMsg.Content))
	}
	m.Prompts["director"] = state.SHA256([]byte(director.Prompt))
	if cfg.SummarizerPrompt != "" {
		m.Prompts["summarizer"] = state.SHA256([]byte(cfg.SummarizerPrompt))
	}
	m.Tools, m.ToolsSHA256 = toolsDigest(cs.Tools.GetToolDefs())
	for _, c := range conns {
		transport, err := c.Config.TransportType()
		if err != nil {
			transport = "invalid"
		}
		name, version := c.ServerInfo()
		m.MCPServers = append(m.MCPServers, state.MCPServer{Name: c.Name, Transport: transport, Server: name, Version: version})
	}
	slices.SortFunc(m.MCPServers, func(a, b state.MCPServer) int { return cmp.Compare(a.Name, b.Name) })
	return m, snapshot, nil
}

// configSnapshot returns cfg as indented JSON for the run folder's config.json, with the parts that can hold secrets
// taken out.
//
// Notes:
//   - The same rule as the trace's run_start: each MCP server keeps only its transport type ("stdio", "http", …;
//     "invalid" if it can't be told); its command, args, env, url and headers are dropped, because they commonly hold
//     tokens. baseUrl is shown as config.DisplayURL shows it (no user:password, no query). The API key itself is never
//     in the config, only the name of its environment variable.
//   - So changing only a token leaves the snapshot, and its hash, the same: a token isn't behaviour.
//   - Deterministic: the same config always gives the same bytes (encoding/json sorts map keys). Fields tagged
//     json:"-" (the flag-only ones such as -plan and -yes, and the I/O streams) are left out.
func configSnapshot(cfg config.Config) ([]byte, error) {
	servers := map[string]map[string]string{}
	for name, s := range cfg.McpServers {
		kind, err := s.TransportType()
		if err != nil {
			kind = "invalid"
		}
		servers[name] = map[string]string{"type": kind}
	}
	cfg.McpServers = nil
	cfg.BaseURL = config.DisplayURL(cfg.BaseURL)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config snapshot: %w", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("config snapshot: %w", err)
	}
	fields["mcpServers"] = servers
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("config snapshot: %w", err)
	}
	return append(out, '\n'), nil
}

// toolsDigest returns the tool names, sorted, and a hash over every tool's name, description and parameters.
//
// Notes:
//   - The order tools were registered in doesn't matter. A reworded description or a changed parameter changes the
//     hash: the model sees both, so both change behaviour.
//   - Parameters are compacted before hashing, so whitespace in a schema doesn't count.
func toolsDigest(defs []model.ToolDef) (names []string, hash string) {
	type entry struct {
		Name   string          `json:"name"`
		Desc   string          `json:"description"`
		Params json.RawMessage `json:"parameters"`
	}
	entries := make([]entry, 0, len(defs))
	for _, d := range defs {
		params := d.Function.Params
		var buf bytes.Buffer
		if json.Compact(&buf, params) == nil {
			params = buf.Bytes()
		}
		entries = append(entries, entry{Name: d.Function.Name, Desc: d.Function.Desc, Params: params})
	}
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(a.Name, b.Name) })
	for _, e := range entries {
		names = append(names, e.Name)
	}
	data, _ := json.Marshal(entries)
	return names, state.SHA256(data)
}
