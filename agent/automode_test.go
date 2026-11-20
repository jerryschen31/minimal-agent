package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/objective"
)

// newAutoFixture returns a session fixture set up for auto mode, with the plan at planPath (not written) and a state
// folder in a new temp dir (outside the current directory). The session's provider is an autoProvider: worker calls go
// to fx.Provider, which replies "" unless a test sets its Script (so each worker finishes at once), and the director
// says task_done unless the test sets a director script (directorOf).
func newAutoFixture(t *testing.T, planPath string) (*chatSessionFixture, *autoModeDetails) {
	t.Helper()
	fx := newChatSessionFixture(t, 10, 100)
	fx.Session.Provider = &autoProvider{worker: fx.Provider}
	fx.Session.Config.MaxPlanTasks = config.GetDefaultConfig(config.ModeAuto).MaxPlanTasks
	fx.Session.Config.MaxTaskAttempts = config.GetDefaultConfig(config.ModeAuto).MaxTaskAttempts
	fx.Session.Config.PlanFile = planPath
	fx.Session.Config.StateDir = t.TempDir()
	fx.Session.Config.RedactionLevel = config.GetDefaultConfig(config.ModeAuto).RedactionLevel // the tool journal masks previews
	setupAutoMode(fx.Session, nil)
	return fx, fx.Session.ModeDetails.(*autoModeDetails)
}

// runAutoModeWith runs autoMode on fx with input as the typed lines (a plainReader whose prompts go to fx.Out), which
// answers both "Run?" and any gate, as on a terminal.
func runAutoModeWith(t *testing.T, fx *chatSessionFixture, input string) error {
	t.Helper()
	ctx := t.Context()
	reader := newPlainReader(ctx, strings.NewReader(input), fx.Out)
	return autoMode(ctx, nil, fx.Session, reader, reader)
}

