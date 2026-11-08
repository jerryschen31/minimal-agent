package objective

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// [agent] the Cleanup tests (and mustCleanup, fileOf, checkFixes, checkTasks) are in cleanup_test.go.

// complete is a hand-built objective that passes CheckComplete and uses every field.
func complete() Objective {
	return Objective{
		Format:       FormatV1,
		ObjText:      "Build a tiny CLI that prints Fibonacci numbers.",
		Instructions: "- go test ./... passes.\n- Keep it small:\n  one file per concern.",
		Constraints:  "- Standard library only.",
		Context:      "The module path is example.com/fib.",
		TaskNotes:    "Work through these in order.",
		Tasks: []Task{
			{ID: "t1", Title: "Create the module", Body: "Run go mod init example.com/fib."},
			{ID: "t2", Title: "Write fib", Depends: []string{"t1"}, Body: "Write fib.go with fib(n int) []int."},
			{ID: "t3", Title: "Write the tests", Depends: []string{"t1", "t2"},
				Body: "Steps:\n\n- table-driven test\n- run it\n\n```go\nfunc TestFib(t *testing.T) {}\n```"},
		},
		Other: []Section{{"Notes", "Keep the README short."}, {"References", "- https://go.dev/doc/"}},
	}
}

// completeMarkdown is complete().Markdown(), written out by hand.
var completeMarkdown = strings.Join([]string{
	"---",
	"format: minagent-objective/v1",
	"---",
	"",
	"# Objective",
	"Build a tiny CLI that prints Fibonacci numbers.",
	"",
	"## Instructions",
	"- go test ./... passes.",
	"- Keep it small:",
	"  one file per concern.",
	"",
	"## Constraints",
	"- Standard library only.",
	"",
	"## Context",
	"The module path is example.com/fib.",
	"",
	"## Notes",
	"Keep the README short.",
	"",
	"## References",
	"- https://go.dev/doc/",
	"",
	"## Tasks",
	"Work through these in order.",
	"",
	"### t1: Create the module",
	"",
	"Run go mod init example.com/fib.",
	"",
	"### t2: Write fib",
	"depends: t1",
	"",
	"Write fib.go with fib(n int) []int.",
	"",
	"### t3: Write the tests",
	"depends: t1, t2",
	"",
	"Steps:",
	"",
	"- table-driven test",
	"- run it",
	"",
	"```go",
	"func TestFib(t *testing.T) {}",
	"```",
}, "\n") + "\n"

// checkRoundTrip fails the test unless Cleanup(o.Markdown()) makes no fix and gives back o.
func checkRoundTrip(t *testing.T, o Objective) {
	t.Helper()
	md := o.Markdown()
	o2, fixes, err := Cleanup([]byte(md))
	if err != nil {
		t.Fatalf("Cleanup(Markdown()): %v\nmarkdown:\n%s", err, md)
	}
	if len(fixes) != 0 {
		t.Errorf("Cleanup(Markdown()) made fixes: %v\nmarkdown:\n%s", fixes, md)
	}
	if !reflect.DeepEqual(o2, o) {
		t.Errorf("round trip changed the objective:\n got  %#v\n want %#v\nmarkdown:\n%s", o2, o, md)
	}
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
// zero fixes.
func Test_Unit_Markdown_RoundTrip(t *testing.T) {
	files, err := filepath.Glob("testdata/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata files (err %v)", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			o, _ := mustCleanup(t, string(data))
			checkRoundTrip(t, o)
		})
	}
	t.Run("complete", func(t *testing.T) { checkRoundTrip(t, complete()) })
	t.Run("complete, approved", func(t *testing.T) {
		o := complete()
		o.Approved = o.BodyHash()
		checkRoundTrip(t, o)
	})
	t.Run("empty Other section", func(t *testing.T) {
		o := complete()
		o.Other = []Section{{Heading: "Notes"}}
		checkRoundTrip(t, o)
	})
	t.Run("task notes only", func(t *testing.T) {
		checkRoundTrip(t, Objective{Format: FormatV1, ObjText: "x", TaskNotes: "No tasks yet."})
	})
}

