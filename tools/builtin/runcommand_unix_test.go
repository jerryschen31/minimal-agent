//go:build unix

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runArgs(command string, args []string, dir string) json.RawMessage {
	m := map[string]any{"command": command, "args": args}
	if dir != "" {
		m["dir"] = dir
	}
	b, _ := json.Marshal(m)
	return b
}

func sh(script string) []string { return []string{"-c", script} }

// - Verify output is returned with an exit-status line, and that a non-zero exit is a normal result, not an error
func Test_RunCommand_ExitStatus_NormalResult(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"sh"}, nil, 4096)
	got, err := tool.CallTool(context.Background(), runArgs("sh", sh("echo hello; echo oops >&2"), ""))
	if err != nil || !strings.Contains(got, "hello") || !strings.Contains(got, "oops") || !strings.HasSuffix(got, "[exit status 0]") {
		t.Errorf("(%q, %v)", got, err)
	}
	got, err = tool.CallTool(context.Background(), runArgs("sh", sh("echo failing; exit 3"), ""))
	if err != nil || !strings.Contains(got, "failing") || !strings.HasSuffix(got, "[exit status 3]") {
		t.Errorf("non-zero exit: (%q, %v)", got, err)
	}
}

// - Verify a command not on the list is refused with the list of allowed ones, and that bad arguments are an error
func Test_RunCommand_UnlistedCommand_Refused(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"sh"}, nil, 4096)
	if _, err := tool.CallTool(context.Background(), runArgs("rm", []string{"-rf", "x"}, "")); err == nil || !strings.Contains(err.Error(), "use one of: sh") {
		t.Errorf("unlisted: err = %v", err)
	}
	if _, err := tool.CallTool(context.Background(), json.RawMessage(`{"command": 5}`)); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("bad arguments: err = %v", err)
	}
}

// - Verify there is no shell: pipes, && and $VAR in the arguments are literal text
func Test_RunCommand_NoShell_ArgumentsAreLiteral(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"echo"}, nil, 4096)
	got, err := tool.CallTool(context.Background(), runArgs("echo", []string{"a", "&&", "b", "|", "c", "$HOME", "$(id)"}, ""))
	if err != nil || !strings.HasPrefix(got, "a && b | c $HOME $(id)\n") {
		t.Errorf("(%q, %v)", got, err)
	}
}

// - Verify the environment is filtered: a variable of the agent's environment is not passed, one named in commandEnv is,
// and a built-in name (HOME) still is
func Test_RunCommand_Environment_FilteredAndExtendable(t *testing.T) {
	t.Setenv("RUNCMD_TEST_SECRET", "s3cret")
	t.Setenv("HOME", "/home/test-user")
	script := sh(`echo "secret=[$RUNCMD_TEST_SECRET] home=[$HOME]"`)

	plain, _ := newRunTool(t, t.TempDir(), []string{"sh"}, nil, 4096)
	got, err := plain.CallTool(context.Background(), runArgs("sh", script, ""))
	if err != nil || !strings.Contains(got, "secret=[] home=[/home/test-user]") {
		t.Errorf("filtered: (%q, %v)", got, err)
	}

	extended, _ := newRunTool(t, t.TempDir(), []string{"sh"}, []string{"RUNCMD_TEST_SECRET"}, 4096)
	got, err = extended.CallTool(context.Background(), runArgs("sh", script, ""))
	if err != nil || !strings.Contains(got, "secret=[s3cret]") {
		t.Errorf("extended: (%q, %v)", got, err)
	}
	if len(DefaultCommandEnv) == 0 || len(extended.env) != len(DefaultCommandEnv)+1 {
		t.Errorf("commandEnv must add to the built-in list, never replace it: %v", extended.env)
	}
}

// - Verify stdin is empty: a program reading it gets end-of-file at once instead of waiting
func Test_RunCommand_Stdin_IsEmpty(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"cat"}, nil, 4096)
	// the agent's own stdin is a pipe nobody writes to: a program that inherited it would wait forever (go test's own
	// stdin is /dev/null, which would hide that)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close(); w.Close() })
	done := make(chan string, 1)
	go func() {
		got, _ := tool.CallTool(context.Background(), runArgs("cat", nil, ""))
		done <- got
	}()
	select {
	case got := <-done:
		if got != "\n[exit status 0]" {
			t.Errorf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cat waited for stdin")
	}
}

// - Verify the output cap: past maxBytes the program is killed (an endless writer returns quickly), the result is cut at
// the cap and says so
func Test_RunCommand_OutputCap_KillsAndSaysSo(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"sh"}, nil, 100)
	start := time.Now()
	got, err := tool.CallTool(context.Background(), runArgs("sh", sh("while true; do echo xxxxxxxxxxxxxxxxxxxx; done"), ""))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v: the endless writer was not killed", time.Since(start))
	}
	body, note, _ := strings.Cut(got, "\n...[")
	if len(body) != 100 || !strings.HasPrefix(note, "output capped at 100 bytes; program killed]") {
		t.Errorf("len(body) = %d, note = %q", len(body), note)
	}
}

