package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/agent/planner"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
)

// validPlanArgs are propose_plan arguments for a two-task plan that passes every check.
const validPlanArgs = `{"title":"Word counter","summary":"Write it, then test it.","tasks":[` +
	`{"id":"t1","title":"Write wc.go","needs":"write files in the working directory","done_when":"wc.go exists","body":"Count words on stdin."},` +
	`{"id":"t2","title":"Test it","depends":["t1"],"needs":"run go test","done_when":"go test passes","body":"Run go test."}]}`

// oneTaskPlanArgs are propose_plan arguments for a one-task plan that passes every check.
const oneTaskPlanArgs = `{"title":"Line counter","summary":"Write it.","tasks":[` +
	`{"id":"t1","title":"Write lc.go","needs":"write files in the working directory","done_when":"lc.go exists","body":"Count lines on stdin."}]}`

// proposeCall returns an assistant reply that calls propose_plan with args.
func proposeCall(id, args string) model.ChatMessage {
	return modeltest.AssistantToolCall(id, "propose_plan", args)
}

// readPlanFile reads the plan file at path, failing the test unless it cleans up with zero fixes.
func readPlanFile(t *testing.T, path string) plan.Plan {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	p, fixes, err := plan.Cleanup(data)
	if err != nil || len(fixes) != 0 {
		t.Fatalf("plan.Cleanup(%s) = %v fixes, %v; want no fixes and no error", path, fixes, err)
	}
	return p
}

// acceptedPlan returns the plan propose_plan accepts for args and objective o saved at objPath.
func acceptedPlan(t *testing.T, objPath string, o objective.Objective, args string) plan.Plan {
	t.Helper()
	propose := planner.NewProposePlan(objPath, o, 30)
	if _, err := propose.CallTool(t.Context(), []byte(args)); propose.Accepted == nil {
		t.Fatalf("propose_plan refused the test plan: %v", err)
	}
	return *propose.Accepted
}

