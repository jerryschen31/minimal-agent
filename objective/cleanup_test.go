package objective

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// [agent] testdata/: objective-fib.md is hand-written with mistakes Cleanup fixes; objective-deepwiki.md is in the
// standard layout (front matter, "### tN:" tasks, depends, an unknown "## Notes" section) and cleans up with zero
// fixes; the two objective-build-* files are drafts without tasks.

// fileOf joins its arguments into a file, one per line, ending in "\n", so a test can count line numbers.
func fileOf(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// mustCleanup cleans up s and fails the test on an error.
func mustCleanup(t *testing.T, s string) (Objective, []Fix) {
	t.Helper()
	o, fixes, err := Cleanup([]byte(s))
	if err != nil {
		t.Fatalf("Cleanup: %v\ninput:\n%s", err, s)
	}
	return o, fixes
}

// checkFixes fails the test unless got equals want, listing both.
func checkFixes(t *testing.T, got, want []Fix) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("fixes:\n got  %#v\n want %#v", got, want)
	}
}

// checkTasks fails the test unless got equals want (deep equality, so nil and empty slices differ).
func checkTasks(t *testing.T, got, want []Task) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tasks:\n got  %#v\n want %#v", got, want)
	}
}

// --- Cleanup: reading and errors ---

// Test_Unit_Cleanup_MinimalDraft: only "# Objective" cleans up with no fixes; Format is FormatV1; the rest is empty.
func Test_Unit_Cleanup_MinimalDraft(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("# Objective", "Build a thing."))
	checkFixes(t, fixes, nil)
	want := Objective{Format: FormatV1, ObjText: "Build a thing."}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("got %#v\nwant %#v", o, want)
	}
}

// Test_Unit_Cleanup_NoHeadings: a file of plain text becomes the Objective, with one fix (Now "# Objective").
func Test_Unit_Cleanup_NoHeadings(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("", "Build a CLI.", "It should be fast."))
	if o.ObjText != "Build a CLI.\nIt should be fast." {
		t.Errorf("ObjText = %q", o.ObjText)
	}
	checkFixes(t, fixes, []Fix{{Line: 2, Now: "# Objective"}})
}

// Test_Unit_Cleanup_DraftWithoutObjectiveHeading: opening text plus other sections; the text becomes the Objective.
func Test_Unit_Cleanup_DraftWithoutObjectiveHeading(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("Build a CLI.", "", "## Constraints", "- Go only"))
	if o.ObjText != "Build a CLI." || o.Constraints != "- Go only" {
		t.Errorf("ObjText = %q, Constraints = %q", o.ObjText, o.Constraints)
	}
	checkFixes(t, fixes, []Fix{{Line: 1, Now: "# Objective"}})
}

