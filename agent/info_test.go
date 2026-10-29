package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/tools/builtin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runInfo runs PrintInfo with a buffer as OutBuffer and returns the exit code and the output.
func runInfo(t *testing.T, cfg config.Config, configFile string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	cfg.OutBuffer = &out
	return PrintInfo(context.Background(), cfg, configFile, nil), out.String()
}

// - Verify the working dir line shows WorkDir as an absolute path with symlinks resolved, and the exit code is 0
func Test_Unit_PrintInfo_WorkDir_ResolvesSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = link

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d", InfoOK, code)
	}
	if !strings.Contains(out, "working dir:  "+want+"\n") {
		t.Errorf("expected working dir %q in output:\n%s", want, out)
	}
}

// - Verify the config file line shows the given path, or "none, defaults only" when there is none
func Test_Unit_PrintInfo_ConfigFileLine(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	for configFile, want := range map[string]string{
		"my-config.json": "config file:  my-config.json\n",
		"":               "config file:  none, defaults only\n",
	} {
		if _, out := runInfo(t, cfg, configFile); !strings.Contains(out, want) {
			t.Errorf("configFile %q: expected %q in output:\n%s", configFile, want, out)
		}
	}
}

// - Verify the api key line says whether the variable is set, never prints its value, and handles no apiKeyName
func Test_Unit_PrintInfo_ApiKeyLine(t *testing.T) {
	const name = "MINAGENT_TEST_INFO_API_KEY"
	const fakeKey = "sk-test-not-a-real-key-123"
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.ApiKeyName = name

	t.Setenv(name, fakeKey)
	_, out := runInfo(t, cfg, "")
	if !strings.Contains(out, name+" is set: yes") {
		t.Errorf("expected %q is set: yes in output:\n%s", name, out)
	}
	if strings.Contains(out, fakeKey) {
		t.Errorf("output leaked the api key value:\n%s", out)
	}

	t.Setenv(name, "") // [agent] os.Getenv returns "" for empty and unset alike, so this counts as not set
	if _, out := runInfo(t, cfg, ""); !strings.Contains(out, name+" is set: no") {
		t.Errorf("expected %q is set: no in output:\n%s", name, out)
	}

	cfg.ApiKeyName = ""
	if _, out := runInfo(t, cfg, ""); !strings.Contains(out, "no apiKeyName configured") {
		t.Errorf("expected no apiKeyName configured in output:\n%s", out)
	}
}

// - Verify no MCP server secret (args, env, url, headers) and no baseUrl secret (userinfo, query) appears in the output
func Test_Unit_PrintInfo_DoesNotLeakMcpSecrets(t *testing.T) {
	secrets := []string{"FAKE_SECRET_ARG_123", "FAKE_SECRET_ENV_123", "FAKE_SECRET_URL_123", "FAKE_SECRET_HDR_123",
		"FAKE_SECRET_USERINFO_123", "FAKE_SECRET_QUERY_123"}
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.BaseURL = "https://user:" + secrets[4] + "@llm.example.invalid/v1?key=" + secrets[5]
	cfg.McpServers = map[string]config.McpServerConfig{
		"local":  {Command: "fake-server", Args: []string{"--token", secrets[0]}, Env: map[string]string{"TOKEN": secrets[1]}},
		"remote": {URL: "https://example.invalid/?token=" + secrets[2], Headers: map[string]string{"Authorization": "Bearer " + secrets[3]}},
	}

	_, out := runInfo(t, cfg, "")
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("output leaked %q:\n%s", s, out)
		}
	}
}

// - Verify the default config lists read_file with a "never reads:" line naming every deny-list entry, and returns 0
func Test_Unit_PrintInfo_BuiltinTools_Default(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d", InfoOK, code)
	}
	if !strings.Contains(out, "Built-in tools (1)\n") {
		t.Errorf("expected Built-in tools (1) in output:\n%s", out)
	}
	var toolLine, neverLine string
	for line := range strings.Lines(out) {
		switch {
		case strings.Contains(line, "never reads:"):
			neverLine = line
		case strings.Contains(line, "read_file"):
			toolLine = line
		}
	}
	if toolLine == "" {
		t.Errorf("expected a read_file line in output:\n%s", out)
	}
	if neverLine == "" {
		t.Fatalf("expected a never reads: line in output:\n%s", out)
	}
	for _, name := range builtin.DeniedNames() {
		if !strings.Contains(neverLine, name) {
			t.Errorf("expected %q in never reads: line %q", name, neverLine)
		}
	}
}