// writePlanFor writes the plan propose_plan accepts for args and objective o to plan.PathFor(objPath), and returns its bytes.
func writePlanFor(t *testing.T, objPath string, o objective.Objective, args string) []byte {
	t.Helper()
	p := acceptedPlan(t, objPath, o, args)
	data := []byte(p.Markdown())
	if err := os.WriteFile(plan.PathFor(objPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// writeObjective writes o's Markdown to path.
func writeObjective(t *testing.T, path string, o objective.Objective) {
	t.Helper()
	if err := os.WriteFile(path, []byte(o.Markdown()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkPlannerRequest checks that model call i was a planner request: PlanPrompt, then only the plan message for o.
func checkPlannerRequest(t *testing.T, fx *chatSessionFixture, i int, objPath string, o objective.Objective) {
	t.Helper()
	req := fx.Provider.Calls[i]
	if len(req) != 2 {
		t.Fatalf("planner request %d has %d messages, want 2 (system + plan message): %+v", i, len(req), req)
	}
	if req[0].Role != "system" || req[0].Content != planner.PlanPrompt {
		t.Errorf("planner request %d: first message = %s %.40q, want the PlanPrompt system message", i, req[0].Role, req[0].Content)
	}
	if req[1].Role != "user" || req[1].Content != planner.PlanMessage(objPath, o) {
		t.Errorf("planner request %d: message = %s %q, want the plan message", i, req[1].Role, req[1].Content)
	}
	if !strings.Contains(req[1].Content, objPath) || !strings.Contains(req[1].Content, o.ObjText) {
		t.Errorf("planner request %d: plan message lacks the path or the objective: %q", i, req[1].Content)
	}
	names := toolNames(fx.Provider.ToolDefs[i])
	if !slices.Contains(names, "propose_plan") || slices.Contains(names, "update_objective") {
		t.Errorf("planner request %d tools = %v, want propose_plan and no update_objective", i, names)
	}
}

// checkNoConsecutiveUserMessages fails the test if any request sent to the fake provider has two user messages in a
// row (some providers, Anthropic among them, refuse that).
func checkNoConsecutiveUserMessages(t *testing.T, fx *chatSessionFixture) {
	t.Helper()
	for i, req := range fx.Provider.Calls {
		for j := 1; j < len(req); j++ {
			if req[j-1].Role == "user" && req[j].Role == "user" {
				t.Errorf("request %d has user messages %d and %d in a row: %.60q, %.60q", i, j-1, j, req[j-1].Content, req[j].Content)
			}
		}
	}
}

// newFlagPlanFixture returns a plan fixture whose -objective names a file holding o in a new temp dir (also the cwd).
func newFlagPlanFixture(t *testing.T, o objective.Objective) (*chatSessionFixture, *planModeDetails, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "goal.md")
	writeObjective(t, path, o)
	fx, pd := newPlanFixture(t)
	fx.Session.Config.ObjectiveFile = path
	return fx, pd, path
}

// noPlanLine is what plan mode prints when it ends without writing a plan for objPath.
func noPlanLine(objPath string) string {
	return "No plan written. To plan again: minagent -mode plan -objective " + objPath
}

// countObjective is the objective most planStep tests plan for.
var countObjective = objective.Objective{ObjText: "Count words in a file."}

// ---- objectiveFromFlag ----

// - Verify objectiveFromFlag returns an existing file's path unchanged and prints nothing
// - Verify a directory is an error, and one word ending in .md (any case) that names no file is a no-such-file error
func Test_Unit_ObjectiveFromFlag_FileValues(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("goal.md", []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if got, err := objectiveFromFlag("goal.md", time.Now(), &out); err != nil || got != "goal.md" {
		t.Errorf("existing file: (%q, %v), want goal.md", got, err)
	}
	if out.Len() != 0 {
		t.Errorf("existing file printed %q", out.String())
	}
	if _, err := objectiveFromFlag("sub", time.Now(), &out); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("directory: err = %v, want an is-a-directory error", err)
	}
	for _, v := range []string{"missing.md", "Typo.MD", "  other.Md  "} {
		_, err := objectiveFromFlag(v, time.Now(), &out)
		if want := "-objective: " + strings.TrimSpace(v) + ": no such file"; err == nil || err.Error() != want {
			t.Errorf("%q: err = %v, want %q", v, err, want)
		}
	}
	entries, _ := os.ReadDir(".")
	if len(entries) != 2 {
		t.Errorf("expected only goal.md and sub, got %v", entries)
	}
}

// - Verify text is saved trimmed with one trailing newline to objective.md and "Saved the objective to objective.md." is printed
// - Verify a single word without .md and text with spaces ending in .md are objective text, not file names
func Test_Unit_ObjectiveFromFlag_SavesText(t *testing.T) {
	for _, v := range []string{"  Count words in a file.  \n", "counter", "write notes.md"} {
		t.Run(v, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var out bytes.Buffer
			got, err := objectiveFromFlag(v, time.Now(), &out)
			if err != nil || got != "objective.md" {
				t.Fatalf("objectiveFromFlag = (%q, %v), want objective.md", got, err)
			}
			data, err := os.ReadFile("objective.md")
			if want := strings.TrimSpace(v) + "\n"; err != nil || string(data) != want {
				t.Errorf("objective.md = (%q, %v), want %q", data, err, want)
			}
			if out.String() != "Saved the objective to objective.md.\n" {
				t.Errorf("output = %q", out.String())
			}
		})
	}
}

// - Verify text goes to objective-YYYYMMDD-HHMMSS.md (from now) when objective.md exists, leaving objective.md unchanged
// - Verify it is an error when the timestamped name exists too, and neither file is overwritten
func Test_Unit_ObjectiveFromFlag_NeverOverwrites(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("objective.md", []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 9, 15, 30, 45, 0, time.Local)
	var out bytes.Buffer
	got, err := objectiveFromFlag("Count words.", now, &out)
	if err != nil || got != "objective-20261109-153045.md" {
		t.Fatalf("objectiveFromFlag = (%q, %v), want objective-20261109-153045.md", got, err)
	}
	if data, _ := os.ReadFile(got); string(data) != "Count words.\n" {
		t.Errorf("%s = %q", got, data)
	}
	if !strings.Contains(out.String(), "Saved the objective to objective-20261109-153045.md.") {
		t.Errorf("output = %q", out.String())
	}

	if _, err := objectiveFromFlag("Something else.", now, &out); err == nil {
		t.Error("expected an error when the timestamped file exists too")
	}
	for name, want := range map[string]string{"objective.md": "old\n", got: "Count words.\n"} {
		if data, _ := os.ReadFile(name); string(data) != want {
			t.Errorf("%s = %q, want %q (overwritten)", name, data, want)
		}
	}
}

// ---- runPlanMode with -objective ----

// - Verify -objective with -yes runs without a terminal (stdin is a buffer) and shows an up-to-date plan
// - Verify -objective without -yes, and -yes without -objective, still need a terminal and print nothing
func Test_Unit_RunPlanMode_TerminalUnlessObjectiveAndYes(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	writePlanFor(t, path, countObjective, validPlanArgs)
	fx.Session.InBuffer = strings.NewReader("")
	fx.Session.Config.AssumeYes = true
	if err := runPlanMode(t.Context(), nil, fx.Session); err != nil {
		t.Fatalf("runPlanMode with -objective and -yes: %v", err)
	}
	if !strings.Contains(fx.Out.String(), "is up to date") {
		t.Errorf("output lacks the up-to-date line:\n%s", fx.Out.String())
	}

	for name, set := range map[string]func(fx *chatSessionFixture){
		"-objective without -yes": func(fx *chatSessionFixture) { fx.Session.Config.ObjectiveFile = path },
		"-yes without -objective": func(fx *chatSessionFixture) { fx.Session.Config.AssumeYes = true },
	} {
		t.Run(name, func(t *testing.T) {
			fx, _ := newPlanFixture(t)
			fx.Session.InBuffer = strings.NewReader("y\n")
			set(fx)
			if err := runPlanMode(t.Context(), nil, fx.Session); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
				t.Errorf("err = %v, want a needs-a-terminal error", err)
			}
			if fx.Out.Len() != 0 {
				t.Errorf("expected nothing printed, got:\n%s", fx.Out.String())
			}
		})
	}
}

// ---- planMode with -objective ----

// - Verify -objective text with -yes saves objective.md, runs the planner on it and writes objective.plan.md without asking
func Test_Unit_PlanMode_ObjectiveFlag_TextPlannedWithYes(t *testing.T) {
	t.Chdir(t.TempDir())
	fx, pd := newPlanFixture(t)
	fx.Session.Config.ObjectiveFile = "Count words in a file."
	fx.Session.Config.AssumeYes = true
	fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs)}
	if err := runPlanModeWith(t, fx, ""); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if data, _ := os.ReadFile("objective.md"); string(data) != "Count words in a file.\n" {
		t.Errorf("objective.md = %q", data)
	}
	if pd.ObjectivePath != "objective.md" || *pd.Objective != countObjective {
		t.Errorf("pd = %q %+v", pd.ObjectivePath, *pd.Objective)
	}
	if len(fx.Provider.Calls) != 1 {
		t.Fatalf("expected 1 model call, got %d", len(fx.Provider.Calls))
	}
	checkPlannerRequest(t, fx, 0, "objective.md", countObjective)
	p := readPlanFile(t, "objective.plan.md")
	if p.IsStale(countObjective.Hash()) || p.ObjectivePath != "objective.md" || len(p.Tasks) != 2 {
		t.Errorf("plan = %+v", p)
	}
	out := fx.Out.String()
	for _, w := range []string{"Saved the objective to objective.md.", "Plan 1 (Word counter): 2 to add", "Wrote objective.plan.md (2 tasks)."} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "Accept this plan") || strings.Contains(out, "Name or path") {
		t.Errorf("-yes asked a question:\n%s", out)
	}
}

