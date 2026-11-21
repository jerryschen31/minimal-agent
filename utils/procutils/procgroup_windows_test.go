//go:build windows

package procutils

import (
	"os/exec"
	"testing"
	"time"
)

// - Verify KillGroup on Windows kills the command's own process (so exec.Cmd.Cancel = KillGroup stops the command),
// and that a command that never started or already exited is ignored
func Test_KillGroup_Windows_KillsTheDirectProcess(t *testing.T) {
	KillGroup(&exec.Cmd{}) // never started: no panic
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	StartInOwnGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start ping: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	KillGroup(cmd)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the process was still running after KillGroup")
	}
	KillGroup(cmd) // already exited: no panic
}
