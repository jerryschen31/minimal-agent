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
	"strconv"
	"strings"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/hooks"
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
	// a stdio server can print its environment (tokens) on stderr; info's output is meant to be safe to paste
	cfg.McpServerStderr = io.Discard
	printInfoHeader(w, cfg, configFile)
	fmt.Fprintln(w)
	config.PrintConfig(cfg)
	code := InfoOK
	// the same checks SetupAgent makes, so info never passes a config that chat mode refuses
	configErr := checkConfig(cfg)
	if configErr != nil {
		fmt.Fprintf(w, "Configuration: FAILED: %v\n", configErr)
		code = InfoFailed
	}
	fmt.Fprintln(w)
	toolNames, allListed := printBuiltinTools(w, cfg)
	if !allListed {
		code = InfoFailed
	}

	fmt.Fprintln(w)
	if configErr != nil {
		allListed = false
		// a bad mcpResultMaxBytes makes every MCP tool fail to load, so the servers would look connected with no tools
		fmt.Fprintln(w, "MCP servers: not started (fix the configuration error above first)")
	} else {
		// Ctrl+C while the MCP servers start aborts info, as it aborts setup
		infoCtx, stopWatch := createInterruptWatcher(ctx, interrupts)
		defer stopWatch()
		mcpNames, ok := printMCP(infoCtx, w, cfg)
		if !ok {
			code = InfoFailed
			allListed = false
		}
		if infoCtx.Err() != nil {
			fmt.Fprintln(w, "interrupted")
			return InfoInterrupted
		}
		toolNames = append(toolNames, mcpNames...)
	}

	fmt.Fprintln(w)
	if !printDenyRules(w, cfg.DenyRules, toolNames, allListed) {
		code = InfoFailed
	}
	return code
}

// printDenyRules lists every deny rule in force (the defaults from config.GetDefaultConfig, or the config file's list
// that replaced them) with the tools it matches, using the same matching as the gate itself, and reports false if
// the rules are invalid (chat mode would refuse to start). A config rule that matches no tool gets a warning: usually
// a typo. allListed is false when some tools could not be listed (built-ins failed, a server failed or was not
// started), so the warning could be wrong and is left out.
func printDenyRules(w io.Writer, rulesIn, toolNames []string, allListed bool) bool {
	denyRules, err := hooks.NewDenyRules(rulesIn)
	if err != nil {
		fmt.Fprintf(w, "Deny rules: FAILED: %v\n", err)
		return false
	}
	rules := denyRules.Rules()
	if len(rules) == 0 {
		fmt.Fprintln(w, "Deny rules (0): none, every tool call is allowed")
		return true
	}
	fmt.Fprintf(w, "Deny rules (%d)\n", len(rules))
	if !allListed {
		fmt.Fprintln(w, "  (some tools could not be listed above, so a rule may match a tool not shown)")
	}
	for _, rule := range rules {
		source := "config"
		if isDefaultDenyRule(rule) {
			source = "default"
		}
		var matches []string
		for _, name := range toolNames {
			if hooks.RuleMatches(rule, name) {
				matches = append(matches, safeText(name))
			}
		}
		switch {
		case len(matches) > 0:
			fmt.Fprintf(w, "  %-20s %-9s matches: %s\n", rule, source, strings.Join(matches, ", "))
		case !allListed:
			fmt.Fprintf(w, "  %-20s %-9s matches nothing among the tools listed\n", rule, source)
		case source == "default":
			// [agent] the defaults are a safety net for tools that may appear later, so matching nothing today is normal
			fmt.Fprintf(w, "  %-20s %-9s matches nothing\n", rule, source)
		default:
			fmt.Fprintf(w, "  %-20s %-9s matches nothing  <- warning: check the spelling; tool names use '_' (as listed above)\n", rule, source)
		}
	}
	return true
}

// isDefaultDenyRule reports whether rule (lowercased, as Rules returns it) is one of the default rules. A config
// file that repeats a default rule shows it as "default" too; that's harmless, and telling them apart would mean
// recording which keys the file set.
func isDefaultDenyRule(rule string) bool {
	return slices.Contains(config.GetDefaultConfig().DenyRules, rule) // the defaults are written in lowercase
}

// printBuiltinTools lists the built-in tools by the names the model sees and returns those names. ok is false if
// they can't be set up.
func printBuiltinTools(w io.Writer, cfg config.Config) (names []string, ok bool) {
	builtins, err := tools.SetupBuiltinTools(cfg.BuiltinTools, cfg.WorkDir, cfg.TraceDir, cfg.ReadFileMaxBytes, cfg.DenyFileNames)
	if err != nil {
		fmt.Fprintf(w, "Built-in tools: FAILED: %v\n", err)
		return nil, false
	}
	fmt.Fprintf(w, "Built-in tools (%d)\n", len(builtins))
	for _, t := range builtins {
		def := t.GetToolDefinition().Function
		names = append(names, def.Name)
		fmt.Fprintf(w, "  %-28s %s\n", def.Name, firstLine(def.Desc))
		if def.Name == "read_file" {
			fmt.Fprintf(w, "  %-28s never reads: %s (.env.example is allowed)\n", "", strings.Join(builtin.DeniedNames(), ", "))
			if len(cfg.DenyFileNames) > 0 {
				fmt.Fprintf(w, "  %-28s also never reads (denyFileNames): %s\n", "", safeText(strings.Join(cfg.DenyFileNames, ", ")))
			}
			if cfg.TraceDir != "" {
				fmt.Fprintf(w, "  %-28s never reads: anything in the trace directory (%s)\n", "", safeText(cfg.TraceDir))
			}
		}
	}
	return names, true
}

