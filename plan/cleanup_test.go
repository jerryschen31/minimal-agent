package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// [agent] testdata/: plan-standard.md is in the standard layout (front matter, every field, an unknown "## Notes"
// section) and cleans up with zero fixes; plan-messy.md is hand-edited with mistakes Cleanup fixes.

// fileOf joins its arguments into a file, one per line, ending in "\n", so a test can count line numbers.
func fileOf(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// mustCleanup cleans up s and fails the test on an error.
func mustCleanup(t *testing.T, s string) (Plan, []Fix) {
	t.Helper()
	p, fixes, err := Cleanup([]byte(s))
	if err != nil {
		t.Fatalf("Cleanup: %v\ninput:\n%s", err, s)
	}
	return p, fixes
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

// tasksFile puts lines under "## Tasks" after a title, so task tests start at line 3.
func tasksFile(ls ...string) string {
	return fileOf(append([]string{"# Plan: x", "## Tasks"}, ls...)...)
}

// --- Cleanup: reading and errors ---

// - Verify a title-only plan cleans up with no fixes; Format is FormatV1; the rest is empty
func Test_Unit_Cleanup_Minimal(t *testing.T) {
	p, fixes := mustCleanup(t, fileOf("# Plan: Build a thing", "In two steps."))
	checkFixes(t, fixes, nil)
	want := Plan{Format: FormatV1, Title: "Build a thing", Summary: "In two steps."}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %#v\nwant %#v", p, want)
	}
}

// - Verify what Cleanup can't decide is an error naming the problem
func Test_Unit_Cleanup_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string // a substring the error must contain, so the test fails if the wrong check fires
	}{
		{"empty", "", "plan file is empty"},
		{"only a byte-order mark", "\xef\xbb\xbf", "empty"},
		{"not UTF-8", "# Plan\n\xff\xfe\n", "not UTF-8"},
		{"two titles", fileOf("# Plan: a", "# Plan: b"), `line 2: a second "# Plan" section (the first is on line 1)`},
		{"two Tasks sections (singular and level 3)", fileOf("# Plan", "## Task", "### Tasks"),
			`line 3: a second "## Tasks" section (the first is on line 2)`},
		{"duplicate task ID", tasksFile("### t1: a", "### t1: b"), "line 4: task ID t1 is used twice (first on line 3)"},
		{"duplicate task ID after normalizing", tasksFile("### t1: a", "### Task 01: b"), "line 4: task ID t1 is used twice"},
		{"text before the first heading", fileOf("Note to self", "# Plan: x"), "line 1: text before the first heading"},
		{"text before the first heading, after front matter", fileOf("---", "---", "stray", "# Plan"),
			"line 3: text before the first heading"},
		{"task heading under Assumptions", fileOf("# Plan", "## Assumptions", "### t1: x"),
			`line 3: "### t1: x" is a task heading outside "## Tasks"`},
		{"front matter: unknown format", fileOf("---", "format: minagent-plan/v9", "---", "# Plan"),
			`line 2: unknown format "minagent-plan/v9"; supported: minagent-plan/v1`},
		{"front matter: an objective format", fileOf("---", "format: minagent-objective/v1", "---", "# Plan"),
			`unknown format "minagent-objective/v1"`},
		{"front matter: bad plan_version", fileOf("---", "plan_version: two", "---", "# Plan"),
			`line 2: plan_version "two" is not a whole number`},
		{"front matter: negative plan_version", fileOf("---", "plan_version: -1", "---", "# Plan"),
			`plan_version "-1" is not a whole number`},
		{"front matter: line without a colon", fileOf("---", "format", "---", "# Plan"),
			`line 2: front matter line "format" is not "key: value"`},
		{"front matter: no closing line", fileOf("---", "format: minagent-plan/v1"),
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

// - Verify the front matter keys are read (any case and spacing), no front matter means FormatV1, and an unknown key
// is one fix with Now "" and is not kept
func Test_Unit_Cleanup_FrontMatter(t *testing.T) {
	p, fixes := mustCleanup(t, fileOf("", "---", "  Format :  minagent-plan/v1  ", "objective: obj.md",
		"OBJECTIVE_HASH: sha256:abc", "plan_version: 007", "name: fib", "---", "# Plan: x"))
	want := Plan{Format: FormatV1, ObjectivePath: "obj.md", ObjectiveHash: "sha256:abc", Version: 7, Title: "x"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %#v\nwant %#v", p, want)
	}
	checkFixes(t, fixes, []Fix{{Line: 7, Was: "name: fib"}})

	for _, input := range []string{fileOf("# Plan: x"), fileOf("---", "---", "# Plan: x")} {
		p, fixes := mustCleanup(t, input)
		if p.Format != FormatV1 || p.Version != 0 || len(fixes) != 0 {
			t.Errorf("%q: Format %q, Version %d, fixes %v", input, p.Format, p.Version, fixes)
		}
	}
}

// - Verify Windows ("\r\n") and old Mac ("\r") line endings and a byte-order mark read the same as "\n", with no fix
func Test_Unit_Cleanup_LineEndingsAndBOM(t *testing.T) {
	p, fixes := mustCleanup(t, "\xef\xbb\xbf# Plan: x\r\n\r\n## Assumptions\r- a\r\n")
	if p.Title != "x" || p.Assumptions != "- a" {
		t.Errorf("Title %q, Assumptions %q", p.Title, p.Assumptions)
	}
	checkFixes(t, fixes, nil)
}

// --- Cleanup: the fixes ---

// - Verify each heading mistake fills the right field and makes exactly one fix
func Test_Unit_Cleanup_HeadingFixes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantFix Fix
		check   func(Plan) bool // the field the section went to
	}{
		{"## Plan: x", fileOf("## Plan: x"), Fix{1, "## Plan: x", "# Plan: x"},
			func(p Plan) bool { return p.Title == "x" }},
		{"#Plan: x", fileOf("#Plan: x"), Fix{1, "#Plan: x", "# Plan: x"}, func(p Plan) bool { return p.Title == "x" }},
		{"# plan:   x", fileOf("# plan:   x"), Fix{1, "# plan:   x", "# Plan: x"},
			func(p Plan) bool { return p.Title == "x" }},
		{"# Plan: (no title)", fileOf("# Plan:"), Fix{1, "# Plan:", "# Plan"}, func(p Plan) bool { return p.Title == "" }},
		{"# Plan: # (only hashes)", fileOf("# Plan:#"), Fix{1, "# Plan:#", "# Plan"},
			func(p Plan) bool { return p.Title == "" }},
		{"### Assumption", fileOf("# Plan", "### Assumption", "- a"), Fix{2, "### Assumption", "## Assumptions"},
			func(p Plan) bool { return p.Assumptions == "- a" }},
		{"## Tasks:", fileOf("# Plan", "## Tasks:", "### t1: a"), Fix{2, "## Tasks:", "## Tasks"},
			func(p Plan) bool { return len(p.Tasks) == 1 }},
		{"## tasks", fileOf("# Plan", "## tasks", "### t1: a"), Fix{2, "## tasks", "## Tasks"},
			func(p Plan) bool { return len(p.Tasks) == 1 }},
		{"leading spaces", fileOf("# Plan", "  ## Assumptions", "a"), Fix{2, "  ## Assumptions", "## Assumptions"},
			func(p Plan) bool { return p.Assumptions == "a" }},
		{"# Notes (unknown, level 1)", fileOf("# Plan", "# Notes", "n"), Fix{2, "# Notes", "## Notes"},
			func(p Plan) bool { return slices.Equal(p.Other, []Section{{"Notes", "n"}}) }},
		{"## Notes ## (closing hashes)", fileOf("# Plan", "## Notes ##", "n"), Fix{2, "## Notes ##", "## Notes"},
			func(p Plan) bool { return slices.Equal(p.Other, []Section{{"Notes", "n"}}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fixes := mustCleanup(t, tt.input)
			checkFixes(t, fixes, []Fix{tt.wantFix})
			if !tt.check(p) {
				t.Errorf("section not where expected: %#v", p)
			}
		})
	}
}

// - Verify "#hashtag", "####### seven", a 4-space indented "#", "# Planning" and headings inside a ``` or ~~~ fence
// stay text
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
	p, fixes := mustCleanup(t, fileOf(append([]string{"# Plan: x"}, body...)...))
	checkFixes(t, fixes, nil)
	if p.Summary != strings.Join(body, "\n") {
		t.Errorf("Summary = %q", p.Summary)
	}
	if p.Tasks != nil || p.Other != nil {
		t.Errorf("Tasks %#v, Other %#v; want none", p.Tasks, p.Other)
	}
}