// - Verify -objective naming a file with an up-to-date plan prints the summary and "is up to date", with no model call and the plan unchanged
func Test_Unit_PlanMode_ObjectiveFlag_UpToDatePlan(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	before := writePlanFor(t, path, countObjective, validPlanArgs)
	if err := runPlanModeWith(t, fx, ""); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
	}
	planPath := plan.PathFor(path)
	if after, _ := os.ReadFile(planPath); !bytes.Equal(after, before) {
		t.Errorf("plan file changed:\n%s", after)
	}
	out := fx.Out.String()
	for _, w := range []string{"Plan 1 (Word counter): 2 to add", planPath + " is up to date with " + path + "."} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// - Verify -objective with a stale plan is an error mentioning step 4 and delete, with no model call and the plan unchanged
func Test_Unit_PlanMode_ObjectiveFlag_StalePlan(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	before := writePlanFor(t, path, objective.Objective{ObjText: "Count lines."}, validPlanArgs)
	err := runPlanModeWith(t, fx, "")
	if err == nil || !strings.Contains(err.Error(), "step 4") || !strings.Contains(err.Error(), "delete") {
		t.Errorf("err = %v, want a stale-plan error mentioning step 4 and delete", err)
	}
	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
	}
	if after, _ := os.ReadFile(plan.PathFor(path)); !bytes.Equal(after, before) {
		t.Error("the stale plan file changed")
	}
}

// - Verify -objective with a plan file that can't be read back (empty, or a directory) is an error naming the plan file
func Test_Unit_PlanMode_ObjectiveFlag_BadPlanFile(t *testing.T) {
	for name, create := range map[string]func(path string) error{
		"empty":       func(path string) error { return os.WriteFile(path, nil, 0o644) },
		"a directory": func(path string) error { return os.Mkdir(path, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			fx, _, path := newFlagPlanFixture(t, countObjective)
			planPath := plan.PathFor(path)
			if err := create(planPath); err != nil {
				t.Fatal(err)
			}
			err := runPlanModeWith(t, fx, "")
			if err == nil || !strings.Contains(err.Error(), planPath) {
				t.Errorf("err = %v, want an error naming %s", err, planPath)
			}
			if len(fx.Provider.Calls) != 0 {
				t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
			}
		})
	}
}

