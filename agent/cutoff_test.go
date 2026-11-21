package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
)

// - Verify a text reply cut off at the output limit is kept as the answer and the user is told, with the configured
// limit, or that the server's own limit applied when maxTokens is not set
func Test_ReActLoop_CutOffTextReply_WarnsAndKeepsReply(t *testing.T) {
	cases := []struct {
		maxTokens int
		want      string
	}{
		{20, "cut off at the output limit (maxTokens 20)"},
		{0, "cut off at the server's output limit (maxTokens is not set)"},
	}
	for _, c := range cases {
		fx := newChatSessionFixture(t, 10, 100)
		fx.Session.Config.MaxTokens = c.maxTokens
		fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("the first half of")}
		fx.Provider.StopReasons = []string{model.StopMaxTokens}

		turn, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

		if err != nil {
			t.Fatalf("maxTokens %d: expected no error, got %v", c.maxTokens, err)
		}
		if last := turn[len(turn)-1]; last.Content != "the first half of" {
			t.Errorf("maxTokens %d: expected the partial reply to be kept, got %+v", c.maxTokens, last)
		}
		if !strings.Contains(fx.Out.String(), c.want) {
			t.Errorf("maxTokens %d: expected the warning %q, got output:\n%s", c.maxTokens, c.want, fx.Out.String())
		}
	}
}

// - Verify that in a reply cut off at the output limit, a complete tool call still runs, while the call whose
// arguments were cut in half fails with the cut-off hint, and the loop goes on to the next model call
func Test_ReActLoop_CutOffToolCall_FailingCallGetsHint(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	calls := 0
	if err := fx.Tools.Register(namedTool{name: "write", result: "written", calls: &calls}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	response := modeltest.AssistantToolCall("c1", "write", `{"path":"a.txt","content":"short"}`)
	response.ToolCalls = append(response.ToolCalls, model.ToolCall{ID: "c2", Type: "function",
		Function: model.ToolCallFunc{Name: "write", Arguments: `{"path":"b.txt","content":"a very long fi`}})
	fx.Provider.Script = []model.ChatMessage{response, modeltest.AssistantText("done")}
	fx.Provider.StopReasons = []string{model.StopMaxTokens}
	fx.Session.Config.MaxTokens = 50

	turn, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

	if err != nil || len(fx.Provider.Calls) != 2 {
		t.Fatalf("err %v, model calls %d; want nil and 2", err, len(fx.Provider.Calls))
	}
	if calls != 1 {
		t.Errorf("expected the complete call to run once and the cut one not at all, got %d runs", calls)
	}
	// user, assistant, result c1, result c2, final answer
	if len(turn) != 5 || turn[2].Content != "written" {
		t.Fatalf("turn = %+v", turn)
	}
	if got := turn[3].Content; !strings.Contains(got, "invalid JSON arguments") || !strings.Contains(got, cutOffCallHint) {
		t.Errorf("expected the cut call's error to carry the hint, got %q", got)
	}
	if !strings.Contains(fx.Out.String(), "cut off at the output limit (maxTokens 50)") {
		t.Errorf("expected the cut-off warning, got output:\n%s", fx.Out.String())
	}
}

// - Verify an empty reply cut off at the output limit (a server that dropped a half-written tool call, as Ollama does)
// says the model was probably writing a tool call; a cut-off reply with text does not
func Test_ReActLoop_CutOffEmptyReply_SaysToolCallWasDropped(t *testing.T) {
	for _, text := range []string{"", "some text"} {
		fx := newChatSessionFixture(t, 10, 100)
		fx.Session.Config.MaxTokens = 40
		fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText(text)}
		fx.Provider.StopReasons = []string{model.StopMaxTokens}

		if _, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi")); err != nil {
			t.Fatalf("reply %q: expected no error, got %v", text, err)
		}
		said := strings.Contains(fx.Out.String(), "probably writing a tool call")
		if text == "" && !said {
			t.Errorf("empty reply: expected the dropped-tool-call note, got output:\n%s", fx.Out.String())
		}
		if text != "" && said {
			t.Errorf("reply with text: expected no dropped-tool-call note, got output:\n%s", fx.Out.String())
		}
	}
}

// - Verify a call with broken arguments in a reply that was NOT cut off gets the plain error, without the hint
func Test_ReActLoop_BrokenArgsWithoutCutOff_NoHint(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	if err := fx.Tools.Register(namedTool{name: "write", result: "written"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("c1", "write", `{"path":"b.txt",`),
		modeltest.AssistantText("done"),
	}
	fx.Provider.StopReasons = []string{model.StopToolCalls, model.StopEnd}

	turn, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

	if err != nil || len(turn) != 4 {
		t.Fatalf("err %v, turn %+v", err, turn)
	}
	if got := turn[2].Content; !strings.Contains(got, "invalid JSON arguments") || strings.Contains(got, "cut off") {
		t.Errorf("expected the plain invalid-JSON error with no cut-off hint, got %q", got)
	}
	if strings.Contains(fx.Out.String(), "cut off") {
		t.Errorf("no reply was cut off, but the output says so:\n%s", fx.Out.String())
	}
}

// - Verify checkConfig accepts maxTokens 0 (not sent) and the three maxTokensField values, and rejects a negative
// maxTokens and an unknown field, naming the key
func Test_Unit_CheckConfig_MaxTokens(t *testing.T) {
	for _, field := range []string{"", "max_tokens", "max_completion_tokens"} {
		cfg := config.GetDefaultConfig(config.ModeChat)
		cfg.MaxTokensField = field
		if err := checkConfig(cfg); err != nil {
			t.Errorf("maxTokensField %q: expected no error, got %v", field, err)
		}
	}
	cfg := config.GetDefaultConfig(config.ModeChat)
	cfg.MaxTokens = -1
	if err := checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "maxTokens must not be negative") {
		t.Errorf("maxTokens -1: err = %v, want a maxTokens error", err)
	}
	cfg = config.GetDefaultConfig(config.ModeChat)
	cfg.MaxTokensField = "max_output_tokens"
	if err := checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "maxTokensField") {
		t.Errorf("maxTokensField typo: err = %v, want a maxTokensField error", err)
	}
}

//   - Verify a failure in a cut-off reply that is not about broken arguments (here an unknown tool) gets no cut-off hint
//     (PR #46 review): the hint is only for arguments that don't parse
func Test_ReActLoop_CutOff_OtherFailureNoHint(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantToolCall("c1", "no_such_tool", `{}`), modeltest.AssistantText("done")}
	fx.Provider.StopReasons = []string{model.StopMaxTokens}

	turn, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

	if err != nil || len(turn) != 4 {
		t.Fatalf("err %v, turn %+v", err, turn)
	}
	if got := turn[2].Content; !strings.Contains(got, "unknown tool") || strings.Contains(got, cutOffCallHint) {
		t.Errorf("expected the unknown-tool error without the cut-off hint, got %q", got)
	}
}