// - Verify "# Planning" and "## Plan for X" are not the title heading: they are unknown sections
func Test_Unit_Cleanup_PlanLookalikes(t *testing.T) {
	p, fixes := mustCleanup(t, fileOf("# Plan: x", "## Planning", "a", "## Plan for later", "b"))
	checkFixes(t, fixes, nil)
	if !slices.Equal(p.Other, []Section{{"Planning", "a"}, {"Plan for later", "b"}}) || p.Title != "x" {
		t.Errorf("Title %q, Other %#v", p.Title, p.Other)
	}
}

// - Verify unknown "##" sections go to Other as written, in file order, with no fix, and a "### Details" subheading
// stays in the body of the section above it
func Test_Unit_Cleanup_UnknownSections(t *testing.T) {
	p, fixes := mustCleanup(t, fileOf("# Plan: x", "## Notes", "a note", "### Details", "more", "## References", "- a link"))
	checkFixes(t, fixes, nil)
	want := []Section{{"Notes", "a note\n### Details\nmore"}, {"References", "- a link"}}
	if !slices.Equal(p.Other, want) {
		t.Errorf("Other = %#v; want %#v", p.Other, want)
	}
}

// - Verify bullet spacing and continuation fixes: "-    a" → "- a"; an unindented line straight after a bullet is
// indented under it; a line after a blank line, a subheading, quote or table line, and anything inside a fence is left
// alone
func Test_Unit_Cleanup_BodyFixes(t *testing.T) {
	p, fixes := mustCleanup(t, fileOf(
		"# Plan: x", // 1
		"## Assumptions",
		"-    assumption 1", // 3
		"- assumption 2",
		"extra details", // 5
		"",
		"A new paragraph.",
		"1.   numbered", // 8
		"- b",
		"> a quote",
		"- c",
		"| a | table |",
		"```",
		"-    in a fence",
		"```",
	))
	want := strings.Join([]string{
		"- assumption 1",
		"- assumption 2",
		"  extra details",
		"",
		"A new paragraph.",
		"1. numbered",
		"- b",
		"> a quote",
		"- c",
		"| a | table |",
		"```",
		"-    in a fence",
		"```",
	}, "\n")
	if p.Assumptions != want {
		t.Errorf("Assumptions:\n%s\nwant:\n%s", p.Assumptions, want)
	}
	checkFixes(t, fixes, []Fix{
		{3, "-    assumption 1", "- assumption 1"},
		{5, "extra details", "  extra details"},
		{8, "1.   numbered", "1. numbered"},
	})
}

