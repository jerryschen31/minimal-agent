package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/hymkor/go-multiline-ny"  // imports multiline - built on top of readline to support multiline inputs
	"github.com/nyaosorg/go-readline-ny" // imports readline - single-line functionality at a terminal prompt
	"github.com/nyaosorg/go-readline-ny/keys"
	"github.com/nyaosorg/go-readline-ny/simplehistory"
	"github.com/nyaosorg/go-ttyadapter"
	"golang.org/x/term"
)

// *********************************************
// promptReader: contains code for reading an input prompt (chat terminal, pipes, or files)
// *********************************************

type promptReader interface {
	ReadPrompt(ctx context.Context) (string, error)
	ReadPromptFilled(ctx context.Context, lines []string) (string, error) // the prompt starts out holding lines (plan mode's /edit)
	AskLine(ctx context.Context, prompt string) (string, error)           // one answer to a question; io.EOF = Ctrl+D (cancel)
}

// newPromptReader creates a prompt reader that reads user input keystroke by keystroke from a terminal, or per line from
// a non-terminal stream (a pipe or a file).
//
// Notes:
//   - A chat terminal gets the ttyReader, which supports multiline input and history; anything else gets a plainReader.
func newPromptReader(ctx context.Context, in io.Reader, out io.Writer) promptReader {
	// if this is a chat terminal, use the ttyReader which supports multiline input and history
	if isTerminal(in) {
		return newTTYReaderWith(nil, out)
	} else {
		// otherwise this is a non-terminal input (pipe or file)
		return newPlainReader(ctx, in, out)
	}
}

// isTerminal reports whether v (an input or output) is an *os.File connected to a terminal.
//
// Notes:
//   - A pipe, a file on disk, or an in-memory buffer is not a terminal.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// ---- terminal: go-multiline-ny ----

// implementation of terminal prompt reader using go-multiline-ny
// contains line editor and input history
type ttyReader struct {
	ed      multiline.Editor
	history *simplehistory.Container
}

// newTTYReader creates a terminal prompt reader on the real terminal and stdout.
//
// Notes:
//   - Passing nil to newTTYReaderWith means the real terminal and stdout.
func newTTYReader() *ttyReader {
	return newTTYReaderWith(nil, nil) // nil = the real terminal and stdout
}

// newTTYReaderWith creates a terminal prompt reader using go-multiline-ny, with its line editor and input history.
//
// Notes:
//   - Tests pass a scripted terminal (auto.Pilot) and io.Discard; it must be set before the first BindKey, because that
//     call sets the editor up with whatever terminal it has. A nil tty or out means the real terminal or stdout.
//   - Sets the "> " prompt; Up/Down move between lines of the prompt first, then into history at the top/bottom edge.
//   - go-multiline-ny's own default is Enter = new line, Ctrl+J = submit (SQL-style). The three key bindings flip it to
//     chat style: Enter submits, Ctrl+J / Alt+Enter insert a new line.
//   - BindKey's error is ignored: it only fails if the terminal can't be set up, and Read reports the same error.
func newTTYReaderWith(tty ttyadapter.Tty, out io.Writer) *ttyReader {
	r := &ttyReader{history: simplehistory.New()}
	if tty != nil {
		r.ed.SetTty(tty)
	}
	if out != nil {
		r.ed.SetWriter(out)
	}

	// sets the '> ' prompt
	r.ed.SetPrompt(writePrompt)

	// ↑/↓ move between lines of the prompt first, then into history at the top/bottom edge
	r.ed.SetHistory(r.history)
	r.ed.SetHistoryCycling(true)

	// go-multiline-ny's own default is Enter = new line, Ctrl+J = submit (SQL-style).
	// These three bindings flip it to chat style: Enter submits, Ctrl+J / Alt+Enter insert a new line.
	// BindKey's error is ignored: it only fails if the terminal can't be set up, and Read reports the same error.
	r.ed.BindKey(keys.CtrlM, readline.AnonymousCommand(r.ed.Submit))        // Enter submits
	r.ed.BindKey(keys.CtrlJ, readline.AnonymousCommand(r.ed.NewLine))       // Ctrl+J new line
	r.ed.BindKey(keys.Escape+"\r", readline.AnonymousCommand(r.ed.NewLine)) // Alt/Option+Enter new line
	return r
}

