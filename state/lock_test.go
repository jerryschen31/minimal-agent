package state

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

//   - Verify a second Lock of the same plan is a *LockedError naming the plan and this process's pid, a different plan can
//     be locked at the same time, and after unlock the plan can be locked again
func Test_Unit_Home_Lock(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	a := writePlan(t, "a.md", "a")
	b := writePlan(t, "b.md", "b")
	unlockA, err := h.Lock(a)
	if err != nil {
		t.Fatalf("Lock(a): %v", err)
	}
	_, err = h.Lock(a)
	var le *LockedError
	if !errors.As(err, &le) || le.PID != os.Getpid() || le.Since.IsZero() {
		t.Fatalf("second Lock(a): err = %v (%+v), want a LockedError with this pid", err, le)
	}
	if key, _ := PlanKey(a); le.Plan != key {
		t.Errorf("LockedError.Plan = %q, want %q", le.Plan, key)
	}
	unlockB, err := h.Lock(b)
	if err != nil {
		t.Fatalf("Lock(b) while a is locked: %v", err)
	}
	unlockB()
	if err := unlockA(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	unlockA, err = h.Lock(a)
	if err != nil {
		t.Fatalf("Lock(a) after unlock: %v", err)
	}
	unlockA()
}

// - Verify a lock held by a process that exits without unlocking (a crash) is free again: the OS releases it
func Test_Unit_Home_Lock_ReleasedWhenProcessDies(t *testing.T) {
	if os.Getenv("STATE_LOCK_CHILD") != "" {
		h, _ := OpenHome(os.Getenv("STATE_LOCK_HOME"))
		if _, err := h.Lock(os.Getenv("STATE_LOCK_PLAN")); err != nil {
			os.Exit(2)
		}
		os.Exit(0) // [agent] no unlock: exits holding the lock, like a crash
	}
	home := t.TempDir()
	h, _ := OpenHome(home)
	plan := writePlan(t, "a.md", "a")
	cmd := exec.Command(os.Args[0], "-test.run=^Test_Unit_Home_Lock_ReleasedWhenProcessDies$")
	cmd.Env = append(os.Environ(), "STATE_LOCK_CHILD=1", "STATE_LOCK_HOME="+home, "STATE_LOCK_PLAN="+plan)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	unlock, err := h.Lock(plan)
	if err != nil {
		t.Fatalf("Lock after the holder died: %v", err)
	}
	unlock()
}