// Test_Unit_Markdown_Layout: front matter first; sections in order; empty optional sections left out; a task with
// and without depends; the file ends with exactly one "\n".
func Test_Unit_Markdown_Layout(t *testing.T) {
	if got := complete().Markdown(); got != completeMarkdown {
		t.Errorf("Markdown():\n%s\nwant:\n%s", got, completeMarkdown)
	}

	o := Objective{ObjText: "x", Approved: "sha256:abc", Tasks: []Task{{ID: "t1", Title: "Only"}}}
	want := "---\nformat: minagent-objective/v1\napproved: sha256:abc\n---\n\n# Objective\nx\n\n## Tasks\n\n### t1: Only\n"
	if got := o.Markdown(); got != want {
		t.Errorf("minimal Markdown():\n%q\nwant:\n%q", got, want)
	}

	if got, want := (Objective{}).Markdown(), "---\nformat: minagent-objective/v1\n---\n\n# Objective\n"; got != want {
		t.Errorf("empty Markdown() = %q; want %q", got, want)
	}
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
		{"complete", nil, nil},
		{"no objective text", func(o *Objective) { o.ObjText = " \n" }, []string{`"# Objective" is missing or empty`}},
		{"no instructions", func(o *Objective) { o.Instructions = "" }, []string{`"## Instructions" is missing or empty`}},
		{"no tasks", func(o *Objective) { o.Tasks = nil }, []string{`"## Tasks" has no tasks`}},
		{"too many tasks", func(o *Objective) {
			o.Tasks = append(o.Tasks, Task{ID: "t4", Title: "a"}, Task{ID: "t5", Title: "b"})
		}, []string{"there are 5 tasks; the limit is 4"}},
		{"missing ID", func(o *Objective) { o.Tasks[0].ID = "" },
			[]string{`task 1 ("Create the module"): ID "" is not t<number>`}},
		{"non-numeric ID", func(o *Objective) { o.Tasks[0].ID = "setup" }, []string{`ID "setup" is not t<number>`}},
		{"duplicate ID", func(o *Objective) { o.Tasks[2].ID = "t1"; o.Tasks[2].Depends = nil },
			[]string{"task ID t1 is used by more than one task"}},
		{"blank title", func(o *Objective) { o.Tasks[1].Title = "  " }, []string{"task t2 has no title"}},
		{"depends on a missing task", func(o *Objective) { o.Tasks[1].Depends = []string{"t9"} },
			[]string{`task t2 depends on "t9", which is not a task in this file`}},
		{"depends on itself", func(o *Objective) { o.Tasks[1].Depends = []string{"t2"} },
			[]string{"task t2 depends on itself"}},
		{"cycle", func(o *Objective) { o.Tasks[0].Depends = []string{"t2"} },
			[]string{"in a loop (t1 → t2 → t1)"}},
		{"two problems at once", func(o *Objective) { o.Instructions = ""; o.Tasks = nil },
			[]string{`"## Instructions" is missing or empty`, `"## Tasks" has no tasks`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := complete()
			if tt.edit != nil {
				tt.edit(&o)
			}
			err := o.CheckComplete(maxTasks)
			if len(tt.wantSubs) == 0 {
				if err != nil {
					t.Errorf("CheckComplete = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckComplete = nil; want an error containing %q", tt.wantSubs)
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("CheckComplete = %v; want it to contain %q", err, sub)
				}
			}
		})
	}
}

// Test_Unit_CheckComplete_SelfDependencyIsNotACycle: depending on itself is reported once, not also as a loop.
func Test_Unit_CheckComplete_SelfDependencyIsNotACycle(t *testing.T) {
	o := complete()
	o.Tasks[0].Depends = []string{"t1"}
	err := o.CheckComplete(4)
	if err == nil || strings.Contains(err.Error(), "loop") {
		t.Errorf("CheckComplete = %v; want only the self-dependency", err)
	}
}