// - Verify -objective with an up-to-date plan that CheckComplete refuses (no tasks, unknown depends, a loop, over MaxPlanTasks) is an error
// - Verify the error names the plan, says "not ready to run" and the problem; no model call, no summary, the plan file unchanged
func Test_Unit_PlanMode_ObjectiveFlag_UpToDatePlanNotReady(t *testing.T) {
	tests := []struct {
		name     string
		maxTasks int // 0 = the fixture's default
		edit     func(p *plan.Plan)
		want     string
	}{
		{"no tasks", 0, func(p *plan.Plan) { p.Tasks = nil }, `"## Tasks" has no tasks`},
		{"unknown depends", 0, func(p *plan.Plan) { p.Tasks[1].Depends = []string{"t9"} },
			`task t2 depends on "t9", which is not a task in this file`},
		{"loop", 0, func(p *plan.Plan) { p.Tasks[0].Depends = []string{"t2"} }, "in a loop"},
		{"over MaxPlanTasks", 1, nil, "there are 2 tasks; the limit is 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, _, path := newFlagPlanFixture(t, countObjective)
			if tt.maxTasks != 0 {
				fx.Session.Config.MaxPlanTasks = tt.maxTasks
			}
			p := acceptedPlan(t, path, countObjective, validPlanArgs)
			if tt.edit != nil {
				tt.edit(&p)
			}
			planPath := plan.PathFor(path)
			before := []byte(p.Markdown())
			if err := os.WriteFile(planPath, before, 0o644); err != nil {
				t.Fatal(err)
			}
			err := runPlanModeWith(t, fx, "")
			if err == nil {
				t.Fatal("err = nil, want a not-ready error")
			}
			for _, w := range []string{planPath + " is not ready to run", "delete it", tt.want} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err lacks %q:\n%v", w, err)
				}
			}
			if len(fx.Provider.Calls) != 0 {
				t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
			}
			if out := fx.Out.String(); strings.Contains(out, "is up to date") || strings.Contains(out, "Plan 1") {
				t.Errorf("a plan that is not ready was shown as up to date:\n%s", out)
			}
			if after, _ := os.ReadFile(planPath); !bytes.Equal(after, before) {
				t.Errorf("plan file changed:\n%s", after)
			}
		})
	}
}

// - Verify -objective naming a file objective.Parse refuses is an error naming the file, with no model call
func Test_Unit_PlanMode_ObjectiveFlag_BadObjectiveFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("goal.md", []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx, _ := newPlanFixture(t)
	fx.Session.Config.ObjectiveFile = "goal.md"
	err := runPlanModeWith(t, fx, "")
	if err == nil || !strings.HasPrefix(err.Error(), "goal.md: ") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want goal.md: ... empty", err)
	}
	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
	}
}

// ---- planStep: accepting and declining ----

// - Verify a blank, y or YES answer to Accept writes the plan, prints "Wrote <plan> (N tasks).", and the plan is clean and not stale
func Test_Unit_PlanMode_Accept_Writes(t *testing.T) {
	for _, answer := range []string{"", "y", "YES"} {
		t.Run(answer, func(t *testing.T) {
			fx, _, path := newFlagPlanFixture(t, countObjective)
			fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs)}
			if err := runPlanModeWith(t, fx, answer+"\n"); err != nil {
				t.Fatalf("planMode: %v", err)
			}
			planPath := plan.PathFor(path)
			p := readPlanFile(t, planPath)
			if p.IsStale(countObjective.Hash()) || len(p.Tasks) != 2 {
				t.Errorf("plan = %+v", p)
			}
			out := fx.Out.String()
			for _, w := range []string{"Accept this plan? [Y/n]: ", "Wrote " + planPath + " (2 tasks)."} {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

// - Verify the new plan is shown in full (its file without the front matter) before the summary and the question
// - Verify the planner's reply text (here the plan as JSON) is not printed, and is printed with -debug
func Test_Unit_PlanMode_ShowsPlanBodyNotPlannerText(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v", debug), func(t *testing.T) {
			fx, _, path := newFlagPlanFixture(t, countObjective)
			if debug {
				fx.Session.Output = newPlanOutput(fx.Out, true)
			}
			call := proposeCall("call_1", validPlanArgs)
			call.Content = "```json\n{\"title\": \"Word counter\"}\n```"
			fx.Provider.Script = []model.ChatMessage{call}
			if err := runPlanModeWith(t, fx, "y\n"); err != nil {
				t.Fatalf("planMode: %v", err)
			}
			out := fx.Out.String()
			body := acceptedPlan(t, path, countObjective, validPlanArgs).Body()
			bodyAt, summaryAt, askAt := strings.Index(out, body), strings.Index(out, "Plan 1 (Word counter)"), strings.Index(out, "Accept this plan?")
			if bodyAt < 0 || !(bodyAt < summaryAt && summaryAt < askAt) {
				t.Errorf("want the plan body, then the summary, then the question:\n%s", out)
			}
			if strings.Contains(out, "objective_hash") {
				t.Errorf("output shows the front matter:\n%s", out)
			}
			if got := strings.Contains(out, "```json"); got != debug {
				t.Errorf("planner text shown = %v, want %v:\n%s", got, debug, out)
			}
		})
	}
}