// - Verify an empty BuiltinTools list prints "Built-in tools (0)" and returns 0
func Test_Unit_PrintInfo_BuiltinTools_None(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.BuiltinTools = []string{}

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d", InfoOK, code)
	}
	if !strings.Contains(out, "Built-in tools (0)\n") {
		t.Errorf("expected Built-in tools (0) in output:\n%s", out)
	}
}

// - Verify an unknown built-in tool prints a FAILED row after the config section and returns InfoFailed
func Test_Unit_PrintInfo_BuiltinTools_Unknown_Fails(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.BuiltinTools = []string{"Bogus"}

	code, out := runInfo(t, cfg, "")
	if code != InfoFailed {
		t.Errorf("expected exit code %d, got %d", InfoFailed, code)
	}
	cfgAt := strings.Index(out, "Current agent configuration:")
	failAt := strings.Index(out, "Built-in tools: FAILED")
	if cfgAt < 0 || failAt < 0 || cfgAt > failAt {
		t.Errorf("expected the config section, then Built-in tools: FAILED, in output:\n%s", out)
	}
}

// - Verify firstLine keeps only the first sentence of the first line
func Test_Unit_FirstLine(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"multi-line", "Reads a file\nMore detail here.", "Reads a file"},
		{"two sentences", "One. Two.", "One."},
		{"no period", "Reads a file", "Reads a file"},
		{"leading whitespace", "\n\n  Reads a file\nmore", "Reads a file"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// startInfoMCPServer serves an in-process MCP server named "infotest" 1.2.3 over Streamable HTTP, with two tools:
// "lookup" (annotated read-only) and "write" (no annotations). Both descriptions have two sentences.
func startInfoMCPServer(t *testing.T) string {
	t.Helper()
	noop := func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	}
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "infotest", Version: "1.2.3"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "lookup", Description: "Looks a word up. Second sentence is hidden.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, noop)
		mcp.AddTool(server, &mcp.Tool{Name: "write", Description: "Writes a note. Also hidden."}, noop)
		return server
	}, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// lineWith returns the first output line containing every one of parts, or "" if none does.
func lineWith(out string, parts ...string) string {
	for line := range strings.Lines(out) {
		matches := true
		for _, p := range parts {
			matches = matches && strings.Contains(line, p)
		}
		if matches {
			return line
		}
	}
	return ""
}

// - Verify a working MCP server is shown as connected with its name, version and tool count, and each tool is
// listed by its model-facing name with its hint label and only the first sentence of its description
func Test_Unit_PrintInfo_MCP_ConnectedServerAndTools(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{"docs": {URL: startInfoMCPServer(t)}}

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoOK, code, out)
	}
	if lineWith(out, "docs", "connected  server: infotest 1.2.3  2 tools") == "" {
		t.Errorf("expected docs connected with server infotest 1.2.3 and 2 tools in output:\n%s", out)
	}
	if !strings.Contains(out, "MCP tools (2)\n") {
		t.Errorf("expected MCP tools (2) in output:\n%s", out)
	}
	if lineWith(out, "docs_lookup", "read-only", "Looks a word up.") == "" {
		t.Errorf("expected a docs_lookup read-only row in output:\n%s", out)
	}
	if lineWith(out, "docs_write", "(no hints)", "Writes a note.") == "" {
		t.Errorf("expected a docs_write (no hints) row in output:\n%s", out)
	}
	if strings.Contains(out, "hidden") {
		t.Errorf("expected only the first sentence of each description in output:\n%s", out)
	}
}

