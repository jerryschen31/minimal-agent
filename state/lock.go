package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// errLockBusy is what the per-OS tryLock returns when another process holds the lock.
var errLockBusy = errors.New("lock is held")

// LockedError is Lock's error when another process is running the same plan.
type LockedError struct {
	Plan  string    // the plan's path, as the holder recorded it
	PID   int       // 0 when the holder's details couldn't be read
	Since time.Time // when the holder took the lock
}

// Error says which plan is running, in which process, since when.
//
// Notes: none.
func (e *LockedError) Error() string {
	if e.PID == 0 {
		return fmt.Sprintf("a run of %s is in progress", e.Plan)
	}
	return fmt.Sprintf("a run of %s is in progress (pid %d, since %s)", e.Plan, e.PID, e.Since.Local().Format("2006-01-02 15:04:05"))
}

// lockHolder is what the lock file holds while locked, so a refused run can say who holds it.
type lockHolder struct {
	Plan  string    `json:"plan"`
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
}

// Lock takes the plan's lock, so only one run of a plan goes at a time; unlock releases it.
//
// Notes:
//   - One lock file per plan: locks/<key>.lock, key = the first 16 hex characters of sha256(PlanKey(planPath)).
//     Different plans can run at the same time.
//   - A real OS lock (flock on Unix, LockFileEx on Windows): the OS releases it when the process ends, even on kill -9
//     or a power cut, so a crashed run never leaves a stale lock.
//   - Held by another process (or another open in this one): a *LockedError naming the plan, the pid and since when.
//   - While held, the file holds the plan path, the pid and the time (JSON); it is not cleared on unlock.
func (h Home) Lock(planPath string) (unlock func() error, err error) {
	key, err := PlanKey(planPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(h.dir, "locks", hex.EncodeToString(sum[:])[:16]+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("plan lock: %w", err)
	}
	if err := tryLock(f); err != nil {
		holder := readHolder(f)
		f.Close()
		if errors.Is(err, errLockBusy) {
			if holder.Plan == "" {
				holder.Plan = key
			}
			return nil, &LockedError{Plan: holder.Plan, PID: holder.PID, Since: holder.Since}
		}
		return nil, fmt.Errorf("plan lock %s: %w", path, err)
	}
	data, _ := json.Marshal(lockHolder{Plan: key, PID: os.Getpid(), Since: time.Now()})
	if err := f.Truncate(0); err == nil {
		f.WriteAt(append(data, '\n'), 0) // [agent] best effort: the details only make the "in progress" message nicer
	}
	return func() error {
		unlockErr := unlockFile(f)
		closeErr := f.Close()
		return errors.Join(unlockErr, closeErr)
	}, nil
}

// readHolder reads the holder's details from a lock file; the zero value if they can't be read.
//
// Notes: none.
func readHolder(f *os.File) lockHolder {
	var h lockHolder
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 4096))
	if err == nil {
		json.Unmarshal(data, &h)
	}
	return h
}
