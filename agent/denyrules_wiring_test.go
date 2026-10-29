package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// setupDenyWiringAgent runs SetupAgent and returns the agent and its single *hooks.DenyRules hook.
func setupDenyWiringAgent(t *testing.T, cfg config.Config) (*Agent, *hooks.DenyRules) {
	t.Helper()
	a := &Agent{}
	if err := a.SetupAgent(context.Background(), cfg, nil); err != nil {
		t.Fatalf("SetupAgent: %v", err)
	}
	t.Cleanup(func() { a.ShutdownAgent(context.Background()) })
	if len(a.session.Hooks) != 1 {
		t.Fatalf("expected exactly 1 hook on the session, got %d", len(a.session.Hooks))
	}
	dr, ok := a.session.Hooks[0].(*hooks.DenyRules)
	if !ok {
		t.Fatalf("expected the hook to be a *hooks.DenyRules, got %T", a.session.Hooks[0])
	}
	return a, dr
}

func namedCall(name string) model.ToolCall {
	return model.ToolCall{ID: "call_1", Function: model.ToolCallFunc{Name: name, Arguments: `{}`}}
}

// - Verify SetupAgent with no denyRules in config installs exactly one hook, a *hooks.DenyRules with the defaults:
// a *delete* name is denied, a name matching no rule is allowed
func Test_Unit_SetupAgent_NoConfigDenyRules_InstallsDefaultDenyRules(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.DenyRules = nil
	_, dr := setupDenyWiringAgent(t, cfg)

	err := dr.BeforeTool(context.Background(), namedCall("github_delete_file"))
	if err == nil || !strings.Contains(err.Error(), `denied by rule "*delete*"`) {
		t.Errorf("github_delete_file: err = %v, want one containing %q", err, `denied by rule "*delete*"`)
	}
	if err := dr.BeforeTool(context.Background(), namedCall("filesystem_move_file")); err != nil {
		t.Errorf("filesystem_move_file: expected allowed, got %v", err)
	}
}

// - Verify a config extra rule is applied, lowercased and trimmed, on top of the defaults
func Test_Unit_SetupAgent_ConfigDenyRule_IsApplied(t *testing.T) {
	cfg := denyWiringConfig(t)
	cfg.DenyRules = []string{"  *Purge* "}
	_, dr := setupDenyWiringAgent(t, cfg)

	err := dr.BeforeTool(context.Background(), namedCall("db_purge_all"))
	if err == nil || !strings.Contains(err.Error(), `denied by rule "*purge*"`) {
		t.Errorf("db_purge_all: err = %v, want one containing %q", err, `denied by rule "*purge*"`)
	}
	if err := dr.BeforeTool(context.Background(), namedCall("github_delete_file")); err == nil {
		t.Error("github_delete_file: expected the default *delete* rule to still deny it")
	}
}

// - Verify an invalid deny rule fails SetupAgent with an error naming the rule, before any MCP server is started.
// The configured stdio server would create a marker file if it were ever launched; it must not exist afterwards.
// (It uses sh, so on a system without sh the marker check passes vacuously; the error check still holds.)
func Test_Unit_SetupAgent_InvalidDenyRule_ErrorsBeforeMCP(t *testing.T) {
	for _, rule := range []string{"*delete[", "x:y"} {
		t.Run(rule, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "mcp-started")
			cfg := denyWiringConfig(t)
			cfg.DenyRules = []string{rule}
			cfg.McpServers = map[string]config.McpServerConfig{
				"marker": {Command: "sh", Args: []string{"-c", `touch "$1"`, "sh", marker}}, // path passed as $1
			}
			a := &Agent{}
			err := a.SetupAgent(context.Background(), cfg, nil)
			defer a.ShutdownAgent(context.Background())

			if err == nil || !strings.Contains(err.Error(), `"`+rule+`"`) {
				t.Fatalf("SetupAgent err = %v, want an error naming %q", err, rule)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Error("the MCP server was started even though the deny rules were invalid")
			}
			if len(a.mcpConns) != 0 || a.session != nil {
				t.Errorf("setup went on past the deny rules: %d MCP conns, session %v", len(a.mcpConns), a.session != nil)
			}
		})
	}
}

// - Verify end to end, through the session SetupAgent built, that a model-issued call to a *delete* tool is vetoed:
// the tool never runs and the model's next request carries the denial
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
	if last := second[len(second)-1]; last.Role != "tool" || last.Content != `error: denied by rule "*delete*"` {
		t.Errorf("expected the second request to end with the denial observation, got %+v", last)
	}
}
