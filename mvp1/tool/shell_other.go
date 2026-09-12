//go:build !unix

package tool

import "os/exec"

// Process groups are unavailable here; the context still kills the direct
// child and WaitDelay releases its pipes.
func setProcessGroup(*exec.Cmd)        {}
func killProcessGroup(*exec.Cmd) error { return nil }