// Test_Unit_Cleanup_Errors: what Cleanup can't decide is an error naming the problem.
func Test_Unit_Cleanup_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string // a substring the error must contain, so the test fails if the wrong check fires
	}{
		{"empty", "", "empty"},
		{"whitespace only", " \n\t\n", "empty"},
		{"only a byte-order mark", "\xef\xbb\xbf", "empty"},
		{"too big", strings.Repeat("x", MaxBytes+1), "the limit is"},
		{"not UTF-8", "# Objective\n\xff\xfe\n", "not UTF-8"},
		{"two Objective sections", fileOf("# Objective", "a", "# Objective", "b"),
			`line 3: a second "# Objective" section (the first is on line 1)`},
		{"two Instructions sections (singular and level 3)",
			fileOf("# Objective", "x", "## Instruction", "a", "### Instructions", "b"),
			`line 5: a second "## Instructions" section (the first is on line 3)`},
		{"duplicate task ID", fileOf("# Objective", "x", "## Tasks", "### t1: a", "### t1: b"),
			"line 5: task ID t1 is used twice (first on line 4)"},
		{"duplicate task ID after normalizing", fileOf("# Objective", "x", "## Tasks", "### t1: a", "### Task 01: b"),
			"line 5: task ID t1 is used twice"},
		{"text before the first heading", fileOf("Note to self", "# Objective", "x"),
			"line 1: text before the first heading"},
		{"task heading under Instructions", fileOf("# Objective", "x", "## Instructions", "### t1: x"),
			`line 4: "### t1: x" is a task heading outside "## Tasks"`},
		{"task heading before Tasks", fileOf("# Objective", "x", "## Task 2: x", "## Tasks"),
			`line 3: "## Task 2: x" is a task heading outside "## Tasks"`},
		{"front matter: unknown format", fileOf("---", "format: minagent-objective/v9", "---", "# Objective", "x"),
			`line 2: unknown format "minagent-objective/v9"; supported: minagent-objective/v1`},
		{"front matter: line without a colon", fileOf("---", "format", "---", "# Objective", "x"),
			`line 2: front matter line "format" is not "key: value"`},
		{"front matter: no closing line", fileOf("---", "format: minagent-objective/v1", "approved: x"),
			`line 1: front matter has no closing "---"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Cleanup([]byte(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("err = %v; want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

// Test_Unit_Cleanup_FrontMatter: no front matter or no format line → FormatV1 and no fix; "format: …v1" and
// "approved: sha256:…" read as is; an unknown key ("name: x") is one fix with Now "" and is not kept.
func Test_Unit_Cleanup_FrontMatter(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantApproved string
		wantFixes    []Fix
	}{
		{"none", fileOf("# Objective", "x"), "", nil},
		{"format", fileOf("---", "format: minagent-objective/v1", "---", "# Objective", "x"), "", nil},
		{"empty block", fileOf("---", "---", "# Objective", "x"), "", nil},
		{"blank lines before it", fileOf("", "", "---", "format: minagent-objective/v1", "---", "# Objective", "x"), "", nil},
		{"key case and spacing", fileOf("---", "  Format :  minagent-objective/v1  ", "---", "# Objective", "x"), "", nil},
		{"approved", fileOf("---", "format: minagent-objective/v1", "approved: sha256:abc", "---", "# Objective", "x"),
			"sha256:abc", nil},
		{"unknown key", fileOf("---", "format: minagent-objective/v1", "name: fib", "---", "# Objective", "x"),
			"", []Fix{{Line: 3, Was: "name: fib"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, fixes := mustCleanup(t, tt.input)
			if o.Format != FormatV1 || o.Approved != tt.wantApproved || o.ObjText != "x" {
				t.Errorf("Format %q, Approved %q, ObjText %q; want %q, %q, \"x\"", o.Format, o.Approved, o.ObjText,
					FormatV1, tt.wantApproved)
			}
			checkFixes(t, fixes, tt.wantFixes)
		})
	}
}

// Test_Unit_Cleanup_BOM: a file starting with a UTF-8 byte-order mark reads the same as without it (front matter
// and "# Objective" found), with no fix.
func Test_Unit_Cleanup_BOM(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	for _, input := range []string{
		bom + fileOf("---", "format: minagent-objective/v1", "approved: sha256:abc", "---", "# Objective", "x"),
		bom + fileOf("# Objective", "x"),
	} {
		o, fixes := mustCleanup(t, input)
		if o.ObjText != "x" || o.Format != FormatV1 {
			t.Errorf("input %q: ObjText %q, Format %q", input, o.ObjText, o.Format)
		}
		checkFixes(t, fixes, nil)
	}
}

// Test_Unit_Cleanup_CRLF: Windows ("\r\n") and old Mac ("\r") line endings read the same as "\n", with no fix.
func Test_Unit_Cleanup_CRLF(t *testing.T) {
	o, fixes := mustCleanup(t, "# Objective\r\nx\r\n\r\n## Instructions\r- a\r\n")
	if o.ObjText != "x" || o.Instructions != "- a" {
		t.Errorf("ObjText %q, Instructions %q", o.ObjText, o.Instructions)
	}
	checkFixes(t, fixes, nil)
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
		{"## Objective", fileOf("## Objective", "x"),
			Fix{1, "## Objective", "# Objective"}, func(o Objective) bool { return o.ObjText == "x" }},
		{"#Objective", fileOf("#Objective", "x"),
			Fix{1, "#Objective", "# Objective"}, func(o Objective) bool { return o.ObjText == "x" }},
		{"### Constraints", fileOf("# Objective", "x", "### Constraints", "- go"),
			Fix{3, "### Constraints", "## Constraints"}, func(o Objective) bool { return o.Constraints == "- go" }},
		{"## Instruction", fileOf("# Objective", "x", "## Instruction", "- a"),
			Fix{3, "## Instruction", "## Instructions"}, func(o Objective) bool { return o.Instructions == "- a" }},
		{"## Tasks:", fileOf("# Objective", "x", "## Tasks:", "### t1: a"),
			Fix{3, "## Tasks:", "## Tasks"}, func(o Objective) bool { return len(o.Tasks) == 1 }},
		{"## tasks", fileOf("# Objective", "x", "## tasks", "### t1: a"),
			Fix{3, "## tasks", "## Tasks"}, func(o Objective) bool { return len(o.Tasks) == 1 }},
		{"leading spaces", fileOf("# Objective", "x", "  ## Context", "c"),
			Fix{3, "  ## Context", "## Context"}, func(o Objective) bool { return o.Context == "c" }},
		{"# Notes (unknown, level 1)", fileOf("# Objective", "x", "# Notes", "n"),
			Fix{3, "# Notes", "## Notes"},
			func(o Objective) bool { return slices.Equal(o.Other, []Section{{"Notes", "n"}}) }},
		{"## Notes ## (closing hashes)", fileOf("# Objective", "x", "## Notes ##", "n"),
			Fix{3, "## Notes ##", "## Notes"},
			func(o Objective) bool { return slices.Equal(o.Other, []Section{{"Notes", "n"}}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, fixes := mustCleanup(t, tt.input)
			checkFixes(t, fixes, []Fix{tt.wantFix})
			if !tt.check(o) {
				t.Errorf("section not where expected: %#v", o)
			}
		})
	}
}

// Test_Unit_Cleanup_NotHeadings: "#hashtag", "####### seven", a 4-space indented "#", and headings inside a ``` or
// ~~~ fence stay text.
func Test_Unit_Cleanup_NotHeadings(t *testing.T) {
	body := []string{
		"x",
		"#hashtag",
		"####### seven",
		"    # indented code",
		"```",
		"## Tasks",
		"- not a task",
		"```",
		"~~~",
		"### t1: not a task either",
		"~~~",
	}
	o, fixes := mustCleanup(t, fileOf(append([]string{"# Objective"}, body...)...))
	checkFixes(t, fixes, nil)
	if o.ObjText != strings.Join(body, "\n") {
		t.Errorf("ObjText = %q", o.ObjText)
	}
	if o.Tasks != nil || o.Other != nil {
		t.Errorf("Tasks %#v, Other %#v; want none", o.Tasks, o.Other)
	}
}

// Test_Unit_Cleanup_UnknownSections: "## Notes" and "## References" go to Other as written, in file order, with no
// fix; a "### Details" subheading stays in the body of the section above it.
func Test_Unit_Cleanup_UnknownSections(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Notes", "a note", "### Details", "more",
		"## References", "- a link"))
	checkFixes(t, fixes, nil)
	want := []Section{{"Notes", "a note\n### Details\nmore"}, {"References", "- a link"}}
	if !slices.Equal(o.Other, want) {
		t.Errorf("Other = %#v; want %#v", o.Other, want)
	}
	if o.Instructions != "" || o.Constraints != "" || o.Context != "" {
		t.Errorf("a known section was filled: %#v", o)
	}
}