// - Verify declining the plan, then declining the chat, or Ctrl+D at Accept, the chat question or the change question writes nothing
// - Verify each prints the No-plan-written line and returns no error, and a blank change answer asks again
func Test_Unit_PlanMode_Decline_NoPlanWritten(t *testing.T) {
	tests := []struct {
		name, input string
		wantAsked   []string
	}{
		{"n then n", "n\nn\n", []string{"Enter chat mode to discuss and revise the plan (Y/n)? : "}},
		{"no then no", "no\nno\n", nil},
		{"EOF at Accept", "", []string{"Accept this plan? [Y/n]: "}},
		{"EOF at the chat question", "n\n", nil},
		{"EOF at the change question", "n\ny\n", []string{"What would you like to change in the plan? "}},
		{"blank change answer asks again, then EOF", "n\n\n  \n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, _, path := newFlagPlanFixture(t, countObjective)
			fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs)}
			if err := runPlanModeWith(t, fx, tt.input); err != nil {
				t.Fatalf("planMode: %v", err)
			}
			if _, err := os.Stat(plan.PathFor(path)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a plan file was written (%v)", err)
			}
			out := fx.Out.String()
			for _, w := range append(tt.wantAsked, noPlanLine(path)) {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if len(fx.Provider.Calls) != 1 {
				t.Errorf("expected only the planner call, got %d", len(fx.Provider.Calls))
			}
		})
	}
}

// - Verify a blank answer to the change question asks it again
func Test_Unit_PlanMode_Decline_BlankChangeAsksAgain(t *testing.T) {
	fx, _, _ := newFlagPlanFixture(t, countObjective)
	fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs)}
	if err := runPlanModeWith(t, fx, "n\ny\n\n  \n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if n := strings.Count(fx.Out.String(), "What would you like to change in the plan? "); n != 3 {
		t.Errorf("change question asked %d times, want 3:\n%s", n, fx.Out.String())
	}
}

// - Verify n, y, a change, a chat where the model calls update_objective, /save, y to "Save over" and y writes a plan for the revised objective
// - Verify the reopened chat's request is the system message plus one user message, RevisionNote(turned-down plan, change)
// - Verify no request has two user messages in a row, the objective file is rewritten, and the second planner request has only the revised objective
func Test_Unit_PlanMode_Revise_ThenAccept(t *testing.T) {
	fx, pd, path := newFlagPlanFixture(t, countObjective)
	revised := objective.Objective{ObjText: "Count lines in a file."}
	fx.Provider.Script = []model.ChatMessage{
		proposeCall("call_1", validPlanArgs),
		modeltest.AssistantToolCall("call_2", "update_objective", `{"objective":"Count lines in a file."}`),
		modeltest.AssistantText("Updated."),
		proposeCall("call_3", oneTaskPlanArgs),
	}
	if err := runPlanModeWith(t, fx, "n\ny\n\nlines, not words\n/save\ny\ny\n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if len(fx.Provider.Calls) != 4 {
		t.Fatalf("expected 4 model calls (planner, 2 chat, planner), got %d", len(fx.Provider.Calls))
	}
	checkPlannerRequest(t, fx, 0, path, countObjective)
	checkPlannerRequest(t, fx, 3, path, revised)
	checkNoConsecutiveUserMessages(t, fx)

	chat := fx.Provider.Calls[1]
	wantNote := planner.RevisionNote(acceptedPlan(t, path, countObjective, validPlanArgs), "lines, not words")
	if len(chat) != 2 || chat[0].Role != "system" || chat[1].Role != "user" || chat[1].Content != wantNote {
		t.Errorf("chat request = %+v, want the system message and one user message %q", chat, wantNote)
	}
	if got := toolNames(fx.Provider.ToolDefs[1]); !slices.Contains(got, "update_objective") || slices.Contains(got, "propose_plan") {
		t.Errorf("chat tools = %v, want update_objective and no propose_plan", got)
	}

	if *pd.Objective != revised {
		t.Errorf("draft = %+v, want %+v", *pd.Objective, revised)
	}
	if data, _ := os.ReadFile(path); string(data) != revised.Markdown() {
		t.Errorf("objective file = %q, want %q", data, revised.Markdown())
	}
	planPath := plan.PathFor(path)
	p := readPlanFile(t, planPath)
	if p.ObjectiveHash != revised.Hash() || p.Title != "Line counter" || len(p.Tasks) != 1 {
		t.Errorf("plan = %+v, want the one-task plan for the revised objective", p)
	}
	out := fx.Out.String()
	if n := strings.Count(out, "Save over "+path+"? (y/N): "); n != 1 {
		t.Errorf("Save over asked %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "Accept this plan? [Y/n]: "); n != 2 {
		t.Errorf("Accept asked %d times, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "Wrote "+planPath+" (1 tasks).") {
		t.Errorf("output lacks the wrote line:\n%s", out)
	}
}

// - Verify leaving the revise chat with /exit, without /save, writes no plan and prints the No-plan-written line
func Test_Unit_PlanMode_Revise_ExitWithoutSave(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs), modeltest.AssistantText("What should change?")}
	if err := runPlanModeWith(t, fx, "n\ny\nsomething\n/exit\n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if _, err := os.Stat(plan.PathFor(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a plan file was written (%v)", err)
	}
	if data, _ := os.ReadFile(path); string(data) != countObjective.Markdown() {
		t.Errorf("objective file changed: %q", data)
	}
	if !strings.Contains(fx.Out.String(), noPlanLine(path)) {
		t.Errorf("output lacks the no-plan line:\n%s", fx.Out.String())
	}
	if len(fx.Provider.Calls) != 2 {
		t.Errorf("expected 2 model calls, got %d", len(fx.Provider.Calls))
	}
}

