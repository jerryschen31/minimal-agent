//go:build windows

package state

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffsetHigh places the locked byte far past the file's contents: on Windows a locked range can't be read by other
// handles, and readHolder must still read the holder's details.
const lockOffsetHigh = 0x7fffffff

// tryLock takes an exclusive LockFileEx lock on one byte of f without waiting; errLockBusy if another handle holds it.
//
// Notes: none.
func tryLock(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: lockOffsetHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLockBusy
	}
	return err
}

// unlockFile releases tryLock's lock (closing the file would too).
//
// Notes: none.
func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: lockOffsetHigh}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
