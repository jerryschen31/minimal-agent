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

// KillGroup kills the command's own process.
//
// Notes:
//   - Only the direct process: there is no process-group kill on Windows, so a grandchild (node under npx) can outlive
//     the command. Killing the whole tree needs a Job Object; deferred, since nothing here is run or tested on Windows
//     (DECISIONS.md § PR #38 review).
//   - Killing the direct process matters on its own: exec.Cmd.Cancel is set to KillGroup, which replaces Go's default
//     (kill the process), so a no-op here would leave even the command running after a cancel.
//   - A command that never started or has already exited is ignored (Kill returns an error then).
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil { // never started
		return
	}
	_ = cmd.Process.Kill()
}
