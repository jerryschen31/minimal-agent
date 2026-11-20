package state

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// Home is the state folder (config stateDir, ~/.minagent by default) holding every run's folder and the plan locks.
type Home struct {
	dir string // absolute
}

// Run is one run's folder: runs/<Number>, four digits or more.
type Run struct {
	Number int
	Dir    string // absolute
}

const (
	journalName    = "journal.jsonl"
	planName       = "plan.md"
	traceName      = "trace"
	dirPerm        = 0o700 // private: journals hold tool-argument previews
	filePerm       = 0o600
	maxNewRunTries = 1000 // NewRun tries this many numbers before giving up (another process keeps taking them)
)

// OpenHome returns the state folder at dir, creating it and its runs/ and locks/ folders (0700) if needed.
//
// Notes:
//   - dir is made absolute; "~" is expanded by the caller (config.Config.ResolveStateDir).
//   - Existing folders keep their permissions.
func OpenHome(dir string) (Home, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Home{}, fmt.Errorf("state folder %q: %w", dir, err)
	}
	for _, d := range []string{abs, filepath.Join(abs, "runs"), filepath.Join(abs, "locks")} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return Home{}, fmt.Errorf("state folder: %w", err)
		}
	}
	return Home{dir: abs}, nil
}

// Dir returns the state folder's absolute path.
//
// Notes: none.
func (h Home) Dir() string { return h.dir }

// TraceDir returns the folder for run r's trace (not created).
//
// Notes: none.
func (r Run) TraceDir() string { return filepath.Join(r.Dir, traceName) }

// PlanKey returns the plan's identity across runs: its absolute path with symlinks resolved.
//
// Notes:
//   - The plan file must exist. Moving or renaming a plan starts it with no history: every task runs again, which is the
//     safe side (an ID kept inside the plan would make a copied plan inherit its original's history).
func PlanKey(planPath string) (string, error) {
	abs, err := filepath.Abs(planPath)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// SHA256 returns "sha256:<hex>" of data, the form the manifest records.
//
// Notes: none.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NewRun creates the next run folder, writes plan (the plan file's bytes) into it as plan.md, and starts its journal
// with m as the manifest.
//
// Notes:
//   - The number is one more than the highest existing run. The folder is created with os.Mkdir, which only one process
//     can win: if another run took the number first, the next number is tried (up to maxNewRunTries).
//   - m.PlanSHA256 is set from plan when empty.
//   - Folders are 0700, files 0600. On an error after the folder was made, the folder is left (Load skips a run with no
//     manifest).
func (h Home) NewRun(m Manifest, plan []byte) (*Journal, Run, error) {
	runs, err := h.runs()
	if err != nil {
		return nil, Run{}, err
	}
	next := 1
	if len(runs) > 0 {
		next = runs[len(runs)-1].Number + 1
	}
	var r Run
	for tries := 0; ; tries++ {
		if tries == maxNewRunTries {
			return nil, Run{}, fmt.Errorf("could not create a run folder in %s after %d tries", h.runsDir(), tries)
		}
		r = Run{Number: next, Dir: filepath.Join(h.runsDir(), fmt.Sprintf("%04d", next))}
		err := os.Mkdir(r.Dir, dirPerm)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, Run{}, fmt.Errorf("new run folder: %w", err)
		}
		next++
	}
	if err := os.WriteFile(filepath.Join(r.Dir, planName), plan, filePerm); err != nil {
		return nil, Run{}, fmt.Errorf("run %d: copy of the plan: %w", r.Number, err)
	}
	if m.PlanSHA256 == "" {
		m.PlanSHA256 = SHA256(plan)
	}
	f, err := os.OpenFile(filepath.Join(r.Dir, journalName), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, filePerm)
	if err != nil {
		return nil, Run{}, fmt.Errorf("run %d: %w", r.Number, err)
	}
	j := &Journal{f: f, path: f.Name(), now: time.Now}
	if err := j.Append(EventManifest, "", m); err != nil {
		j.Close()
		return nil, Run{}, err
	}
	return j, r, nil
}

// ContinueRun opens run r's journal to append to it (a resume): a torn last line is cut off first, and the next event
// gets the seq after the last one.
//
// Notes:
//   - Only call it while holding the plan's lock: two writers would interleave lines.
func (h Home) ContinueRun(r Run) (*Journal, error) {
	path := filepath.Join(r.Dir, journalName)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("run %d: %w", r.Number, err)
	}
	events, goodLen, torn, err := readEvents(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("run %d: %s: %w", r.Number, path, err)
	}
	if torn {
		if err := f.Truncate(goodLen); err != nil {
			f.Close()
			return nil, fmt.Errorf("run %d: cutting the torn last line: %w", r.Number, err)
		}
	}
	if _, err := f.Seek(goodLen, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("run %d: %w", r.Number, err)
	}
	j := &Journal{f: f, path: path, now: time.Now}
	if len(events) > 0 {
		j.seq = events[len(events)-1].Seq
	}
	return j, nil
}

// Load rebuilds the state of the plan at planPath from every earlier run of it, oldest first (Fold).
//
// Notes:
//   - A run belongs to the plan when its manifest's PlanPath equals PlanKey(planPath). Only the first line of each
//     other run's journal is read.
//   - A run folder with no journal, an empty one, or a first line that isn't a manifest is skipped (a crash between
//     creating the folder and writing the manifest leaves one). A damaged journal of this plan is an error.
//   - No runs: an empty State (no tasks), not an error.
func (h Home) Load(planPath string) (State, error) {
	key, err := PlanKey(planPath)
	if err != nil {
		return State{}, err
	}
	runs, err := h.runs()
	if err != nil {
		return State{}, err
	}
	var mine []RunEvents
	for _, r := range runs {
		path := filepath.Join(r.Dir, journalName)
		m, ok := readManifest(path)
		if !ok || m.PlanPath != key {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return State{}, fmt.Errorf("run %d: %w", r.Number, err)
		}
		events, _, torn, err := readEvents(f)
		f.Close()
		if err != nil {
			return State{}, fmt.Errorf("run %d: %s: %w", r.Number, path, err)
		}
		mine = append(mine, RunEvents{Run: r, Events: events, TornTail: torn})
	}
	return Fold(mine)
}

// readManifest reads the manifest from the first line of the journal at path; ok is false when there is none.
//
// Notes: none.
func readManifest(path string) (Manifest, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return Manifest{}, false // empty, or only a torn first line
	}
	var e Event
	var m Manifest
	if json.Unmarshal(line, &e) != nil || e.Type != EventManifest || json.Unmarshal(e.Data, &m) != nil {
		return Manifest{}, false
	}
	return m, true
}

// runsDir returns the folder holding the run folders.
//
// Notes: none.
func (h Home) runsDir() string { return filepath.Join(h.dir, "runs") }

// runs lists the run folders, by number.
//
// Notes:
//   - Only folders whose name is all digits count; anything else in runs/ is ignored.
func (h Home) runs() ([]Run, error) {
	entries, err := os.ReadDir(h.runsDir())
	if err != nil {
		return nil, fmt.Errorf("state folder: %w", err)
	}
	var runs []Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n <= 0 || strconv.Itoa(n) != trimZeros(e.Name()) {
			continue
		}
		runs = append(runs, Run{Number: n, Dir: filepath.Join(h.runsDir(), e.Name())})
	}
	slices.SortFunc(runs, func(a, b Run) int { return a.Number - b.Number })
	return runs, nil
}

// trimZeros drops leading zeros from a run folder's name ("0007" -> "7").
//
// Notes: none.
func trimZeros(s string) string {
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}
