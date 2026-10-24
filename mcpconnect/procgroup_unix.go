//go:build unix

package mcpconnect

import (
	"os/exec"
	"syscall"
)

// startInOwnGroup makes the server the leader of a new process group, one group per server. The terminal sends
// Ctrl+C's SIGINT to its foreground process group only, so a server in its own group keeps running when Ctrl+C
// cancels a chat turn. See DECISIONS.md § Deferred for Later, item 14.
func startInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills every process still left in the server's group, e.g. the node process that npx started.
// Closing the session only stops the direct child. The group ID is the server's PID, since the server leads its
// group. ESRCH (group already empty) is ignored.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil { // never started
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // a negative PID means "the whole group"
}