// - Verify a code fence left open at the end of a section is closed, with one fix; the headings after it were already
// part of the code
func Test_Unit_Cleanup_UnclosedFence(t *testing.T) {
	p, fixes := mustCleanup(t, fileOf("# Plan: x", "## Assumptions", "```", "code", "## Tasks", "### t1: a", ""))
	if p.Assumptions != "```\ncode\n## Tasks\n### t1: a\n```" || p.Tasks != nil {
		t.Errorf("Assumptions %q, Tasks %#v", p.Assumptions, p.Tasks)
	}
	checkFixes(t, fixes, []Fix{{Line: 6, Now: "```"}})
}

// --- Cleanup: tasks ---

// - Verify task headings in any accepted spelling become ID "t2", title "x", with a fix; a heading without an ID gets
// one from AssignTaskIDs
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
			p, fixes := mustCleanup(t, tasksFile(tt.heading, "body"))
			checkTasks(t, p.Tasks, []Task{tt.want})
			var want []Fix
			if tt.fixed {
				want = []Fix{{Line: 3, Was: tt.heading, Now: taskHeadingLine(tt.want)}}
			}
			checkFixes(t, fixes, want)
		})
	}
}

// - Verify each field line is read into its field, in standard form without a fix
func Test_Unit_Cleanup_Fields(t *testing.T) {
	p, fixes := mustCleanup(t, tasksFile(
		"### t1: a",
		"### t3: x",
		"depends: t1",
		"reason: asked for",
		"effects: external",
		"gate: ask first",
		"needs: web search, PDF reading",
		"done when: report.md: has 3 sections",
		"",
		"body",
	))
	checkFixes(t, fixes, nil)
	checkTasks(t, p.Tasks, []Task{{ID: "t1", Title: "a"}, {ID: "t3", Title: "x", Depends: []string{"t1"},
		Reason: "asked for", Effects: EffectsExternal, Gate: "ask first", Needs: "web search, PDF reading",
		DoneWhen: "report.md: has 3 sections", Body: "body"}})
}