// Test_Unit_Cleanup_BodyFixes: "-    instruction 1" → "- instruction 1"; an unindented line straight after a bullet
// is indented under it; a line after a blank line is left alone; nothing inside a ``` fence changes.
func Test_Unit_Cleanup_BodyFixes(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf(
		"# Objective", // 1
		"x",
		"## Instructions",
		"-    instruction 1", // 4
		"- instruction 2",
		"extra details", // 6
		"",
		"A new paragraph.",
		"1.   numbered", // 9
		"   - nested    ",
		"```",
		"-    in a fence",
		"```",
	))
	want := strings.Join([]string{
		"- instruction 1",
		"- instruction 2",
		"  extra details",
		"",
		"A new paragraph.",
		"1. numbered",
		"   - nested    ",
		"```",
		"-    in a fence",
		"```",
	}, "\n")
	if o.Instructions != want {
		t.Errorf("Instructions:\n%s\nwant:\n%s", o.Instructions, want)
	}
	checkFixes(t, fixes, []Fix{
		{4, "-    instruction 1", "- instruction 1"},
		{6, "extra details", "  extra details"},
		{9, "1.   numbered", "1. numbered"},
	})
}

// Test_Unit_Cleanup_BodyNotContinuation: a subheading, quote or table line straight after a bullet is not indented
// under it.
func Test_Unit_Cleanup_BodyNotContinuation(t *testing.T) {
	body := []string{"- a", "### Details", "- b", "> a quote", "- c", "| a | table |"}
	o, fixes := mustCleanup(t, fileOf(append([]string{"# Objective", "x", "## Instructions"}, body...)...))
	checkFixes(t, fixes, nil)
	if o.Instructions != strings.Join(body, "\n") {
		t.Errorf("Instructions = %q", o.Instructions)
	}
}

