package director

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// - Verify a full objective file: tasks (bullets, numbers, checkboxes, nested lines), task notes,
// the other sections in order, and HasDone
func Test_ParseObjective_FullFile(t *testing.T) {
	src := `A note for the human author, ignored.

# Objective
Summarize the MCP config.

## Tasks
Work in this order.
- [ ] Read README.md
* [x] List the fields
  - including "env"
1. Write notes/mcp.md
2) Check it

## Done when
- notes/mcp.md exists

## Constraints
Don't modify files.

### Not a section
still constraints text
`
	obj, err := ParseObjective([]byte(src), testMaxTasks)
	if err != nil {
		t.Fatalf("ParseObjective: %v", err)
	}
	if obj.Text != "Summarize the MCP config." {
		t.Errorf("Text = %q", obj.Text)
	}
	wantTasks := []string{"Read README.md", "List the fields\n  - including \"env\"", "Write notes/mcp.md", "Check it"}
	if !slices.Equal(obj.Tasks, wantTasks) {
		t.Errorf("Tasks = %q, want %q", obj.Tasks, wantTasks)
	}
	if obj.TaskNotes != "Work in this order." {
		t.Errorf("TaskNotes = %q", obj.TaskNotes)
	}
	if len(obj.Sections) != 2 || obj.Sections[0].Heading != "Done when" || obj.Sections[1].Heading != "Constraints" {
		t.Fatalf("Sections = %+v", obj.Sections)
	}
	if !strings.Contains(obj.Sections[1].Body, "### Not a section") {
		t.Errorf("### heading should stay body text, got %q", obj.Sections[1].Body)
	}
	if !obj.HasDone() {
		t.Error("HasDone() = false, want true")
	}
}

// - Verify only "# Objective" is required, and no Tasks section means nil Tasks (the PLAN phase)
func Test_ParseObjective_OnlyObjective_NoTasks(t *testing.T) {
	obj, err := ParseObjective([]byte("# objective\nDo the thing.\n"), testMaxTasks)
	if err != nil {
		t.Fatalf("ParseObjective: %v", err)
	}
	if obj.Tasks != nil || obj.HasDone() {
		t.Errorf("got Tasks=%q HasDone=%v, want nil and false", obj.Tasks, obj.HasDone())
	}
}

// - Verify "Done" headings match case-insensitively by prefix
func Test_ParseObjective_DoneHeadingVariants(t *testing.T) {
	for _, h := range []string{"Done", "done when", "DONE criteria"} {
		obj, err := ParseObjective([]byte("# Objective\nx\n\n## "+h+"\n- y\n"), testMaxTasks)
		if err != nil || !obj.HasDone() {
			t.Errorf("heading %q: HasDone=%v err=%v, want true, nil", h, obj.HasDone(), err)
		}
	}
}

// - Verify headings inside ``` fences are ignored
func Test_ParseObjective_FencedHeadingIgnored(t *testing.T) {
	obj, err := ParseObjective([]byte("# Objective\nx\n```\n# not a heading\n## Tasks\n```\n"), testMaxTasks)
	if err != nil {
		t.Fatalf("ParseObjective: %v", err)
	}
	if obj.Tasks != nil || !strings.Contains(obj.Text, "# not a heading") {
		t.Errorf("fenced headings were parsed: %+v", obj)
	}
}

// - Verify every rejected file gives an error that names the problem
func Test_ParseObjective_Errors(t *testing.T) {
	var many strings.Builder
	many.WriteString("# Objective\nx\n## Tasks\n")
	for i := range testMaxTasks + 1 {
		fmt.Fprintf(&many, "- task %d\n", i)
	}
	cases := map[string]struct{ src, want string }{
		"empty":             {"  \n", "empty"},
		"no objective":      {"## Tasks\n- a\n", `no "# Objective"`},
		"empty objective":   {"# Objective\n\n## Tasks\n- a\n", "is empty"},
		"other h1":          {"# Objective\nx\n# Notes\ny\n", "unexpected top-level heading"},
		"two objectives":    {"# Objective\nx\n# Objective\ny\n", "more than once"},
		"tasks no bullets":  {"# Objective\nx\n## Tasks\njust text\n", "no bullet items"},
		"too many tasks":    {many.String(), "the limit is 20"},
		"two task sections": {"# Objective\nx\n## Tasks\n- a\n## Tasks\n- b\n", "more than once"},
		"too large":         {"# Objective\n" + strings.Repeat("x", MaxObjectiveBytes), "the limit is"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseObjective([]byte(tc.src), testMaxTasks)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