// writeAutoPlan writes the plan propose_plan accepts for validPlanArgs to path in a new temp dir (also the cwd), and
// returns the path. objectiveFile names the objective in the plan's front matter; that file is never written.
func writeAutoPlan(t *testing.T, name, objectiveFile string) string {
	t.Helper()
	t.Chdir(t.TempDir())
	p := acceptedPlan(t, objectiveFile, countObjective, validPlanArgs)
	if err := os.WriteFile(name, []byte(p.Markdown()), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

// checkNothingWritten fails the test if the current directory holds anything but the files named.
func checkNothingWritten(t *testing.T, keep ...string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.Contains(strings.Join(keep, "\x00")+"\x00", e.Name()+"\x00") {
			t.Errorf("auto mode wrote %s", e.Name())
		}
	}
}

// - Verify autoModeDetails names its mode "auto"
func Test_Unit_AutoModeDetails_ModeName(t *testing.T) {
	if got := (&autoModeDetails{}).ModeName(); got != config.ModeAuto {
		t.Errorf("ModeName() = %q, want %q", got, config.ModeAuto)
	}
}

//   - Verify SetupAgent in auto mode puts *autoModeDetails with the -plan path on the session, uses autoOutput and
//     workerPrompt, and writes its trace in <stateDir>/traces (not traceDir)
func Test_Unit_SetupAgent_AutoMode(t *testing.T) {
	cfg := denyWiringConfig(t)
	auto := config.GetDefaultConfig(config.ModeAuto)
	cfg.AgentMode, cfg.AskRules, cfg.MaxPlanTasks, cfg.PlanFile = auto.AgentMode, auto.AskRules, auto.MaxPlanTasks, "run.md"
	cfg.MaxTaskAttempts = auto.MaxTaskAttempts
	cfg.StateDir, cfg.TraceEnabled, cfg.TraceDir = t.TempDir(), true, filepath.Join(t.TempDir(), "not-used")
	a := &Agent{}
	if err := a.SetupAgent(context.Background(), cfg, nil); err != nil {
		t.Fatalf("SetupAgent: %v", err)
	}
	t.Cleanup(func() { a.ShutdownAgent(context.Background()) })
	ad, ok := a.session.ModeDetails.(*autoModeDetails)
	if !ok || ad.PlanPath != "run.md" {
		t.Fatalf("ModeDetails = %#v, want *autoModeDetails with PlanPath run.md", a.session.ModeDetails)
	}
	if _, ok := a.session.Output.(autoOutput); !ok {
		t.Errorf("Output = %T, want autoOutput", a.session.Output)
	}
	if a.session.SystemMsg.Content != workerPrompt {
		t.Errorf("system prompt = %q, want workerPrompt", a.session.SystemMsg.Content)
	}
	if want := filepath.Join(cfg.StateDir, "traces"); a.session.Config.TraceDir != want {
		t.Errorf("TraceDir = %s, want %s", a.session.Config.TraceDir, want)
	}
	if _, err := os.Stat(cfg.TraceDir); err == nil {
		t.Errorf("auto mode created the configured traceDir %s", cfg.TraceDir)
	}
}

//   - Verify a plan with any file name and no objective file (the front matter names one that doesn't exist, and no
//     objective hash matches anything) is read, its summary shown, and "y" runs it: both tasks done, one model call each
//   - Verify the same for a hand-written plan with no front matter at all (no objective, hash or version)
func Test_Unit_AutoMode_RunsAnyNameWithoutObjective(t *testing.T) {
	for name, objectiveFile := range map[string]string{"objective file missing": "gone.md", "no front matter": ""} {
		t.Run(name, func(t *testing.T) {
			path := writeAutoPlan(t, "agent-setup-for-auto-run.md", objectiveFile)
			if objectiveFile == "" { // a hand-written plan: no front matter at all
				data, _ := os.ReadFile(path)
				_, body, _ := strings.Cut(strings.TrimPrefix(string(data), "---\n"), "---\n")
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(body, "objective:") || !strings.HasPrefix(strings.TrimSpace(body), "# Plan") {
					t.Fatalf("front matter not removed:\n%s", body)
				}
			}
			fx, ad := newAutoFixture(t, path)
			if err := runAutoModeWith(t, fx, "y\n"); err != nil {
				t.Fatalf("autoMode: %v", err)
			}
			out := fx.Out.String()
			for _, want := range []string{"+ t1", "Write wc.go", "Run? [Y/n]: ", "Finished: 2 done"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if len(ad.Plan.Tasks) != 2 || !strings.Contains(ad.Plan.Objective, countObjective.ObjText) {
				t.Errorf("plan on the session: %d tasks, objective %q", len(ad.Plan.Tasks), ad.Plan.Objective)
			}
			if len(fx.Provider.Calls) != 2 {
				t.Errorf("expected 2 model calls (one per task), got %d", len(fx.Provider.Calls))
			}
			checkNothingWritten(t, path)
		})
	}
}

//   - Verify each plan auto mode refuses is an error naming the file and the reason, asks nothing and writes nothing:
//     a missing file, a file Cleanup can't read, a file with no tasks, a plan CheckComplete refuses
func Test_Unit_AutoMode_Refusals(t *testing.T) {
	tests := []struct {
		name, content, want string // content "" = don't create the file
	}{
		{"missing", "", "no such file"},
		{"unreadable", "text before any heading\n", "can't be read as a plan"},
		{"not a plan", "# My notes\n\nSome notes.\n", "is not a plan"},
		{"not ready", "# Plan: x\n\n## Tasks\n\n### t1: a\ndepends: t9\n", "is not ready to run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := "notes.md"
			if tt.content != "" {
				if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fx, _ := newAutoFixture(t, path)
			err := runAutoModeWith(t, fx, "y\n")
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one naming %s and saying %q", err, path, tt.want)
			}
			if strings.Contains(fx.Out.String(), "Run?") {
				t.Errorf("asked to run a refused plan:\n%s", fx.Out.String())
			}
			if tt.content != "" {
				checkNothingWritten(t, path)
			} else {
				checkNothingWritten(t)
			}
		})
	}
}

// - Verify the fixes Cleanup makes to a hand-edited plan are printed (the file is not rewritten) and the plan still runs
func Test_Unit_AutoMode_ShowsFixes(t *testing.T) {
	t.Chdir(t.TempDir())
	data := "# Plan: x\n\n## Tasks\n\n### t1: a\ndone when: it works\n\n-    loose bullet\n"
	if err := os.WriteFile("run.md", []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	fx, _ := newAutoFixture(t, "run.md")
	if err := runAutoModeWith(t, fx, "n\n"); err != nil {
		t.Fatalf("autoMode: %v", err)
	}
	if out := fx.Out.String(); !strings.Contains(out, "run.md: read as fixed: line 8") || !strings.Contains(out, "Not run.") {
		t.Errorf("output lacks the fix note or Not run:\n%s", out)
	}
	if got, _ := os.ReadFile("run.md"); string(got) != data {
		t.Errorf("the plan file was rewritten:\n%s", got)
	}
}

// - Verify the Run? answers: "n" and Ctrl+D (EOF) print "Not run.", an empty line means yes, and -yes asks nothing
func Test_Unit_AutoMode_RunQuestion(t *testing.T) {
	tests := []struct {
		name, input string
		yes, run    bool
	}{
		{"no", "n\n", false, false},
		{"Ctrl+D", "", false, false},
		{"default yes", "\n", false, true},
		{"-yes", "", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeAutoPlan(t, "run.md", "goal.md")
			fx, _ := newAutoFixture(t, path)
			fx.Session.Config.AssumeYes = tt.yes
			if err := runAutoModeWith(t, fx, tt.input); err != nil {
				t.Fatalf("autoMode: %v", err)
			}
			out := fx.Out.String()
			if asked := strings.Contains(out, "Run? [Y/n]"); asked == tt.yes {
				t.Errorf("asked = %v with -yes %v:\n%s", asked, tt.yes, out)
			}
			if ran := strings.Contains(out, "Run 0001"); ran != tt.run {
				t.Errorf("reached the run step = %v, want %v:\n%s", ran, tt.run, out)
			}
			if !tt.run && !strings.Contains(out, "Not run.") {
				t.Errorf("output lacks Not run.:\n%s", out)
			}
		})
	}
}

// - Verify runAutoMode without a terminal and without -yes is an error naming -yes, printing nothing; with -yes it runs
func Test_Unit_RunAutoMode_TerminalUnlessYes(t *testing.T) {
	path := writeAutoPlan(t, "run.md", "goal.md")
	fx, _ := newAutoFixture(t, path)
	fx.Session.InBuffer = strings.NewReader("y\n")
	if err := runAutoMode(t.Context(), nil, fx.Session); err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Errorf("err = %v, want a needs-a-terminal error naming -yes", err)
	}
	if fx.Out.Len() != 0 {
		t.Errorf("expected nothing printed, got:\n%s", fx.Out.String())
	}
	fx.Session.Config.AssumeYes = true
	if err := runAutoMode(t.Context(), nil, fx.Session); err != nil {
		t.Errorf("runAutoMode with -yes: %v", err)
	}
}

// - Verify the plan auto mode reads holds the objective it was made from (plan mode copies it into "## Objective")
func Test_Unit_AutoMode_PlanCarriesObjective(t *testing.T) {
	o := objective.Objective{ObjText: "Count words in a file.", Instructions: "- Go only.", Context: "A teaching example."}
	t.Chdir(t.TempDir())
	p := acceptedPlan(t, "goal.md", o, validPlanArgs)
	if err := os.WriteFile(filepath.Join(".", "run.md"), []byte(p.Markdown()), 0o644); err != nil {
		t.Fatal(err)
	}
	fx, ad := newAutoFixture(t, "run.md")
	if err := runAutoModeWith(t, fx, "n\n"); err != nil {
		t.Fatalf("autoMode: %v", err)
	}
	for _, want := range []string{"### Objective\nCount words in a file.", "### Instructions\n- Go only.", "### Context\nA teaching example."} {
		if !strings.Contains(ad.Plan.Objective, want) {
			t.Errorf("plan's Objective lacks %q:\n%s", want, ad.Plan.Objective)
		}
	}
}
