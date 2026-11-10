package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nyaosorg/go-readline-ny/keys"
	"github.com/nyaosorg/go-ttyadapter/auto"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/objective"
)

// newPlanFixture returns a chat session fixture with plan mode set up on it (setupPlanMode, no base tools).
func newPlanFixture(t *testing.T) (*chatSessionFixture, *planModeDetails) {
	t.Helper()
	fx := newChatSessionFixture(t, 10, 100)
	if err := setupPlanMode(fx.Session, nil); err != nil {
		t.Fatalf("setupPlanMode: %v", err)
	}
	return fx, fx.Session.ModeDetails.(*planModeDetails)
}

// runPlanModeWith runs planMode on fx with input as the typed lines (a plainReader) and returns its error.
func runPlanModeWith(t *testing.T, fx *chatSessionFixture, input string) error {
	t.Helper()
	ctx := t.Context()
	return planMode(ctx, nil, fx.Session, newPlainReader(ctx, strings.NewReader(input), fx.Out))
}

// plainReaderFor returns a plainReader over input that writes its prompts to io.Discard.
func plainReaderFor(t *testing.T, input string) *plainReader {
	t.Helper()
	return newPlainReader(t.Context(), strings.NewReader(input), io.Discard)
}

// planWiringConfig is denyWiringConfig with plan mode's defaults: a SetupAgent config that needs no network.
func planWiringConfig(t *testing.T, out io.Writer) config.Config {
	t.Helper()
	cfg := denyWiringConfig(t)
	plan := config.GetDefaultConfig(config.ModePlan)
	cfg.AgentMode, cfg.SystemPrompt, cfg.BuiltinTools = plan.AgentMode, plan.SystemPrompt, plan.BuiltinTools
	cfg.OutBuffer = out
	return cfg
}

// toolNames returns the names of tools' definitions, sorted.
func toolNames(defs []model.ToolDef) []string {
	var names []string
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	slices.Sort(names)
	return names
}

// ---- runPlanMode, SetupAgent ----

// - Verify plan mode without -objective refuses to start without a terminal (here stdin is a buffer)
func Test_Unit_RunPlanMode_NeedsTerminal(t *testing.T) {
	fx, _ := newPlanFixture(t)
	fx.Session.InBuffer = strings.NewReader("x\n")
	err := runPlanMode(t.Context(), nil, fx.Session)
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("expected a needs-a-terminal error, got %v", err)
	}
	if fx.Out.Len() != 0 {
		t.Errorf("expected nothing printed, got:\n%s", fx.Out.String())
	}
}

// - Verify planModeDetails names its mode "plan"
func Test_Unit_PlanModeDetails_ModeName(t *testing.T) {
	if got := (&planModeDetails{}).ModeName(); got != config.ModePlan {
		t.Errorf("ModeName() = %q, want %q", got, config.ModePlan)
	}
}

// - Verify SetupAgent in plan mode puts a *planModeDetails with an empty draft on the session
// - Verify the registry holds plan's default built-in tools plus update_objective, and plannerTools holds the built-ins only
// - Verify calling the registered update_objective changes the session's draft (the same pointer) and prints the draft
func Test_Unit_SetupAgent_PlanMode_AddsDetailsAndUpdateObjective(t *testing.T) {
	var out bytes.Buffer
	a := &Agent{}
	if err := a.SetupAgent(context.Background(), planWiringConfig(t, &out), nil); err != nil {
		t.Fatalf("SetupAgent: %v", err)
	}
	t.Cleanup(func() { a.ShutdownAgent(context.Background()) })

	pd, ok := a.session.ModeDetails.(*planModeDetails)
	if !ok {
		t.Fatalf("ModeDetails = %T, want *planModeDetails", a.session.ModeDetails)
	}
	if pd.Objective == nil || *pd.Objective != (objective.Objective{}) {
		t.Errorf("draft = %+v, want an empty objective", pd.Objective)
	}
	if got, want := toolNames(a.session.Tools.GetToolDefs()), []string{"list_files", "read_file", "search_files", "update_objective"}; !slices.Equal(got, want) {
		t.Errorf("registered tools = %v, want %v", got, want)
	}
	var plannerDefs []model.ToolDef
	for _, tool := range pd.plannerTools {
		plannerDefs = append(plannerDefs, tool.GetToolDefinition())
	}
	if got, want := toolNames(plannerDefs), []string{"list_files", "read_file", "search_files"}; !slices.Equal(got, want) {
		t.Errorf("plannerTools = %v, want %v", got, want)
	}

	tool, ok := a.session.Tools.Lookup("update_objective")
	if !ok {
		t.Fatal("update_objective is not registered")
	}
	draft := pd.Objective
	if _, err := tool.CallTool(context.Background(), json.RawMessage(`{"objective":"Count words."}`)); err != nil {
		t.Fatalf("update_objective: %v", err)
	}
	if pd.Objective != draft || draft.ObjText != "Count words." {
		t.Errorf("draft = %+v (same pointer: %v), want ObjText %q", *pd.Objective, pd.Objective == draft, "Count words.")
	}
	if !strings.Contains(out.String(), "Here is the objective") || !strings.Contains(out.String(), "# Objective\nCount words.\n") {
		t.Errorf("the draft was not printed:\n%s", out.String())
	}
}

