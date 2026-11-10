package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
)

// blockingTool waits for its context to end and returns the context's error, like a tool that honours ctx.
type blockingTool struct{}

func (blockingTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef("blocker", "waits for its context", nil)
}
func (blockingTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func runBlocker(t *testing.T, ctx context.Context, toolSecs int) (string, time.Duration) {
	t.Helper()
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.ToolTimeoutSeconds = toolSecs
	if err := fx.Tools.Register(blockingTool{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	msg, _ := runToolCall(ctx, fx.Session, trace.StartTask(fx.Session.Recorder, "t"), model.ToolCall{ID: "c1", Function: model.ToolCallFunc{Name: "blocker", Arguments: "{}"}})
	return msg.Content, time.Since(start)
}

// - Verify a tool call that runs past toolTimeoutSeconds is stopped and the model gets an error observation that says the
// tool timed out (so it can react), for any tool, not only run_command
func Test_ToolTimeout_StopsTheCall_ModelSeesIt(t *testing.T) {
	got, took := runBlocker(t, context.Background(), 1)
	if !strings.HasPrefix(got, "error: tool timed out after 1s (toolTimeoutSeconds)") {
		t.Errorf("content = %q", got)
	}
	if took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}

// - Verify the label "tool timed out" is used only for the tool's own limit: a Ctrl+C (cancelled parent) and the turn's
// deadline (a parent with a deadline) end the call without it
func Test_ToolTimeout_NotUsedForParentCancelOrTurnDeadline(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, _ := runBlocker(t, cancelled, 60); strings.Contains(got, "timed out") {
		t.Errorf("Ctrl+C labelled as a tool timeout: %q", got)
	}
	turn, cancelTurn := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelTurn()
	if got, _ := runBlocker(t, turn, 60); strings.Contains(got, "tool timed out") {
		t.Errorf("turn deadline labelled as a tool timeout: %q", got)
	}
}

// - Verify a tool timeout of 0 (a hand-built config in a test) means no limit rather than an instant timeout
func Test_ToolTimeout_ZeroMeansNoLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, _ := runBlocker(t, ctx, 0)
	if strings.Contains(got, "toolTimeoutSeconds") {
		t.Errorf("content = %q", got)
	}
}

// - Verify a turn that runs past turnTimeoutSeconds is stopped with its own message (not [interrupted]) and adds nothing
// to memory
func Test_TurnTimeout_StopsTheTurn_NothingInMemory(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.TurnTimeoutSeconds = 1
	fx.Provider.Gate = make(chan struct{}) // the model never answers
	start := time.Now()
	handleUserInput(context.Background(), fx.Session, "hello")
	out := fx.Out.String()
	if !strings.Contains(out, "turn timed out after 1s (turnTimeoutSeconds)") || strings.Contains(out, "[interrupted]") {
		t.Errorf("output = %q", out)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if n := len(fx.History.GetMessages()); n != 0 {
		t.Errorf("history has %d messages, want 0", n)
	}
}

// - Verify Ctrl+C (a cancelled parent) during a turn that has a turn timeout still prints [interrupted], not the timeout message
func Test_TurnTimeout_CtrlCStillInterrupted(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.TurnTimeoutSeconds = 60
	fx.Provider.Gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handleUserInput(ctx, fx.Session, "hello")
	out := fx.Out.String()
	if !strings.Contains(out, "[interrupted]") || strings.Contains(out, "timed out") {
		t.Errorf("output = %q", out)
	}
}

// - Verify a turn within the limit is unaffected
func Test_TurnTimeout_FastTurnSucceeds(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Config.TurnTimeoutSeconds = 60
	fx.Session.Config.ToolTimeoutSeconds = 30
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("hi")}
	handleUserInput(context.Background(), fx.Session, "hello")
	if n := len(fx.History.GetMessages()); n != 2 {
		t.Errorf("history has %d messages, want 2 (output: %q)", n, fx.Out.String())
	}
}

// - Verify checkConfig rejects non-positive tool and turn timeouts, and a turn limit below the tool limit; equal is fine
func Test_Unit_CheckConfig_Timeouts(t *testing.T) {
	cases := []struct {
		name       string
		tool, turn int
		want       string
	}{
		{"tool zero", 0, 600, "toolTimeoutSeconds"},
		{"tool negative", -1, 600, "toolTimeoutSeconds"},
		{"turn zero", 300, 0, "turnTimeoutSeconds"},
		{"turn below tool", 300, 299, "must not be less than"},
		{"equal is allowed", 300, 300, ""},
		{"defaults", 300, 600, ""},
	}
	for _, c := range cases {
		cfg := config.GetDefaultConfig(config.ModeChat)
		cfg.ToolTimeoutSeconds, cfg.TurnTimeoutSeconds = c.tool, c.turn
		err := checkConfig(cfg)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
	d := config.GetDefaultConfig(config.ModeChat)
	if d.ToolTimeoutSeconds != 300 || d.TurnTimeoutSeconds != 600 || errors.Is(checkConfig(d), context.Canceled) {
		t.Errorf("defaults: tool %d, turn %d", d.ToolTimeoutSeconds, d.TurnTimeoutSeconds)
	}
}