// - Verify n to "Save over <file>? (y/N): " prints "Not saved; ...", writes nothing and the chat goes on; /exit then leaves the file unchanged
// - Verify after a no, the next /save asks again, and yes writes the revised objective and plans it
func Test_Unit_PlanMode_Revise_SaveOverDeclined(t *testing.T) {
	tests := []struct {
		name, input string
		wantAsked   int
		saved       bool
	}{
		{"no then exit", "n\ny\nlines, not words\n/save\nn\n/exit\n", 1, false},
		{"no then yes", "n\ny\nlines, not words\n/save\nno\n/save\ny\ny\n", 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, _, path := newFlagPlanFixture(t, countObjective)
			revised := objective.Objective{ObjText: "Count lines in a file."}
			fx.Provider.Script = []model.ChatMessage{
				proposeCall("call_1", validPlanArgs),
				modeltest.AssistantToolCall("call_2", "update_objective", `{"objective":"Count lines in a file."}`),
				modeltest.AssistantText("Updated."),
				proposeCall("call_3", oneTaskPlanArgs),
			}
			if err := runPlanModeWith(t, fx, tt.input); err != nil {
				t.Fatalf("planMode: %v", err)
			}
			out := fx.Out.String()
			if n := strings.Count(out, "Save over "+path+"? (y/N): "); n != tt.wantAsked {
				t.Errorf("Save over asked %d times, want %d:\n%s", n, tt.wantAsked, out)
			}
			if !strings.Contains(out, "Not saved; tell me what to change, or /save again.") {
				t.Errorf("output lacks the not-saved line:\n%s", out)
			}
			data, _ := os.ReadFile(path)
			if !tt.saved {
				if string(data) != countObjective.Markdown() {
					t.Errorf("objective file changed after no: %q", data)
				}
				if _, err := os.Stat(plan.PathFor(path)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a plan file was written (%v)", err)
				}
				return
			}
			if string(data) != revised.Markdown() {
				t.Errorf("objective file = %q, want %q", data, revised.Markdown())
			}
			if p := readPlanFile(t, plan.PathFor(path)); p.ObjectiveHash != revised.Hash() || len(p.Tasks) != 1 {
				t.Errorf("plan = %+v, want the one-task plan for the revised objective", p)
			}
		})
	}
}

// - Verify an objective given to -objective as text (saved to a new file this run) is revised and saved without "Save over"
func Test_Unit_PlanMode_Revise_TextObjectiveNeverAsksSaveOver(t *testing.T) {
	t.Chdir(t.TempDir())
	fx, pd := newPlanFixture(t)
	fx.Session.Config.ObjectiveFile = "Count words in a file."
	revised := objective.Objective{ObjText: "Count lines in a file."}
	fx.Provider.Script = []model.ChatMessage{
		proposeCall("call_1", validPlanArgs),
		modeltest.AssistantToolCall("call_2", "update_objective", `{"objective":"Count lines in a file."}`),
		modeltest.AssistantText("Updated."),
		proposeCall("call_3", oneTaskPlanArgs),
	}
	if err := runPlanModeWith(t, fx, "n\ny\nlines, not words\n/save\ny\n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if pd.confirmSave != nil {
		t.Error("confirmSave set for an objective saved from text")
	}
	if out := fx.Out.String(); strings.Contains(out, "Save over") {
		t.Errorf("asked to save over a file made this run:\n%s", out)
	}
	if data, _ := os.ReadFile("objective.md"); string(data) != revised.Markdown() {
		t.Errorf("objective.md = %q, want %q", data, revised.Markdown())
	}
	if p := readPlanFile(t, "objective.plan.md"); p.ObjectiveHash != revised.Hash() || len(p.Tasks) != 1 {
		t.Errorf("plan = %+v, want the one-task plan for the revised objective", p)
	}
}

