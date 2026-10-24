package agent

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/tools/builtin"
)

// Exit codes for info mode, so a script can use `-mode info` as a preflight check.
const (
	InfoOK          = 0   // everything listed, every MCP server connected
	InfoFailed      = 1   // something could not be set up, e.g. an MCP server failed (its row says why)
	InfoInterrupted = 130 // Ctrl+C or SIGTERM (130 = 128 + SIGINT, the shell's convention)
)

// PrintInfo prints what the agent would have if it started (config, working dir, tools, MCP servers) and returns
// the exit code. It skips SetupAgent on purpose: no provider, memory or trace recorder, so it makes no model call,
// needs no API key and writes no files. configFile is the -config path, "" when none was given.
// [agent] It reuses the same setup functions chat mode calls, so the names it prints are the names the model gets.
func PrintInfo(ctx context.Context, cfg config.Config, configFile string, interrupts <-chan os.Signal) int {
	w := cfg.OutBuffer
	printInfoHeader(w, cfg, configFile)
	fmt.Fprintln(w)
	config.PrintConfig(cfg)
	code := InfoOK
	fmt.Fprintln(w)
	if !printBuiltinTools(w, cfg) {
		code = InfoFailed
	}

	// Ctrl+C while the MCP servers start aborts info, as it aborts setup
	infoCtx, stopWatch := createInterruptWatcher(ctx, interrupts)
	defer stopWatch()
	fmt.Fprintln(w)
	if !printMCP(infoCtx, w, cfg) {
		code = InfoFailed
	}
	if infoCtx.Err() != nil {
		fmt.Fprintln(w, "interrupted")
		return InfoInterrupted
	}
	return code
}

// printBuiltinTools lists the built-in tools by the names the model sees, and reports false if they can't be set up.
func printBuiltinTools(w io.Writer, cfg config.Config) bool {
	builtins, err := tools.SetupBuiltinTools(cfg.BuiltinTools, cfg.WorkDir, cfg.ReadFileMaxBytes)
	if err != nil {
		fmt.Fprintf(w, "Built-in tools: FAILED: %v\n", err)
		return false
	}
	fmt.Fprintf(w, "Built-in tools (%d)\n", len(builtins))
	for _, t := range builtins {
		def := t.GetToolDefinition().Function
		fmt.Fprintf(w, "  %-28s %s\n", def.Name, firstLine(def.Desc))
		if def.Name == "read_file" {
			fmt.Fprintf(w, "  %-28s never reads: %s (.env.example is allowed)\n", "", strings.Join(builtin.DeniedNames(), ", "))
		}
	}
	return true
}

// printMCP connects to each MCP server in name order, lists its tools and closes it, then prints a row per server
// and a row per tool. It reports false if any server failed. A failed server does not stop the others.
func printMCP(ctx context.Context, w io.Writer, cfg config.Config) bool {
	names := slices.Sorted(maps.Keys(cfg.McpServers))
	var rows []string
	var mcpTools []tools.Tool
	allOK := true
	for _, name := range names {
		if ctx.Err() != nil { // interrupted: don't start the remaining servers
			rows = append(rows, fmt.Sprintf("  %-12s not started (interrupted)", name))
			continue
		}
		status, serverTools := inspectServer(ctx, cfg, name)
		if serverTools == nil {
			allOK = false
		}
		rows = append(rows, fmt.Sprintf("  %-12s %-6s %-22s %s", name, transportOf(cfg.McpServers[name]), runsWhat(cfg.McpServers[name]), status))
		mcpTools = append(mcpTools, serverTools...)
	}

	fmt.Fprintf(w, "MCP servers (%d)\n", len(names))
	for _, row := range rows {
		fmt.Fprintln(w, row)
	}
	fmt.Fprintf(w, "\nMCP tools (%d)\n", len(mcpTools))
	for _, t := range mcpTools {
		def := t.GetToolDefinition().Function
		hints, _ := mcpconnect.HintsOf(t)
		fmt.Fprintf(w, "  %-36s %-24s %s\n", def.Name, hintLabel(hints), firstLine(def.Desc))
	}
	return allOK
}

