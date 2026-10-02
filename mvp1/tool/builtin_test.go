package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func call(t *testing.T, tl interface {
	Call(context.Context, json.RawMessage) (string, error)
}, args string) (string, error) {
	t.Helper()
	return tl.Call(context.Background(), json.RawMessage(args))
}

func TestShellTimeoutKillsBackgroundChildren(t *testing.T) {
	start := time.Now()
	// [agent] the & child inherits the stdout pipe; without WaitDelay + a process
	// group kill this used to block for the full 5s and leave sleep running.
	out, err := call(t, Shell(t.TempDir(), 300*time.Millisecond), `{"command":"echo hi; sleep 5 & sleep 5"}`)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout not enforced: took %v (out=%q err=%v)", el, out, err)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "hi") {
		t.Fatalf("want timeout error carrying partial output, got out=%q err=%v", out, err)
	}
}

func TestShellOrphansDoNotOutliveTheCall(t *testing.T) {
	// [agent] sh exits at once; the background sleep would otherwise live on
	// (and, holding the stdout pipe, would block the call for its full duration).
	start := time.Now()
	out, err := call(t, Shell(t.TempDir(), 10*time.Second), `{"command":"sleep 30 & echo $!"}`)
	if err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("out=%q err=%v elapsed=%v", out, err, time.Since(start))
	}
	pid := strings.Fields(out)[0]
	if exec.Command("kill", "-0", pid).Run() == nil {
		_ = exec.Command("kill", "-9", pid).Run()
		t.Fatalf("background pid %s survived the tool call (out=%q)", pid, out)
	}
	if !strings.Contains(out, "background processes killed") {
		t.Fatalf("expected note about killed background processes: %q", out)
	}
}

func TestShellOutputIsCappedWhileStreaming(t *testing.T) {
	start := time.Now()
	out, err := call(t, Shell(t.TempDir(), 10*time.Second), `{"command":"yes | head -c 100000000"}`) // 100 MB
	if err != nil || len(out) > maxOutput+200 || !strings.Contains(out, "output capped") {
		t.Fatalf("len=%d err=%v tail=%q", len(out), err, out[max(0, len(out)-80):])
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("runaway command not killed promptly: %v", el)
	}
}

func TestShellNonZeroExitIsAnObservation(t *testing.T) {
	out, err := call(t, Shell(t.TempDir(), time.Second), `{"command":"echo boom >&2; exit 3"}`)
	if err != nil || !strings.Contains(out, "boom") || !strings.Contains(out, "exit status 3") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestConfineRejectsSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cret"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := call(t, ReadFile(root), `{"path":"link/secret"}`); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("read through symlink allowed: %v", err)
	}
	if _, err := call(t, WriteFile(root), `{"path":"link/new/pwned","content":"x"}`); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("write through symlink allowed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Fatal("MkdirAll ran outside root")
	}
	for _, p := range []string{"../x", "..", "a/../../x"} {
		if _, err := call(t, ReadFile(root), `{"path":"`+p+`"}`); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("%q: %v", p, err)
		}
	}
	// [agent] absolute paths are interpreted relative to root, never as-is
	if out, err := call(t, ReadFile(root), `{"path":"/etc/passwd"}`); err == nil || strings.Contains(out, "root:") {
		t.Fatalf("absolute path read host file: %q %v", out, err)
	}
	// [agent] a dotfile whose name merely starts with ".." is not an escape
	_ = os.WriteFile(filepath.Join(root, "..rc"), []byte("ok"), 0o600)
	if out, err := call(t, ReadFile(root), `{"path":"..rc"}`); err != nil || out != "ok" {
		t.Fatalf("..rc: %q %v", out, err)
	}
}

func TestReadFileIsBounded(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "big"), []byte(strings.Repeat("a", 3*maxOutput)), 0o600)
	out, err := call(t, ReadFile(root), `{"path":"big"}`)
	if err != nil || len(out) > maxOutput+100 || !strings.Contains(out, "truncated 32768 bytes") {
		t.Fatalf("len=%d err=%v tail=%q", len(out), err, out[max(0, len(out)-60):])
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := call(t, WriteFile(root), `{"path":"a/b/c.txt","content":"hello"}`); err != nil {
		t.Fatal(err)
	}
	if out, err := call(t, ReadFile(root), `{"path":"a/b/c.txt"}`); err != nil || out != "hello" {
		t.Fatalf("%q %v", out, err)
	}
}