// - Verify a stdio server that can't start shows its command and arg count and FAILED, never its args or env,
// the working server is still listed with its tools, and the exit code is InfoFailed
func Test_Unit_PrintInfo_MCP_FailedStdioServer_OthersStillListed(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{
		"broken": {Command: "does-not-exist-minagent-test", Args: []string{"--token=FAKE_SECRET_ARG_123"},
			Env: map[string]string{"T": "FAKE_SECRET_ENV_123"}},
		"docs": {URL: startInfoMCPServer(t)},
	}

	code, out := runInfo(t, cfg, "")
	if code != InfoFailed {
		t.Errorf("expected exit code %d, got %d", InfoFailed, code)
	}
	if lineWith(out, "broken", "stdio", "does-not-exist-minagent-test (1 arg)", "FAILED") == "" {
		t.Errorf("expected a broken stdio row with (1 arg) and FAILED in output:\n%s", out)
	}
	if lineWith(out, "docs", "connected", "2 tools") == "" {
		t.Errorf("expected docs still connected in output:\n%s", out)
	}
	if lineWith(out, "docs_lookup") == "" || lineWith(out, "docs_write") == "" {
		t.Errorf("expected docs tools still listed in output:\n%s", out)
	}
	for _, s := range []string{"FAKE_SECRET_ARG_123", "FAKE_SECRET_ENV_123"} {
		if strings.Contains(out, s) {
			t.Errorf("output leaked %q:\n%s", s, out)
		}
	}
}

// - Verify a remote server that can't be reached shows FAILED without leaking the token in its URL or headers
func Test_Unit_PrintInfo_MCP_FailedRemoteServer_HidesURLAndHeaders(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{
		"remote": {URL: "http://127.0.0.1:1/mcp?token=FAKE_SECRET_URL_123",
			Headers: map[string]string{"Authorization": "Bearer FAKE_SECRET_HDR_123"}},
	}

	code, out := runInfo(t, cfg, "")
	if code != InfoFailed {
		t.Errorf("expected exit code %d, got %d", InfoFailed, code)
	}
	if lineWith(out, "remote", "FAILED") == "" {
		t.Errorf("expected a remote FAILED row in output:\n%s", out)
	}
	for _, s := range []string{"FAKE_SECRET_URL_123", "FAKE_SECRET_HDR_123"} {
		if strings.Contains(out, s) {
			t.Errorf("output leaked %q:\n%s", s, out)
		}
	}
}