// Test_Unit_Cleanup_UnclosedFence: a code fence left open at the end of a section is closed, with one fix; the
// headings after it in the file were already part of the code.
func Test_Unit_Cleanup_UnclosedFence(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Context", "```", "code", "## Tasks", "### t1: a", ""))
	if o.Context != "```\ncode\n## Tasks\n### t1: a\n```" || o.Tasks != nil {
		t.Errorf("Context %q, Tasks %#v", o.Context, o.Tasks)
	}
	checkFixes(t, fixes, []Fix{{Line: 7, Now: "```"}})
}

// --- Cleanup: tasks ---

// Test_Unit_Cleanup_TaskHeadings: "## t2: x", "#### t2: x", "### T2 - x", "### Task 2: x" all become ID "t2",
// title "x", with a fix; "### t2: x" makes none; "### Test the API" is a task without an ID (gets one from AssignTaskIDs).
func Test_Unit_Cleanup_TaskHeadings(t *testing.T) {
	tests := []struct {
		heading string
		want    Task
		fixed   bool // a fix is expected on the heading line
	}{
		{"### t2: x", Task{ID: "t2", Title: "x", Body: "body"}, false},
		{"## t2: x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"#### t2: x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"### T2 - x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"### t2. x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"### Task 2: x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"###t2: x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"### t02: x", Task{ID: "t2", Title: "x", Body: "body"}, true},
		{"### t2:", Task{ID: "t2", Body: "body"}, false},
		{"### Test the API", Task{ID: "t1", Title: "Test the API", Body: "body"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.heading, func(t *testing.T) {
			o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", tt.heading, "body"))
			checkTasks(t, o.Tasks, []Task{tt.want})
			var want []Fix
			if tt.fixed {
				want = []Fix{{Line: 4, Was: tt.heading, Now: taskHeadingLine(tt.want)}}
			}
			checkFixes(t, fixes, want)
		})
	}
}

// Test_Unit_Cleanup_Depends: the depends line is read and fixed when not standard; "depends: setup" keeps "setup"
// (for CheckComplete); an empty "depends:" is not a depends line and stays in the body.
func Test_Unit_Cleanup_Depends(t *testing.T) {
	tests := []struct {
		line string
		want []string
		now  string // the fixed line; "" with fixed = true means dropped
		fix  bool
	}{
		{"depends: t1, t2", []string{"t1", "t2"}, "", false},
		{"Depends:T1,t2", []string{"t1", "t2"}, "depends: t1, t2", true},
		{"depends on: task1, 3", []string{"t1", "t3"}, "depends: t1, t3", true},
		{"Depends On : Task 1", []string{"t1"}, "depends: t1", true},
		{"depends: t1 t2", []string{"t1", "t2"}, "depends: t1, t2", true},
		{"  depends: t1", []string{"t1"}, "depends: t1", true},
		{"depends: setup", []string{"setup"}, "", false},
		{"depends: t1 setup", []string{"t1 setup"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", "### t3: x", tt.line, "", "body"))
			checkTasks(t, o.Tasks, []Task{{ID: "t3", Title: "x", Depends: tt.want, Body: "body"}})
			var want []Fix
			if tt.fix {
				want = []Fix{{Line: 5, Was: tt.line, Now: tt.now}}
			}
			checkFixes(t, fixes, want)
		})
	}

	t.Run("dedenting would create a heading", func(t *testing.T) {
		o, _ := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", "- a", "    # not a heading", "    more"))
		checkTasks(t, o.Tasks, []Task{{ID: "t1", Title: "a", Body: "    # not a heading\n    more"}})
	})

	t.Run("closing hashes in a bullet title", func(t *testing.T) {
		o, _ := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", "- Ship it ##", "- #", "- C#"))
		checkTasks(t, o.Tasks, []Task{{ID: "t1", Title: "Ship it"}, {ID: "t2"}, {ID: "t3", Title: "C#"}})
	})

	t.Run("empty depends stays in the body", func(t *testing.T) {
		o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", "### t3: x", "depends:", "", "depends:"))
		checkTasks(t, o.Tasks, []Task{{ID: "t3", Title: "x", Body: "depends:\n\ndepends:"}})
		checkFixes(t, fixes, nil)
	})
}

// Test_Unit_Cleanup_TaskBody: body newlines are kept; a "#### Steps" subheading, bullets and a fence (even one holding
// a task heading) stay in the body; a depends line further down is body text.
func Test_Unit_Cleanup_TaskBody(t *testing.T) {
	body := []string{
		"First paragraph.",
		"",
		"#### Steps",
		"- step one",
		"- step two",
		"",
		"depends: t9",
		"",
		"```go",
		"### t9: not a task",
		"```",
	}
	o, fixes := mustCleanup(t, fileOf(append([]string{"# Objective", "x", "## Tasks", "### t1: Build it"}, body...)...))
	checkFixes(t, fixes, nil)
	checkTasks(t, o.Tasks, []Task{{ID: "t1", Title: "Build it", Body: strings.Join(body, "\n")}})
}

// Test_Unit_Cleanup_BulletTasks: bullets under "## Tasks" become tasks (title from the bullet, checkbox stripped,
// continuation lines dedented as the body), with one fix per bullet; text before the first bullet is TaskNotes;
// after the first "###" task, bullets are body text.
func Test_Unit_Cleanup_BulletTasks(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf(
		"# Objective", // 1
		"x",
		"## Tasks",
		"Work through these in order.",
		"- [ ] Create the module", // 5
		"  Run go mod init fib.",
		"-    Write the tests", // 7
		"* t5: Write the README",
		"  - mention go test",
		"### t9: Last",
		"- a bullet in t9's body",
	))
	if o.TaskNotes != "Work through these in order." {
		t.Errorf("TaskNotes = %q", o.TaskNotes)
	}
	checkTasks(t, o.Tasks, []Task{
		{ID: "t10", Title: "Create the module", Body: "Run go mod init fib."},
		{ID: "t11", Title: "Write the tests"},
		{ID: "t5", Title: "Write the README", Body: "- mention go test"},
		{ID: "t9", Title: "Last", Body: "- a bullet in t9's body"},
	})
	checkFixes(t, fixes, []Fix{
		{5, "- [ ] Create the module", "### t10: Create the module"},
		{7, "-    Write the tests", "### t11: Write the tests"},
		{8, "* t5: Write the README", "### t5: Write the README"},
	})
}

// Test_Unit_Cleanup_TaskIDs: missing IDs get the highest + 1 in file order, existing IDs never change, and each new
// ID is a fix on its heading's line.
func Test_Unit_Cleanup_TaskIDs(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", "### t1: a", "### b", "### t5: c", "### d"))
	checkTasks(t, o.Tasks, []Task{{ID: "t1", Title: "a"}, {ID: "t6", Title: "b"}, {ID: "t5", Title: "c"},
		{ID: "t7", Title: "d"}})
	checkFixes(t, fixes, []Fix{{5, "### b", "### t6: b"}, {7, "### d", "### t7: d"}})
}

// Test_Unit_Cleanup_UnknownHeadingEndsTasks: an unknown "##" heading after the tasks starts an Other section; a known
// section name at any level starts that section, even inside Tasks.
func Test_Unit_Cleanup_UnknownHeadingEndsTasks(t *testing.T) {
	o, fixes := mustCleanup(t, fileOf("# Objective", "x", "## Tasks", "### t1: a", "## Notes", "n",
		"### Context", "c"))
	checkTasks(t, o.Tasks, []Task{{ID: "t1", Title: "a"}})
	if !slices.Equal(o.Other, []Section{{"Notes", "n"}}) || o.Context != "c" {
		t.Errorf("Other %#v, Context %q", o.Other, o.Context)
	}
	checkFixes(t, fixes, []Fix{{7, "### Context", "## Context"}})
}

// Test_Unit_Cleanup_Testdata: the hand-written fib file gets every expected fix; the standard-layout and draft files
// get none.
func Test_Unit_Cleanup_Testdata(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("fib (hand-written)", func(t *testing.T) {
		o, fixes := mustCleanup(t, read("objective-fib.md"))
		var lines []int
		for _, f := range fixes {
			lines = append(lines, f.Line)
		}
		if want := []int{1, 4, 5, 6, 7, 9, 10, 12, 14}; !slices.Equal(lines, want) {
			t.Errorf("fix lines = %v; want %v\nfixes: %#v", lines, want, fixes)
		}
		var ids []string
		for _, task := range o.Tasks {
			ids = append(ids, task.ID)
		}
		if !slices.Equal(ids, []string{"t1", "t2", "t3"}) || o.Constraints == "" ||
			!strings.Contains(o.Instructions, "\n  one per line.") {
			t.Errorf("ids %v, Constraints %q, Instructions %q", ids, o.Constraints, o.Instructions)
		}
	})

	t.Run("deepwiki (standard layout)", func(t *testing.T) {
		o, fixes := mustCleanup(t, read("objective-deepwiki.md"))
		checkFixes(t, fixes, nil)
		if len(o.Tasks) != 2 || !slices.Equal(o.Tasks[1].Depends, []string{"t1"}) || len(o.Other) != 1 {
			t.Errorf("tasks %#v, Other %#v", o.Tasks, o.Other)
		}
	})

	for _, name := range []string{"objective-build-checksum-checker.md", "objective-build-minisearch.md"} {
		t.Run(name+" (draft)", func(t *testing.T) {
			o, fixes := mustCleanup(t, read(name))
			checkFixes(t, fixes, nil)
			if o.ObjText == "" || o.Instructions == "" || o.Tasks != nil {
				t.Errorf("ObjText %q, Instructions empty %v, Tasks %#v", o.ObjText, o.Instructions == "", o.Tasks)
			}
		})
	}
}

// --- helpers ---

// Test_Unit_ReadHeading: what counts as a heading, its level and text.
func Test_Unit_ReadHeading(t *testing.T) {
	tests := []struct {
		in        string
		wantLevel int
		wantText  string
		wantOK    bool
	}{
		{"# x", 1, "x", true},
		{"###### six", 6, "six", true},
		{"####### seven", 0, "", false},
		{"## Tasks ##", 2, "Tasks", true},
		{"### C#", 3, "C#", true},
		{"#\tTabbed", 1, "Tabbed", true},
		{"   # three spaces", 1, "three spaces", true},
		{"    # four spaces", 0, "", false},
		{"#Objective", 1, "Objective", true},
		{"#Objective ##", 1, "Objective", true},
		{"###t2: x", 3, "t2: x", true},
		{"#hashtag", 0, "", false},
		{"#", 0, "", false},
		{"# ", 0, "", false},
		{"## ##", 0, "", false},
		{"# # #", 0, "", false},
		{"## x # #", 2, "x", true},
		{"# ## x", 1, "## x", true},
		{"plain", 0, "", false},
	}
	for _, tt := range tests {
		level, text, ok := readHeading(tt.in)
		if level != tt.wantLevel || text != tt.wantText || ok != tt.wantOK {
			t.Errorf("readHeading(%q) = %d, %q, %v; want %d, %q, %v", tt.in, level, text, ok,
				tt.wantLevel, tt.wantText, tt.wantOK)
		}
	}
}

// Test_Unit_NormalizeTaskID: "t3", "T3", "task3", "Task 3", "3", "t03" → "t3"; "", "t", "setup", "t-1", "3a" → false.
func Test_Unit_NormalizeTaskID(t *testing.T) {
	for _, in := range []string{"t3", "T3", "task3", "Task 3", "TASK3", "3", "t03", " t3 "} {
		if got, ok := normalizeTaskID(in); !ok || got != "t3" {
			t.Errorf("normalizeTaskID(%q) = %q, %v; want \"t3\", true", in, got, ok)
		}
	}
	for _, in := range []string{"", "t", "task", "setup", "t-1", "3a", "t3x", "t 3 4"} {
		if got, ok := normalizeTaskID(in); ok {
			t.Errorf("normalizeTaskID(%q) = %q, true; want false", in, got)
		}
	}
}

// Test_Unit_TaskHeading: ID and title from heading text; text without an ID is all title.
func Test_Unit_TaskHeading(t *testing.T) {
	tests := []struct {
		in, wantID, wantTitle string
		wantHasID             bool
	}{
		{"t2: Build it", "t2", "Build it", true},
		{"T2 - Build it", "t2", "Build it", true},
		{"task 12) Build it", "t12", "Build it", true},
		{"t2 Build it", "t2", "Build it", true},
		{"t2", "t2", "", true},
		{"t1: 3 things", "t1", "3 things", true},
		{"t1: : odd", "t1", ": odd", true},
		{"t2:#", "t2", "", true},
		{"t2: C#", "t2", "C#", true},
		{"Test the API", "", "Test the API", false},
		{"Tasks", "", "Tasks", false},
		{"t2x title", "", "t2x title", false},
	}
	for _, tt := range tests {
		id, title, hasID := taskHeading(tt.in)
		if id != tt.wantID || title != tt.wantTitle || hasID != tt.wantHasID {
			t.Errorf("taskHeading(%q) = %q, %q, %v; want %q, %q, %v", tt.in, id, title, hasID,
				tt.wantID, tt.wantTitle, tt.wantHasID)
		}
	}
}
