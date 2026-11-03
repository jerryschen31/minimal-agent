//go:build windows

package procutils

import (
	"os/exec"
	"syscall"
)

// StartInOwnGroup starts the command in a new process group, so the console's Ctrl+C does not reach it.
//
// Notes:
//   - Ctrl+Break still reaches it. See DECISIONS.md § Deferred for Later, item 14.
func StartInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// KillGroup does nothing on Windows.
//
// Notes:
//   - There is no process-group kill, so a grandchild (node under npx) can outlive the command. Killing the whole tree
//     needs a Job Object; deferred, since nothing here is run or tested on Windows.
func KillGroup(cmd *exec.Cmd) {}