// - Verify runsWhat shows the command and arg count (singular for one), and nothing for a remote server
func Test_Unit_RunsWhat(t *testing.T) {
	tests := []struct {
		name string
		in   config.McpServerConfig
		want string
	}{
		{"no args", config.McpServerConfig{Command: "srv"}, "srv (0 args)"},
		{"one arg", config.McpServerConfig{Command: "srv", Args: []string{"a"}}, "srv (1 arg)"},
		{"two args", config.McpServerConfig{Command: "srv", Args: []string{"a", "b"}}, "srv (2 args)"},
		{"remote", config.McpServerConfig{URL: "https://example.invalid/mcp"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runsWhat(tt.in); got != tt.want {
				t.Errorf("runsWhat(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// - Verify hideURL replaces the configured URL with its scheme and host, and leaves the message alone without one
func Test_Unit_HideURL(t *testing.T) {
	const rawURL = "https://example.invalid/mcp?token=FAKE_SECRET_URL_123"
	got := hideURL(`Post "`+rawURL+`": connection refused`, rawURL)
	if want := `Post "https://example.invalid/[rest hidden]": connection refused`; got != want {
		t.Errorf("hideURL = %q, want %q", got, want)
	}
	if got := hideURL("some error", ""); got != "some error" {
		t.Errorf("hideURL with empty URL = %q, want the message unchanged", got)
	}
}

// - Verify an already-cancelled context (SIGTERM before info starts) starts no MCP server: every row says
// "not started (interrupted)", "interrupted" is printed, and the exit code is InfoInterrupted
func Test_Unit_PrintInfo_ContextAlreadyCancelled_StartsNoServer(t *testing.T) {
	url := startInfoMCPServer(t)
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{"a": {URL: url}, "b": {URL: url}}
	var out bytes.Buffer
	cfg.OutBuffer = &out
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := PrintInfo(ctx, cfg, "", nil)
	if code != InfoInterrupted {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoInterrupted, code, out.String())
	}
	for _, name := range []string{"a", "b"} {
		if lineWith(out.String(), name+" ", "not started (interrupted)") == "" {
			t.Errorf("expected %s not started (interrupted) in output:\n%s", name, out.String())
		}
	}
	if !strings.Contains(out.String(), "\ninterrupted\n") {
		t.Errorf("expected an interrupted line in output:\n%s", out.String())
	}
}

// - Verify Ctrl+C while a server is starting cancels that server's connect (FAILED, context canceled), skips the
// remaining servers, prints "interrupted", returns InfoInterrupted, and does so promptly
func Test_Unit_PrintInfo_CtrlC_WhileServerStarts(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	release := make(chan struct{})
	// [agent] the hung server presses Ctrl+C on its first request, then never answers. The MCP SDK leaves its
	// in-flight POSTs open after a cancelled connect, so the handler also returns on release, closed at cleanup
	// before hung.Close (cleanups run last-in first-out), or hung.Close would wait for it forever.
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case interrupts <- os.Interrupt:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) })

	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{"a": {URL: hung.URL}, "b": {URL: startInfoMCPServer(t)}}
	var out bytes.Buffer
	cfg.OutBuffer = &out
	// [agent] guard: a missed cancel fails instead of hanging. ~5s is expected: the SDK's failed Connect closes its
	// session, which waits up to 5s for the "notifications/cancelled" POST the hung server never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	code := PrintInfo(ctx, cfg, "", interrupts)
	t.Logf("PrintInfo returned after %v", time.Since(start))
	if ctx.Err() != nil {
		t.Fatalf("PrintInfo only returned after the 10s guard; Ctrl+C did not cancel the connect:\n%s", out.String())
	}
	if code != InfoInterrupted {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoInterrupted, code, out.String())
	}
	if lineWith(out.String(), "a ", "FAILED", "context canceled") == "" {
		t.Errorf("expected a FAILED (context canceled) row in output:\n%s", out.String())
	}
	if lineWith(out.String(), "b ", "not started (interrupted)") == "" {
		t.Errorf("expected b not started (interrupted) in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "\ninterrupted\n") {
		t.Errorf("expected an interrupted line in output:\n%s", out.String())
	}
}

// - Verify a built-in tool failure still returns InfoFailed when every MCP server connects
func Test_Unit_PrintInfo_BuiltinsFail_ServersFine_Fails(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.BuiltinTools = []string{"Bogus"}
	cfg.McpServers = map[string]config.McpServerConfig{"docs": {URL: startInfoMCPServer(t)}}

	code, out := runInfo(t, cfg, "")
	if code != InfoFailed {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoFailed, code, out)
	}
	if lineWith(out, "docs", "connected") == "" {
		t.Errorf("expected docs connected in output:\n%s", out)
	}
}

// - Verify a config SetupAgent would refuse (maxSteps or mcpResultMaxBytes not positive) prints
// "Configuration: FAILED" naming the field, still lists the built-in tools, starts no MCP server, and returns InfoFailed
func Test_Unit_PrintInfo_BadConfig_StartsNoServer(t *testing.T) {
	tests := []struct {
		name  string
		field string
		set   func(*config.Config)
	}{
		{"maxSteps 0", "maxSteps", func(c *config.Config) { c.MaxSteps = 0 }},
		{"mcpResultMaxBytes 0", "mcpResultMaxBytes", func(c *config.Config) { c.MCPResultMaxBytes = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// [agent] counts every request; a started MCP server would send at least an initialize request
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				http.Error(w, "should not be contacted", http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			cfg := config.GetDefaultConfig()
			cfg.WorkDir = t.TempDir()
			cfg.McpServers = map[string]config.McpServerConfig{"docs": {URL: srv.URL}}
			tt.set(&cfg)

			code, out := runInfo(t, cfg, "")
			if code != InfoFailed {
				t.Errorf("expected exit code %d, got %d:\n%s", InfoFailed, code, out)
			}
			if lineWith(out, "Configuration: FAILED", tt.field) == "" {
				t.Errorf("expected Configuration: FAILED naming %s in output:\n%s", tt.field, out)
			}
			if !strings.Contains(out, "Built-in tools (1)\n") {
				t.Errorf("expected the built-in tools still listed in output:\n%s", out)
			}
			if !strings.Contains(out, "MCP servers: not started (fix the configuration error above first)\n") {
				t.Errorf("expected the MCP servers not started line in output:\n%s", out)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("expected no request to the MCP server, got %d", n)
			}
		})
	}
}

// - Verify printDenyRules lists each rule with its source and every tool it matches, built-in and MCP alike
func Test_Unit_PrintDenyRules_Matches(t *testing.T) {
	var out bytes.Buffer
	ok := printDenyRules(&out, []string{"*FILE*"}, []string{"read_file", "docs_delete_note", "docs_remove_file"}, true)
	if !ok {
		t.Fatalf("expected ok, got false:\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "Deny rules (3)\n") {
		t.Errorf("expected Deny rules (3) in output:\n%s", got)
	}
	if lineWith(got, "*delete*", "built-in", "matches: docs_delete_note\n") == "" {
		t.Errorf("expected *delete* built-in matching docs_delete_note in output:\n%s", got)
	}
	if lineWith(got, "*remove*", "built-in", "matches: docs_remove_file\n") == "" {
		t.Errorf("expected *remove* built-in matching docs_remove_file in output:\n%s", got)
	}
	if lineWith(got, "*file*", "config", "matches: read_file, docs_remove_file\n") == "" {
		t.Errorf("expected *file* (lowercased) config matching read_file, docs_remove_file in output:\n%s", got)
	}
	if strings.Contains(got, "warning") || strings.Contains(got, "some tools could not be listed") {
		t.Errorf("expected no warning and no note in output:\n%s", got)
	}
}

// - Verify a built-in rule matching nothing prints plain "matches nothing", while a config rule gets a warning
func Test_Unit_PrintDenyRules_NoMatch_WarnsOnlyForConfig(t *testing.T) {
	var out bytes.Buffer
	if !printDenyRules(&out, []string{"*move*"}, []string{"read_file"}, true) {
		t.Fatalf("expected ok, got false:\n%s", out.String())
	}
	got := out.String()
	for _, rule := range []string{"*delete*", "*remove*"} {
		line := lineWith(got, rule, "built-in", "matches nothing")
		if line == "" || strings.Contains(line, "warning") {
			t.Errorf("expected %s built-in matches nothing with no warning, got line %q in output:\n%s", rule, line, got)
		}
	}
	if lineWith(got, "*move*", "config", "matches nothing  <- warning:") == "" {
		t.Errorf("expected *move* config matches nothing with a warning in output:\n%s", got)
	}
}

// - Verify allListed=false prints the "some tools could not be listed" note, says "among the tools listed", and never warns
func Test_Unit_PrintDenyRules_McpNotListed_NoteNoWarning(t *testing.T) {
	var out bytes.Buffer
	if !printDenyRules(&out, []string{"*move*", "read_*"}, []string{"read_file"}, false) {
		t.Fatalf("expected ok, got false:\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "(some tools could not be listed above, so a rule may match a tool not shown)") {
		t.Errorf("expected the some-tools-not-listed note in output:\n%s", got)
	}
	for _, rule := range []string{"*delete*", "*remove*", "*move*"} {
		if lineWith(got, rule, "matches nothing among the tools listed") == "" {
			t.Errorf("expected %s matches nothing among the tools listed in output:\n%s", rule, got)
		}
	}
	if lineWith(got, "read_*", "config", "matches: read_file") == "" {
		t.Errorf("expected read_* still listing its match in output:\n%s", got)
	}
	if strings.Contains(got, "warning") {
		t.Errorf("expected no warning when MCP was not listed:\n%s", got)
	}
}

// - Verify an invalid rule (bad glob, or the tool:program form) returns false and prints one FAILED line naming it
func Test_Unit_PrintDenyRules_InvalidRule_Fails(t *testing.T) {
	for _, rule := range []string{"*delete[", "run_command:rm"} {
		var out bytes.Buffer
		if printDenyRules(&out, []string{rule}, []string{"read_file"}, true) {
			t.Errorf("%q: expected false, got true", rule)
		}
		got := out.String()
		if lineWith(got, "Deny rules: FAILED: invalid deny rule", rule) == "" {
			t.Errorf("%q: expected a FAILED line naming the rule in output:\n%s", rule, got)
		}
		if strings.Contains(got, "Deny rules (") {
			t.Errorf("%q: expected no rule list after FAILED:\n%s", rule, got)
		}
	}
}

// - Verify the default config ends with both built-in deny rules (no warning) and returns InfoOK
func Test_Unit_PrintInfo_DenyRules_Default(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoOK, code, out)
	}
	if !strings.Contains(out, "Deny rules (2)\n") {
		t.Errorf("expected Deny rules (2) in output:\n%s", out)
	}
	for _, rule := range []string{"*delete*", "*remove*"} {
		if lineWith(out, rule, "built-in", "matches nothing") == "" {
			t.Errorf("expected %s built-in matches nothing in output:\n%s", rule, out)
		}
	}
	if strings.Contains(out, "warning") {
		t.Errorf("expected no warning in output:\n%s", out)
	}
	if mcpAt, denyAt := strings.Index(out, "MCP tools ("), strings.Index(out, "Deny rules ("); mcpAt < 0 || denyAt < mcpAt {
		t.Errorf("expected the deny rules section after the MCP tools section:\n%s", out)
	}
}

// - Verify a config rule matching the built-in read_file shows that match and returns InfoOK
func Test_Unit_PrintInfo_DenyRules_ConfigRuleMatchesBuiltin(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DenyRules = []string{"READ_*"}

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoOK, code, out)
	}
	if !strings.Contains(out, "Deny rules (3)\n") {
		t.Errorf("expected Deny rules (3) in output:\n%s", out)
	}
	if lineWith(out, "read_*", "config", "matches: read_file") == "" {
		t.Errorf("expected read_* config matches: read_file in output:\n%s", out)
	}
}

