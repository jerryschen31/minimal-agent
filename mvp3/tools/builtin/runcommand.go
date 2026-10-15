package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jerryschen31/minimal-agent/mvp3/model"
)

// run_command: runs one of a fixed set of developer programs (go, python3, python, pytest) inside workDir,
// so a coding worker can build, test and run what it wrote.
//
// [agent] One combined tool rather than run_go + run_python: one schema, one set of rules (timeout, output
// cap, environment), and adding a program is one line in RunCommandPrograms. The "command" argument is an
// enum of the programs actually installed, so the model can't ask for anything else.
// It is NOT a shell: the program is started directly with an argument list (no `sh -c`), so pipes,
// redirection, `&&`, `$VAR` and backticks are just literal text. That closes "run go, then curl | sh",
// but it is not a sandbox (see the warning below).
//
// [agent] Honest downside, and why this tool is opt-in: `go run` and `python3` execute code the model
// wrote, and that code can do anything your user account can do: read files outside workDir (the
// read_file deny list does not apply), use the network, delete things. Two things narrow it:
//   - the environment is rebuilt from an allowlist (RunCommandEnv), so API keys such as
//     OPENAI_API_KEY / ANTHROPIC_API_KEY are not handed to the program;
//   - a hard timeout, an output cap, and a process-group kill, so nothing it starts outlives the call.
// Real isolation means running the program in a container or VM; that is a replacement Tool with the
// same name, not a change to the loop (DECISIONS.md § mvp3 reference build).

// RunCommandPrograms are the programs run_command may start, looked up on PATH when the tool is built.
var RunCommandPrograms = []string{"go", "python3", "python", "pytest"}

// RunCommandEnv lists the environment variables passed through to the program; everything else is dropped.
var RunCommandEnv = []string{
	"PATH", "HOME", "USER", "TMPDIR", "LANG", "LC_ALL", "TERM",
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOFLAGS", "GOTOOLCHAIN",
	"VIRTUAL_ENV", "PYTHONPATH",
}

const (
	RunCommandTimeout  = 3 * time.Minute // per call; `go test` with a cold build cache can take a while
	RunCommandMaxBytes = 32 * 1024       // combined stdout+stderr returned to the model; the program is killed past this
)

// RunCommandTool runs an allowlisted program with arguments in workDir (or a subdirectory of it).
type RunCommandTool struct {
	rt       *ReadFileTool     // reuses read_file's root and resolve() for the "dir" argument
	programs map[string]string // name -> absolute path, found at construction
	timeout  time.Duration
}

// NewRunCommandTool finds which of RunCommandPrograms are installed. It fails if none is.
func NewRunCommandTool(workDir string) (*RunCommandTool, error) {
	programs := map[string]string{}
	for _, name := range RunCommandPrograms {
		// [agent] Resolve to an absolute path now, from the agent's own PATH, so a file named "go" that the
		// model writes into workDir can never be picked up instead.
		if path, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				programs[name] = abs
			}
		}
	}
	if len(programs) == 0 {
		return nil, fmt.Errorf("run_command: none of %v found on PATH", RunCommandPrograms)
	}
	return newRunCommandTool(workDir, programs, RunCommandTimeout)
}

// newRunCommandTool takes the program table and timeout directly (tests use stand-in programs).
func newRunCommandTool(workDir string, programs map[string]string, timeout time.Duration) (*RunCommandTool, error) {
	rt, err := NewReadFileTool(workDir)
	if err != nil {
		return nil, err
	}
	return &RunCommandTool{rt: rt, programs: programs, timeout: timeout}, nil
}

func (t *RunCommandTool) GetToolDefinition() model.ToolDef {
	names := slices.Sorted(maps.Keys(t.programs)) // stable schema text, so the prompt prefix stays cacheable
	enum, _ := json.Marshal(names)
	return model.NewToolDef(
		"run_command",
		fmt.Sprintf("Runs a developer program (one of %s) with arguments in the working directory and returns its combined output and exit status. "+
			"Use it to build, test and run code, e.g. command=go args=[\"test\",\"./...\"], or command=python3 args=[\"main.py\"]. "+
			"It is not a shell: no pipes, redirection, && or environment variables. Stdin is empty, so the program must not wait for input. "+
			"Calls time out after %s.", strings.Join(names, ", "), t.timeout),
		json.RawMessage(fmt.Sprintf(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "enum": %s, "description": "The program to run"},
				"args": {"type": "array", "items": {"type": "string"}, "description": "Arguments, one per element, e.g. [\"run\", \"main.go\"]"},
				"dir": {"type": "string", "description": "Subdirectory of the working directory to run in (default: the working directory itself)"}
			},
			"required": ["command"]
		}`, enum)),
	)
}

func (t *RunCommandTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Dir     string   `json:"dir"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	path, ok := t.programs[params.Command]
	if !ok {
		return "", fmt.Errorf("command %q is not allowed; use one of: %s", params.Command, strings.Join(slices.Sorted(maps.Keys(t.programs)), ", "))
	}
	dir := t.rt.root
	if params.Dir != "" && params.Dir != "." {
		rel, err := t.rt.resolve(params.Dir) // inside workDir, not deny-listed, symlinks resolved
		if err != nil {
			return "", err
		}
		dir = filepath.Join(t.rt.root, rel)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", params.Dir)
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, params.Args...)
	cmd.Dir = dir
	cmd.Env = allowedEnv()
	cmd.WaitDelay = time.Second // [agent] if a background child keeps the output pipe open, stop waiting after 1s
	setProcessGroup(cmd)
	out := &capWriter{limit: RunCommandMaxBytes, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, out // Stdin stays nil, which means /dev/null
	err := cmd.Run()
	killProcessGroup(cmd) // [agent] the program exited; now kill anything it left running in the background

	text := out.String()
	switch {
	case out.exceeded():
		return text + fmt.Sprintf("\n...[output capped at %d bytes; program killed]", RunCommandMaxBytes), nil
	case ctx.Err() != nil: // the turn itself was cancelled (Ctrl+C, the run's -timeout)
		return "", ctx.Err()
	case runCtx.Err() != nil:
		return "", fmt.Errorf("%s timed out after %s and was killed; output so far:\n%s", params.Command, t.timeout, text)
	case errors.Is(err, exec.ErrWaitDelay):
		return text + "\n[exit status 0; background processes killed]", nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// [agent] A failing build or test is useful information for the model, not a failure of the tool.
		return text + fmt.Sprintf("\n[exit status %d]", exit.ExitCode()), nil
	}
	if err != nil {
		return "", err // the program couldn't be started at all
	}
	return text + "\n[exit status 0]", nil
}

// allowedEnv copies only the RunCommandEnv variables from the agent's environment.
func allowedEnv() []string {
	var env []string
	for _, name := range RunCommandEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// capWriter keeps at most limit bytes and cancels the program once more arrives.
// [agent] Capping while streaming (instead of after) means a program printing in a loop is killed early,
// not buffered until it runs the machine out of memory. Same design as mvp1's shell tool.
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
	return len(p), nil // [agent] keep accepting bytes so the program never blocks on a full pipe
}

func (w *capWriter) exceeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped > 0
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
