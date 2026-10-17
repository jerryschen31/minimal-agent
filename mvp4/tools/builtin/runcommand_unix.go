//go:build unix

package builtin

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the program in its own process group, so a kill reaches everything it started
// (go run's compiled binary, a test's child processes), not just the program itself.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

// killProcessGroup kills every process in the program's group; a no-op if it never started or the group is gone.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
