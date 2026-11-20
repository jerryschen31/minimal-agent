package plan

import (
	"strings"
	"testing"
)

// - Verify ObjectiveSection moves every heading two levels down, leaves other lines and fenced code alone, caps at level 6
func Test_Unit_ObjectiveSection(t *testing.T) {
	in := "# Objective\nBuild it.\n\n# Instructions\n- Go only.\n## Constraints\nNone.\n```\n# not a heading\n```\n##### Deep\n#hashtag\n"
	want := "### Objective\nBuild it.\n\n### Instructions\n- Go only.\n#### Constraints\nNone.\n```\n# not a heading\n```\n###### Deep\n#hashtag"
	if got := ObjectiveSection(in); got != want {
		t.Errorf("ObjectiveSection =\n%s\nwant\n%s", got, want)
	}
}

//   - Verify a plan with an Objective section (holding "### Objective", "### Instructions", "### Tasks" and "### t1:" lines)
//     writes and reads back with zero fixes, the section word for word, and no extra section or task
//   - Verify the section comes after the summary and before Assumptions
func Test_Unit_Plan_ObjectiveSection_RoundTrip(t *testing.T) {
	obj := ObjectiveSection("# Objective\nBuild a game.\n\n# Instructions\n### Tasks\n### t1: not a task\n- Go 1.26.\n\n# Context\nTeaching.\n")
	p := Plan{
		Format: FormatV1, Title: "Game", Summary: "Write it.", Objective: obj, Assumptions: "- none",
		Tasks: []Task{{ID: "t1", Title: "Write it", DoneWhen: "it runs", Body: "Do it."}},
	}
	md := p.Markdown()
	back, fixes, err := Cleanup([]byte(md))
	if err != nil || len(fixes) != 0 {
		t.Fatalf("Cleanup = %v fixes, %v; want none\n%s", fixes, err, md)
	}
	if back.Objective != obj {
		t.Errorf("Objective read back as\n%q\nwant\n%q", back.Objective, obj)
	}
	if len(back.Tasks) != 1 || len(back.Other) != 0 || back.Assumptions != "- none" {
		t.Errorf("read back tasks %d, other %v, assumptions %q", len(back.Tasks), back.Other, back.Assumptions)
	}
	if i, j, k := strings.Index(md, "Write it."), strings.Index(md, "## Objective"), strings.Index(md, "## Assumptions"); !(i < j && j < k) {
		t.Errorf("section order wrong:\n%s", md)
	}
}

//   - Verify "## Objective" (any case, "Objectives", level 1) is the Objective section, but "#### Objective" in a task body
//     stays in the task body (in Tasks a level-3 heading is a task, as before)
func Test_Unit_Cleanup_ObjectiveHeadingLevels(t *testing.T) {
	data := "# Plan: x\n\n# objectives:\nDo it.\n\n## Tasks\n\n### t1: a\n\n#### Objective\nInside the task.\n"
	p, _, err := Cleanup([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if p.Objective != "Do it." {
		t.Errorf("Objective = %q", p.Objective)
	}
	if len(p.Tasks) != 1 || !strings.Contains(p.Tasks[0].Body, "#### Objective") {
		t.Errorf("tasks = %+v; want the subheading kept in t1's body", p.Tasks)
	}
}

// - Verify a plan without an Objective section still reads and writes as before (no section added)
func Test_Unit_Plan_NoObjectiveSection(t *testing.T) {
	p := Plan{Format: FormatV1, Title: "x", Tasks: []Task{{ID: "t1", Title: "a"}}}
	if strings.Contains(p.Markdown(), "## Objective") {
		t.Errorf("empty Objective was written:\n%s", p.Markdown())
	}
}
