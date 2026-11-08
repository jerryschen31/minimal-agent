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
	cfg := config.GetDefaultConfig()
	cfg.OutBuffer = &out
	cfg.ObjectiveFile = file
	ctx := t.Context()
	err := planStub(ctx, cfg, newPlainReader(ctx, strings.NewReader(input), &out))
	return out.String(), err
}

// - Verify a draft is cleaned up, its fixes are printed, and it is printed in the standard layout with nothing written
func Test_Unit_PlanStub_Draft(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "draft.md")
	if err := os.WriteFile(draft, []byte("#Objective\nBuild fib.\n\n## Task\n- Write fib.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := planStubFixture(t, draft, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"(3 fixes)",
		`fixed line 1: "#Objective" → "# Objective"`,
		"format: minagent-objective/v1",
		"# Objective\nBuild fib.\n",
		"### t1: Write fib.go",
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

// - Verify a draft Cleanup refuses is an error that names the file
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
	cfg := config.GetDefaultConfig()
	cfg.InBuffer = strings.NewReader("x\n")
	cfg.OutBuffer = &bytes.Buffer{}
	err := RunPlanMode(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("expected a needs-a-terminal error, got %v", err)
	}
}