// - Verify loosely written field lines (key case, spacing, "-"/"_", out of order) are read and written back in
// standard form and order, one fix per changed line
func Test_Unit_Cleanup_FieldFixes(t *testing.T) {
	p, fixes := mustCleanup(t, tasksFile(
		"### t3: x", // 3
		"",
		"Done-When:  it works", // 5
		"NEEDS: a browser",
		"effects: External", // 7
		"",
		"body",
	))
	checkTasks(t, p.Tasks, []Task{{ID: "t3", Title: "x", Effects: EffectsExternal, Needs: "a browser",
		DoneWhen: "it works", Body: "body"}})
	checkFixes(t, fixes, []Fix{
		{5, "Done-When:  it works", "effects: external"},
		{6, "NEEDS: a browser", "needs: a browser"},
		{7, "effects: External", "done when: it works"},
	})
}

// - Verify the depends line is read and fixed when not standard; "depends: setup" keeps "setup" (for CheckComplete)
func Test_Unit_Cleanup_Depends(t *testing.T) {
	tests := []struct {
		line string
		want []string
		now  string
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
			p, fixes := mustCleanup(t, tasksFile("### t3: x", tt.line, "", "body"))
			checkTasks(t, p.Tasks, []Task{{ID: "t3", Title: "x", Depends: tt.want, Body: "body"}})
			var want []Fix
			if tt.fix {
				want = []Fix{{Line: 4, Was: tt.line, Now: tt.now}}
			}
			checkFixes(t, fixes, want)
		})
	}
}

// - Verify what ends the field block: an empty value ("needs:"), a repeated key, a blank line, or an ordinary line
// all leave the rest in the body as written
func Test_Unit_Cleanup_FieldBlockEnds(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  Task
	}{
		{"empty value stays in the body", []string{"needs:", "", "needs:"},
			Task{ID: "t3", Title: "x", Body: "needs:\n\nneeds:"}},
		{"repeated key", []string{"needs: a", "needs: b"}, Task{ID: "t3", Title: "x", Needs: "a", Body: "needs: b"}},
		{"blank line", []string{"needs: a", "", "gate: b"}, Task{ID: "t3", Title: "x", Needs: "a", Body: "gate: b"}},
		{"ordinary line", []string{"Note: read this", "needs: a"},
			Task{ID: "t3", Title: "x", Body: "Note: read this\nneeds: a"}},
		{"field after body text", []string{"First.", "", "depends: t9"},
			Task{ID: "t3", Title: "x", Body: "First.\n\ndepends: t9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fixes := mustCleanup(t, tasksFile(append([]string{"### t3: x"}, tt.lines...)...))
			checkTasks(t, p.Tasks, []Task{tt.want})
			checkFixes(t, fixes, nil)
			checkRoundTrip(t, p)
		})
	}
}

