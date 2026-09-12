package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mvp1/agent"
)

const maxOutput = 16 << 10

// [agent] Shell runs `sh -c cmd` in dir with a hard timeout. Sandboxing is a
// swap, not a rewrite: replace this Tool with one that execs inside a container,
// microVM, or remote runner; the agent loop and the model never notice.
//
// Nothing the command spawns outlives the call: sh runs in its own process
// group, which is killed on timeout and again once sh exits (so `sleep 300 &`
// cannot linger). Output is capped while streaming (a runaway printer is killed,
// not buffered), and a pipe held open by a background child is abandoned after
// WaitDelay rather than blocking the agent.
func Shell(dir string, timeout time.Duration) agent.Tool {
	return Func{Name: "shell", Description: "Run a shell command and return combined stdout/stderr.",
		Schema: `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`,
		Fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
			in, err := Args[struct{ Command string }](raw)
			if err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
			cmd.Dir = dir
			cmd.WaitDelay = time.Second
			setProcessGroup(cmd)
			out := &capWriter{limit: maxOutput, cancel: cancel}
			cmd.Stdout, cmd.Stderr = out, out
			err = cmd.Run()
			killProcessGroup(cmd)
			text := out.String()
			switch {
			case out.exceeded():
				return text + fmt.Sprintf("\n...[output capped at %d bytes; command killed]", maxOutput), nil
			case errors.Is(err, exec.ErrWaitDelay): // [agent] sh finished; only an orphaned child held the pipe
				return text + "\n[background processes killed]", nil
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				return "", fmt.Errorf("command timed out after %s; output so far:\n%s", timeout, text)
			case ctx.Err() != nil:
				return "", ctx.Err()
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) { // [agent] non-zero exit is useful information, not a failure of the tool
				return text + "\n[exit: " + err.Error() + "]", nil
			}
			return text, err // nil, or a start failure (bad dir, sh missing)
		}}
}

// capWriter buffers at most limit bytes and cancels the command once exceeded.
type capWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	limit   int
	dropped int
	cancel  context.CancelFunc
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.limit - w.buf.Len(); len(p) <= room {
		w.buf.Write(p)
		return len(p), nil
	} else if room > 0 {
		w.buf.Write(p[:room])
		p = p[room:]
	}
	w.dropped += len(p)
	w.cancel()
	return len(p), nil // [agent] keep draining so the child is never blocked on a full pipe
}

func (w *capWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }
func (w *capWriter) exceeded() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.dropped > 0 }

// [agent] ReadFile / WriteFile are confined to root: paths that escape it,
// including via symlinks, are rejected. That is the authorization boundary for
// filesystem access.
func ReadFile(root string) agent.Tool {
	return Func{Name: "read_file", Description: "Read a UTF-8 text file relative to the working directory.",
		Schema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		Fn: func(_ context.Context, raw json.RawMessage) (string, error) {
			in, err := Args[struct{ Path string }](raw)
			if err != nil {
				return "", err
			}
			p, err := confine(root, in.Path)
			if err != nil {
				return "", err
			}
			fh, err := os.Open(p)
			if err != nil {
				return "", err
			}
			defer fh.Close()
			b, err := io.ReadAll(io.LimitReader(fh, maxOutput+1)) // [agent] never load more than we can return
			if err != nil {
				return "", err
			}
			if len(b) > maxOutput {
				total := int64(len(b))
				if st, err := fh.Stat(); err == nil {
					total = st.Size()
				}
				return string(b[:maxOutput]) + fmt.Sprintf("\n...[truncated %d bytes]", total-int64(maxOutput)), nil
			}
			return string(b), nil
		}}
}

func WriteFile(root string) agent.Tool {
	return Func{Name: "write_file", Description: "Create or overwrite a text file relative to the working directory.",
		Schema: `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`,
		Fn: func(_ context.Context, raw json.RawMessage) (string, error) {
			in, err := Args[struct{ Path, Content string }](raw)
			if err != nil {
				return "", err
			}
			p, err := confine(root, in.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), os.WriteFile(p, []byte(in.Content), 0o644)
		}}
}

// confine resolves p (relative to root) to a real path and checks it is inside
// root after following symlinks in root and in every existing ancestor of p.
func confine(root, p string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	real, err := resolveExisting(filepath.Join(rootAbs, p))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes working directory", p)
	}
	return real, nil
}

// resolveExisting follows symlinks in the longest existing prefix of p and
// re-appends the not-yet-existing tail (for files about to be created).
func resolveExisting(p string) (string, error) {
	var tail []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{r}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		dir, base := filepath.Dir(p), filepath.Base(p)
		if dir == p {
			return "", err
		}
		tail, p = append([]string{base}, tail...), dir
	}
}
