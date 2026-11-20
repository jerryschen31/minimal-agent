package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestJournal returns a journal writing to a new file in a temp dir, with a fixed clock.
func newTestJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), journalName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, filePerm)
	if err != nil {
		t.Fatal(err)
	}
	j := &Journal{f: f, path: path, now: func() time.Time { return time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC) }}
	t.Cleanup(func() { j.Close() })
	return j, path
}

// - Verify Append writes one JSON line per event ending in "\n", with v, seq 1, 2, 3, the time, type, task and data
// - Verify Read gives the same events back with tornTail false
func Test_Unit_Journal_AppendRead_RoundTrip(t *testing.T) {
	j, path := newTestJournal(t)
	if err := j.Append(EventManifest, "", Manifest{PlanPath: "/p.md", PlanSHA256: "sha256:x"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(EventTaskStarted, "t1", TaskStarted{Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(EventRunEnded, "", nil); err != nil {
		t.Fatal(err)
	}
	if j.Seq() != 3 {
		t.Errorf("Seq() = %d, want 3", j.Seq())
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(string(data), "\n")
	if len(lines) != 4 || lines[3] != "" {
		t.Fatalf("want 3 lines each ending in a newline, got %q", data)
	}
	for _, l := range lines[:3] {
		if !json.Valid([]byte(l)) {
			t.Errorf("not valid JSON: %s", l)
		}
	}
	if !strings.Contains(lines[1], `"task":"t1"`) || !strings.Contains(lines[1], `"data":{"attempt":1}`) || strings.Contains(lines[2], `"data"`) {
		t.Errorf("unexpected lines:\n%s", data)
	}
	events, torn, err := Read(bytes.NewReader(data))
	if err != nil || torn {
		t.Fatalf("Read: torn %v, err %v", torn, err)
	}
	for i, e := range events {
		if e.V != FormatVersion || e.Seq != i+1 || !e.At.Equal(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)) {
			t.Errorf("event %d = %+v", i, e)
		}
	}
	if events[1].Type != EventTaskStarted || events[1].Task != "t1" {
		t.Errorf("event 2 = %+v", events[1])
	}
}

// - Verify Append after Close is an error, a second Close is nil, and a value that can't be marshalled writes nothing
func Test_Unit_Journal_AppendErrors(t *testing.T) {
	j, path := newTestJournal(t)
	if err := j.Append(EventResumed, "", make(chan int)); err == nil {
		t.Error("Append of an unmarshalable value: want an error")
	}
	if data, _ := os.ReadFile(path); len(data) != 0 || j.Seq() != 0 {
		t.Errorf("a failed Append wrote %q (seq %d)", data, j.Seq())
	}
	j.Close()
	if err := j.Append(EventResumed, "", nil); err == nil {
		t.Error("Append after Close: want an error")
	}
	if err := j.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// - Verify Read drops a torn last line (no newline) and reports it, keeping every complete line
// - Verify a broken line in the middle is an error naming its line number, and blank lines are skipped
func Test_Unit_Read_TornTailAndDamage(t *testing.T) {
	good := `{"v":1,"seq":1,"type":"manifest"}` + "\n\n" + `{"v":1,"seq":2,"type":"task_started","task":"t1"}` + "\n"
	events, torn, err := Read(strings.NewReader(good + `{"v":1,"seq":3,"ty`))
	if err != nil || !torn || len(events) != 2 {
		t.Errorf("torn tail: %d events, torn %v, err %v; want 2, true, nil", len(events), torn, err)
	}
	if _, torn, _ := Read(strings.NewReader(good)); torn {
		t.Error("a journal ending in a newline was reported torn")
	}
	_, _, err = Read(strings.NewReader(good + "garbage\n" + `{"v":1,"seq":3,"type":"x"}` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Errorf("damaged middle line: err = %v, want one naming line 4", err)
	}
}
