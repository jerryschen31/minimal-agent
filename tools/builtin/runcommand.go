package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/procutils"
	"github.com/jerryschen31/minimal-agent/textutils"
)

// ErrNoCommands is returned by NewRunCommandTool when none of the configured programs can be used, so there is nothing
// for the tool to run. The caller treats it as "do not set the tool up", not as a broken config.
var ErrNoCommands = errors.New("run_command: none of the runCommands programs can be used")

// waitDelay is how long to keep waiting for output pipes after the program has exited: a background child that holds the
// pipe open must not hang the call.
const waitDelay = time.Second

// RunCommandTool runs one of the configured programs with arguments and returns its output.
//
// [agent] Not a shell: the program starts directly with an argument list, so pipes, &&, redirection and $VAR are literal
// text. Downside: the model cannot chain commands, so "go mod init" then "go test" is two calls.
// Honest limit: this is NOT contained. A running program can read and write anything the agent's user can, which
// bypasses every Workspace check (.git, the config file, the deny list, the hard-link refusal); only the dir argument is
// checked. An allowed interpreter (python3, go, npm) is code execution. What limits it is approval rules and, later, a
// sandbox. See DECISIONS.md § Command execution: contain, don't detect.
// On Windows only the program itself is killed when the call ends, not what it started (procutils.KillGroup); a
// Job Object would fix that and is deferred (DECISIONS.md § PR #38 review).
type RunCommandTool struct {
	ws       *Workspace
	programs map[string]string // name -> absolute path, resolved once at setup
	env      []string          // names of the environment variables passed on
	maxBytes int               // most output returned
}

// NewRunCommandTool resolves the configured programs and builds the tool.
//
// Notes:
//   - names are bare program names (no path separators, no spaces); anything else is a config error, since a typo in a
//     safety setting should fail setup. extraEnv are variable names (no "=") added to DefaultCommandEnv.
//   - Each name is looked up on the agent's PATH once, here, and stored as an absolute path, so a file named "go" written
//     into workDir later cannot stand in for it. A name that is not found, or resolves to a file inside workDir (a "."
//     entry on PATH), is left out with a line on warn.
//   - Returns ErrNoCommands when no program is left.
//   - ws must not be nil and maxBytes must be positive.
func NewRunCommandTool(ws *Workspace, names, extraEnv []string, maxBytes int, warn io.Writer) (*RunCommandTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("run_command needs a workspace")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("run_command max output bytes must be positive, got %d", maxBytes)
	}
	for _, v := range extraEnv {
		if v == "" || strings.ContainsAny(v, "= \t\n\x00") {
			return nil, fmt.Errorf("commandEnv: %q is not an environment variable name", v)
		}
	}
	programs := map[string]string{}
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, "/\\ \t\n\x00") {
			return nil, fmt.Errorf("runCommands: %q must be a bare program name such as \"go\" (no path, no spaces)", name)
		}
		path, err := exec.LookPath(name)
		if err != nil {
			fmt.Fprintf(warn, "[warning] runCommands: %q not found on PATH; run_command will not offer it\n", name)
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			fmt.Fprintf(warn, "[warning] runCommands: %q: %v; run_command will not offer it\n", name, err)
			continue
		}
		if ws.inside(abs) {
			fmt.Fprintf(warn, "[warning] runCommands: %q resolves inside workDir; run_command will not offer it\n", name)
			continue
		}
		programs[name] = abs
	}
	if len(programs) == 0 {
		return nil, ErrNoCommands
	}
	env := slices.Concat(DefaultCommandEnv, extraEnv)
	return &RunCommandTool{ws: ws, programs: programs, env: env, maxBytes: maxBytes}, nil
}

