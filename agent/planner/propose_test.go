package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/tools"
)

// testObjective is the objective the propose_plan tests plan for.
var testObjective = objective.Objective{ObjText: "Build a word counter.", Instructions: "Use Go."}

// testMaxTasks is the task limit the propose_plan tests use (the plan-mode default).
const testMaxTasks = 30

// validProposal is a proposal that passes every check; tests change one thing in it.
func validProposal() proposal {
	return proposal{
		Title:       "Word counter",
		Summary:     "Write it, then test it.",
		Assumptions: "- Go is installed",
		Tasks: []proposedTask{
			{ID: "t1", Title: "Write wc.go", Needs: "write files in the working directory", DoneWhen: "wc.go exists",
				Body: "Count words on stdin."},
			{ID: "t2", Title: "Test it", Depends: []string{"t1"}, Needs: "run go test", DoneWhen: "go test passes"},
		},
	}
}

// callPropose marshals prop and calls the tool with it.
func callPropose(t *testing.T, tool *ProposePlan, prop any) (string, error) {
	t.Helper()
	args, err := json.Marshal(prop)
	if err != nil {
		t.Fatal(err)
	}
	return tool.CallTool(context.Background(), args)
}

// - Verify propose_plan's definition has its name and valid JSON parameters
func Test_Unit_ProposePlan_Definition(t *testing.T) {
	def := NewProposePlan("objective.md", testObjective, testMaxTasks).GetToolDefinition()
	if def.Function.Name != "propose_plan" {
		t.Errorf("name = %q, want propose_plan", def.Function.Name)
	}
	if !json.Valid(def.Function.Params) {
		t.Errorf("Params is not valid JSON: %s", def.Function.Params)
	}
}

// - Verify a valid proposal returns tools.ErrEndTurn and sets Accepted to the read-back plan
// - Verify the accepted plan has FormatV1, the objective's base name, its hash and version 1, and the given tasks
// - Verify the accepted plan is not stale against the objective and its Markdown cleans up with zero fixes
func Test_Unit_ProposePlan_Accepts(t *testing.T) {
	tool := NewProposePlan("/some/dir/goal.md", testObjective, testMaxTasks)
	result, err := callPropose(t, tool, validProposal())
	if !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("err = %v, want tools.ErrEndTurn", err)
	}
	if !strings.Contains(result, "passed") {
		t.Errorf("result = %q", result)
	}
	p := tool.Accepted
	if p == nil {
		t.Fatal("Accepted is nil after a valid proposal")
	}
	if p.Format != plan.FormatV1 || p.ObjectivePath != "goal.md" || p.ObjectiveHash != testObjective.Hash() || p.Version != 1 {
		t.Errorf("front matter = %q %q %q %d", p.Format, p.ObjectivePath, p.ObjectiveHash, p.Version)
	}
	if p.Title != "Word counter" || p.Summary != "Write it, then test it." || p.Assumptions != "- Go is installed" {
		t.Errorf("title/summary/assumptions = %q / %q / %q", p.Title, p.Summary, p.Assumptions)
	}
	if len(p.Tasks) != 2 || p.Tasks[0].ID != "t1" || p.Tasks[0].Body != "Count words on stdin." ||
		p.Tasks[1].ID != "t2" || len(p.Tasks[1].Depends) != 1 || p.Tasks[1].Depends[0] != "t1" ||
		p.Tasks[1].DoneWhen != "go test passes" {
		t.Errorf("tasks = %+v", p.Tasks)
	}
	if p.IsStale(testObjective.Hash()) {
		t.Error("the accepted plan is stale against its own objective")
	}
	if _, fixes, err := plan.Cleanup([]byte(p.Markdown())); err != nil || len(fixes) != 0 {
		t.Errorf("Cleanup of the accepted plan: fixes %v, err %v", fixes, err)
	}
}

