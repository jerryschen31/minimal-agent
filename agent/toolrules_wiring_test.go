package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
)

// denyWiringConfig is a SetupAgent config that needs no network: the OpenAI-compatible provider is only constructed
// (nothing is sent until a turn runs), tracing is off, and the API key is an obviously fake value.
func denyWiringConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("MINAGENT_TEST_FAKE_API_KEY", "FAKE_SECRET_123")
	cfg := config.GetDefaultConfig()
	cfg.ApiKeyName = "MINAGENT_TEST_FAKE_API_KEY"
	cfg.BaseURL = "http://127.0.0.1:0/v1" // never dialled; a fake endpoint in case a test did reach the provider
	cfg.InBuffer, cfg.OutBuffer = strings.NewReader(""), io.Discard
	cfg.TraceEnabled, cfg.WorkDir = false, t.TempDir()
	return cfg
}

// setupDenyWiringAgent runs SetupAgent and returns the agent and its single *hooks.ToolRules hook.
func setupDenyWiringAgent(t *testing.T, cfg config.Config) (*Agent, *hooks.ToolRules) {
	t.Helper()
	a := &Agent{}
	if err := a.SetupAgent(context.Background(), cfg, nil); err != nil {
		t.Fatalf("SetupAgent: %v", err)
	}
	t.Cleanup(func() { a.ShutdownAgent(context.Background()) })
	if len(a.session.Hooks) != 1 {
		t.Fatalf("expected exactly 1 hook on the session, got %d", len(a.session.Hooks))
	}
	tr, ok := a.session.Hooks[0].(*hooks.ToolRules)
	if !ok {
		t.Fatalf("expected the hook to be a *hooks.ToolRules, got %T", a.session.Hooks[0])
	}
	return a, tr
}

func namedCall(name string) model.ToolCall {
	return model.ToolCall{ID: "call_1", Function: model.ToolCallFunc{Name: name, Arguments: `{}`}}
}

// defaultAskRules mirrors config.GetDefaultConfig().AskRules, written out so a change to the defaults shows up here.
var defaultAskRules = []string{"*delete*", "*remove*", "write_file", "edit_file", "create_directory", "copy_file", "move_file"}

// - Verify SetupAgent with the default config installs exactly one hook, a *hooks.ToolRules with no deny rules and
// exactly the default ask rules: a *delete* name needs approval (wrapping hooks.ErrNeedsApproval), a name matching no
// rule is allowed
func Test_Unit_SetupAgent_DefaultConfig_InstallsDefaultToolRules(t *testing.T) {
	cfg := denyWiringConfig(t) // [agent] starts from config.GetDefaultConfig, as a config file without the rule keys does
	_, tr := setupDenyWiringAgent(t, cfg)

	if got := tr.DenyRules(); len(got) != 0 {
		t.Errorf("DenyRules() = %q, want none by default", got)
	}
	if got := tr.AskRules(); !slices.Equal(got, defaultAskRules) {
		t.Errorf("AskRules() = %q, want %q", got, defaultAskRules)
	}

	err := tr.BeforeTool(context.Background(), namedCall("github_delete_file"))
	if err == nil || err.Error() != `needs approval by rule "*delete*"` || !errors.Is(err, hooks.ErrNeedsApproval) {
		t.Errorf("github_delete_file: err = %v, want %q wrapping ErrNeedsApproval", err, `needs approval by rule "*delete*"`)
	}
	if err := tr.BeforeTool(context.Background(), namedCall("filesystem_list_directory")); err != nil {
		t.Errorf("filesystem_list_directory: expected allowed, got %v", err)
	}
}

// - Verify empty or nil denyRules and askRules lists install a ToolRules hook with no rules that vetoes nothing
func Test_Unit_SetupAgent_EmptyToolRules_VetoesNothing(t *testing.T) {
	for _, rulesIn := range [][]string{{}, nil} {
		cfg := denyWiringConfig(t)
		cfg.DenyRules, cfg.AskRules = rulesIn, rulesIn
		_, tr := setupDenyWiringAgent(t, cfg)

		if got := tr.DenyRules(); len(got) != 0 {
			t.Errorf("rules %#v: DenyRules() = %q, want none", rulesIn, got)
		}
		if got := tr.AskRules(); len(got) != 0 {
			t.Errorf("rules %#v: AskRules() = %q, want none", rulesIn, got)
		}
		for _, name := range []string{"github_delete_file", "fs_remove_dir", "write_file", "read_file"} {
			if err := tr.BeforeTool(context.Background(), namedCall(name)); err != nil {
				t.Errorf("rules %#v: %s: expected allowed, got %v", rulesIn, name, err)
			}
		}
	}
}

