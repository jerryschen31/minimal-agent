//go:build unix

package agent

import (
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/trace"
)

func runCommandCall(args string) model.ToolCall {
	return model.ToolCall{ID: "c1", Function: model.ToolCallFunc{Name: "run_command", Arguments: args}}
}

// - Verify run_command through the whole agent setup: listed in builtinTools and runCommands it runs unasked when no rule
// matches; an ask rule on its command line is refused with nobody to ask, runs under allow-all, and a deny rule is refused
// even under allow-all; a program not in runCommands is refused by the tool
func Test_SetupAgent_RunCommand_ApprovalLevels(t *testing.T) {
	run := func(cfgEdit func(c *config.Config), allowAll bool, args string) string {
		cfg := denyWiringConfig(t)
		cfg.BuiltinTools = []string{"run_command"}
		cfg.RunCommands = []string{"echo"}
		cfgEdit(&cfg)
		a, _ := setupDenyWiringAgent(t, cfg)
		a.session.AllowAskRules = allowAll
		return runToolCall(t.Context(), a.session, trace.StartTask(a.session.Recorder, "t"), runCommandCall(args)).Content
	}
	echo := `{"command":"echo","args":["hello","world"]}`

	if got := run(func(c *config.Config) {}, false, echo); !strings.HasPrefix(got, "hello world\n") || !strings.HasSuffix(got, "[exit status 0]") {
		t.Errorf("no rule: %q", got)
	}
	ask := func(c *config.Config) { c.AskRules = []string{"run_command:echo hello*"} }
	if got := run(ask, false, echo); !strings.Contains(got, "needs approval") || strings.Contains(got, "hello world") {
		t.Errorf("ask rule, nobody to ask: %q", got)
	}
	if got := run(ask, true, echo); !strings.HasPrefix(got, "hello world\n") {
		t.Errorf("ask rule, allow-all: %q", got)
	}
	if got := run(ask, false, `{"command":"echo","args":["other"]}`); !strings.HasPrefix(got, "other\n") {
		t.Errorf("ask rule does not match this command line: %q", got)
	}
	deny := func(c *config.Config) { c.DenyRules = []string{"run_command:echo*"} }
	if got := run(deny, true, echo); !strings.Contains(got, "denied by rule") || strings.Contains(got, "hello world") {
		t.Errorf("deny rule under allow-all: %q", got)
	}
	if got := run(func(c *config.Config) {}, false, `{"command":"rm","args":["-rf","x"]}`); !strings.Contains(got, "is not allowed") {
		t.Errorf("program not in runCommands: %q", got)
	}
}
