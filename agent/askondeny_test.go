package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/trace"
	"github.com/nyaosorg/go-readline-ny/keys"
	"github.com/nyaosorg/go-ttyadapter/auto"
)

// askRecord is one call to a fake PromptAllowDenyCall: the call it was asked about and the veto reason.
type askRecord struct {
	call   model.ToolCall
	reason error
}

// fakeAsker returns a PromptAllowDenyCall that answers from answers in order (false once they run out)
// and records each question in *asked.
func fakeAsker(asked *[]askRecord, answers ...bool) func(context.Context, model.ToolCall, error) bool {
	return func(_ context.Context, call model.ToolCall, reason error) bool {
		*asked = append(*asked, askRecord{call, reason})
		if len(*asked) > len(answers) {
			return false
		}
		return answers[len(*asked)-1]
	}
}

// ---- checkBeforeToolHooks / runToolCall with a fake PromptAllowDenyCall ----

// - Verify the user approving a vetoed call lets the tool run, and the result is the tool's output
func Test_RunToolCall_AskOnDeny_ApprovedRuns(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true)

	got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

	if tool.calls != 1 || got.Content != "deleted" {
		t.Errorf("expected the tool to run once and return %q; ran %d time(s), content %q", "deleted", tool.calls, got.Content)
	}
	if len(asked) != 1 {
		t.Errorf("expected the user to be asked once, got %d", len(asked))
	}
}

// - Verify the user declining keeps the denial: the tool does not run and the veto becomes the observation
func Test_RunToolCall_AskOnDeny_DeclinedDenied(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, false)

	got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
	if want := `error: denied by rule "*delete*"`; got.Content != want {
		t.Errorf("expected content %q, got %q", want, got.Content)
	}
	if len(asked) != 1 {
		t.Errorf("expected the user to be asked once, got %d", len(asked))
	}
}

// - Verify a nil PromptAllowDenyCall (non-terminal input) denies without asking
func Test_CheckBeforeToolHooks_AskOnDeny_NilDeniesWithoutAsking(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	fx.Session.PromptAllowDenyCall = nil

	err := checkBeforeToolHooks(context.Background(), fx.Session, deleteCall())

	if !errors.Is(err, errDenied) {
		t.Errorf("expected the hook's veto error, got %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
}

// - Verify the prompt is given the exact call the model made and the hook's veto error as the reason
func Test_CheckBeforeToolHooks_AskOnDeny_ReceivesCallAndReason(t *testing.T) {
	fx, _ := newHookFixture(t, &fakeHook{err: errDenied})
	var asked []askRecord
	fx.Session.PromptAllowDenyCall = fakeAsker(&asked, false)
	call := deleteCall()

	_ = checkBeforeToolHooks(context.Background(), fx.Session, call)

	if len(asked) != 1 {
		t.Fatalf("expected the user to be asked once, got %d", len(asked))
	}
	if asked[0].call != call {
		t.Errorf("prompt got call %+v, want %+v", asked[0].call, call)
	}
	if asked[0].reason != errDenied {
		t.Errorf("prompt got reason %v, want the hook's error %v", asked[0].reason, errDenied)
	}
}

// - Verify the user is not asked when no hook vetoes (with one allowing hook, and with no hooks at all)
func Test_RunToolCall_AskOnDeny_NotAskedWhenAllowed(t *testing.T) {
	for name, hs := range map[string][]*fakeHook{"allowing hook": {{}}, "no hooks": nil} {
		t.Run(name, func(t *testing.T) {
			fx, tool := newHookFixture(t)
			for _, h := range hs {
				fx.Session.Hooks = append(fx.Session.Hooks, h)
			}
			var asked []askRecord
			fx.Session.PromptAllowDenyCall = fakeAsker(&asked, true)

			got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

			if len(asked) != 0 {
				t.Errorf("expected no question, got %d", len(asked))
			}
			if tool.calls != 1 || got.Content != "deleted" {
				t.Errorf("expected the tool to run; ran %d time(s), content %q", tool.calls, got.Content)
			}
		})
	}
}

// - Verify each vetoing hook asks separately: approving only the first still leaves the second's denial;
// approving both runs the tool
func Test_RunToolCall_AskOnDeny_TwoVetoingHooks(t *testing.T) {
	errSecond := errors.New(`denied by rule "*file*"`)
	tests := []struct {
		name        string
		answers     []bool
		wantCalls   int
		wantContent string
	}{
		{"approve first only", []bool{true, false}, 0, `error: denied by rule "*file*"`},
		{"approve both", []bool{true, true}, 1, "deleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &fakeHook{name: "first", err: errDenied}
			second := &fakeHook{name: "second", err: errSecond}
			fx, tool := newHookFixture(t, first, second)
			var asked []askRecord
			fx.Session.PromptAllowDenyCall = fakeAsker(&asked, tt.answers...)

			got := runToolCall(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "t"), deleteCall())

			if len(asked) != 2 {
				t.Fatalf("expected the user to be asked twice, got %d", len(asked))
			}
			if asked[0].reason != errDenied || asked[1].reason != errSecond {
				t.Errorf("questions had reasons %v, %v; want %v, %v", asked[0].reason, asked[1].reason, errDenied, errSecond)
			}
			if tool.calls != tt.wantCalls || got.Content != tt.wantContent {
				t.Errorf("tool ran %d time(s), content %q; want %d, %q", tool.calls, got.Content, tt.wantCalls, tt.wantContent)
			}
		})
	}
}