// - Verify a config deny rule is applied, lowercased and trimmed, and replaces the (empty) deny default without
// touching the ask defaults: *delete* still needs approval rather than being denied
func Test_Unit_SetupAgent_ConfigDenyRule_IsApplied(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.DenyRules = []string{"  *Purge* "}
	_, tr := setupDenyWiringAgent(t, cfg)

	err := tr.BeforeTool(context.Background(), namedCall("db_purge_all"))
	if err == nil || err.Error() != `denied by rule "*purge*"` || errors.Is(err, hooks.ErrNeedsApproval) {
		t.Errorf("db_purge_all: err = %v, want %q (final, not ErrNeedsApproval)", err, `denied by rule "*purge*"`)
	}
	if level, rule := tr.Level("github_delete_file"); level != hooks.LevelAsk || rule != "*delete*" {
		t.Errorf("github_delete_file: Level = (%q, %q), want (ask, *delete*): the ask defaults still apply", level, rule)
	}
	if got, want := tr.DenyRules(), []string{"*purge*"}; !slices.Equal(got, want) {
		t.Errorf("DenyRules() = %q, want %q", got, want)
	}
}

// - Verify a config ask rule replaces the ask defaults rather than adding to them
func Test_Unit_SetupAgent_ConfigAskRule_ReplacesDefaults(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.AskRules = []string{"*Push*"}
	_, tr := setupDenyWiringAgent(t, cfg)

	if level, rule := tr.Level("git_push"); level != hooks.LevelAsk || rule != "*push*" {
		t.Errorf("git_push: Level = (%q, %q), want (ask, *push*)", level, rule)
	}
	if level, _ := tr.Level("github_delete_file"); level != hooks.LevelRun {
		t.Errorf("github_delete_file: Level = %q, want run once the config list replaced the ask defaults", level)
	}
}

// - Verify an invalid deny or ask rule fails SetupAgent with an error naming the list and the rule, before any MCP
// server is started. The configured stdio server would create a marker file if it were ever launched; it must not
// exist afterwards. (It uses sh, so on a system without sh the marker check passes vacuously; the error check still holds.)
func Test_Unit_SetupAgent_InvalidToolRule_ErrorsBeforeMCP(t *testing.T) {
	cases := []struct {
		list, rule string
	}{
		{"deny", "*delete["}, {"deny", "x:y"}, {"ask", "*delete["}, {"ask", "x:y"},
	}
	for _, c := range cases {
		t.Run(c.list+" "+c.rule, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "mcp-started")
			cfg := denyWiringConfig(t)
			if c.list == "deny" {
				cfg.DenyRules = []string{c.rule}
			} else {
				cfg.AskRules = []string{c.rule}
			}
			cfg.McpServers = map[string]config.McpServerConfig{
				"marker": {Command: "sh", Args: []string{"-c", `touch "$1"`, "sh", marker}}, // path passed as $1
			}
			a := &Agent{}
			err := a.SetupAgent(context.Background(), cfg, nil)
			defer a.ShutdownAgent(context.Background())

			if want := "invalid " + c.list + " rule " + `"` + c.rule + `"`; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("SetupAgent err = %v, want an error containing %q", err, want)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Error("the MCP server was started even though the tool rules were invalid")
			}
			if len(a.mcpConns) != 0 || a.session != nil {
				t.Errorf("setup went on past the tool rules: %d MCP conns, session %v", len(a.mcpConns), a.session != nil)
			}
		})
	}
}

// - Verify end to end, through the session SetupAgent built, that a model-issued call to a *delete* tool with nobody to
// ask (no prompter, allow-all not confirmed) is vetoed by the default ask rule: the tool never runs and the model's next
// request carries the veto
func Test_Unit_SetupAgent_ReActLoop_DeleteToolCallVetoed(t *testing.T) {
	a, _ := setupDenyWiringAgent(t, denyWiringConfig(t))
	fake := &modeltest.FakeProvider{Script: []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "delete_file", `{"path":"a.txt"}`),
		modeltest.AssistantText("I was not allowed to delete it."),
	}}
	a.session.Provider = fake // swap the real (never-dialled) provider for the scripted fake
	tool := &countingTool{}   // named delete_file; see hooks_test.go
	if err := a.session.Tools.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := reActLoop(context.Background(), a.session, trace.StartTask(a.session.Recorder, "t"), nil, modeltest.Msg("user", "u1", "delete a.txt"))

	if err != nil {
		t.Fatalf("reActLoop: %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("expected delete_file not to run, ran %d time(s)", tool.calls)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(fake.Calls))
	}
	second := fake.Calls[1]
	if last := second[len(second)-1]; last.Role != "tool" || last.Content != `error: needs approval by rule "*delete*"` {
		t.Errorf("expected the second request to end with the veto observation, got %+v", last)
	}
}

// - Verify SetupAgent leaves AllowAskRules off even with approval "allow-all": only confirmAllowAll (chat startup, on a
// terminal) may turn it on
func Test_Unit_SetupAgent_AllowAll_DoesNotSetAllowAskRules(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.Approval = config.ApprovalAllowAll
	a, _ := setupDenyWiringAgent(t, cfg)
	if a.session.AllowAskRules {
		t.Error("expected AllowAskRules false after SetupAgent; allow-all must be confirmed at chat startup")
	}
}