// - Verify SetupAgent in chat mode leaves ModeDetails nil and registers no update_objective
func Test_Unit_SetupAgent_ChatMode_NoModeDetails(t *testing.T) {
	a := &Agent{}
	if err := a.SetupAgent(context.Background(), denyWiringConfig(t), nil); err != nil {
		t.Fatalf("SetupAgent: %v", err)
	}
	t.Cleanup(func() { a.ShutdownAgent(context.Background()) })
	if a.session.ModeDetails != nil {
		t.Errorf("ModeDetails = %T, want nil", a.session.ModeDetails)
	}
	if _, ok := a.session.Tools.Lookup("update_objective"); ok {
		t.Error("update_objective is registered in chat mode")
	}
}

// ---- the name and objective questions ----

// - Verify newObjectivePath defaults to objective.md, adds .md when missing, keeps .MD, and cleans the path
// - Verify a missing parent directory is the error "No such directory"
func Test_Unit_NewObjectivePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("file", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		answer, want, wantErr string
	}{
		{answer: "", want: "objective.md"},
		{answer: "   ", want: "objective.md"},
		{answer: "notes", want: "notes.md"},
		{answer: " notes.md ", want: "notes.md"},
		{answer: "x.MD", want: "x.MD"},
		{answer: "sub/plan", want: filepath.Join("sub", "plan.md")},
		{answer: "./sub/../a", want: "a.md"},
		{answer: filepath.Join(dir, "abs"), want: filepath.Join(dir, "abs.md")},
		{answer: "missing/x", wantErr: "No such directory: missing"},
		{answer: "file/x", wantErr: "No such directory: file"},
	}
	for _, tt := range tests {
		got, err := newObjectivePath(tt.answer)
		if tt.wantErr != "" {
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("newObjectivePath(%q) = (%q, %v), want error %q", tt.answer, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("newObjectivePath(%q) = (%q, %v), want %q", tt.answer, got, err, tt.want)
		}
	}
}

