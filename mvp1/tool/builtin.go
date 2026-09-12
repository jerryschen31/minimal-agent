package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mvp1/agent"
)

const maxOutput = 16 << 10

// [agent] Shell runs `sh -c cmd` in dir with a hard timeout. Sandboxing is a
// swap, not a rewrite: replace this Tool with one that execs inside a container,
// microVM, or remote runner; the agent loop and the model never notice.
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
			out, err := cmd.CombinedOutput()
			if err != nil { // [agent] non-zero exit is useful information, not a failure of the tool
				out = append(out, []byte("\n[exit: "+err.Error()+"]")...)
			}
			return Truncate(string(out), maxOutput), nil
		}}
}

// [agent] ReadFile / WriteFile are confined to root: paths that escape it are
// rejected. That is the authorization boundary for filesystem access.
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
			b, err := os.ReadFile(p)
			return Truncate(string(b), maxOutput), err
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

func confine(root, p string) (string, error) {
	abs, err := filepath.Abs(filepath.Join(root, p))
	if err != nil {
		return "", err
	}
	rootAbs, _ := filepath.Abs(root)
	if rel, err := filepath.Rel(rootAbs, abs); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes working directory", p)
	}
	return abs, nil
}