// ReadPrompt reads a single- or multi-line user prompt from the terminal.
//
// Notes:
//   - Ctrl+C at the prompt discards what was typed and goes back to a fresh prompt.
//   - Ctrl+D on an empty prompt arrives as io.EOF, which the chat loop treats as quit.
//   - A prompt that is not blank is added to the input history.
func (r *ttyReader) ReadPrompt(ctx context.Context) (string, error) {
	for {
		lines, err := r.ed.Read(ctx)
		if errors.Is(err, readline.CtrlC) {
			continue // Ctrl+C at the prompt: discard what was typed -> this goes back to a fresh prompt
		}
		if err != nil {
			return "", err // Ctrl+D on an empty prompt arrives as io.EOF, which the loop treats as quit
		}
		text := strings.Join(lines, "\n")
		if strings.TrimSpace(text) != "" {
			r.history.Add(text)
		}
		return text, nil
	}
}

// ReadPromptFilled reads a prompt that starts out holding lines, which the user edits and then submits.
//
// Notes:
//   - Used by plan mode's /edit: the draft objective is put in the prompt so the user can change it in place.
//   - The cursor starts at the end of the last line.
//   - Ctrl+C discards the changes and starts again from lines; Ctrl+D on an empty prompt returns io.EOF.
//   - The answer is not added to the input history (a whole objective would crowd it), and the fill is cleared
//     afterwards, so the next ReadPrompt starts empty.
func (r *ttyReader) ReadPromptFilled(ctx context.Context, lines []string) (string, error) {
	r.ed.SetDefault(lines)
	r.ed.SetMoveEnd(true) // the cursor starts at the end of the last line, where an edit usually adds text
	defer func() {
		r.ed.SetDefault(nil) // the next prompt starts empty
		r.ed.SetMoveEnd(false)
	}()
	for {
		got, err := r.ed.Read(ctx)
		if errors.Is(err, readline.CtrlC) {
			continue // discard the changes: the prompt starts again from lines
		}
		if err != nil {
			return "", err
		}
		return strings.Join(got, "\n"), nil
	}
}

// AskLine asks the user a question at the terminal and returns the answer as typed.
//
// Notes:
//   - prompt is shown instead of "> ", and the normal prompt is put back afterwards; the history is empty while asking,
//     so Up can't bring back an old chat message.
//   - Ctrl+C clears what was typed and asks again; Ctrl+D on an empty line returns io.EOF, which callers treat as
//     cancel.
//   - A multi-line answer comes back joined with newlines; callers trim it.
func (r *ttyReader) AskLine(ctx context.Context, prompt string) (string, error) {
	r.ed.SetPrompt(func(w io.Writer, _ int) (int, error) { return io.WriteString(w, prompt) })
	defer r.ed.SetPrompt(writePrompt) // put the normal "> " prompt back for the next ReadPrompt
	r.ed.SetHistory(simplehistory.New())
	defer r.ed.SetHistory(r.history)

	for {
		lines, err := r.ed.Read(ctx)
		if errors.Is(err, readline.CtrlC) {
			continue // Ctrl+C clears the line and asks again; only Ctrl+D cancels
		}
		if err != nil {
			return "", err
		}
		return strings.Join(lines, "\n"), nil
	}
}