// - Verify askObjectivePath accepts the default, asks again after a refused overwrite, a directory or a missing directory
// - Verify an accepted overwrite returns the existing file's path
func Test_Unit_AskObjectivePath(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("taken.md", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("dir.md", 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, input, want string
		wantOut           []string
	}{
		{"default", "\n", "objective.md", nil},
		{"existing file, n asks for the name again", "taken\nn\nfresh\n", "fresh.md", []string{"taken.md already exists. Overwrite (y/N)?"}},
		{"existing file, blank is no", "taken\n\nfresh\n", "fresh.md", nil},
		{"existing file, y accepts", "taken\ny\n", "taken.md", nil},
		{"existing file, YES accepts", "taken\nYES\n", "taken.md", nil},
		{"a directory asks again", "dir.md\nfresh\n", "fresh.md", []string{"dir.md is a directory"}},
		{"a missing directory asks again", "nope/x\nfresh\n", "fresh.md", []string{"No such directory: nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := askObjectivePath(t.Context(), newPlainReader(t.Context(), strings.NewReader(tt.input), &out), &out)
			if err != nil || got != tt.want {
				t.Errorf("askObjectivePath() = (%q, %v), want %q", got, err, tt.want)
			}
			for _, w := range tt.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
		})
	}
}

// - Verify end of input at the name question and at the overwrite question returns io.EOF
func Test_Unit_AskObjectivePath_EOF(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("taken.md", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{"at the name": "", "at the overwrite question": "taken\n", "after asking again": "taken\nn\n"} {
		if _, err := askObjectivePath(t.Context(), plainReaderFor(t, input), io.Discard); !errors.Is(err, io.EOF) {
			t.Errorf("%s: err = %v, want io.EOF", name, err)
		}
	}
}

// - Verify askObjectiveText asks again after a blank answer and returns the answer trimmed
// - Verify end of input returns io.EOF
func Test_Unit_AskObjectiveText(t *testing.T) {
	got, err := askObjectiveText(t.Context(), plainReaderFor(t, "\n   \n  Count words  \n"))
	if err != nil || got != "Count words" {
		t.Errorf("askObjectiveText() = (%q, %v), want %q", got, err, "Count words")
	}
	if _, err := askObjectiveText(t.Context(), plainReaderFor(t, "  \n")); !errors.Is(err, io.EOF) {
		t.Errorf("end of input: err = %v, want io.EOF", err)
	}
}

// - Verify isYes accepts y and yes in any case, uses def for blank, and treats anything else as no
func Test_Unit_IsYes(t *testing.T) {
	tests := []struct {
		answer string
		def    bool
		want   bool
	}{
		{"y", false, true}, {"Y", false, true}, {" yes ", false, true}, {"YeS", false, true},
		{"", false, false}, {"  ", true, true}, {"", true, true},
		{"n", true, false}, {"no", true, false}, {"yep", false, false}, {"y es", false, false},
	}
	for _, tt := range tests {
		if got := isYes(tt.answer, tt.def); got != tt.want {
			t.Errorf("isYes(%q, %v) = %v, want %v", tt.answer, tt.def, got, tt.want)
		}
	}
}

// - Verify writeFileAtomic creates a file, replaces an existing one, sets mode 0644 and leaves no temporary file
func Test_Unit_WriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "objective.md")
	for _, data := range []string{"first\n", "second\n"} {
		if err := writeFileAtomic(path, []byte(data)); err != nil {
			t.Fatalf("writeFileAtomic: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Errorf("file = (%q, %v), want %q", got, err, data)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v (%v), want 0644", info.Mode().Perm(), err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("expected only the file in the directory, got %v", entries)
		}
	}
}

// - Verify writeFileAtomic into a missing directory is an error and creates nothing
func Test_Unit_WriteFileAtomic_MissingDir(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileAtomic(filepath.Join(dir, "nope", "x.md"), []byte("x")); err == nil {
		t.Error("expected an error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected nothing created, got %v", entries)
	}
}

// ---- the objective chat (planMode) ----

// - Verify the whole objective chat: the first answer is the first model request's user message
// - Verify the model's update_objective call changes the draft and shows it, and /save writes draft.Markdown() and ends plan mode
func Test_Unit_PlanMode_ObjectiveChat_Save(t *testing.T) {
	dir := t.TempDir()
	fx, pd := newPlanFixture(t)
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "update_objective", `{"objective":"Count words in a file.","context":"A CLI."}`),
		modeltest.AssistantText("Anything else?"),
	}
	input := filepath.Join(dir, "goal") + "\n  I want a word counter  \n/save\n"
	if err := runPlanModeWith(t, fx, input); err != nil {
		t.Fatalf("planMode: %v", err)
	}

	path := filepath.Join(dir, "goal.md")
	if pd.ObjectivePath != path || !pd.saved {
		t.Errorf("ObjectivePath = %q, saved = %v; want %q, true", pd.ObjectivePath, pd.saved, path)
	}
	want := objective.Objective{ObjText: "Count words in a file.", Context: "A CLI."}
	if *pd.Objective != want {
		t.Errorf("draft = %+v, want %+v", *pd.Objective, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want.Markdown() {
		t.Errorf("saved file = (%q, %v), want %q", got, err, want.Markdown())
	}

	if len(fx.Provider.Calls) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(fx.Provider.Calls))
	}
	first := fx.Provider.Calls[0]
	if last := first[len(first)-1]; last.Role != "user" || last.Content != "I want a word counter" {
		t.Errorf("first request's last message = %s %q, want the first answer as a user message", last.Role, last.Content)
	}
	out := fx.Out.String()
	for _, w := range []string{
		"What is your objective?",
		"Type /save to save the objective",
		"Here is the objective (" + path + ", not saved yet):\n\n" + want.Markdown(),
		planFooter,
		"Saved " + path + ".",
		"Making the plan from the saved objective is not built yet",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// - Verify /exit leaves without writing and says no objective was saved
func Test_Unit_PlanMode_ObjectiveChat_Exit(t *testing.T) {
	dir := t.TempDir()
	fx, pd := newPlanFixture(t)
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantToolCall("call_1", "update_objective", `{"objective":"Count words."}`),
		modeltest.AssistantText("ok"),
	}
	if err := runPlanModeWith(t, fx, filepath.Join(dir, "goal")+"\ncount words\n/exit\n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if pd.saved {
		t.Error("saved is true after /exit")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
	if out := fx.Out.String(); !strings.Contains(out, "No objective saved; nothing written.") || strings.Contains(out, "not built yet") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

// - Verify end of input at the name question or the objective question cancels with nothing written and no error
func Test_Unit_PlanMode_CancelAtQuestions(t *testing.T) {
	for name, input := range map[string]string{"at the name": "", "at the objective": "goal\n"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			fx, _ := newPlanFixture(t)
			if err := runPlanModeWith(t, fx, input); err != nil {
				t.Fatalf("planMode: %v", err)
			}
			if !strings.Contains(fx.Out.String(), "Cancelled; nothing written.") {
				t.Errorf("output lacks the cancel line:\n%s", fx.Out.String())
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("files written: %v", entries)
			}
			if len(fx.Provider.Calls) != 0 {
				t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
			}
		})
	}
}