// - Verify text is trimmed and an external task is accepted
func Test_Unit_ProposePlan_TrimsAndAcceptsExternal(t *testing.T) {
	prop := validProposal()
	prop.Title = "  Word counter  "
	prop.Tasks[0].ID = " t1 "
	prop.Tasks[0].Title = " Write wc.go "
	prop.Tasks[1].Depends = []string{" t1 "}
	prop.Tasks[1].Effects = " external "
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	if _, err := callPropose(t, tool, prop); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("err = %v, want tools.ErrEndTurn", err)
	}
	p := tool.Accepted
	if p.Title != "Word counter" || p.Tasks[0].ID != "t1" || p.Tasks[0].Title != "Write wc.go" ||
		p.Tasks[1].Depends[0] != "t1" || !p.Tasks[1].External() {
		t.Errorf("accepted plan = %+v", *p)
	}
}

// - Verify a task with a gate is accepted and keeps the trimmed reason; a task without one keeps an empty gate
// - Verify the plan file has a "gate: <reason>" line and cleans up with zero fixes, and plan.Summary marks the task " [gate]"
func Test_Unit_ProposePlan_AcceptsGate(t *testing.T) {
	prop := validProposal()
	prop.Tasks[1].Gate = "  publishes the results  "
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	if _, err := callPropose(t, tool, prop); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("err = %v, want tools.ErrEndTurn", err)
	}
	p := tool.Accepted
	if p.Tasks[1].Gate != "publishes the results" || p.Tasks[0].Gate != "" {
		t.Errorf("gates = %q / %q, want \"\" / \"publishes the results\"", p.Tasks[0].Gate, p.Tasks[1].Gate)
	}
	md := p.Markdown()
	if !strings.Contains(md, "\ngate: publishes the results\n") {
		t.Errorf("plan file lacks the gate line:\n%s", md)
	}
	if _, fixes, err := plan.Cleanup([]byte(md)); err != nil || len(fixes) != 0 {
		t.Errorf("Cleanup of the accepted plan: fixes %v, err %v", fixes, err)
	}
	summary := plan.Summary(*p)
	if !strings.Contains(summary, "Test it  (after t1) [gate]") || strings.Count(summary, "[gate]") != 1 {
		t.Errorf("summary does not mark only t2 [gate]:\n%s", summary)
	}
}

