//go:build unix

package procutils

import (
	"bufio"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startWithBackgroundChild starts `sh` in its own group; sh starts a background `sleep` that inherits stdout, prints
// the sleep's pid and waits. It returns the command and its stdout.
func startWithBackgroundChild(t *testing.T) (*exec.Cmd, io.Reader, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	StartInOwnGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	br := bufio.NewReader(out)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the background pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("bad pid %q: %v", line, err)
	}
	// Never leave the sleep behind, whatever the test does. This kills it by pid, not with KillGroup, so a broken
	// KillGroup makes the test fail instead of hanging in cmd.Wait.
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	return cmd, br, pid
}

// - Verify the command leads its own process group, not ours: the terminal's Ctrl+C goes to the foreground group only.
func Test_Unit_StartInOwnGroup_CommandLeadsItsOwnGroup(t *testing.T) {
	cmd, _, _ := startWithBackgroundChild(t)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("command pid %d is in group %d, want its own group", cmd.Process.Pid, pgid)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("command is in our process group %d, so the terminal's Ctrl+C would reach it", pgid)
	}
}

// - Verify KillGroup also stops what the command started, not only the command. Liveness is the stdout pipe: the
// background sleep inherited its write end, so EOF arrives only once every holder is dead (kill(pid, 0) would still
// succeed on a zombie that nobody reaped).
func Test_Unit_KillGroup_KillsTheCommandsChildren(t *testing.T) {
	cmd, out, _ := startWithBackgroundChild(t)

	KillGroup(cmd)

	done := make(chan struct{})
	go func() { io.Copy(io.Discard, out); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the background child is still running after KillGroup")
	}
}

// - Verify KillGroup on a command that never started does nothing instead of panicking.
func Test_Unit_KillGroup_NeverStartedCommandIsIgnored(t *testing.T) {
	KillGroup(exec.Command("sh", "-c", "true"))
}