// - Verify an invalid rule returns InfoFailed with a FAILED line, and the built-in and MCP tools are still listed
func Test_Unit_PrintInfo_DenyRules_Invalid_Fails(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DenyRules = []string{"*delete["}
	cfg.McpServers = map[string]config.McpServerConfig{"docs": {URL: startInfoMCPServer(t)}}

	code, out := runInfo(t, cfg, "")
	if code != InfoFailed {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoFailed, code, out)
	}
	if lineWith(out, "Deny rules: FAILED: invalid deny rule", "*delete[") == "" {
		t.Errorf("expected Deny rules: FAILED naming *delete[ in output:\n%s", out)
	}
	if !strings.Contains(out, "Built-in tools (1)\n") || lineWith(out, "read_file") == "" {
		t.Errorf("expected the built-in tools still listed in output:\n%s", out)
	}
	if lineWith(out, "docs_lookup") == "" || lineWith(out, "docs_write") == "" {
		t.Errorf("expected the MCP tools still listed in output:\n%s", out)
	}
	if failAt, mcpAt := strings.Index(out, "Deny rules: FAILED"), strings.Index(out, "MCP tools ("); mcpAt < 0 || failAt < mcpAt {
		t.Errorf("expected the deny rules FAILED line after the MCP tools section:\n%s", out)
	}
}

