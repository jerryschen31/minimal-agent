package agent

import (
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