// - Verify body newlines are kept; a "#### Steps" subheading, bullets and a fence (even one holding a task heading)
// stay in the body
func Test_Unit_Cleanup_TaskBody(t *testing.T) {
	body := []string{
		"First paragraph.",
		"",
		"#### Steps",
		"- step one",
		"- step two",
		"",
		"```go",
		"### t9: not a task",
		"```",
	}
	p, fixes := mustCleanup(t, tasksFile(append([]string{"### t1: Build it"}, body...)...))
	checkFixes(t, fixes, nil)
	checkTasks(t, p.Tasks, []Task{{ID: "t1", Title: "Build it", Body: strings.Join(body, "\n")}})
}

// - Verify bullets under "## Tasks" become tasks (checkbox stripped, continuation lines dedented, field lines read),
// with one fix per bullet; text before the first bullet is TaskNotes; after the first "###" task, bullets are body
func Test_Unit_Cleanup_BulletTasks(t *testing.T) {
	p, fixes := mustCleanup(t, tasksFile(
		"Work through these in order.", // 3
		"- [ ] Create the module",      // 4
		"  needs: a Go toolchain",
		"  Run go mod init fib.",
		"-    Write the tests", // 7
		"* t5: Write the README",
		"  - mention go test",
		"### t9: Last",
		"- a bullet in t9's body",
		"",
		"Plan: not a heading",
		"- Ship it ##",
		"- C#",
	))
	if p.TaskNotes != "Work through these in order." {
		t.Errorf("TaskNotes = %q", p.TaskNotes)
	}
	checkTasks(t, p.Tasks, []Task{
		{ID: "t10", Title: "Create the module", Needs: "a Go toolchain", Body: "Run go mod init fib."},
		{ID: "t11", Title: "Write the tests"},
		{ID: "t5", Title: "Write the README", Body: "- mention go test"},
		{ID: "t9", Title: "Last", Body: "- a bullet in t9's body\n\nPlan: not a heading\n- Ship it ##\n- C#"},
	})
	checkFixes(t, fixes, []Fix{
		{4, "- [ ] Create the module", "### t10: Create the module"},
		{7, "-    Write the tests", "### t11: Write the tests"},
		{8, "* t5: Write the README", "### t5: Write the README"},
	})

	t.Run("dedenting would create a heading", func(t *testing.T) {
		p, _ := mustCleanup(t, tasksFile("- a", "    # not a heading", "    more"))
		checkTasks(t, p.Tasks, []Task{{ID: "t1", Title: "a", Body: "    # not a heading\n    more"}})
	})
	t.Run("closing hashes in a bullet title", func(t *testing.T) {
		p, _ := mustCleanup(t, tasksFile("- Ship it ##", "- #", "- C#"))
		checkTasks(t, p.Tasks, []Task{{ID: "t1", Title: "Ship it"}, {ID: "t2"}, {ID: "t3", Title: "C#"}})
	})
}

// - Verify missing IDs get the highest + 1 in file order, existing IDs never change, and each new ID is a fix on its
// heading's line
func Test_Unit_Cleanup_TaskIDs(t *testing.T) {
	p, fixes := mustCleanup(t, tasksFile("### t1: a", "### b", "### t5: c", "### d"))
	checkTasks(t, p.Tasks, []Task{{ID: "t1", Title: "a"}, {ID: "t6", Title: "b"}, {ID: "t5", Title: "c"},
		{ID: "t7", Title: "d"}})
	checkFixes(t, fixes, []Fix{{4, "### b", "### t6: b"}, {6, "### d", "### t7: d"}})
}