// checkCommandArgs refuses a command whose arguments name a file that the file tools refuse. It is a guard against
// mistakes, best effort, not a boundary.
//
// Notes:
//   - Each argument is read as a path relative to dir (or absolute); a flag such as --file=.env is also read as the
//     part after its first "=". The path is resolved through symlinks as far as it exists (a link to .env is refused),
//     and one that falls inside rootDir goes through checkPath: the deny list (.env but not .env.example, *.pem, denyPaths
//     and so on) and the trace directory. An argument that does not exist is judged by its name, so "cat .env" is refused
//     whether or not the file is there. An argument outside rootDir is not checked (it is no workspace path).
//   - A hit is an error, never an approval question: the same rule as the file tools.
//   - [agent] What it cannot see: a directory argument that contains a protected file ("grep -r KEY ."), a name the
//     program builds itself (an interpreter, "sh -c", a script it runs), a "rev:path" form such as "git show HEAD:.env",
//     and a file reached by a path the program receives some other way (a config file, an environment variable). An
//     allowed interpreter bypasses it completely. See DECISIONS.md § run_command: arguments are checked against the deny list.
func (w *Workspace) checkCommandArgs(dir string, args []string) error {
	for _, arg := range args {
		candidates := []string{arg}
		if _, value, found := strings.Cut(arg, "="); found && strings.HasPrefix(arg, "-") {
			candidates = append(candidates, value)
		}
		for _, c := range candidates {
			if c == "" || strings.ContainsRune(c, 0) {
				continue
			}
			p := c
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			real, err := resolveSymLinks(p)
			if err != nil {
				real = filepath.Clean(p) // cannot be resolved: judge the path as written
			}
			if !w.inside(real) {
				continue
			}
			if _, err := w.checkPath(arg, real); err != nil {
				return fmt.Errorf("command refused, argument %w", err)
			}
		}
	}
	return nil
}

