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
}

// initializes a new prompt reader instance, that reads user input keystroke by keystroke from a terminal (if this input is coming from a terminal) OR per-line from a non-terminal read stream (if input is coming from a pipe or a file)
func newPromptReader(ctx context.Context, in io.Reader, out io.Writer) promptReader {
	// if this is a chat terminal, use the ttyReader which supports multiline input and history
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return newTTYReaderWith(nil, out)
	} else {
		// otherwise this is a non-terminal input (pipe or file)
		return newPlainReader(ctx, in, out)
	}
}

// ---- terminal: go-multiline-ny ----

// implementation of terminal prompt reader using go-multiline-ny
// contains line editor and input history
type ttyReader struct {
	ed      multiline.Editor
	history *simplehistory.Container
}

func newTTYReader() *ttyReader {
	return newTTYReaderWith(nil, nil) // nil = the real terminal and stdout
}

// tests pass a scripted terminal (auto.Pilot) and io.Discard; it must be set
// before the first BindKey, because that call sets the editor up with whatever terminal it has
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

// reads a single- or multi-line user prompt from the terminal
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
		r.history.Add(text)
		return text, nil
	}
}

// ---- reader for non-terminal input (pipes, tests) ----

type plainReader struct {
	lines <-chan string
	out   io.Writer
}

// initializes a non-terminal reader (pipe or file input)
// examples would be:
//
//	echo "hi" | go run .  (piped input into the Go agent)
//	go run . < notes.txt  (file input into the Go agent)
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

// reads a single line of input from a non-terminal source (pipe or file)
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

// ---- other functions ----

// "> " on the first line of a prompt, two spaces on continuation lines so the text lines up
func writePrompt(w io.Writer, lnum int) (int, error) {
	if lnum == 0 {
		return io.WriteString(w, "> ")
	}
	return io.WriteString(w, "  ")
}