// Test_Unit_FindCycle: no cycle, a two-task cycle, a longer one reached from an earlier task, and missing tasks.
func Test_Unit_FindCycle(t *testing.T) {
	task := func(id string, deps ...string) Task { return Task{ID: id, Depends: deps} }
	tests := []struct {
		name  string
		tasks []Task
		want  []string
	}{
		{"none", []Task{task("t1"), task("t2", "t1"), task("t3", "t1", "t2")}, nil},
		{"two tasks", []Task{task("t1", "t2"), task("t2", "t1")}, []string{"t1", "t2", "t1"}},
		{"three tasks, entered from outside", []Task{task("t1", "t2"), task("t2", "t3"), task("t3", "t4"),
			task("t4", "t2")}, []string{"t2", "t3", "t4", "t2"}},
		{"missing and self dependencies are skipped", []Task{task("t1", "t9", "t1"), task("t2", "t1")}, nil},
	}
	for _, tt := range tests {
		if got := findCycle(tt.tasks); !slices.Equal(got, tt.want) {
			t.Errorf("%s: findCycle = %v; want %v", tt.name, got, tt.want)
		}
	}
}

// Test_Unit_Fix_String: with and without a line number; Was "" (added) and Now "" (ignored).
func Test_Unit_Fix_String(t *testing.T) {
	tests := []struct {
		fix  Fix
		want string
	}{
		{Fix{7, "### Constraints", "## Constraints"}, `line 7: "### Constraints" → "## Constraints"`},
		{Fix{2, "", "# Objective"}, `line 2: added "# Objective"`},
		{Fix{3, "name: fib", ""}, `line 3: ignored "name: fib"`},
		{Fix{0, "a", "b"}, `"a" → "b"`},
	}
	for _, tt := range tests {
		if got := tt.fix.String(); got != tt.want {
			t.Errorf("%#v.String() = %q; want %q", tt.fix, got, tt.want)
		}
	}
}

// --- Approval ---

// Test_Unit_BodyHash: the same for objectives that differ only in front matter (Format, Approved); different when
// any section or task changes; stable for a file and its cleaned-up copy (a cosmetic fix is not an edit).
func Test_Unit_BodyHash(t *testing.T) {
	base := complete()
	h := base.BodyHash()
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("BodyHash = %q; want sha256:<64 hex digits>", h)
	}

	frontOnly := complete()
	frontOnly.Format, frontOnly.Approved = "", "sha256:whatever"
	if frontOnly.BodyHash() != h {
		t.Error("the hash changed with the front matter")
	}

	for name, edit := range map[string]func(o *Objective){
		"objective":   func(o *Objective) { o.ObjText += "!" },
		"other":       func(o *Objective) { o.Other[0].Body = "changed" },
		"task body":   func(o *Objective) { o.Tasks[0].Body = "changed" },
		"task depend": func(o *Objective) { o.Tasks[2].Depends = []string{"t2"} },
	} {
		o := complete()
		edit(&o)
		if o.BodyHash() == h {
			t.Errorf("%s: the hash did not change", name)
		}
	}

	tidy, _ := mustCleanup(t, fileOf("# Objective", "x", "", "## Instructions", "- a"))
	messy, fixes := mustCleanup(t, fileOf("#Objective", "x", "", "### Instruction", "-   a"))
	if len(fixes) == 0 || tidy.BodyHash() != messy.BodyHash() {
		t.Errorf("a cosmetic difference changed the hash (fixes %v)", fixes)
	}
}