// inside reports whether path (or where it points, with symlinks followed) is rootDir or below it.
//
// Notes:
//   - Used by run_command to refuse a program that lives in the workspace.
func (w *Workspace) inside(path string) bool {
	for _, p := range []string{path, resolveSymLinksOrSelf(path)} {
		if rel, err := filepath.Rel(w.rootDir, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolveSymLinksOrSelf returns path with symlinks followed, or path itself if they can't be resolved.
//
// Notes: none.
func resolveSymLinksOrSelf(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// GetToolDefinition returns the run_command schema, with the programs actually found as the enum for "command".
//
// Notes:
//   - Names are sorted so the text is stable and the prompt prefix stays cacheable.
//   - The description tells the model the limits that shape how it should use the tool: no shell, no stdin, the output
//     cap, and that read_file is the way to read a file.
func (t *RunCommandTool) GetToolDefinition() model.ToolDef {
	names := slices.Sorted(maps.Keys(t.programs))
	enum, _ := json.Marshal(names)
	return model.NewToolDef(
		"run_command",
		fmt.Sprintf("Runs a program (one of %s) with arguments in the working directory and returns its combined output and exit status. "+
			"Use it to build, test and run code, e.g. command=go args=[\"test\",\"./...\"]. "+
			"It is not a shell: no pipes, redirection, && or environment variables. Stdin is empty, so the program must not wait for input. "+
			"To read a file, use read_file instead; an argument naming a file the file tools refuse (such as .env) is refused. Output is cut at %d KB and the program is killed past that, so prefer a narrow command "+
			"(for example one package, one test) over a noisy one. A call that runs too long is stopped.",
			strings.Join(names, ", "), t.maxBytes/1024),
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

// CallTool runs the program and returns its output followed by an exit-status line.
//
// Notes:
//   - A non-zero exit is a normal result ("[exit status N]"): a failing build or test is information for the model, not
//     a failure of the tool. Only a program that cannot start, or a stopped call, is an error.
//   - dir goes through Workspace.resolvePath (inside workDir, not deny-listed, symlinks followed). It is a check made
//     before the program starts; the program itself is not confined (see RunCommandTool).
//   - Before anything starts, checkCommandArgs refuses an argument that names a file the file tools refuse (best effort).
//   - The program gets an empty stdin, the filtered environment and its own process group. The group is killed when ctx
//     ends (Ctrl+C, the tool timeout, the turn timeout) and again after the program exits, so nothing it started outlives
//     the call.
//   - Output (stdout and stderr together) is kept up to maxBytes; past that the program is killed and the result says so.
//     The cut never leaves half a character.
//   - When ctx ends, the error wraps ctx.Err() and holds the output so far; the caller says which limit it was.
//   - A background child that keeps the output open after the program exited is killed; the result says so.
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
	dir := t.ws.rootDir
	if params.Dir != "" && params.Dir != "." {
		rel, info, err := t.ws.resolvePath(params.Dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", params.Dir)
		}
		dir = filepath.Join(t.ws.rootDir, rel)
	}

	if err := t.ws.checkCommandArgs(dir, params.Args); err != nil {
		return "", err
	}

	runCtx, cancel := context.WithCancel(ctx) // cancelled by the output cap too
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, params.Args...)
	cmd.Dir = dir
	cmd.Env = t.environment()
	cmd.WaitDelay = waitDelay
	procutils.StartInOwnGroup(cmd)
	cmd.Cancel = func() error { // kill the whole group, not only the direct child
		procutils.KillGroup(cmd)
		return nil
	}
	out := &capWriter{limit: t.maxBytes, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, out // Stdin stays nil: /dev/null
	err := cmd.Run()
	procutils.KillGroup(cmd) // the program exited; now kill anything it left running in the background

	text := out.String()
	switch {
	case out.exceeded():
		return text + fmt.Sprintf("\n...[output capped at %d bytes; program killed]", t.maxBytes), nil
	case ctx.Err() != nil: // the caller ended the call: Ctrl+C, the tool timeout or the turn timeout
		return "", fmt.Errorf("%s stopped (%w); output so far:\n%s", params.Command, ctx.Err(), text)
	case errors.Is(err, exec.ErrWaitDelay):
		return text + "\n[exit status 0; background processes killed]", nil
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := exit.ExitCode(); code >= 0 {
			return text + fmt.Sprintf("\n[exit status %d]", code), nil
		}
		return text + fmt.Sprintf("\n[%s]", exit.ProcessState), nil // killed by a signal: no exit code
	}
	if err != nil {
		return "", err // the program could not be started at all
	}
	return text + "\n[exit status 0]", nil
}

// environment returns the child's environment: the allowed names that the agent's own environment has.
//
// Notes:
//   - Everything else is dropped on purpose (API keys).
func (t *RunCommandTool) environment() []string {
	var env []string
	for _, name := range t.env {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// capWriter keeps at most limit bytes and calls cancel once more arrives.
//
// [agent] Capping while streaming (not after) means a program printing in a loop is killed early instead of buffered until
// memory runs out. It keeps accepting bytes after the cap so the program never blocks on a full pipe before the kill.
type capWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	limit   int
	dropped int
	cancel  context.CancelFunc
}

// Write stores what fits, counts the rest, and cancels the program once anything is dropped.
//
// Notes:
//   - Always reports the whole write as accepted.
func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	room := max(w.limit-w.buf.Len(), 0)
	if len(p) > room {
		w.dropped += len(p) - room
		p = p[:room]
		w.cancel()
	}
	w.buf.Write(p)
	return n, nil
}

// exceeded reports whether anything was dropped.
//
// Notes: none.
func (w *capWriter) exceeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped > 0
}

// String returns what was kept, never ending in half a character.
//
// Notes:
//   - Only when output was dropped is a trailing partial character removed (textutils.TrimPartialRune): the cap fell
//     inside it. Output the program wrote whole is returned as it is, even if its own last bytes are invalid UTF-8.
//   - [agent] textutils.CutBytes would not do here: the buffer already holds exactly the limit, so it sees nothing to cut.
func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.buf.Bytes()
	if w.dropped > 0 {
		b = textutils.TrimPartialRune(b)
	}
	return string(b)
}