// - Verify a second revision after a confirmed "Save over" saves without asking again, and no request has two user messages in a row
func Test_Unit_PlanMode_Revise_Twice_AsksSaveOverOnce(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	final := objective.Objective{ObjText: "Count lines and words."}
	fx.Provider.Script = []model.ChatMessage{
		proposeCall("call_1", validPlanArgs),
		modeltest.AssistantToolCall("call_2", "update_objective", `{"objective":"Count lines in a file."}`),
		modeltest.AssistantText("Updated."),
		proposeCall("call_3", oneTaskPlanArgs),
		modeltest.AssistantToolCall("call_4", "update_objective", `{"objective":"Count lines and words."}`),
		modeltest.AssistantText("Updated again."),
		proposeCall("call_5", validPlanArgs),
	}
	input := "n\ny\nlines, not words\n/save\ny\n" + "n\ny\nwords too\n/save\ny\n"
	if err := runPlanModeWith(t, fx, input); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if len(fx.Provider.Calls) != 7 {
		t.Fatalf("expected 7 model calls, got %d", len(fx.Provider.Calls))
	}
	checkNoConsecutiveUserMessages(t, fx)
	out := fx.Out.String()
	if n := strings.Count(out, "Save over "+path+"? (y/N): "); n != 1 {
		t.Errorf("Save over asked %d times, want 1:\n%s", n, out)
	}
	if data, _ := os.ReadFile(path); string(data) != final.Markdown() {
		t.Errorf("objective file = %q, want %q", data, final.Markdown())
	}
	if p := readPlanFile(t, plan.PathFor(path)); p.ObjectiveHash != final.Hash() || len(p.Tasks) != 2 {
		t.Errorf("plan = %+v, want the two-task plan for the final objective", p)
	}
}

// - Verify a new objective written over objective.md keeps the old objective.plan.md byte-identical when the plan is turned down
// - Verify accepting the new plan replaces the old plan file with one made from the new objective
// - Verify /save of a new objective never asks "Save over" (the overwrite was agreed at the name question)
func Test_Unit_PlanMode_NewObjective_OldPlanKeptUntilAccepted(t *testing.T) {
	tests := []struct {
		name, answers string
		replaced      bool
	}{
		{"declined", "n\nn\n", false},
		{"accepted", "y\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			old := []byte("old plan\n")
			if err := os.WriteFile("objective.md", []byte("Old goal.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("objective.plan.md", old, 0o644); err != nil {
				t.Fatal(err)
			}
			fx, _ := newPlanFixture(t)
			fx.Provider.Script = []model.ChatMessage{
				modeltest.AssistantToolCall("call_1", "update_objective", `{"objective":"Count words in a file."}`),
				modeltest.AssistantText("Ready."),
				proposeCall("call_2", validPlanArgs),
			}
			if err := runPlanModeWith(t, fx, "objective.md\ny\ncount words\n/save\n"+tt.answers); err != nil {
				t.Fatalf("planMode: %v", err)
			}
			if strings.Contains(fx.Out.String(), "Save over") {
				t.Errorf("a new objective asked Save over:\n%s", fx.Out.String())
			}
			if len(fx.Provider.Calls) != 3 {
				t.Fatalf("expected 3 model calls, got %d", len(fx.Provider.Calls))
			}
			checkPlannerRequest(t, fx, 2, "objective.md", countObjective)
			data, _ := os.ReadFile("objective.plan.md")
			if !tt.replaced {
				if !bytes.Equal(data, old) {
					t.Errorf("old plan changed to %q", data)
				}
				if !strings.Contains(fx.Out.String(), noPlanLine("objective.md")) {
					t.Errorf("output lacks the no-plan line:\n%s", fx.Out.String())
				}
				return
			}
			if p := readPlanFile(t, "objective.plan.md"); p.IsStale(countObjective.Hash()) {
				t.Errorf("new plan is stale: %+v", p)
			}
		})
	}
}

// ---- runPlanner ----

//   - Verify a planner that replies with text and never calls propose_plan is reminded plannerReminders times, then is
//     the error "stopped without proposing a plan" showing its last reply and how to plan again, with nothing written
//   - Verify each reminder is one user message after the planner's reply (never two user messages in a row)
func Test_Unit_PlanMode_PlannerNoProposal(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	fx.Session.Config.AssumeYes = true
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantText("I'll now call propose_plan."),
		modeltest.AssistantText("Calling it now."),
		modeltest.AssistantText("Here is my plan:\ndo it."),
	}
	err := runPlanModeWith(t, fx, "")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"stopped without proposing a plan", "reminded 2 times", "Its last reply:\n  Here is my plan:\n  do it.",
		"-mode plan -objective " + path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q:\n%v", want, err)
		}
	}
	if len(fx.Provider.Calls) != 1+plannerReminders {
		t.Fatalf("model calls = %d, want %d", len(fx.Provider.Calls), 1+plannerReminders)
	}
	last := fx.Provider.Calls[plannerReminders]
	if m := last[len(last)-1]; m.Role != "user" || m.Content != planner.ProposeReminder {
		t.Errorf("last request ends with %s %q, want the reminder", m.Role, m.Content)
	}
	checkNoConsecutiveUserMessages(t, fx)
	if _, err := os.Stat(plan.PathFor(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a plan file was written (%v)", err)
	}
}