// Test_Unit_ApprovalStatus: no Approved → unplanned; Approved = BodyHash() → approved; then change a task's Body →
// edited; a malformed Approved ("sha256:zz") → edited.
func Test_Unit_ApprovalStatus(t *testing.T) {
	o := complete()
	if got := o.ApprovalStatus(); got != ApprovalUnplanned {
		t.Errorf("no Approved: %q; want %q", got, ApprovalUnplanned)
	}
	o.Approved = o.BodyHash()
	if got := o.ApprovalStatus(); got != ApprovalApproved {
		t.Errorf("Approved = BodyHash(): %q; want %q", got, ApprovalApproved)
	}

	saved, _ := mustCleanup(t, o.Markdown())
	if got := saved.ApprovalStatus(); got != ApprovalApproved {
		t.Errorf("after writing and reading back: %q; want %q", got, ApprovalApproved)
	}

	o.Tasks[0].Body = "edited by hand"
	if got := o.ApprovalStatus(); got != ApprovalEdited {
		t.Errorf("after an edit: %q; want %q", got, ApprovalEdited)
	}
	o.Approved = "sha256:zz"
	if got := o.ApprovalStatus(); got != ApprovalEdited {
		t.Errorf("malformed Approved: %q; want %q", got, ApprovalEdited)
	}
}

// --- Documentation ---

// Test_Unit_ExampleFile: examples/objective-example.md is in the standard form (zero fixes), ready to run, and round-
// trips, so the example people copy from stays valid.
func Test_Unit_ExampleFile(t *testing.T) {
	data, err := os.ReadFile("../examples/objective-example.md")
	if err != nil {
		t.Fatal(err)
	}
	o, fixes := mustCleanup(t, string(data))
	checkFixes(t, fixes, nil)
	if err := o.CheckComplete(20); err != nil {
		t.Errorf("CheckComplete: %v", err)
	}
	if got := o.Markdown(); got != string(data) {
		t.Errorf("the example is not exactly what Markdown writes:\n%s", got)
	}
}

// Test_Unit_DocHandWrittenExample: the before/after example in docs/objective-format.md is what Cleanup does.
func Test_Unit_DocHandWrittenExample(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf(
		"#Objective",
		"Build a tiny Go program that prints the first 10 Fibonacci numbers.",
		"",
		"## Task",
		"- Write fib.go and run it with go run .",
		"- Write fib_test.go and run go test ./...",
		"",
		"## Instruction",
		"-   go test ./... passes and go run . prints 0 1 1 2 3 5 8 13 21 34,",
		"one per line.",
	))
	want := fileOf(
		"# Objective",
		"Build a tiny Go program that prints the first 10 Fibonacci numbers.",
		"",
		"## Instructions",
		"- go test ./... passes and go run . prints 0 1 1 2 3 5 8 13 21 34,",
		"  one per line.",
		"",
		"## Tasks",
		"",
		"### t1: Write fib.go and run it with go run .",
		"",
		"### t2: Write fib_test.go and run go test ./...",
	)
	if got := o.body(); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
	if len(fixes) != 7 {
		t.Errorf("%d fixes; the doc says 7: %v", len(fixes), fixes)
	}
}

// --- Fuzzing ---

// FuzzCleanup: for any input, Cleanup never panics, and when it succeeds, Cleanup(o.Markdown()) succeeds with zero
// fixes and gives back o. Run: go test ./objective -run XXX -fuzz FuzzCleanup -fuzztime 30s
func FuzzCleanup(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.md")
	for _, p := range files {
		if data, err := os.ReadFile(p); err == nil {
			f.Add(data)
		}
	}
	for _, s := range []string{
		completeMarkdown,
		"",
		"# Objective\nx\n",
		"Build it.\n## Tasks\n- a\n  more\n- [x] b\n### t5: c\ndepends on: task1, 3\n",
		"## Context\n```\nunclosed\n## Tasks\n### t1: x\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		o, fixes, err := Cleanup(data)
		if err != nil {
			return // rejecting an input is fine; panicking is not
		}
		for _, x := range fixes {
			if x.Was == x.Now {
				t.Fatalf("a fix that changes nothing: %#v", x)
			}
		}
		checkRoundTrip(t, o)
	})
}
