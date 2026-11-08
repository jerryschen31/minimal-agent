package objective

import (
	"testing"
)

// [agent] testdata/: objective-fib.md is hand-written with mistakes Cleanup fixes; objective-deepwiki.md is in the
// standard layout (front matter, "### tN:" tasks, depends, an unknown "## Notes" section) and cleans up with zero
// fixes; the two objective-build-* files are drafts without tasks.

// mustCleanup cleans up s and fails the test on an error.
func mustCleanup(t *testing.T, s string) (Objective, []Fix) {
	t.Helper()
	// TODO: o, fixes, err := Cleanup([]byte(s)); t.Fatalf on err (include the input); return o, fixes
	return Objective{}, nil
}

// complete is a hand-built objective that passes CheckComplete and uses every field.
func complete() Objective {
	// TODO: Format FormatV1, every section, TaskNotes, three tasks (t1, t2 depends t1, t3 with a multi-line body
	//   holding a bullet list and a code fence), two Other sections
	// Hint: a body with leading/trailing blank lines is one Cleanup can't produce, so the round trip would fail
	return Objective{}
}

// --- Cleanup: reading and errors ---

// Test_Unit_Cleanup_MinimalDraft: only "# Objective" cleans up with no fixes; Format is FormatV1; the rest is empty.
func Test_Unit_Cleanup_MinimalDraft(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_NoHeadings: a file of plain text becomes the Objective, with one fix (Now "# Objective").
func Test_Unit_Cleanup_NoHeadings(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_Errors: what Cleanup can't decide is an error naming the problem.
func Test_Unit_Cleanup_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string // a substring the error must contain, so the test fails if the wrong check fires
	}{
		// TODO: empty · whitespace only · too big (strings.Repeat("x", MaxBytes+1))
		//   "# Objective" twice · "## Instructions" twice (also "## Instruction" + "### Instructions": same section)
		//   duplicate task ID ("### t1: a" and "### t1: b") · text before the first heading plus "# Objective"
		//   a task heading outside "## Tasks" ("### t1: x" under "## Instructions"; also "## Task 2: x" before Tasks)
		//   front matter: unknown format · a line without ":" · no closing "---"
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// TODO: _, _, err := Cleanup([]byte(tt.input)); want err != nil and strings.Contains(err.Error(), tt.wantSub)
		})
	}
}

// Test_Unit_Cleanup_FrontMatter: no front matter or no format line → FormatV1 and no fix; "format: …v1" and
// "approved: sha256:…" read as is; an unknown key ("name: x") is one fix with Now "" and is not kept.
func Test_Unit_Cleanup_FrontMatter(t *testing.T) {
	// TODO: table of inputs → want Format, Approved, and the fixes
}

// Test_Unit_Cleanup_BOM: a file starting with a UTF-8 byte-order mark reads the same as without it (front matter
// and "# Objective" found), with no fix.
func Test_Unit_Cleanup_BOM(t *testing.T) {
	// TODO: "\uFEFF---\nformat: …\n---\n# Objective\nx\n" and "\uFEFF# Objective\nx\n"
}

// --- Cleanup: the fixes ---

// Test_Unit_Cleanup_HeadingFixes: each heading mistake fills the right field and makes exactly one fix.
func Test_Unit_Cleanup_HeadingFixes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantFix Fix                  // Line, Was and Now
		check   func(Objective) bool // the field the section went to
	}{
		// TODO: "## Objective" → "# Objective" · "#Objective" → "# Objective" · "### Constraints" → "## Constraints"
		//   "## Instruction" → "## Instructions" · "## Tasks:" → "## Tasks" · "## tasks" → "## Tasks"
		//   "# Notes" (unknown, level 1) → "## Notes" in Other
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// TODO: mustCleanup; exactly one fix, equal to wantFix; tt.check(o)
		})
	}
}

// Test_Unit_Cleanup_NotHeadings: "#hashtag", "####### seven", and a "## Tasks" line inside a ``` or ~~~ fence stay
// text.
func Test_Unit_Cleanup_NotHeadings(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_UnknownSections: "## Notes" and "## References" go to Other as written, in file order, with no
// fix; a "### Details" subheading stays in the body of the section above it.
func Test_Unit_Cleanup_UnknownSections(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_BodyFixes: "-    instruction 1" → "- instruction 1"; an unindented line straight after a bullet
// is indented under it; a line after a blank line is left alone; nothing inside a ``` fence changes.
func Test_Unit_Cleanup_BodyFixes(t *testing.T) {
	// TODO: check the resulting Instructions text and the fixes (line numbers included)
}

// --- Cleanup: tasks ---

// Test_Unit_Cleanup_TaskHeadings: "## t2: x", "#### t2: x", "### T2 - x", "### Task 2: x" all become ID "t2",
// title "x", with a fix; "### t2: x" makes none; "### Test the API" is a task without an ID (gets one from AssignTaskIDs).
func Test_Unit_Cleanup_TaskHeadings(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_TaskBody: the depends line is read and fixed when not standard ("Depends:T1,t2",
// "depends on: task1, 3", "depends: t1 t2" → t1, t2 / t1, t3 / t1, t2); "depends: setup" keeps "setup" (for
// CheckComplete); body newlines are kept; a "#### Steps" subheading and bullets stay in the body.
func Test_Unit_Cleanup_TaskBody(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_BulletTasks: bullets under "## Tasks" become tasks t1, t2, … (title from the bullet, checkbox
// stripped, continuation lines as the body) with fixes; text before the first bullet is TaskNotes.
func Test_Unit_Cleanup_BulletTasks(t *testing.T) {
	// TODO
}

// Test_Unit_Cleanup_TaskIDs: missing IDs get the highest + 1 in file order, existing IDs never change, and each new
// ID is a fix on its heading's line.
func Test_Unit_Cleanup_TaskIDs(t *testing.T) {
	// TODO: "### t1", "### (none)", "### t5", "### (none)" → t1, t6, t5, t7; two fixes, line numbers right
}

// Test_Unit_AssignTaskIDs: the rule on its own (it is also used by plan mode's propose_objective).
func Test_Unit_AssignTaskIDs(t *testing.T) {
	tests := []struct {
		name        string
		ids         []string // "" = no ID
		want        []string
		wantChanged []int
	}{
		// TODO: none → t1,t2,t3 · all set (t1,t5,t2) → unchanged, nil · mixed (t1,"",t5,"") → t1,t6,t5,t7
		//   a non-"t<digits>" ID ("setup","") → kept, the empty one becomes t1 · empty slice → nil
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// TODO: build []Task from ids, AssignTaskIDs, compare IDs and the returned indexes (slices.Equal)
		})
	}
}

// Test_Unit_NormalizeTaskID: "t3", "T3", "task3", "Task 3", "3", "t03" → "t3"; "", "t", "setup", "t-1", "3a" → false.
func Test_Unit_NormalizeTaskID(t *testing.T) {
	// TODO: table
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
