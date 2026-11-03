//go:build unix

package procutils

import (
	"os/exec"
	"syscall"
)

// StartInOwnGroup makes the command the leader of a new process group, one group per command.
//
// Notes:
//   - The terminal sends Ctrl+C's SIGINT to its foreground process group only, so a command in its own group keeps
//     running when Ctrl+C cancels a chat turn. See DECISIONS.md § Deferred for Later, item 14.
//   - Call it before the command starts; it sets cmd.SysProcAttr.
func StartInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillGroup kills every process still left in the command's group, e.g. the node process that npx started.
//
// Notes:
//   - Closing a session or waiting for the command only deals with the direct child.
//   - The group ID is the command's PID, since it leads its group (StartInOwnGroup).
//   - ESRCH (group already empty) is ignored, as is a command that never started.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil { // never started
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // a negative PID means "the whole group"
}