// printMCP connects to each MCP server in name order, lists its tools and closes it, then prints a row per server
// and a row per tool, and returns the tool names. ok is false if any server failed. A failed server does not stop
// the others.
func printMCP(ctx context.Context, w io.Writer, cfg config.Config) (names []string, ok bool) {
	servers := slices.Sorted(maps.Keys(cfg.McpServers))
	var rows []string
	var mcpTools []tools.Tool
	allOK, stdioFailed := true, false
	for _, name := range servers {
		if ctx.Err() != nil { // interrupted: don't start the remaining servers
			rows = append(rows, fmt.Sprintf("  %-12s not started (interrupted)", name))
			continue
		}
		status, serverTools := inspectServer(ctx, cfg, name)
		if serverTools == nil {
			allOK = false
			stdioFailed = stdioFailed || transportOf(cfg.McpServers[name]) == "stdio"
		}
		rows = append(rows, fmt.Sprintf("  %-12s %-6s %-22s %s", name, transportOf(cfg.McpServers[name]), runsWhat(cfg.McpServers[name]), status))
		mcpTools = append(mcpTools, serverTools...)
	}

	fmt.Fprintf(w, "MCP servers (%d)\n", len(servers))
	for _, row := range rows {
		fmt.Fprintln(w, row)
	}
	if stdioFailed {
		fmt.Fprintln(w, "  (stdio servers' own error output is hidden in info mode, as it can hold tokens; chat mode shows it)")
	}
	fmt.Fprintf(w, "\nMCP tools (%d)\n", len(mcpTools))
	for _, t := range mcpTools {
		def := t.GetToolDefinition().Function
		hints, _ := mcpconnect.HintsOf(t)
		fmt.Fprintf(w, "  %-36s %-24s %s\n", safeText(def.Name), hintLabel(hints), safeText(firstLine(def.Desc)))
		names = append(names, def.Name)
	}
	return names, allOK
}

// inspectServer connects to one server, lists its tools and closes it again. serverTools is nil when it failed
// (an empty, non-nil slice when it connected and has no tools); status says which.
func inspectServer(ctx context.Context, cfg config.Config, name string) (status string, serverTools []tools.Tool) {
	conn, err := mcpconnect.ConnectMCPServer(ctx, name, cfg)
	if err != nil {
		return "FAILED  " + safeText(hideURL(err.Error(), cfg.McpServers[name].URL)), nil
	}
	defer conn.Close() // stops the server (and, for stdio, its process group) before info moves on
	serverTools, err = mcpconnect.GetMCPTools(ctx, conn, cfg.MCPResultMaxBytes)
	if err != nil {
		return "FAILED listing tools  " + safeText(hideURL(err.Error(), cfg.McpServers[name].URL)), nil
	}
	status = "connected"
	if res := conn.Session.InitializeResult(); res != nil && res.ServerInfo != nil {
		status += fmt.Sprintf("  server: %s %s", safeText(res.ServerInfo.Name), safeText(res.ServerInfo.Version))
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

// safeMCPError prepares an MCP setup error (possibly several servers' errors, one per line) for the terminal: every
// configured server URL is hidden with hideURL, then each line goes through safeText.
func safeMCPError(err error, servers map[string]config.McpServerConfig) string {
	msg := err.Error()
	// longest URL first: if one URL starts with another (…/mcp and …/mcp?token=…), hiding the short one first would
	// leave the rest of the long one, its token included, showing
	var urls []string
	for _, sc := range servers {
		if sc.URL != "" {
			urls = append(urls, sc.URL)
		}
	}
	slices.SortFunc(urls, func(a, b string) int { return len(b) - len(a) })
	for _, u := range urls {
		msg = hideURL(msg, u)
	}
	lines := strings.Split(msg, "\n")
	for i, line := range lines {
		lines[i] = safeText(line)
	}
	return strings.Join(lines, "\n")
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

// safeText returns s unchanged if every character is printable, otherwise Go-quoted ("\x1b[2J", "a\rb"). Names,
// descriptions, versions and error text from an MCP server go straight to the terminal here, and a carriage return
// or an escape sequence in them could redraw or hide part of the report. Quoting only the odd value keeps the
// normal report readable and makes a strange one stand out.
func safeText(s string) string {
	for _, r := range s {
		if !strconv.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
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