// ---- (*ttyReader).AskYesNo with a scripted terminal ----

// - Verify only "y"/"yes" (any case, surrounding spaces ignored) count as yes; anything else, and Ctrl+C, is no
func Test_TTYReader_AskYesNo_Answers(t *testing.T) {
	tests := []struct {
		name  string
		keyin []string
		want  bool
	}{
		{"y", keystrokes(typed("y"), []string{keys.CtrlM}), true},
		{"YES", keystrokes(typed("YES"), []string{keys.CtrlM}), true},
		{"padded yes", keystrokes(typed(" yes "), []string{keys.CtrlM}), true},
		{"n", keystrokes(typed("n"), []string{keys.CtrlM}), false},
		{"just Enter", []string{keys.CtrlM}, false},
		{"maybe", keystrokes(typed("maybe"), []string{keys.CtrlM}), false},
		{"Ctrl+C", []string{keys.CtrlC}, false},
		{"Ctrl+C after typing y", keystrokes(typed("y"), []string{keys.CtrlC}), false},
		{"Ctrl+D (input ends)", []string{keys.CtrlD}, false},
		{"multi-line y, es", keystrokes(typed("y"), []string{keys.CtrlJ}, typed("es"), []string{keys.CtrlM}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newScriptedTTYReader(tt.keyin)
			if got := r.AskYesNo(context.Background(), "Allow once? [y/N] "); got != tt.want {
				t.Errorf("AskYesNo() = %v, want %v", got, tt.want)
			}
		})
	}
}

// - Verify the question shows its own prompt, the next ReadPrompt goes back to "> ",
// and the answer is not saved to history
func Test_TTYReader_AskYesNo_RestoresPromptAndSkipsHistory(t *testing.T) {
	out := &bytes.Buffer{}
	keyin := keystrokes(typed("y"), []string{keys.CtrlM}, typed("hi"), []string{keys.CtrlM})
	r := newTTYReaderWith(&auto.Pilot{Text: keyin}, out)
	ctx := context.Background()

	if !r.AskYesNo(ctx, "Allow once? [y/N] ") {
		t.Fatal("AskYesNo() = false, want true for \"y\"")
	}
	asked := out.String()
	if !strings.Contains(asked, "Allow once? [y/N] ") {
		t.Errorf("expected the question prompt in the output, got %q", asked)
	}
	if strings.Contains(asked, "> ") {
		t.Errorf("expected no \"> \" prompt during the question, got %q", asked)
	}
	if r.history.Len() != 0 {
		t.Errorf("expected the answer not to be saved to history, got %d entries", r.history.Len())
	}

	out.Reset()
	line, err := r.ReadPrompt(ctx)
	if err != nil || line != "hi" {
		t.Fatalf("ReadPrompt() = (%q, %v), want (%q, nil)", line, err, "hi")
	}
	if after := out.String(); !strings.Contains(after, "> ") || strings.Contains(after, "Allow once?") {
		t.Errorf("expected the normal \"> \" prompt after the question, got %q", after)
	}
	if r.history.Len() != 1 {
		t.Errorf("expected only the real prompt in history, got %d entries", r.history.Len())
	}
}

// - Verify ↑ at the question can't recall an earlier chat message: with "y" in the prompt history, ↑ then Enter
// is an empty answer (no), and the chat history is back in place for the next ReadPrompt
func Test_TTYReader_AskYesNo_UpArrowDoesNotRecallHistory(t *testing.T) {
	keyin := keystrokes([]string{keys.Up, keys.CtrlM}, []string{keys.Up, keys.CtrlM})
	r := newScriptedTTYReader(keyin)
	r.history.Add("y") // the user once sent "y" as a chat message
	ctx := context.Background()

	if r.AskYesNo(ctx, "Allow once? [y/N] ") {
		t.Fatal("AskYesNo() = true after ↑ Enter, want false: the question must not see the chat history")
	}
	line, err := r.ReadPrompt(ctx)
	if err != nil || line != "y" {
		t.Errorf("ReadPrompt() after the question = (%q, %v), want ↑ to recall %q", line, err, "y")
	}
}

// ---- runChatLoop wiring ----

// - Verify that with non-terminal input (plainReader) no ask-on-deny prompt is installed:
// a vetoed call is denied, the tool does not run, and no question is printed
func Test_RunChatLoop_PlainReader_DeniesWithoutAsking(t *testing.T) {
	fx, tool := newHookFixture(t, &fakeHook{err: errDenied})
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "delete_file", `{"path":"a.txt"}`),
		modeltest.AssistantText("I was not allowed to delete it."),
	}

	out, done := startChatLoop(fx, "delete a.txt\n", nil)
	receiveOrFail(t, done, "runChatLoop to return at end of input")

	if fx.Session.PromptAllowDenyCall != nil {
		t.Error("expected PromptAllowDenyCall to stay nil for non-terminal input")
	}
	if tool.calls != 0 {
		t.Errorf("expected the tool not to run, ran %d time(s)", tool.calls)
	}
	text := out.String()
	if strings.Contains(text, "Allow once?") || strings.Contains(text, "denied by hook") {
		t.Errorf("expected no question in the output, got:\n%s", text)
	}
	if !strings.Contains(text, "I was not allowed to delete it.") {
		t.Errorf("expected the turn to finish with the model's answer, got:\n%s", text)
	}
}