// - Verify /save with an empty draft says there is nothing to save, writes nothing, and the chat goes on
func Test_Unit_PlanMode_SaveEmptyDraft(t *testing.T) {
	dir := t.TempDir()
	fx, pd := newPlanFixture(t)
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("What should it do?"), modeltest.AssistantText("ok")}
	if err := runPlanModeWith(t, fx, filepath.Join(dir, "goal")+"\nsomething\n/save\nmore\n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	if pd.saved {
		t.Error("saved is true for an empty draft")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
	if !strings.Contains(fx.Out.String(), "Nothing to save yet") {
		t.Errorf("output lacks the nothing-to-save notice:\n%s", fx.Out.String())
	}
	if len(fx.Provider.Calls) != 2 {
		t.Errorf("expected the chat to go on after /save (2 model calls), got %d", len(fx.Provider.Calls))
	}
}

// - Verify planMode with -objective set returns the not-built-yet error before asking anything
func Test_Unit_PlanMode_ObjectiveFlagNotBuilt(t *testing.T) {
	fx, _ := newPlanFixture(t)
	fx.Session.Config.ObjectiveFile = "objective.md"
	err := runPlanModeWith(t, fx, "goal\n")
	if err == nil || !strings.Contains(err.Error(), "not built yet") {
		t.Errorf("err = %v, want a not-built-yet error", err)
	}
	if strings.Contains(fx.Out.String(), "Name or path") {
		t.Errorf("asked a question:\n%s", fx.Out.String())
	}
}

// - Verify planMode without plan mode details on the session is an error
func Test_Unit_PlanMode_NotSetUp(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	ctx := t.Context()
	if err := planMode(ctx, nil, fx.Session, newPlainReader(ctx, strings.NewReader(""), fx.Out)); err == nil {
		t.Error("expected an error")
	}
}

// ---- /edit ----

// - Verify /edit replaces the draft with valid text, shows it, and adds a user note to history and context
// - Verify /edit makes no model call
func Test_Unit_PlanMode_Edit_Applies(t *testing.T) {
	dir := t.TempDir()
	fx, pd := newPlanFixture(t)
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("ok")}
	if err := runPlanModeWith(t, fx, filepath.Join(dir, "goal")+"\nfirst\n/edit\nBuild a line counter.\n"); err != nil {
		t.Fatalf("planMode: %v", err)
	}
	want := objective.Objective{ObjText: "Build a line counter."}
	if *pd.Objective != want {
		t.Errorf("draft = %+v, want %+v", *pd.Objective, want)
	}
	if len(fx.Provider.Calls) != 1 {
		t.Errorf("expected only the first turn's model call, got %d", len(fx.Provider.Calls))
	}
	if !strings.Contains(fx.Out.String(), "Here is the objective") {
		t.Errorf("the draft was not shown:\n%s", fx.Out.String())
	}
	noteText := "I edited the objective draft myself. It is now:\n\n" + want.Markdown()
	for name, msgs := range map[string][]model.ChatMessage{"history": fx.History.GetMessages(), "context": fx.Context.GetMessages()} {
		if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" || msgs[len(msgs)-1].Content != noteText {
			t.Errorf("%s does not end with the edit note: %+v", name, msgs)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("/edit wrote files: %v", entries)
	}
}

