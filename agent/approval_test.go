package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
	"github.com/nyaosorg/go-readline-ny/keys"
	"github.com/nyaosorg/go-ttyadapter/auto"
)

// newRulesFixture returns a hook fixture whose only hook is a real *hooks.ToolRules built from deny and ask.
func newRulesFixture(t *testing.T, deny, ask []string) (*chatSessionFixture, *countingTool) {
	t.Helper()
	rules, err := hooks.NewToolRules(deny, ask)
	if err != nil {
		t.Fatalf("NewToolRules: %v", err)
	}
	return newHookFixture(t, rules)
}

// ---- checkBeforeToolHooks: deny / ask / run ----

// - Verify a deny-rule match is final: the user is never asked, even with a prompter and AllowAskRules on, the tool
// does not run, and the veto is recorded as denied
func Test_Unit_CheckBeforeToolHooks_DenyRule_NeverPrompts(t *testing.T) {
	fx, tool := newRulesFixture(t, []string{"*delete*"}, []string{"*delete*"}) // deny wins over the same ask rule
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true)
	fx.Session.AllowAskRules = true

	events := runVetoed(t, fx)

	if len(asked) != 0 {
		t.Errorf("expected no question for a deny rule, got %d", len(asked))
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
	vs := vetoes(events)
	want := trace.ToolVetoedData{CallID: "call_1", Name: "delete_file", Reason: `denied by rule "*delete*"`, Decision: trace.VetoDenied}
	if len(vs) != 1 || vs[0] != want {
		t.Errorf("tool_vetoed = %+v, want [%+v]", vs, want)
	}
	if fin := finished(t, events); !fin.Failed || fin.Content != `error: denied by rule "*delete*"` {
		t.Errorf("tool_finished = %+v, want failed with the denial", fin)
	}
}

// - Verify an ask-rule match with AllowAskRules on runs the tool without calling the prompter, and records
// allowed_by_config with no wait
func Test_Unit_CheckBeforeToolHooks_AskRule_AllowAskRulesRunsWithoutPrompt(t *testing.T) {
	fx, tool := newRulesFixture(t, nil, []string{"*delete*"})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, false) // would say no if it were asked
	fx.Session.AllowAskRules = true

	events := runVetoed(t, fx)

	if len(asked) != 0 {
		t.Errorf("expected no question with AllowAskRules on, got %d", len(asked))
	}
	vs := vetoes(events)
	want := trace.ToolVetoedData{CallID: "call_1", Name: "delete_file", Reason: `needs approval by rule "*delete*"`, Decision: trace.VetoAllowedByConfig}
	if len(vs) != 1 || vs[0] != want {
		t.Errorf("tool_vetoed = %+v, want [%+v]", vs, want)
	}
	if fin := finished(t, events); fin.Failed || fin.Content != "deleted" || tool.calls != 1 {
		t.Errorf("tool_finished = %+v, ran %d time(s); want not failed, content \"deleted\", one run", fin, tool.calls)
	}
}

// - Verify an ask-rule match asks the user (with the ask rule's error as the reason): yes runs the tool and records
// allowed_by_user, no keeps the veto and records denied_by_user
func Test_Unit_CheckBeforeToolHooks_AskRule_PrompterDecides(t *testing.T) {
	cases := []struct {
		name         string
		answer       bool
		wantDecision string
		wantRuns     int
	}{
		{"yes", true, trace.VetoAllowedByUser, 1},
		{"no", false, trace.VetoDeniedByUser, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx, tool := newRulesFixture(t, nil, []string{"*delete*"})
			var asked []askRecord
			fx.Session.PromptAllowDenyCall = fakeAsker(&asked, c.answer)

			events := runVetoed(t, fx)

			if len(asked) != 1 {
				t.Fatalf("expected the user to be asked once, got %d", len(asked))
			}
			if r := asked[0].reason; !errors.Is(r, hooks.ErrNeedsApproval) || r.Error() != `needs approval by rule "*delete*"` {
				t.Errorf("prompt reason = %v, want the ask rule's error", r)
			}
			vs := vetoes(events)
			if len(vs) != 1 || vs[0].Decision != c.wantDecision {
				t.Errorf("tool_vetoed = %+v, want one %s", vs, c.wantDecision)
			}
			if tool.calls != c.wantRuns {
				t.Errorf("tool ran %d time(s), want %d", tool.calls, c.wantRuns)
			}
		})
	}
}

