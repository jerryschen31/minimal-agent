//go:build unix

package state

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an exclusive flock on f without waiting; errLockBusy if another open file holds it.
//
// Notes:
//   - flock locks belong to the open file, so a second open of the same file in this process is refused too.
func tryLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLockBusy
	}
	return err
}

// unlockFile releases tryLock's lock (closing the file would too).
//
// Notes: none.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