// - Verify a planner that replies with text, then calls propose_plan after one reminder, gets its plan written
func Test_Unit_PlanMode_PlannerRemindedThenProposes(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	fx.Session.Config.AssumeYes = true
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("I'll now call propose_plan."), proposeCall("call_1", validPlanArgs)}
	if err := runPlanModeWith(t, fx, ""); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if len(fx.Provider.Calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(fx.Provider.Calls))
	}
	second := fx.Provider.Calls[1]
	if n := len(second); n < 2 || second[n-2].Role != "assistant" || second[n-1].Content != planner.ProposeReminder {
		t.Errorf("second request does not end with the planner's reply then the reminder: %+v", second)
	}
	if p := readPlanFile(t, plan.PathFor(path)); len(p.Tasks) != 2 {
		t.Errorf("plan = %+v, want 2 tasks", p)
	}
}

// - Verify plannerNoPlanError shows "(no text)" for an empty reply and cuts a long one with " …"
func Test_Unit_PlannerNoPlanError_EmptyAndLongReply(t *testing.T) {
	if err := plannerNoPlanError("  ", 2, "goal.md"); !strings.Contains(err.Error(), "Its last reply:\n  (no text)\n") {
		t.Errorf("empty reply: %v", err)
	}
	long := strings.Repeat("x", plannerReplyMax+10)
	err := plannerNoPlanError(long, 2, "goal.md").Error()
	if !strings.Contains(err, strings.Repeat("x", plannerReplyMax)+" …") || strings.Contains(err, strings.Repeat("x", plannerReplyMax+1)) {
		t.Errorf("long reply not cut at %d characters:\n%s", plannerReplyMax, err)
	}
}

// - Verify a propose_plan call that fails the checks goes back to the model as an error, and the retried valid plan is accepted after 2 model calls
func Test_Unit_PlanMode_ProposeRefusedThenRetried(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	fx.Session.Config.AssumeYes = true
	bad := `{"title":"Word counter","tasks":[{"id":"x1","title":"Write wc.go","needs":"write files","done_when":"wc.go exists"}]}`
	fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", bad), proposeCall("call_2", validPlanArgs)}
	if err := runPlanModeWith(t, fx, ""); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if len(fx.Provider.Calls) != 2 {
		t.Fatalf("expected 2 model calls (refused, retried), got %d", len(fx.Provider.Calls))
	}
	retry := fx.Provider.Calls[1]
	if last := retry[len(retry)-1]; last.Role != "tool" || !strings.Contains(last.Content, "call propose_plan again") {
		t.Errorf("retry request's last message = %s %q, want the refusal as a tool result", last.Role, last.Content)
	}
	if p := readPlanFile(t, plan.PathFor(path)); len(p.Tasks) != 2 {
		t.Errorf("plan = %+v, want the retried two-task plan", p)
	}
}

// - Verify the planner's propose_plan uses the session's maxPlanTasks: with 1, a 2-task plan is refused naming the limit
// - Verify its description states that limit, and the retried 1-task plan is written
func Test_Unit_PlanMode_PlannerUsesMaxPlanTasks(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	fx.Session.Config.AssumeYes = true
	fx.Session.Config.MaxPlanTasks = 1
	fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs), proposeCall("call_2", oneTaskPlanArgs)}
	if err := runPlanModeWith(t, fx, ""); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if len(fx.Provider.Calls) != 2 {
		t.Fatalf("expected 2 model calls (refused, retried), got %d", len(fx.Provider.Calls))
	}
	for _, d := range fx.Provider.ToolDefs[0] {
		if d.Function.Name == "propose_plan" && !strings.Contains(d.Function.Desc, "at most 1 tasks") {
			t.Errorf("propose_plan description lacks the limit: %s", d.Function.Desc)
		}
	}
	retry := fx.Provider.Calls[1]
	if last := retry[len(retry)-1]; last.Role != "tool" || !strings.Contains(last.Content, "the limit is 1") {
		t.Errorf("retry request's last message = %s %q, want the refusal naming the limit", last.Role, last.Content)
	}
	if p := readPlanFile(t, plan.PathFor(path)); len(p.Tasks) != 1 {
		t.Errorf("plan = %+v, want the one-task plan", p)
	}
}

// - Verify Ctrl+C while the planner waits on the model prints "Planning interrupted." and the No-plan-written line, writes nothing and returns nil
func Test_Unit_PlanMode_PlannerInterrupted(t *testing.T) {
	fx, _, path := newFlagPlanFixture(t, countObjective)
	fx.Session.Config.AssumeYes = true
	fx.Provider.Script = []model.ChatMessage{proposeCall("call_1", validPlanArgs)}
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	interrupts := make(chan os.Signal, 1)

	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = planMode(t.Context(), interrupts, fx.Session, plainReaderFor(t, ""))
	}()
	receiveOrFail(t, fx.Provider.Waiting, "the planner to reach the model")
	interrupts <- os.Interrupt
	receiveOrFail(t, done, "planMode to return")

	if err != nil {
		t.Errorf("planMode: %v, want nil", err)
	}
	out := fx.Out.String()
	for _, w := range []string{"Planning interrupted.", noPlanLine(path)} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if _, err := os.Stat(plan.PathFor(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a plan file was written (%v)", err)
	}
}