// - Verify propose_plan's schema has a "gate" task property
func Test_Unit_ProposePlan_SchemaHasGate(t *testing.T) {
	var schema struct {
		Properties struct {
			Tasks struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	def := NewProposePlan("objective.md", testObjective, testMaxTasks).GetToolDefinition()
	if err := json.Unmarshal(def.Function.Params, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties.Tasks.Items.Properties["gate"]; !ok {
		t.Errorf("task properties lack \"gate\": %s", def.Function.Params)
	}
}

// - Verify Cleanup's tidying of a body (bullet spacing) is accepted, and the tidied body is what is kept
func Test_Unit_ProposePlan_AcceptsTidiedBody(t *testing.T) {
	prop := validProposal()
	prop.Tasks[0].Body = "Steps:\n\n-   read stdin\n-   count words"
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	if _, err := callPropose(t, tool, prop); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("err = %v, want tools.ErrEndTurn", err)
	}
	if got, want := tool.Accepted.Tasks[0].Body, "Steps:\n\n- read stdin\n- count words"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// - Verify bad JSON, multi-line fields, a plan that doesn't read back and one CheckComplete refuses are errors
// - Verify Accepted stays nil on each failure
func Test_Unit_ProposePlan_Refuses(t *testing.T) {
	tests := []struct {
		name    string
		change  func(p *proposal)
		wantErr []string
	}{
		{"multi-line plan title", func(p *proposal) { p.Title = "a\nb" }, []string{"title must be one line"}},
		{"multi-line task title", func(p *proposal) { p.Tasks[0].Title = "a\nb" }, []string{"t1: title must be one line"}},
		{"multi-line needs", func(p *proposal) { p.Tasks[1].Needs = "a\r\nb" }, []string{"t2: needs must be one line"}},
		{"multi-line done_when", func(p *proposal) { p.Tasks[0].DoneWhen = "a\nb" }, []string{"t1: done_when must be one line"}},
		{"multi-line effects", func(p *proposal) { p.Tasks[0].Effects = "external\nx" }, []string{"t1: effects must be one line"}},
		{"multi-line gate", func(p *proposal) { p.Tasks[1].Gate = "publishes\nto prod" }, []string{"t2: gate must be one line"}},
		{"multi-line depends", func(p *proposal) { p.Tasks[1].Depends = []string{"t1\nt3"} }, []string{"t2: depends must be one line"}},
		{"multi-line field of a task without ID", func(p *proposal) { p.Tasks[1].ID = ""; p.Tasks[1].Needs = "a\nb" },
			[]string{"task 2: needs must be one line"}},
		{"summary line reads as a section", func(p *proposal) { p.Summary = "Intro.\n# Notes\nmore" },
			[]string{"doesn't read back", "section heading"}},
		{"body line reads as a section", func(p *proposal) { p.Tasks[1].Body = "x\n## Extra\ny" },
			[]string{"doesn't read back", "## Extra"}},
		{"body line reads as a task", func(p *proposal) { p.Tasks[0].Body = "x\n### t9: sneaky" },
			[]string{"2 tasks were given but 3 read back"}},
		{"body first line reads as a field", func(p *proposal) {
			p.Tasks[0].Needs, p.Tasks[0].DoneWhen, p.Tasks[0].Body = "", "", "needs: a shell"
		},
			[]string{"task t1 reads back differently"}},
		{"task without ID", func(p *proposal) { p.Tasks[1].ID = ""; p.Tasks[1].Depends = nil }, []string{`id "" is not t<number>`}},
		{"duplicate IDs", func(p *proposal) { p.Tasks[1].ID = "t1"; p.Tasks[1].Depends = nil }, []string{"can't be saved"}},
		{"unknown depends", func(p *proposal) { p.Tasks[1].Depends = []string{"t7"} }, []string{"has problems", `"t7"`}},
		{"cycle", func(p *proposal) { p.Tasks[0].Depends = []string{"t2"} }, []string{"has problems", "loop"}},
		{"effects not external", func(p *proposal) { p.Tasks[0].Effects = "internal" }, []string{"has problems", `"internal"`}},
		{"no tasks", func(p *proposal) { p.Tasks = nil }, []string{"has problems", "no tasks"}},
		{"too many tasks", func(p *proposal) {
			p.Tasks = nil
			for i := 1; i <= testMaxTasks+1; i++ {
				p.Tasks = append(p.Tasks, proposedTask{ID: fmt.Sprintf("t%d", i), Title: fmt.Sprintf("Task %d", i)})
			}
		}, []string{"has problems", "the limit is 30"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prop := validProposal()
			tt.change(&prop)
			tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
			_, err := callPropose(t, tool, prop)
			if err == nil || errors.Is(err, tools.ErrEndTurn) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err lacks %q:\n%v", want, err)
				}
			}
			if tool.Accepted != nil {
				t.Errorf("Accepted set on a refused proposal: %+v", *tool.Accepted)
			}
		})
	}
}

// - Verify bad JSON is an error naming the arguments, with Accepted left nil
func Test_Unit_ProposePlan_BadJSON(t *testing.T) {
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	_, err := tool.CallTool(context.Background(), json.RawMessage(`{"title":`))
	if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("err = %v, want an invalid-arguments error", err)
	}
	if tool.Accepted != nil {
		t.Error("Accepted set after bad JSON")
	}
}

// - Verify the maxTasks given to NewProposePlan is the limit: with 2, a 3-task plan is refused naming the limit and a 2-task one is accepted
// - Verify the description states that limit
func Test_Unit_ProposePlan_HonoursMaxTasks(t *testing.T) {
	tool := NewProposePlan("objective.md", testObjective, 2)
	if desc := tool.GetToolDefinition().Function.Desc; !strings.Contains(desc, "at most 2 tasks") {
		t.Errorf("description lacks %q:\n%s", "at most 2 tasks", desc)
	}
	three := validProposal()
	three.Tasks = append(three.Tasks, proposedTask{ID: "t3", Title: "Ship it", Depends: []string{"t2"}})
	_, err := callPropose(t, tool, three)
	if err == nil || errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("3 tasks: err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "the limit is 2") {
		t.Errorf("err lacks %q:\n%v", "the limit is 2", err)
	}
	if tool.Accepted != nil {
		t.Fatal("Accepted set on a refused proposal")
	}
	if _, err := callPropose(t, tool, validProposal()); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("2 tasks: err = %v, want tools.ErrEndTurn", err)
	}
}

