//go:build !unix && !windows

package state

import (
	"errors"
	"os"
)

// tryLock reports that this OS has no file lock here: auto mode can't run on it.
//
// Notes: none.
func tryLock(f *os.File) error { return errors.New("file locks are not supported on this OS") }

// unlockFile does nothing (tryLock never succeeds here).
//
// Notes: none.
func unlockFile(f *os.File) error { return nil }