// AskYesNo asks the user a yes/no question at the terminal, such as allowing a tool call to execute.
//
// Notes:
//   - prompt is shown to the user instead of "> ", and the normal prompt is put back afterwards.
//   - It uses an empty history while asking, so Up can't bring back an old "y" sent as a chat message.
//   - It calls Read, not ReadPrompt: ReadPrompt ignores Ctrl+C and saves to history.
//   - Only "y" or "yes" (any case) is affirmative; anything else is no. That includes Ctrl+C, which here means "no",
//     not "cancel the turn", and a multi-line answer.
func (r *ttyReader) AskYesNo(ctx context.Context, prompt string) bool {
	// whatever was passed into prompt is shown to the user on the terminal, instead of "> "
	r.ed.SetPrompt(func(w io.Writer, _ int) (int, error) { return io.WriteString(w, prompt) })
	defer r.ed.SetPrompt(writePrompt) // put the normal "> " prompt back for the next ReadPrompt
	// an empty history while asking, so ↑ can't bring back an old "y" sent as a chat message
	r.ed.SetHistory(simplehistory.New())
	defer r.ed.SetHistory(r.history)

	lines, err := r.ed.Read(ctx) // not ReadPrompt: that one ignores Ctrl+C and saves to history
	if err != nil || len(lines) != 1 {
		return false // includes Ctrl+C, which here means "no", not "cancel the turn"; a multi-line answer is no too
	}
	answer := strings.ToLower(strings.TrimSpace(lines[0]))
	// only "y" or "yes" is considered affirmative; anything else is treated as negative
	return answer == "y" || answer == "yes"
}

// ---- reader for non-terminal input (pipes, tests) ----

type plainReader struct {
	lines <-chan string
	out   io.Writer
}

// newPlainReader initializes a non-terminal reader (pipe or file input).
//
// Notes:
//   - A goroutine reads lines until input ends, a read fails, or the context is cancelled while handing a line over
//     (the ctx.Done() channel closing exits the goroutine immediately).
//   - The read is blocking, so a goroutine stuck in a read ends only when the input does.
func newPlainReader(ctx context.Context, in io.Reader, out io.Writer) *plainReader {
	lines := make(chan string)

	go func() {
		defer close(lines)
		r := bufio.NewReader(in)
		// reads lines until input ends, a read fails, or context is cancelled while handing a line over
		for {
			lineRead, err := r.ReadString('\n') // read a line of user input from the input buffer (blocking)
			if lineRead != "" {
				select {
				case lines <- lineRead: // send read line to the lines channel
				case <-ctx.Done():
					return // context cancel signal closes ctx.Done() channel, which exits this goroutine immediately
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return &plainReader{lines: lines, out: out}
}

// ReadPrompt reads a single line of input from a non-terminal source (pipe or file).
//
// Notes:
//   - Prints a newline and the "> " prompt first.
//   - io.EOF means the reader goroutine closed the channel: input ended.
func (r *plainReader) ReadPrompt(ctx context.Context) (string, error) {
	io.WriteString(r.out, "\n")
	writePrompt(r.out, 0)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case line, ok := <-r.lines:
		if !ok {
			return "", io.EOF // reader goroutine closed the channel: input ended
		}
		return line, nil
	}
}

// ReadPromptFilled reads one line, like ReadPrompt, after printing lines so the reader can see what is being edited.
//
// Notes:
//   - A pipe can't be edited in place, so the answer is the next line read (it replaces lines). Plan mode only edits
//     on a terminal; this is for tests.
//   - The trailing newline is removed; io.EOF means input ended.
func (r *plainReader) ReadPromptFilled(ctx context.Context, lines []string) (string, error) {
	io.WriteString(r.out, "\n"+strings.Join(lines, "\n")+"\n")
	line, err := r.ReadPrompt(ctx)
	return strings.TrimRight(line, "\r\n"), err
}

// AskLine prints prompt and returns the next line read, without its trailing newline.
//
// Notes:
//   - io.EOF means input ended (callers treat it as cancel); a cancelled ctx returns its error.
func (r *plainReader) AskLine(ctx context.Context, prompt string) (string, error) {
	io.WriteString(r.out, prompt)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case line, ok := <-r.lines:
		if !ok {
			return "", io.EOF // reader goroutine closed the channel: input ended
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
}

// ---- other functions ----

// writePrompt writes the prompt for line lnum: "> " on the first line of a prompt, two spaces on continuation lines
// so the text lines up.
//
// Notes: none.
func writePrompt(w io.Writer, lnum int) (int, error) {
	if lnum == 0 {
		return io.WriteString(w, "> ")
	}
	return io.WriteString(w, "  ")
}