// - Verify a config error (MCP not started) shows the "some tools could not be listed" note on the deny rules and no warning
func Test_Unit_PrintInfo_DenyRules_BadConfig_ShowsNote(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.MCPResultMaxBytes = -1
	cfg.DenyRules = []string{"*move*"}

	code, out := runInfo(t, cfg, "")
	if code != InfoFailed {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoFailed, code, out)
	}
	if !strings.Contains(out, "(some tools could not be listed above, so a rule may match a tool not shown)") {
		t.Errorf("expected the some-tools-not-listed note in output:\n%s", out)
	}
	if lineWith(out, "*move*", "config", "matches nothing among the tools listed") == "" {
		t.Errorf("expected *move* matches nothing among the tools listed in output:\n%s", out)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("expected no warning in output:\n%s", out)
	}
}

// - Verify rules are matched against MCP tools by their model-facing names (server prefix included)
func Test_Unit_PrintInfo_DenyRules_MatchesMCPTool(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DenyRules = []string{"*write*", "docs_*", "write"}
	cfg.McpServers = map[string]config.McpServerConfig{"docs": {URL: startInfoMCPServer(t)}}

	code, out := runInfo(t, cfg, "")
	if code != InfoOK {
		t.Errorf("expected exit code %d, got %d:\n%s", InfoOK, code, out)
	}
	if lineWith(out, "*write*", "config", "matches: docs_write\n") == "" {
		t.Errorf("expected *write* matches: docs_write in output:\n%s", out)
	}
	if lineWith(out, "docs_*", "config", "matches: docs_lookup, docs_write\n") == "" {
		t.Errorf("expected docs_* matches: docs_lookup, docs_write in output:\n%s", out)
	}
	// [agent] the bare MCP name "write" is not what the model sees, so it matches nothing and warns
	if lineWith(out, "write ", "config", "matches nothing  <- warning:") == "" {
		t.Errorf("expected bare write to match nothing with a warning in output:\n%s", out)
	}
}