// - Verify /edit text that objective.Parse refuses leaves the draft unchanged and says why
func Test_Unit_PlanMode_Edit_Refused(t *testing.T) {
	fx, pd := newPlanFixture(t)
	*pd.Objective = objective.Objective{ObjText: "Keep me."}
	historyBefore := len(fx.History.GetMessages())
	reader := plainReaderFor(t, "/edit\n# Objective\n")
	if err := chatLoop(t.Context(), nil, fx.Session, reader); err != nil {
		t.Fatalf("chatLoop: %v", err)
	}
	if pd.Objective.ObjText != "Keep me." {
		t.Errorf("draft changed to %+v", *pd.Objective)
	}
	if !strings.Contains(fx.Out.String(), "The edit was not applied") {
		t.Errorf("output lacks the refusal:\n%s", fx.Out.String())
	}
	if len(fx.History.GetMessages()) != historyBefore {
		t.Error("a refused edit added to the history")
	}
}

// - Verify a multi-line /edit through the terminal editor that adds a second # Objective section is refused
// - Verify a multi-line /edit that adds a # Context section is applied
func Test_Unit_PlanMode_Edit_TTY(t *testing.T) {
	tests := []struct {
		name    string
		added   []string
		want    objective.Objective
		wantOut string
	}{
		{"second objective section is refused", keystrokes([]string{keys.CtrlJ}, typed("# Objective"), []string{keys.CtrlJ}, typed("again")),
			objective.Objective{ObjText: "Keep me."}, "The edit was not applied"},
		{"context section is applied", keystrokes([]string{keys.CtrlJ}, typed("# Context"), []string{keys.CtrlJ}, typed("A CLI.")),
			objective.Objective{ObjText: "Keep me.", Context: "A CLI."}, "Here is the objective"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, pd := newPlanFixture(t)
			*pd.Objective = objective.Objective{ObjText: "Keep me."}
			keyin := keystrokes(typed("/edit"), []string{keys.CtrlM}, tt.added, []string{keys.CtrlM}, []string{keys.CtrlD})
			reader := newTTYReaderWith(&auto.Pilot{Text: keyin}, io.Discard)
			done := make(chan struct{})
			go func() {
				defer close(done)
				chatLoop(context.Background(), nil, fx.Session, reader)
			}()
			receiveOrFail(t, done, "the chat loop to end")
			if *pd.Objective != tt.want {
				t.Errorf("draft = %+v, want %+v", *pd.Objective, tt.want)
			}
			if !strings.Contains(fx.Out.String(), tt.wantOut) {
				t.Errorf("output lacks %q:\n%s", tt.wantOut, fx.Out.String())
			}
			if len(fx.Provider.Calls) != 0 {
				t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
			}
		})
	}
}

// - Verify end of input during /edit cancels it and leaves the draft unchanged
func Test_Unit_PlanMode_Edit_EOF(t *testing.T) {
	fx, pd := newPlanFixture(t)
	*pd.Objective = objective.Objective{ObjText: "Keep me."}
	if err := chatLoop(t.Context(), nil, fx.Session, plainReaderFor(t, "/edit\n")); err != nil {
		t.Fatalf("chatLoop: %v", err)
	}
	if pd.Objective.ObjText != "Keep me." {
		t.Errorf("draft changed to %+v", *pd.Objective)
	}
	if !strings.Contains(fx.Out.String(), "Edit cancelled") {
		t.Errorf("output lacks the cancel notice:\n%s", fx.Out.String())
	}
}

// - Verify /save and /edit are unrecognized commands in chat mode (no ModeDetails), with no model call
func Test_Unit_ChatMode_SaveAndEditUnrecognized(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	if err := chatLoop(t.Context(), nil, fx.Session, plainReaderFor(t, "/save\n/edit\n")); err != nil {
		t.Fatalf("chatLoop: %v", err)
	}
	for _, cmd := range []string{"/save", "/edit"} {
		if !strings.Contains(fx.Out.String(), "Unrecognized command: "+cmd) {
			t.Errorf("output lacks the notice for %s:\n%s", cmd, fx.Out.String())
		}
	}
	if len(fx.Provider.Calls) != 0 {
		t.Errorf("expected no model call, got %d", len(fx.Provider.Calls))
	}
}