// - Verify an unknown "##" heading after the tasks starts an Other section, and a known section name at any level
// starts that section, even inside Tasks
func Test_Unit_Cleanup_UnknownHeadingEndsTasks(t *testing.T) {
	p, fixes := mustCleanup(t, tasksFile("### t1: a", "## Notes", "n", "### Assumptions", "c"))
	checkTasks(t, p.Tasks, []Task{{ID: "t1", Title: "a"}})
	if !slices.Equal(p.Other, []Section{{"Notes", "n"}}) || p.Assumptions != "c" {
		t.Errorf("Other %#v, Assumptions %q", p.Other, p.Assumptions)
	}
	checkFixes(t, fixes, []Fix{{6, "### Assumptions", "## Assumptions"}})
}

// - Verify the standard testdata file cleans up with zero fixes and the messy one gets every expected fix
func Test_Unit_Cleanup_Testdata(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("standard", func(t *testing.T) {
		p, fixes := mustCleanup(t, read("plan-standard.md"))
		checkFixes(t, fixes, nil)
		if err := p.CheckComplete(20); err != nil {
			t.Errorf("CheckComplete: %v", err)
		}
		if len(p.Tasks) != 3 || !p.Tasks[2].External() || p.Tasks[1].Reason == "" || len(p.Other) != 1 ||
			p.Version != 2 || p.Summary == "" {
			t.Errorf("unexpected plan: %#v", p)
		}
		if got := p.Markdown(); got != read("plan-standard.md") {
			t.Errorf("the standard file is not exactly what Markdown writes:\n%s", got)
		}
	})

	t.Run("messy (hand-edited)", func(t *testing.T) {
		p, fixes := mustCleanup(t, read("plan-messy.md"))
		var lines []int
		for _, f := range fixes {
			lines = append(lines, f.Line)
		}
		if want := []int{1, 2, 4, 6, 7, 8, 9, 10}; !slices.Equal(lines, want) {
			t.Errorf("fix lines = %v; want %v\nfixes: %#v", lines, want, fixes)
		}
		checkTasks(t, p.Tasks, []Task{
			{ID: "t1", Title: "Create the module", Body: "Run go mod init fib."},
			{ID: "t2", Title: "Write fib.go", Depends: []string{"t1"}, Needs: "a Go toolchain"},
		})
		if p.Title != "Fib" || p.Assumptions != "- Go 1.26 is installed." || p.TaskNotes != "Do these in order." {
			t.Errorf("Title %q, Assumptions %q, TaskNotes %q", p.Title, p.Assumptions, p.TaskNotes)
		}
	})
}

// --- helpers ---

// - Verify NormalizeTaskID accepts "t3", "T3", "task3", "Task 3", "3", "t03" and refuses anything else
func Test_Unit_NormalizeTaskID(t *testing.T) {
	for _, in := range []string{"t3", "T3", "task3", "Task 3", "TASK3", "3", "t03", " t3 "} {
		if got, ok := NormalizeTaskID(in); !ok || got != "t3" {
			t.Errorf("NormalizeTaskID(%q) = %q, %v; want \"t3\", true", in, got, ok)
		}
	}
	for _, in := range []string{"", "t", "task", "setup", "t-1", "3a", "t3x", "t 3 4"} {
		if got, ok := NormalizeTaskID(in); ok {
			t.Errorf("NormalizeTaskID(%q) = %q, true; want false", in, got)
		}
	}
}

// - Verify taskHeading's ID and title from heading text; text without an ID is all title
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

// - Verify planTitle matches "Plan", "Plan:", "Plan: x" in any case and nothing else
func Test_Unit_PlanTitle(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"Plan", "", true},
		{"plan:", "", true},
		{"PLAN: Build it", "Build it", true},
		{"Plan:   spaced  ", "spaced", true},
		{"Plan: C#", "C#", true},
		{"Plan: #", "", true},
		{"Planning", "", false},
		{"Plan for later", "", false},
		{"Plans", "", false},
	}
	for _, tt := range tests {
		got, ok := planTitle(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("planTitle(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