// inspectServer connects to one server, lists its tools and closes it again. serverTools is nil when it failed
// (an empty, non-nil slice when it connected and has no tools); status says which.
func inspectServer(ctx context.Context, cfg config.Config, name string) (status string, serverTools []tools.Tool) {
	conn, err := mcpconnect.ConnectMCPServer(ctx, name, cfg)
	if err != nil {
		return "FAILED  " + hideURL(err.Error(), cfg.McpServers[name].URL), nil
	}
	defer conn.Close() // stops the server (and, for stdio, its process group) before info moves on
	serverTools, err = mcpconnect.GetMCPTools(ctx, conn, cfg.MCPResultMaxBytes)
	if err != nil {
		return "FAILED listing tools  " + hideURL(err.Error(), cfg.McpServers[name].URL), nil
	}
	status = "connected"
	if res := conn.Session.InitializeResult(); res != nil && res.ServerInfo != nil {
		status += fmt.Sprintf("  server: %s %s", res.ServerInfo.Name, res.ServerInfo.Version)
	}
	return status + fmt.Sprintf("  %d tools", len(serverTools)), serverTools
}

func transportOf(sc config.McpServerConfig) string {
	transport, err := sc.TransportType()
	if err != nil {
		return "?"
	}
	return transport
}

// runsWhat shows what a stdio server will run: the program and how many arguments, never the arguments or env,
// which can hold tokens. Enough to notice a server suddenly running curl or sh. Remote servers show nothing (their
// URL can hold a token too).
func runsWhat(sc config.McpServerConfig) string {
	if sc.Command == "" {
		return ""
	}
	if len(sc.Args) == 1 {
		return sc.Command + " (1 arg)"
	}
	return fmt.Sprintf("%s (%d args)", sc.Command, len(sc.Args))
}

// hideURL replaces the configured URL in an error message with its scheme and host: connection errors quote the
// URL, and a URL can carry a token in its path or query.
func hideURL(msg, rawURL string) string {
	if rawURL == "" {
		return msg
	}
	shown := "[url hidden]"
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		shown = u.Scheme + "://" + u.Host + "/[rest hidden]"
	}
	return strings.ReplaceAll(msg, rawURL, shown)
}

// hintLabel shows a tool's annotations. They are the server's claims, so they help spot tools to deny but prove nothing.
func hintLabel(h mcpconnect.ToolHints) string {
	switch {
	case !h.Known:
		return "(no hints)"
	case h.ReadOnly:
		return "read-only"
	}
	var labels []string
	if h.Destructive {
		labels = append(labels, "destructive")
	}
	if h.Idempotent {
		labels = append(labels, "idempotent")
	}
	if len(labels) == 0 {
		return "additive"
	}
	return strings.Join(labels, ", ")
}

// firstLine returns the first sentence of a tool description's first line: enough to tell what the tool does.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if sentence, _, found := strings.Cut(line, ". "); found {
		return sentence + "."
	}
	return line
}

func printInfoHeader(w io.Writer, cfg config.Config, configFile string) {
	fmt.Fprintf(w, "%s %s — info\n", cfg.AgentName, cfg.AgentVersion)
	if configFile == "" {
		configFile = "none, defaults only"
	}
	fmt.Fprintf(w, "config file:  %s\n", configFile)
	fmt.Fprintf(w, "working dir:  %s\n", resolveDir(cfg.WorkDir))
	// only whether the variable is set, never its value
	if cfg.ApiKeyName == "" {
		fmt.Fprintln(w, "api key:      no apiKeyName configured")
	} else {
		fmt.Fprintf(w, "api key:      %s is set: %s\n", cfg.ApiKeyName, yesNo(os.Getenv(cfg.ApiKeyName) != ""))
	}
}

// resolveDir returns dir as an absolute path with symlinks resolved, the directory read_file is confined to.
func resolveDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Sprintf("%s (%v)", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Sprintf("%s (%v)", abs, err)
	}
	return resolved
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
