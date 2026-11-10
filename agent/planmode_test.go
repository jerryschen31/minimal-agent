package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

// planStubFixture runs planStub with ObjectiveFile set to file and stdin replaced by input; it returns what was printed.
func planStubFixture(t *testing.T, file, input string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cfg := config.GetDefaultConfig(config.ModeChat)
	cfg.OutBuffer = &out
	cfg.ObjectiveFile = file
	ctx := t.Context()
	err := planStub(ctx, cfg, newPlainReader(ctx, strings.NewReader(input), &out))
	return out.String(), err
}

// - Verify an objective file is parsed and printed in the standard form, with nothing written
func Test_Unit_PlanStub_Draft(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "objective.md")
	if err := os.WriteFile(draft, []byte("##Objective\nBuild fib.\n\n## Tasks\n- Write fib.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := planStubFixture(t, draft, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"Read the objective " + draft,
		"# Objective\nBuild fib.\n\n# Instructions\n## Tasks\n- Write fib.go\n",
		"nothing was written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("plan mode wrote files: %v", entries)
	}
}

// - Verify an objective file Parse refuses is an error that names the file
func Test_Unit_PlanStub_BadDraft(t *testing.T) {
	draft := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(draft, []byte("# Objective\na\n\n## Context\nb\n\n## Context\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := planStubFixture(t, draft, "")
	if err == nil || !strings.Contains(err.Error(), draft) {
		t.Errorf("expected an error naming %s, got %v", draft, err)
	}
}

// - Verify the answer becomes the objective without a draft; end of input or a blank answer ends without an error
func Test_Unit_PlanStub_AsksForObjective(t *testing.T) {
	out, err := planStubFixture(t, "", "  A CLI that counts words  \n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "What do you want to build?") || !strings.Contains(out, "# Objective\nA CLI that counts words\n") {
		t.Errorf("unexpected output:\n%s", out)
	}

	for name, input := range map[string]string{"end of input": "", "blank answer": "   \n"} {
		out, err := planStubFixture(t, "", input)
		if err != nil || !strings.Contains(out, "No objective given; nothing written.") {
			t.Errorf("%s: err %v, output:\n%s", name, err, out)
		}
	}
}

// - Verify plan mode refuses to start without a terminal (here stdin is a buffer)
func Test_Unit_RunPlanMode_NeedsTerminal(t *testing.T) {
	cfg := config.GetDefaultConfig(config.ModeChat)
	cfg.InBuffer = strings.NewReader("x\n")
	cfg.OutBuffer = &bytes.Buffer{}
	err := RunPlanMode(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("expected a needs-a-terminal error, got %v", err)
	}
}
