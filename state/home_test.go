package state

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writePlan writes a plan file named name in a new temp dir and returns its path.
func writePlan(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// refFor returns the identity of the plan at planPath with plan_id id ("" = none).
func refFor(t *testing.T, id, planPath string) PlanRef {
	t.Helper()
	ref, err := NewPlanRef(id, planPath)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// newRunFor starts a run of the plan at planPath (no plan_id) in h and returns its journal and run.
func newRunFor(t *testing.T, h Home, planPath string) (*Journal, Run) {
	t.Helper()
	return newRunOf(t, h, refFor(t, "", planPath))
}

// newRunOf starts a run of the plan ref in h, with a small config, and returns its journal and run.
func newRunOf(t *testing.T, h Home, ref PlanRef) (*Journal, Run) {
	t.Helper()
	data, _ := os.ReadFile(ref.Path)
	j, r, err := h.NewRun(Manifest{PlanID: ref.ID, PlanPath: ref.Path}, data, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j, r
}

//   - Verify OpenHome creates the folder with runs/ and locks/ (0700), and NewRun numbers runs 0001, 0002, …
//   - Verify a run folder holds plan.md (byte for byte), config.json (0600) and a journal (0600) whose first event is
//     the manifest with the plan's and the config's hashes filled in
func Test_Unit_Home_NewRun(t *testing.T) {
	h, err := OpenHome(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{h.Dir(), filepath.Join(h.Dir(), "runs"), filepath.Join(h.Dir(), "locks")} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != dirPerm {
			t.Errorf("%s: %v, %v; want a 0700 folder", d, info, err)
		}
	}
	planPath := writePlan(t, "setup.md", "# Plan: x\n")
	_, r1 := newRunFor(t, h, planPath)
	_, r2 := newRunFor(t, h, planPath)
	if r1.Number != 1 || r2.Number != 2 || filepath.Base(r2.Dir) != "0002" {
		t.Errorf("runs = %+v, %+v", r1, r2)
	}
	if got, _ := os.ReadFile(filepath.Join(r1.Dir, planName)); string(got) != "# Plan: x\n" {
		t.Errorf("plan.md = %q", got)
	}
	info, err := os.Stat(filepath.Join(r1.Dir, journalName))
	if err != nil || info.Mode().Perm() != filePerm {
		t.Errorf("journal: %v, %v; want 0600", info, err)
	}
	m, ok := readManifest(filepath.Join(r1.Dir, journalName))
	if !ok || m.PlanSHA256 != SHA256([]byte("# Plan: x\n")) || !strings.HasSuffix(m.PlanPath, "setup.md") {
		t.Errorf("manifest = %+v, %v", m, ok)
	}
	cfg, err := os.ReadFile(filepath.Join(r1.Dir, configName))
	if err != nil || string(cfg) != `{"model":"m"}` || m.ConfigSHA256 != SHA256(cfg) {
		t.Errorf("config.json = %q, %v; manifest config hash %q", cfg, err, m.ConfigSHA256)
	}
	if info, _ := os.Stat(filepath.Join(r1.Dir, configName)); info.Mode().Perm() != filePerm {
		t.Errorf("config.json mode = %v, want 0600", info.Mode().Perm())
	}
	if r1.PlanPath != m.PlanPath {
		t.Errorf("Run.PlanPath = %q, want the manifest's %q", r1.PlanPath, m.PlanPath)
	}
}

//   - Verify NewRun skips a number whose folder already exists, ignores folders that aren't numbers, and that parallel
//     NewRun calls never get the same number
func Test_Unit_Home_NewRun_NumbersAreUnique(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	for _, name := range []string{"notes", "00x1"} {
		os.Mkdir(filepath.Join(h.Dir(), "runs", name), 0o700)
	}
	planPath := writePlan(t, "p.md", "x")
	const n = 8
	var wg sync.WaitGroup
	nums := make(chan int, n)
	for range n {
		wg.Go(func() {
			j, r, err := h.NewRun(Manifest{PlanPath: planPath}, []byte("x"), nil)
			if err != nil {
				t.Errorf("NewRun: %v", err)
				return
			}
			j.Close()
			nums <- r.Number
		})
	}
	wg.Wait()
	close(nums)
	seen := map[int]bool{}
	for num := range nums {
		if seen[num] {
			t.Errorf("run number %d given twice", num)
		}
		seen[num] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct runs, want %d", len(seen), n)
	}
}

// - Verify ContinueRun cuts a torn last line, appends after the last seq, and the journal then reads clean
func Test_Unit_Home_ContinueRun_CutsTornTail(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	planPath := writePlan(t, "p.md", "x")
	j, r := newRunFor(t, h, planPath)
	j.Append(EventTaskStarted, "t1", TaskStarted{Attempt: 1})
	j.Close()
	path := filepath.Join(r.Dir, journalName)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"v":1,"seq":3,"type":"tool_bef`) // a crash in the middle of Append
	f.Close()

	j2, err := h.ContinueRun(r)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if err := j2.Append(EventResumed, "", Resumed{PID: 1}); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	data, _ := os.ReadFile(path)
	events, torn, err := Read(bytes.NewReader(data))
	if err != nil || torn || len(events) != 3 || events[2].Seq != 3 || events[2].Type != EventResumed {
		t.Errorf("after resume: %d events, torn %v, err %v:\n%s", len(events), torn, err, data)
	}
}

// - Verify Load finds only the runs of this plan (two plans' runs mixed in runs/), oldest first, and folds them
// - Verify a run folder with no journal, or with a first line that isn't a manifest, is skipped; no runs = empty state
func Test_Unit_Home_Load_OnlyThisPlansRuns(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	a := writePlan(t, "a.md", "a")
	b := writePlan(t, "b.md", "b")
	if s, err := h.Load(refFor(t, "", a)); err != nil || len(s.Runs) != 0 || len(s.Tasks) != 0 {
		t.Errorf("no runs: %+v, %v", s, err)
	}
	ja1, _ := newRunFor(t, h, a)
	ja1.Append(EventTaskFinished, "t1", TaskFinished{Status: TaskDone, DefHash: "h1"})
	jb, _ := newRunFor(t, h, b)
	jb.Append(EventTaskFinished, "t1", TaskFinished{Status: TaskFailed})
	os.Mkdir(filepath.Join(h.Dir(), "runs", "0009"), 0o700) // no journal
	os.Mkdir(filepath.Join(h.Dir(), "runs", "0010"), 0o700)
	os.WriteFile(filepath.Join(h.Dir(), "runs", "0010", journalName), []byte(`{"v":1,"seq":1,"type":"task_started"}`+"\n"), 0o600)
	ja2, _ := newRunFor(t, h, a)
	ja2.Append(EventTaskStarted, "t2", TaskStarted{Attempt: 1})

	s, err := h.Load(refFor(t, "", a))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Runs) != 2 || s.Runs[0].Number != 1 || s.Runs[1].Number != 11 {
		t.Errorf("runs = %+v, want 1 and 11", s.Runs)
	}
	if s.Tasks["t1"].Status != TaskDone || s.Tasks["t1"].DefHash != "h1" || s.Tasks["t2"].Status != TaskRunning {
		t.Errorf("tasks = %+v", s.Tasks)
	}
	if s.Latest.Run.Number != 11 || s.Latest.Ended != nil {
		t.Errorf("latest = %+v", s.Latest)
	}
}

// - Verify Load reports a damaged journal of this plan as an error naming the run
func Test_Unit_Home_Load_DamagedJournal(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	a := writePlan(t, "a.md", "a")
	j, r := newRunFor(t, h, a)
	j.Close()
	f, _ := os.OpenFile(filepath.Join(r.Dir, journalName), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("garbage\n{}\n")
	f.Close()
	if _, err := h.Load(refFor(t, "", a)); err == nil || !strings.Contains(err.Error(), "run 1") {
		t.Errorf("err = %v, want one naming run 1", err)
	}
}

//   - Verify a plan with a plan_id keeps its history after the file moves (Load by ID finds the run at the old path, and
//     Run.PlanPath says where it was), while a plan without one does not; a plan with another ID finds nothing
func Test_Unit_Home_Load_PlanIDSurvivesMove(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	old := writePlan(t, "setup.md", "x")
	j, _ := newRunOf(t, h, refFor(t, "7f3a9c1e5b2d8046", old))
	j.Append(EventTaskFinished, "t1", TaskFinished{Status: TaskDone})
	jn, _ := newRunFor(t, h, old) // a run of a hand-written plan at the same path
	jn.Close()

	moved := writePlan(t, "renamed.md", "x")
	s, err := h.Load(refFor(t, "7f3a9c1e5b2d8046", moved))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Runs) != 1 || s.Tasks["t1"].Status != TaskDone {
		t.Fatalf("by ID after a move: runs %+v, tasks %+v", s.Runs, s.Tasks)
	}
	if want := refFor(t, "", old).Path; s.Runs[0].PlanPath != want {
		t.Errorf("Run.PlanPath = %q, want the old path %q", s.Runs[0].PlanPath, want)
	}
	if s, _ := h.Load(refFor(t, "", moved)); len(s.Runs) != 0 {
		t.Errorf("no ID, new path: found %d runs, want 0", len(s.Runs))
	}
	if s, _ := h.Load(refFor(t, "", old)); len(s.Runs) != 1 {
		t.Errorf("no ID, old path: found %d runs, want only the hand-written plan's run", len(s.Runs))
	}
	if s, _ := h.Load(refFor(t, "0000000000000000", moved)); len(s.Runs) != 0 {
		t.Errorf("another ID: found %d runs, want 0", len(s.Runs))
	}
}

// - Verify an older journal whose manifest has only plan_path and plan_sha256 still loads (new fields are empty)
func Test_Unit_Home_Load_OldManifest(t *testing.T) {
	h, _ := OpenHome(t.TempDir())
	a := writePlan(t, "a.md", "a")
	dir := filepath.Join(h.Dir(), "runs", "0001")
	os.Mkdir(dir, 0o700)
	line := `{"v":1,"seq":1,"type":"manifest","data":{"plan_path":"` + refFor(t, "", a).Path + `","plan_sha256":"sha256:x"}}` + "\n"
	os.WriteFile(filepath.Join(dir, journalName), []byte(line), 0o600)
	if s, err := h.Load(refFor(t, "", a)); err != nil || len(s.Runs) != 1 {
		t.Errorf("old manifest: %+v, %v", s.Runs, err)
	}
}
