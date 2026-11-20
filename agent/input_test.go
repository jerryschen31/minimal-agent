package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nyaosorg/go-readline-ny/keys"
	"github.com/nyaosorg/go-ttyadapter/auto"
)

// typed splits plain text into one keypress per character, the way the fake terminal replays it
func typed(s string) []string {
	return strings.Split(s, "")
}

// keystrokes joins plain text and special keys into one scripted sequence
func keystrokes(parts ...[]string) []string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

// newScriptedTTYReader builds a ttyReader whose keyboard is the given key sequence; when the
// sequence runs out the fake terminal reports io.EOF, like Ctrl+D
func newScriptedTTYReader(keyin []string) *ttyReader {
	return newTTYReaderWith(&auto.Pilot{Text: keyin}, io.Discard)
}

// - Verify each editing key produces the expected prompt text
func Test_TTYReader_ReadPrompt_Keys(t *testing.T) {
	tests := []struct {
		name  string
		keyin []string
		want  string
	}{
		{"Enter submits a single line", keystrokes(typed("hello"), []string{keys.CtrlM}), "hello"},
		{"Ctrl+J inserts a new line, Enter submits both", keystrokes(typed("a"), []string{keys.CtrlJ}, typed("b"), []string{keys.CtrlM}), "a\nb"},
		{"Alt+Enter inserts a new line", keystrokes(typed("a"), []string{keys.Escape + "\r"}, typed("b"), []string{keys.CtrlM}), "a\nb"},
		{"Ctrl+C discards typed text and reads again", keystrokes(typed("abc"), []string{keys.CtrlC}, typed("xyz"), []string{keys.CtrlM}), "xyz"},
		{"Ctrl+C on an empty prompt reads again", keystrokes([]string{keys.CtrlC}, typed("ok"), []string{keys.CtrlM}), "ok"},
		{"Ctrl+A then typing inserts at the start", keystrokes(typed("world"), []string{keys.CtrlA}, typed("hello "), []string{keys.CtrlM}), "hello world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newScriptedTTYReader(tt.keyin)
			got, err := r.ReadPrompt(context.Background())
			if err != nil {
				t.Fatalf("ReadPrompt() returned an error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// - Verify Ctrl+D on an empty prompt returns io.EOF, which the chat loop treats as quit
func Test_TTYReader_ReadPrompt_CtrlDOnEmptyPromptIsEOF(t *testing.T) {
	r := newScriptedTTYReader([]string{keys.CtrlD})
	if _, err := r.ReadPrompt(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// - Verify a submitted prompt goes into history as one entry, so Up on the next prompt brings
// back the whole multi-line text
func Test_TTYReader_ReadPrompt_HistoryRecallsWholeMultiLinePrompt(t *testing.T) {
	keyin := keystrokes(
		typed("a"), []string{keys.CtrlJ}, typed("b"), []string{keys.CtrlM}, // prompt 1: "a\nb"
		[]string{keys.Up, keys.CtrlM}, // prompt 2: Up recalls prompt 1, Enter resubmits it
	)
	r := newScriptedTTYReader(keyin)
	ctx := context.Background()

	first, err := r.ReadPrompt(ctx)
	if err != nil || first != "a\nb" {
		t.Fatalf("prompt 1: got (%q, %v), want (%q, nil)", first, err, "a\nb")
	}
	second, err := r.ReadPrompt(ctx)
	if err != nil || second != "a\nb" {
		t.Fatalf("prompt 2: got (%q, %v), want the recalled %q", second, err, "a\nb")
	}
	if r.history.Len() != 2 {
		t.Errorf("expected 2 history entries, got %d", r.history.Len())
	}
}

// - Verify the prompt is "> " on the first line and two spaces on continuation lines
func Test_WritePrompt_FirstLineAndContinuation(t *testing.T) {
	tests := []struct {
		lnum int
		want string
	}{
		{0, "> "},
		{1, "  "},
		{5, "  "},
	}
	for _, tt := range tests {
		var b strings.Builder
		n, err := writePrompt(&b, tt.lnum)
		if err != nil || b.String() != tt.want || n != len(tt.want) {
			t.Errorf("writePrompt(line %d) = (%q, %d, %v), want (%q, %d, nil)", tt.lnum, b.String(), n, err, tt.want, len(tt.want))
		}
	}
}

// ---- (*ttyReader).AskLine with a scripted terminal ----

// - Verify AskLine returns the answer as typed: one line, a multi-line answer joined by "\n", and Ctrl+C clears and asks again
// - Verify Ctrl+D on an empty line returns io.EOF (cancel)
func Test_TTYReader_AskLine_Answers(t *testing.T) {
	tests := []struct {
		name    string
		keyin   []string
		want    string
		wantErr error
	}{
		{"one line", keystrokes(typed("plan.md"), []string{keys.CtrlM}), "plan.md", nil},
		{"spaces kept", keystrokes(typed(" a b "), []string{keys.CtrlM}), " a b ", nil},
		{"just Enter", []string{keys.CtrlM}, "", nil},
		{"multi-line", keystrokes(typed("a"), []string{keys.CtrlJ}, typed("b"), []string{keys.CtrlM}), "a\nb", nil},
		{"Ctrl+C clears and asks again", keystrokes(typed("abc"), []string{keys.CtrlC}, typed("xyz"), []string{keys.CtrlM}), "xyz", nil},
		{"Ctrl+D on an empty line", []string{keys.CtrlD}, "", io.EOF},
		{"Ctrl+C then Ctrl+D", keystrokes(typed("abc"), []string{keys.CtrlC, keys.CtrlD}), "", io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newScriptedTTYReader(tt.keyin)
			got, err := r.AskLine(context.Background(), "Save as: ")
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Errorf("AskLine() = (%q, %v), want (%q, %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// - Verify AskLine shows its own prompt, keeps the answer out of history, and the next ReadPrompt is back to "> " with history
// - Verify Up while asking can't recall an earlier chat message
func Test_TTYReader_AskLine_RestoresPromptAndHistory(t *testing.T) {
	out := &bytes.Buffer{}
	keyin := keystrokes([]string{keys.Up}, typed("x"), []string{keys.CtrlM}, []string{keys.Up, keys.CtrlM})
	r := newTTYReaderWith(&auto.Pilot{Text: keyin}, out)
	r.history.Add("old message")
	ctx := context.Background()

	got, err := r.AskLine(ctx, "Name? ")
	if err != nil || got != "x" {
		t.Fatalf("AskLine() = (%q, %v), want (%q, nil): Up must not recall the chat history", got, err, "x")
	}
	asked := out.String()
	if !strings.Contains(asked, "Name? ") || strings.Contains(asked, "> ") {
		t.Errorf("expected only the question prompt while asking, got %q", asked)
	}
	if r.history.Len() != 1 {
		t.Errorf("expected the answer not to be saved to history, got %d entries", r.history.Len())
	}

	out.Reset()
	line, err := r.ReadPrompt(ctx)
	if err != nil || line != "old message" {
		t.Fatalf("ReadPrompt() = (%q, %v), want Up to recall %q", line, err, "old message")
	}
	if after := out.String(); !strings.Contains(after, "> ") || strings.Contains(after, "Name? ") {
		t.Errorf("expected the normal \"> \" prompt after the question, got %q", after)
	}
}

// ---- (*ttyReader).ReadPromptFilled with a scripted terminal ----

// - Verify ReadPromptFilled starts with lines in the editor: Enter submits them joined by "\n", typing edits them
// - Verify the cursor starts at the end of the last line (SetMoveEnd)
// - Verify Ctrl+C discards the changes and starts again from lines
func Test_TTYReader_ReadPromptFilled_Keys(t *testing.T) {
	lines := []string{"# Objective", "Count words"}
	tests := []struct {
		name  string
		keyin []string
		want  string
	}{
		{"Enter submits the fill", []string{keys.CtrlM}, "# Objective\nCount words"},
		{"typing edits the fill (cursor starts at the end)", keystrokes(typed("!"), []string{keys.CtrlM}), "# Objective\nCount words!"},
		{"Ctrl+C starts again from lines", keystrokes(typed("!!"), []string{keys.CtrlC, keys.CtrlM}), "# Objective\nCount words"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newScriptedTTYReader(tt.keyin)
			got, err := r.ReadPromptFilled(context.Background(), lines)
			if err != nil || got != tt.want {
				t.Errorf("ReadPromptFilled() = (%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}

// - Verify Ctrl+D on an empty prompt (nothing to fill) returns io.EOF
func Test_TTYReader_ReadPromptFilled_CtrlDOnEmptyIsEOF(t *testing.T) {
	r := newScriptedTTYReader([]string{keys.CtrlD})
	if _, err := r.ReadPromptFilled(context.Background(), nil); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// - Verify the submitted text is not added to history and the next ReadPrompt starts empty
func Test_TTYReader_ReadPromptFilled_NoHistoryNextPromptEmpty(t *testing.T) {
	keyin := keystrokes([]string{keys.CtrlM}, typed("hi"), []string{keys.CtrlM})
	r := newScriptedTTYReader(keyin)
	ctx := context.Background()

	if got, err := r.ReadPromptFilled(ctx, []string{"draft"}); err != nil || got != "draft" {
		t.Fatalf("ReadPromptFilled() = (%q, %v), want (%q, nil)", got, err, "draft")
	}
	if r.history.Len() != 0 {
		t.Errorf("expected nothing in history, got %d entries", r.history.Len())
	}
	if line, err := r.ReadPrompt(ctx); err != nil || line != "hi" {
		t.Errorf("ReadPrompt() = (%q, %v), want (%q, nil): the fill must be cleared", line, err, "hi")
	}
}

// ---- plainReader.AskLine and ReadPromptFilled ----

// - Verify plainReader.AskLine writes the prompt and returns each next line without its trailing "\n" or "\r\n"
// - Verify it returns io.EOF when input ends
func Test_PlainReader_AskLine(t *testing.T) {
	ctx := context.Background()
	out := &bytes.Buffer{}
	r := newPlainReader(ctx, strings.NewReader("first\nsecond\r\nlast"), out)

	for _, want := range []string{"first", "second", "last"} {
		got, err := r.AskLine(ctx, "Name? ")
		if err != nil || got != want {
			t.Fatalf("AskLine() = (%q, %v), want (%q, nil)", got, err, want)
		}
	}
	if _, err := r.AskLine(ctx, "Name? "); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF at end of input, got %v", err)
	}
	if got := out.String(); got != strings.Repeat("Name? ", 4) {
		t.Errorf("output = %q, want the prompt once per question", got)
	}
}

// - Verify plainReader.AskLine returns the context's error when the context is cancelled before a line arrives
func Test_PlainReader_AskLine_CancelledContext(t *testing.T) {
	pr, pw := io.Pipe() // no line ever arrives
	defer pw.Close()
	r := newPlainReader(context.Background(), pr, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.AskLine(ctx, "Name? "); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// - Verify plainReader.ReadPromptFilled prints the lines, then returns the next line read without its trailing "\r\n"
// - Verify it returns io.EOF when input ends
func Test_PlainReader_ReadPromptFilled(t *testing.T) {
	ctx := context.Background()
	out := &bytes.Buffer{}
	r := newPlainReader(ctx, strings.NewReader("new text\r\n"), out)

	got, err := r.ReadPromptFilled(ctx, []string{"# Objective", "old text"})
	if err != nil || got != "new text" {
		t.Fatalf("ReadPromptFilled() = (%q, %v), want (%q, nil)", got, err, "new text")
	}
	if shown := out.String(); !strings.Contains(shown, "# Objective\nold text\n") || !strings.Contains(shown, "> ") {
		t.Errorf("expected the lines and the prompt in the output, got %q", shown)
	}
	if _, err := r.ReadPromptFilled(ctx, []string{"x"}); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF at end of input, got %v", err)
	}
}