// - Verify an ask-rule match with nobody to ask (no prompter, AllowAskRules off) is denied with the ask rule's error
func Test_Unit_CheckBeforeToolHooks_AskRule_NoPrompterDenies(t *testing.T) {
	fx, tool := newRulesFixture(t, nil, []string{"*delete*"})
	fx.Session.PromptAllowDenyCall = nil

	err := checkBeforeToolHooks(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())
	if !errors.Is(err, hooks.ErrNeedsApproval) {
		t.Errorf("expected an error wrapping ErrNeedsApproval, got %v", err)
	}

	vs := vetoes(runVetoed(t, fx))
	if len(vs) != 1 || vs[0].Decision != trace.VetoDenied {
		t.Errorf("tool_vetoed = %+v, want one denied", vs)
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
}

// - Verify a hook error that does not wrap ErrNeedsApproval is final: never asked, not overridden by AllowAskRules,
// recorded as denied, and the tool does not run
func Test_Unit_CheckBeforeToolHooks_PlainHookError_IsFinal(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true)
	fx.Session.AllowAskRules = true

	events := runVetoed(t, fx)

	if len(asked) != 0 {
		t.Errorf("expected no question for a final veto, got %d", len(asked))
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
	if vs := vetoes(events); len(vs) != 1 || vs[0].Decision != trace.VetoDenied || vs[0].Reason != errDenied.Error() {
		t.Errorf("tool_vetoed = %+v, want one denied with reason %q", vs, errDenied)
	}
}

// - Verify a name matching no rule runs with no question and no tool_vetoed, whatever AllowAskRules is
func Test_Unit_CheckBeforeToolHooks_NoRuleMatch_Runs(t *testing.T) {
	for _, allow := range []bool{false, true} {
		fx, tool := newRulesFixture(t, []string{"*push*"}, []string{"*write*"})
		var asked []askRecord
		fx.Session.PromptAllowDenyCall = fakeAsker(&asked, false)
		fx.Session.AllowAskRules = allow

		events := runVetoed(t, fx)

		if len(asked) != 0 || len(vetoes(events)) != 0 || tool.calls != 1 {
			t.Errorf("AllowAskRules=%v: asked %d, vetoes %+v, ran %d; want 0, none, 1", allow, len(asked), vetoes(events), tool.calls)
		}
	}
}

// ---- confirmAllowAll ----

// allowAllFixture returns a session with approval set and Output and OutBuffer on one buffer.
func allowAllFixture(t *testing.T, approval string) (*chatSessionFixture, *syncBuffer) {
	t.Helper()
	fx := newChatSessionFixture(t, 10, 100)
	out := &syncBuffer{}
	fx.Session.OutBuffer = out
	fx.Session.Output = chatOutput{w: out}
	fx.Session.Config.Approval = approval
	fx.Session.Config.DenyRules = []string{"*push*"}
	fx.Session.Config.AskRules = []string{"*delete*"}
	return fx, out
}

const allowAllQuestion = "Run ask-rule tool calls without asking for this session? [y/N] "

// - Verify approval "ask" never asks and prints nothing, even with a terminal whose next answer would be yes
func Test_Unit_ConfirmAllowAll_ApprovalAsk_NoPromptNoOutput(t *testing.T) {
	fx, out := allowAllFixture(t, config.ApprovalAsk)
	tty := newTTYReaderWith(&auto.Pilot{Text: keystrokes(typed("y"), []string{keys.CtrlM})}, out)

	confirmAllowAll(context.Background(), fx.Session, tty)

	if fx.Session.AllowAskRules {
		t.Error("expected AllowAskRules false with approval ask")
	}
	if got := out.String(); got != "" {
		t.Errorf("expected no output, got %q", got)
	}
}

// - Verify allow-all with no terminal leaves AllowAskRules off and says ask-rule calls will be denied
func Test_Unit_ConfirmAllowAll_NoTTY_StaysOffWithNotice(t *testing.T) {
	fx, out := allowAllFixture(t, config.ApprovalAllowAll)

	confirmAllowAll(context.Background(), fx.Session, nil)

	if fx.Session.AllowAskRules {
		t.Error("expected AllowAskRules false with no terminal to confirm")
	}
	want := "[system] approval is allow-all, but there is no terminal to confirm it: tool calls that match ask rules will be denied\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// - Verify allow-all on a terminal explains itself (naming the ask and deny rules), asks, and only "y"/"yes"
// turns AllowAskRules on; n, Enter, other text, Ctrl+C and end of input leave it off and say so
func Test_Unit_ConfirmAllowAll_TTY_AnswerDecides(t *testing.T) {
	cases := []struct {
		name  string
		keyin []string
		want  bool
	}{
		{"y", keystrokes(typed("y"), []string{keys.CtrlM}), true},
		{"YES", keystrokes(typed("YES"), []string{keys.CtrlM}), true},
		{"n", keystrokes(typed("n"), []string{keys.CtrlM}), false},
		{"Enter", []string{keys.CtrlM}, false},
		{"other text", keystrokes(typed("sure"), []string{keys.CtrlM}), false},
		{"Ctrl+C", []string{keys.CtrlC}, false},
		{"Ctrl+D", []string{keys.CtrlD}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx, out := allowAllFixture(t, config.ApprovalAllowAll)
			tty := newTTYReaderWith(&auto.Pilot{Text: c.keyin}, out)

			confirmAllowAll(context.Background(), fx.Session, tty)

			if fx.Session.AllowAskRules != c.want {
				t.Errorf("AllowAskRules = %v, want %v", fx.Session.AllowAskRules, c.want)
			}
			got := out.String()
			explain := "[system] approval is allow-all: tool calls that match ask rules [*delete*] will run without asking; deny rules [*push*] still apply\n"
			if !strings.HasPrefix(got, explain) {
				t.Errorf("expected the output to start with %q, got %q", explain, got)
			}
			if !strings.Contains(got, allowAllQuestion) { // [agent] the line editor may redraw the prompt, so not counted
				t.Errorf("expected the question %q in %q", allowAllQuestion, got)
			}
			on := "[system] allow-all on: ask-rule tool calls run without asking (deny rules still apply)\n"
			off := "[system] allow-all declined: you will be asked before each tool call that matches an ask rule\n"
			wantLine, notLine := off, on
			if c.want {
				wantLine, notLine = on, off
			}
			if !strings.HasSuffix(got, wantLine) || strings.Contains(got, notLine) {
				t.Errorf("expected the output to end with %q and not contain %q, got %q", wantLine, notLine, got)
			}
		})
	}
}

// ---- chatLoop end to end ----

// - Verify on a (scripted) terminal with approval allow-all: confirming at startup lets an ask-rule call run without
// the "Allow once?" question, while a deny-rule call is still refused
func Test_ChatLoop_TTY_AllowAllConfirmed_AskRuleRunsDenyRuleDoesNot(t *testing.T) {
	fx, tool := newRulesFixture(t, []string{"*push*"}, []string{"*delete*"})
	out := &syncBuffer{}
	fx.Session.OutBuffer = out
	fx.Session.Output = chatOutput{w: out}
	fx.Session.Config.Approval = config.ApprovalAllowAll
	pushLog := &eventLog{}
	if err := fx.Tools.Register(loggingTool{name: "git_push", log: pushLog}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "delete_file", `{"path":"a.txt"}`),
		modeltest.AssistantToolCall("call_2", "git_push", `{}`),
		modeltest.AssistantText("done"),
	}
	keyin := keystrokes(typed("y"), []string{keys.CtrlM}, typed("delete a.txt"), []string{keys.CtrlM}, []string{keys.CtrlD})
	reader := newTTYReaderWith(&auto.Pilot{Text: keyin}, out)

	done := make(chan struct{})
	go func() {
		defer close(done)
		chatLoop(context.Background(), nil, fx.Session, reader)
	}()
	receiveOrFail(t, done, "the chat loop to end")

	if !fx.Session.AllowAskRules {
		t.Fatal("expected AllowAskRules on after confirming")
	}
	if tool.calls != 1 {
		t.Errorf("expected the ask-rule tool to run once, ran %d time(s)", tool.calls)
	}
	got := out.String()
	if strings.Contains(got, "Allow once?") || strings.Contains(got, "needs approval:") {
		t.Errorf("expected no per-call question after allow-all was confirmed:\n%s", got)
	}
	if ran := pushLog.list(); len(ran) != 0 {
		t.Errorf("expected the deny-rule tool not to run, got %q", ran)
	}
	last := fx.Provider.Calls[2]
	if obs := last[len(last)-1]; obs.Content != `error: denied by rule "*push*"` {
		t.Errorf("observation for git_push = %q, want the deny rule's veto", obs.Content)
	}
}

// - Verify with approval allow-all and non-terminal input the chat loop prints the "will be denied" notice and an
// ask-rule call is denied without asking
func Test_ChatLoop_PlainReader_AllowAll_AskRuleDenied(t *testing.T) {
	fx, tool := newRulesFixture(t, nil, []string{"*delete*"})
	fx.Session.Config.Approval = config.ApprovalAllowAll
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "delete_file", `{"path":"a.txt"}`),
		modeltest.AssistantText("I was not allowed to delete it."),
	}

	out, done := startChatLoop(fx, "delete a.txt\n", nil)
	receiveOrFail(t, done, "runChatLoop to return at end of input")

	if fx.Session.AllowAskRules {
		t.Error("expected AllowAskRules false without a terminal")
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
	if !strings.Contains(out.String(), "no terminal to confirm it: tool calls that match ask rules will be denied") {
		t.Errorf("expected the will-be-denied notice:\n%s", out.String())
	}
	second := fx.Provider.Calls[1]
	if obs := second[len(second)-1]; obs.Content != `error: needs approval by rule "*delete*"` {
		t.Errorf("observation = %q, want the ask rule's veto", obs.Content)
	}
}