// - Verify an unknown approval value fails SetupAgent (checkConfig) with an error naming the value
func Test_Unit_SetupAgent_UnknownApproval_Fails(t *testing.T) {
	for _, v := range []string{"", "allow_all", "ALLOW-ALL", "yes"} {
		t.Run(v, func(t *testing.T) {
			cfg := denyWiringConfig(t)
			cfg.Approval = v
			a := &Agent{}
			err := a.SetupAgent(context.Background(), cfg, nil)
			defer a.ShutdownAgent(context.Background())
			if err == nil || !strings.Contains(err.Error(), "approval must be") || !strings.Contains(err.Error(), fmt.Sprintf("%q", v)) {
				t.Errorf("SetupAgent err = %v, want an approval error naming %q", err, v)
			}
			if a.session != nil {
				t.Error("expected no session after a bad approval value")
			}
		})
	}
}

// - Verify SetupAgent passes the config's denyPaths to read_file: the read_file it registers refuses a matching file
// and still reads others, and a bad pattern fails SetupAgent before any session is built
func Test_Unit_SetupAgent_DenyPaths_AppliedToReadFile(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.DenyPaths = []string{"*.secret"}
	for name, body := range map[string]string{"db.secret": "FAKE_SECRET_TOKEN_123", "notes.txt": "ALLOWED-NOTES"} {
		if err := os.WriteFile(filepath.Join(cfg.WorkDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := setupDenyWiringAgent(t, cfg)
	readFile, ok := a.session.Tools.Lookup("read_file")
	if !ok {
		t.Fatal("expected read_file to be registered")
	}

	if got, err := readFile.CallTool(context.Background(), []byte(`{"path":"db.secret"}`)); err == nil || !strings.Contains(err.Error(), "*.secret") {
		t.Errorf("expected db.secret to be blocked by *.secret, got (%q, %v)", got, err)
	}
	if got, err := readFile.CallTool(context.Background(), []byte(`{"path":"notes.txt"}`)); err != nil || got != "ALLOWED-NOTES" {
		t.Errorf("expected notes.txt to read, got (%q, %v)", got, err)
	}

	bad := denyWiringConfig(t)
	bad.DenyPaths = []string{"[abc"}
	b := &Agent{}
	err := b.SetupAgent(context.Background(), bad, nil)
	defer b.ShutdownAgent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "denyPaths") || b.session != nil {
		t.Errorf("expected SetupAgent to fail on a bad denyPaths pattern, got err %v, session %v", err, b.session != nil)
	}
}

// - Verify SetupAgent passes the config's maxSearchFiles to list_files: with a limit of 2 the registered list_files
// stops after two of three files, and a limit of 0 fails SetupAgent before any session is built
func Test_Unit_SetupAgent_MaxSearchFiles_AppliedToListFiles(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.BuiltinTools, cfg.MaxSearchFiles = []string{"list_files"}, 2
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(cfg.WorkDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := setupDenyWiringAgent(t, cfg)
	listFiles, ok := a.session.Tools.Lookup("list_files")
	if !ok {
		t.Fatal("expected list_files to be registered")
	}
	got, err := listFiles.CallTool(context.Background(), []byte(`{}`))
	if err != nil || strings.Contains(got, "c.txt") || !strings.Contains(got, "[stopped after looking at 2 entries") {
		t.Errorf("expected a listing stopped after 2 entries without c.txt, got (%q, %v)", got, err)
	}

	bad := denyWiringConfig(t)
	bad.BuiltinTools, bad.MaxSearchFiles = []string{"list_files"}, 0
	b := &Agent{}
	err = b.SetupAgent(context.Background(), bad, nil)
	defer b.ShutdownAgent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must be positive") || b.session != nil {
		t.Errorf("expected SetupAgent to fail on maxSearchFiles 0, got err %v, session %v", err, b.session != nil)
	}
}

// - Verify SetupAgent passes the config's ignorePaths to list_files: the default list leaves out a .pyc file, and a
// bad pattern fails SetupAgent before any session is built
func Test_Unit_SetupAgent_IgnorePaths_AppliedToListFiles(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.BuiltinTools = []string{"list_files"}
	for _, name := range []string{"cache.pyc", "main.py"} {
		if err := os.WriteFile(filepath.Join(cfg.WorkDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := setupDenyWiringAgent(t, cfg)
	listFiles, ok := a.session.Tools.Lookup("list_files")
	if !ok {
		t.Fatal("expected list_files to be registered")
	}
	got, err := listFiles.CallTool(context.Background(), []byte(`{}`))
	if err != nil || strings.Contains(got, "cache.pyc") || !strings.Contains(got, "main.py") {
		t.Errorf("expected main.py without cache.pyc, got (%q, %v)", got, err)
	}

	bad := denyWiringConfig(t)
	bad.IgnorePaths = []string{"a//b"}
	b := &Agent{}
	err = b.SetupAgent(context.Background(), bad, nil)
	defer b.ShutdownAgent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ignorePaths") || b.session != nil {
		t.Errorf("expected SetupAgent to fail on a bad ignorePaths pattern, got err %v, session %v", err, b.session != nil)
	}
}