// - Verify output of exactly the cap is not a cut, and one byte more is
func Test_RunCommand_OutputCap_Boundary(t *testing.T) {
	small, _ := newRunTool(t, t.TempDir(), []string{"printf"}, nil, 5)
	got, _ := small.CallTool(context.Background(), runArgs("printf", []string{"12345"}, ""))
	if strings.Contains(got, "capped") || !strings.HasPrefix(got, "12345") {
		t.Errorf("exactly the cap: %q", got)
	}
	got, _ = small.CallTool(context.Background(), runArgs("printf", []string{"123456"}, ""))
	if !strings.HasPrefix(got, "12345\n...[output capped at 5 bytes") {
		t.Errorf("one byte over: %q", got)
	}
}

// - Verify the cut never leaves half a character: a cap of 4 bytes lands inside the second 2-byte character, which is dropped
func Test_RunCommand_OutputCap_NoHalfCharacter(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"printf"}, nil, 4)
	got, _ := tool.CallTool(context.Background(), runArgs("printf", []string{"aéé"}, "")) // a=1 byte, é=2 bytes each: byte 4 is half of the second é
	body, _, _ := strings.Cut(got, "\n...[")
	if body != "aé" {
		t.Errorf("body = %q, want %q", body, "aé")
	}
}

// - Verify a call ended by its context (the tool timeout, Ctrl+C) kills the program, returns an error that wraps ctx.Err() and
// holds the output so far, and does so quickly
func Test_RunCommand_ContextEnds_KilledWithOutputSoFar(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"sh"}, nil, 4096)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := tool.CallTool(ctx, runArgs("sh", sh("echo started; sleep 30"), ""))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "started") || got != "" {
		t.Errorf("(%q, %v)", got, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	var pid int
	for range 50 {
		if b, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				pid = n
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the background process never wrote its pid")
	}
	for range 100 {
		if processGone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("background process %d outlived the call", pid)
}

// - Verify nothing the program starts outlives the call: a background child is killed when the call is stopped, and also
// when the program exits on its own leaving a child that holds the output open (the result says so)
func Test_RunCommand_NothingOutlivesTheCall(t *testing.T) {
	work := t.TempDir()
	tool, _ := newRunTool(t, work, []string{"sh"}, nil, 4096)

	t.Run("stopped call", func(t *testing.T) {
		pidFile := filepath.Join(work, "pid1")
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, err := tool.CallTool(ctx, runArgs("sh", sh("sleep 60 & echo $! > pid1; wait"), ""))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		waitGone(t, pidFile)
	})
	t.Run("program exits, child keeps the pipe", func(t *testing.T) {
		pidFile := filepath.Join(work, "pid2")
		start := time.Now()
		got, err := tool.CallTool(context.Background(), runArgs("sh", sh("sleep 60 & echo $! > pid2; echo done"), ""))
		if err != nil || !strings.Contains(got, "done") || !strings.HasSuffix(got, "[exit status 0; background processes killed]") {
			t.Errorf("(%q, %v)", got, err)
		}
		if time.Since(start) > 10*time.Second {
			t.Errorf("took %v", time.Since(start))
		}
		waitGone(t, pidFile)
	})
}

// - Verify the dir argument: the program runs in that subdirectory; a path outside workDir, a deny-listed one, a file and a
// missing one are refused; "." means workDir
func Test_RunCommand_Dir(t *testing.T) {
	work := t.TempDir()
	for _, d := range []string{"sub", ".ssh"} {
		if err := os.Mkdir(filepath.Join(work, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, _ := newRunTool(t, work, []string{"pwd"}, nil, 4096)
	real, _ := filepath.EvalSymlinks(work)

	got, err := tool.CallTool(context.Background(), runArgs("pwd", nil, "sub"))
	if err != nil || !strings.HasPrefix(got, filepath.Join(real, "sub")+"\n") {
		t.Errorf("sub: (%q, %v)", got, err)
	}
	got, err = tool.CallTool(context.Background(), runArgs("pwd", nil, "."))
	if err != nil || !strings.HasPrefix(got, real+"\n") {
		t.Errorf(".: (%q, %v)", got, err)
	}
	for _, bad := range []string{"..", "../..", "/", ".ssh", "f.txt", "missing"} {
		if got, err := tool.CallTool(context.Background(), runArgs("pwd", nil, bad)); err == nil {
			t.Errorf("dir %q: want an error, got %q", bad, got)
		} else if strings.Contains(err.Error(), real) {
			t.Errorf("dir %q: error reveals the absolute path: %v", bad, err)
		}
	}
}

// - Verify a program found inside workDir (a "." style PATH entry) is refused with a warning, so a file the model writes
// can't stand in for a program; and that a program resolved at setup is stored as an absolute path
func Test_RunCommand_New_ProgramInsideWorkDirRefused(t *testing.T) {
	work := t.TempDir()
	planted := filepath.Join(work, "mytool")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\necho PLANTED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", work+string(os.PathListSeparator)+os.Getenv("PATH"))
	ws, _ := NewWorkspace(work, "", "", DenyList{})
	var warn strings.Builder
	_, err := NewRunCommandTool(ws, []string{"mytool"}, nil, 100, &warn)
	if !errors.Is(err, ErrNoCommands) || !strings.Contains(warn.String(), `"mytool" resolves inside workDir`) {
		t.Errorf("err = %v, warnings = %q", err, warn.String())
	}
}
