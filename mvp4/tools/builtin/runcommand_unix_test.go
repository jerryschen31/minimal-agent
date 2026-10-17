//go:build unix

package builtin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestRunCommand builds run_command with /bin/sh standing in as an allowed program, so the tests don't
// depend on go or python being installed and can make the program misbehave on purpose.
func newTestRunCommand(t *testing.T, dir string, timeout time.Duration) *RunCommandTool {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	tool, err := newRunCommandTool(dir, map[string]string{"sh": sh}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// - Verify run_command runs in workDir (or a subdirectory), returns output plus the exit status, and treats a
// non-zero exit as a normal result rather than a tool error
func Test_RunCommand_RunsAndReportsExitStatus(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/hello.txt", []byte("hi"))
	tool := newTestRunCommand(t, dir, 10*time.Second)

	out, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", "ls; echo out; echo err >&2"}})
	if err != nil || !strings.Contains(out, "sub\nout\nerr\n") || !strings.HasSuffix(out, "[exit status 0]") {
		t.Errorf("ok run: %q, %v", out, err)
	}
	if out, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", "cat hello.txt"}, "dir": "sub"}); err != nil || !strings.HasPrefix(out, "hi") {
		t.Errorf("dir=sub: %q, %v", out, err)
	}
	if out, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", "echo boom; exit 3"}}); err != nil || !strings.Contains(out, "boom\n\n[exit status 3]") {
		t.Errorf("failing program: %q, %v", out, err)
	}
}

// - Verify run_command refuses programs that aren't allowed, a dir outside workDir or deny-listed, and
// passes the arguments literally (no shell between the model and the program)
func Test_RunCommand_Refusals(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "file.txt", nil)
	tool := newTestRunCommand(t, dir, 10*time.Second)

	for name, args := range map[string]map[string]any{
		"not allowed":  {"command": "rm", "args": []string{"-rf", "."}},
		"empty":        {},
		"outside dir":  {"command": "sh", "dir": ".."},
		"denied dir":   {"command": "sh", "dir": ".ssh"},
		"file not dir": {"command": "sh", "dir": "file.txt"},
		"bad args":     {"command": "sh", "args": "-c ls"},
	} {
		if _, err := call(t, tool, args); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// "$(...)" and ";" reach the program as plain text: here sh prints its first argument literally
	out, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", `printf '%s' "$0"`, "$(echo hacked); ls"}})
	if err != nil || !strings.HasPrefix(out, "$(echo hacked); ls") {
		t.Errorf("literal args: %q, %v", out, err)
	}
}

// - Verify API keys and other unlisted environment variables don't reach the program, while PATH does
func Test_RunCommand_FiltersEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	tool := newTestRunCommand(t, t.TempDir(), 10*time.Second)
	out, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", "echo key=[$ANTHROPIC_API_KEY] path=[$PATH]"}})
	if err != nil || !strings.Contains(out, "key=[]") || strings.Contains(out, "path=[]") {
		t.Errorf("env: %q, %v", out, err)
	}
}

// - Verify a program that runs too long, or prints too much, is killed together with its background children
func Test_RunCommand_TimeoutAndOutputCap(t *testing.T) {
	dir := t.TempDir()
	tool := newTestRunCommand(t, dir, 300*time.Millisecond)

	start := time.Now()
	_, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", "echo started; (sleep 1; touch leaked) & sleep 30"}})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "started") {
		t.Errorf("timeout err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
	time.Sleep(1500 * time.Millisecond) // a surviving background child would create "leaked" by now
	if _, err := os.Stat(filepath.Join(dir, "leaked")); err == nil {
		t.Error("a background child outlived the call")
	}

	tool.timeout = 10 * time.Second
	out, err := call(t, tool, map[string]any{"command": "sh", "args": []string{"-c", "yes"}})
	if err != nil || !strings.HasSuffix(out, "program killed]") || len(out) > RunCommandMaxBytes+100 {
		t.Errorf("output cap: len %d, err %v, tail %q", len(out), err, out[max(0, len(out)-60):])
	}
}

// - Verify a cancelled turn (Ctrl+C) stops the program and returns the context error
func Test_RunCommand_Cancelled(t *testing.T) {
	tool := newTestRunCommand(t, t.TempDir(), 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := tool.CallTool(ctx, []byte(`{"command":"sh","args":["-c","sleep 30"]}`))
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out after") {
		t.Errorf("err = %v, want the turn's own context error", err)
	}
}

// - Verify the real constructor finds go (it is running these tests) and lists only installed programs
func Test_NewRunCommandTool_FindsGo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	tool, err := NewRunCommandTool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t, tool, map[string]any{"command": "go", "args": []string{"version"}})
	if err != nil || !strings.HasPrefix(out, "go version") {
		t.Errorf("go version: %q, %v", out, err)
	}
	if params := string(tool.GetToolDefinition().Function.Params); !strings.Contains(params, `"go"`) {
		t.Errorf("schema enum missing go: %s", params)
	}
}