// - Verify a server that connects but fails tools/list is shown as FAILED (not "connected, 0 tools"), info exits 1,
// and the deny rules say the tool list is incomplete instead of warning that a config rule matches nothing
func Test_Unit_PrintInfo_MCP_ToolsListFails_DenyRulesNoteIncomplete(t *testing.T) {
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "brokenlist", Version: "0.0.1"}, nil)
	}, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte(`"tools/list"`)) {
				http.Error(w, "tools/list is broken", http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		sdk.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{"broken": {URL: srv.URL}}
	cfg.DenyRules = []string{"broken_*"}
	code, out := runInfo(t, cfg, "")

	if code != InfoFailed {
		t.Errorf("exit code = %d, want %d (InfoFailed)\n%s", code, InfoFailed, out)
	}
	if lineWith(out, "broken", "FAILED listing tools") == "" {
		t.Errorf("expected the server row to say FAILED listing tools:\n%s", out)
	}
	if !strings.Contains(out, "(some tools could not be listed above, so a rule may match a tool not shown)") {
		t.Errorf("expected the incomplete-list note in the deny rules section:\n%s", out)
	}
	if strings.Contains(out, "<- warning") {
		t.Errorf("expected no matches-nothing warning when the tool list is incomplete:\n%s", out)
	}
}

// - Verify safeText leaves printable text (including non-ASCII) alone and quotes text holding control or invisible
// formatting characters, so it can't move the cursor, clear the screen or reorder the line
func Test_Unit_SafeText(t *testing.T) {
	cases := map[string]string{
		"read_file":          "read_file",
		"café — naïve":       "café — naïve",
		"clear\x1b[2Jscreen": `"clear\x1b[2Jscreen"`,
		"over\rwrite":        `"over\rwrite"`,
		"tab\there":          `"tab\there"`,
		"rtl\u202eoverride":  `"rtl\u202eoverride"`,
		"c1\u009bcsi":        `"c1\u009bcsi"`,
	}
	for in, want := range cases {
		if got := safeText(in); got != want {
			t.Errorf("safeText(%q) = %s, want %s", in, got, want)
		}
	}
}

// - Verify info mode quotes server-supplied text holding escape codes (tool description, server version) instead of
// writing them raw to the terminal
func Test_Unit_PrintInfo_MCP_ControlCharsAreQuoted(t *testing.T) {
	noop := func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	}
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "evil", Version: "1.0\r\x1b[2J"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "lookup", Description: "Looks up\x1b[8m hidden"}, noop)
		return server
	}, nil))
	t.Cleanup(srv.Close)

	cfg := config.GetDefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.McpServers = map[string]config.McpServerConfig{"evil": {URL: srv.URL}}
	_, out := runInfo(t, cfg, "")

	if strings.ContainsAny(out, "\x1b\r") {
		t.Errorf("expected no raw escape or carriage return in the output:\n%q", out)
	}
	for _, want := range []string{`"1.0\r\x1b[2J"`, `"Looks up\x1b[8m hidden"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the quoted value %s in the output:\n%s", want, out)
		}
	}
}
