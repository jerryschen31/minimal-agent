//go:build unix

package tool

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group so a kill reaches
// sh *and* everything it spawned (e.g. `sleep 300 &`), not just sh.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

// killProcessGroup kills every process in the command's group; no-op if it
// never started or the group is already gone.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