// - Verify a valid call after a refused one is accepted
func Test_Unit_ProposePlan_RetryAfterRefusal(t *testing.T) {
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	bad := validProposal()
	bad.Tasks[0].Depends = []string{"t2"} // a loop
	if _, err := callPropose(t, tool, bad); err == nil || errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("first call: err = %v, want a refusal", err)
	}
	if _, err := callPropose(t, tool, validProposal()); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("second call: err = %v, want tools.ErrEndTurn", err)
	}
	if tool.Accepted == nil || len(tool.Accepted.Tasks) != 2 {
		t.Errorf("Accepted = %+v", tool.Accepted)
	}
}

// - Verify the hash is taken when the tool is made, so changing the objective afterwards does not change the plan's hash
func Test_Unit_ProposePlan_HashTakenAtCreation(t *testing.T) {
	o := testObjective
	tool := NewProposePlan("objective.md", o, testMaxTasks)
	want := o.Hash()
	o.ObjText = "Something else."
	if _, err := callPropose(t, tool, validProposal()); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("err = %v", err)
	}
	if tool.Accepted.ObjectiveHash != want {
		t.Errorf("ObjectiveHash = %q, want %q", tool.Accepted.ObjectiveHash, want)
	}
}

// - Verify IDs, depends and effects in loose form ("T1", "task 2", "t03", ["T1"], "External") are accepted
// - Verify they come back in standard form: t1, t2, t3, depends "t1", effects "external"
func Test_Unit_ProposePlan_AcceptsLooseIDsAndEffects(t *testing.T) {
	prop := validProposal()
	prop.Tasks[0].ID = "T1"
	prop.Tasks[1].ID = "task 2"
	prop.Tasks[1].Depends = []string{"T1"}
	prop.Tasks[1].Effects = "External"
	prop.Tasks = append(prop.Tasks, proposedTask{ID: "t03", Title: "Ship it", Depends: []string{" Task 2 "}})
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	if _, err := callPropose(t, tool, prop); !errors.Is(err, tools.ErrEndTurn) {
		t.Fatalf("err = %v, want tools.ErrEndTurn", err)
	}
	p := tool.Accepted
	var ids []string
	for _, task := range p.Tasks {
		ids = append(ids, task.ID)
	}
	if !slices.Equal(ids, []string{"t1", "t2", "t3"}) {
		t.Errorf("IDs = %v, want [t1 t2 t3]", ids)
	}
	if !slices.Equal(p.Tasks[1].Depends, []string{"t1"}) || !slices.Equal(p.Tasks[2].Depends, []string{"t2"}) {
		t.Errorf("depends = %v / %v, want [t1] / [t2]", p.Tasks[1].Depends, p.Tasks[2].Depends)
	}
	if p.Tasks[1].Effects != plan.EffectsExternal {
		t.Errorf("effects = %q, want %q", p.Tasks[1].Effects, plan.EffectsExternal)
	}
}

// - Verify an ID that is not a task ID ("x1") is still refused, with an error naming it and Accepted left nil
// - Verify the refusal comes from toPlan with a message saying the ID is not t<number>
func Test_Unit_ProposePlan_RefusesNonTaskID(t *testing.T) {
	prop := validProposal()
	prop.Tasks[1].ID = "x1"
	tool := NewProposePlan("objective.md", testObjective, testMaxTasks)
	_, err := callPropose(t, tool, prop)
	if err == nil || errors.Is(err, tools.ErrEndTurn) || !strings.Contains(err.Error(), `id "x1" is not t<number>`) {
		t.Errorf("err = %v, want a refusal naming x1", err)
	}
	if tool.Accepted != nil {
		t.Errorf("Accepted set: %+v", *tool.Accepted)
	}
}

// - Verify standardID trims and puts task IDs in standard form, and keeps anything else as written (trimmed)
func Test_Unit_StandardID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"t1", "t1"}, {" T1 ", "t1"}, {"task 2", "t2"}, {"Task2", "t2"}, {"t03", "t3"}, {"3", "t3"},
		{"x1", "x1"}, {" t1a ", "t1a"}, {"", ""},
	}
	for _, tt := range tests {
		if got := standardID(tt.in); got != tt.want {
			t.Errorf("standardID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
