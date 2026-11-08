package objective

import (
	"slices"
	"testing"
)

// [agent] the Cleanup tests (and mustCleanup, fileOf, checkFixes, checkTasks) are in cleanup_test.go.

// complete is a hand-built objective that passes CheckComplete and uses every field.
func complete() Objective {
	// TODO: Format FormatV1, every section, TaskNotes, three tasks (t1, t2 depends t1, t3 with a multi-line body
	//   holding a bullet list and a code fence), two Other sections
	// Hint: a body with leading/trailing blank lines is one Cleanup can't produce, so the round trip would fail
	return Objective{}
}

// --- Task IDs ---

// Test_Unit_AssignTaskIDs: the rule on its own (it is also used by plan mode's propose_objective).
func Test_Unit_AssignTaskIDs(t *testing.T) {
	tests := []struct {
		name        string
		ids         []string // "" = no ID
		want        []string
		wantChanged []int
	}{
		{"none have IDs", []string{"", "", ""}, []string{"t1", "t2", "t3"}, []int{0, 1, 2}},
		{"all have IDs, out of order", []string{"t1", "t5", "t2"}, []string{"t1", "t5", "t2"}, nil},
		{"mixed", []string{"t1", "", "t5", ""}, []string{"t1", "t6", "t5", "t7"}, []int{1, 3}},
		{"non-numeric ID is kept and ignored", []string{"setup", ""}, []string{"setup", "t1"}, []int{1}},
		{"too long to be a number", []string{"t1234567890", ""}, []string{"t1234567890", "t1"}, []int{1}},
		{"empty", nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tasks []Task
			for _, id := range tt.ids {
				tasks = append(tasks, Task{ID: id})
			}
			changed := AssignTaskIDs(tasks)
			var got []string
			for _, task := range tasks {
				got = append(got, task.ID)
			}
			if !slices.Equal(got, tt.want) || !slices.Equal(changed, tt.wantChanged) {
				t.Errorf("IDs %v, changed %v; want %v, %v", got, changed, tt.want, tt.wantChanged)
			}
		})
	}
}

// --- Markdown ---

// Test_Unit_Markdown_RoundTrip: for every testdata file and complete(), Cleanup(o.Markdown()) gives back o with
// zero fixes; objective-deepwiki.md itself cleans up with zero fixes, objective-fib.md with some.
func Test_Unit_Markdown_RoundTrip(t *testing.T) {
	// TODO 1: files, _ := filepath.Glob("testdata/*.md"); fail if none (a wrong path would pass silently)
	// TODO 2: per file (t.Run): os.ReadFile → mustCleanup → o; then o2, fixes := mustCleanup(t, o.Markdown())
	//         want len(fixes) == 0 (else t.Errorf listing them) and reflect.DeepEqual(o2, o)
	// TODO 3: the same for complete()
	// Hint: DeepEqual tells nil and empty slices apart (Tasks, Depends): pick one in Cleanup and stick to it
}

// Test_Unit_Markdown_Layout: front matter first; sections in order; empty optional sections left out; a task with
// and without depends; the file ends with exactly one "\n".
func Test_Unit_Markdown_Layout(t *testing.T) {
	// TODO: compare complete().Markdown() with a hand-written want string (the layout in Markdown's header)
}

// --- CheckComplete ---

// Test_Unit_CheckComplete: a complete objective passes; each problem is named; several problems give one joined
// error.
func Test_Unit_CheckComplete(t *testing.T) {
	const maxTasks = 4
	tests := []struct {
		name     string
		edit     func(o *Objective) // changes complete() to break it; nil = leave it complete
		wantSubs []string           // substrings the error must contain; empty = want nil
	}{
		// TODO: complete → nil · ObjText "" · Instructions "" · Tasks nil · maxTasks+1 tasks
		//   task ID "" · ID "setup" · duplicate ID · blank Title
		//   depends on "t9" (no such task) · depends on itself · cycle t1 → t2 → t1 (error shows the cycle)
		//   Instructions "" and Tasks nil together → both substrings
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// TODO: o := complete(); if tt.edit != nil { tt.edit(&o) }; err := o.CheckComplete(maxTasks)
			//   no wantSubs → err must be nil; otherwise err != nil and contains every substring
		})
	}
}

// Test_Unit_Fix_String: with and without a line number; Was "" (added) and Now "" (ignored).
func Test_Unit_Fix_String(t *testing.T) {
	// TODO
}

// --- Approval ---

// Test_Unit_BodyHash: the same for objectives that differ only in front matter (Format, Approved); different when
// any section or task changes; stable for a file and its cleaned-up copy (a cosmetic fix is not an edit).
func Test_Unit_BodyHash(t *testing.T) {
	// TODO
}

// Test_Unit_ApprovalStatus: no Approved → unplanned; Approved = BodyHash() → approved; then change a task's Body →
// edited; a malformed Approved ("sha256:zz") → edited.
func Test_Unit_ApprovalStatus(t *testing.T) {
	// TODO
}

// --- Fuzzing ---

// FuzzCleanup: for any input, Cleanup never panics, and when it succeeds, Cleanup(o.Markdown()) succeeds with zero
// fixes and gives back o. Run: go test ./objective -run XXX -fuzz FuzzCleanup -fuzztime 30s
func FuzzCleanup(f *testing.F) {
	// TODO 1: f.Add each testdata file's bytes, complete().Markdown(), and a few small strings ("", "# Objective\nx")
	f.Fuzz(func(t *testing.T, data []byte) {
		// TODO 2: o, _, err := Cleanup(data); err != nil → return (rejecting is fine; panicking is not)
		// TODO 3: o2, fixes, err := Cleanup([]byte(o.Markdown())); want err nil, len(fixes) == 0, reflect.DeepEqual
		// Hint: expect it to find real cases (e.g. a body line Markdown writes that Cleanup then reads as a heading);
		//   each one becomes a normal test row once fixed
	})
}
