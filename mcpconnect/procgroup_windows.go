//go:build windows

package mcpconnect

import (
	"os/exec"
	"syscall"
)

// startInOwnGroup starts the server in a new process group, so the console's Ctrl+C does not reach it (Ctrl+Break
// still does). See DECISIONS.md § Deferred for Later, item 14.
func startInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup does nothing on Windows: there is no process-group kill, so a grandchild (node under npx) can outlive
// the server. Killing the whole tree needs a Job Object; deferred, since nothing here is run or tested on Windows.
func killGroup(cmd *exec.Cmd) {}
