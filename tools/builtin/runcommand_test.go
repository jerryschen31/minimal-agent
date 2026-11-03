package builtin

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newRunTool(t *testing.T, work string, names, extraEnv []string, maxBytes int) (*RunCommandTool, string) {
	t.Helper()
	ws, err := NewWorkspace(work, "", "", DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	tool, err := NewRunCommandTool(ws, names, extraEnv, maxBytes, &warn)
	if err != nil {
		t.Fatalf("NewRunCommandTool: %v (warnings: %s)", err, warn.String())
	}
	return tool, warn.String()
}

// - Verify the constructor rejects a name that is not a bare program name, a non-positive cap and a bad variable name:
// these are config typos in a safety setting, so they fail setup rather than being skipped
func Test_RunCommand_New_RejectsBadConfig(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), "", "", DenyList{})
	cases := []struct {
		name     string
		names    []string
		env      []string
		max      int
		wantText string
	}{
		{"path in name", []string{"/bin/sh"}, nil, 100, "bare program name"},
		{"relative path", []string{"./go"}, nil, 100, "bare program name"},
		{"space in name", []string{"go test"}, nil, 100, "bare program name"},
		{"empty name", []string{""}, nil, 100, "bare program name"},
		{"zero cap", []string{"sh"}, nil, 0, "must be positive"},
		{"env with =", []string{"sh"}, []string{"A=b"}, 100, "not an environment variable name"},
		{"empty env name", []string{"sh"}, []string{""}, 100, "not an environment variable name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, err := NewRunCommandTool(ws, c.names, c.env, c.max, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), c.wantText) || errors.Is(err, ErrNoCommands) {
				t.Errorf("err = %v, want one containing %q", err, c.wantText)
			}
			if tool != nil {
				t.Errorf("want nil tool on error")
			}
		})
	}
}

// - Verify a program missing from PATH is warned about and left out of the schema, and that when none is left the
// constructor returns ErrNoCommands (the caller then skips the tool)
func Test_RunCommand_New_MissingProgramWarnedAndOmitted(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), "", "", DenyList{})
	var warn bytes.Buffer
	tool, err := NewRunCommandTool(ws, []string{"sh", "no-such-program-xyz"}, nil, 100, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), `"no-such-program-xyz" not found on PATH`) {
		t.Errorf("warning missing: %q", warn.String())
	}
	if _, ok := tool.programs["no-such-program-xyz"]; ok || !filepath.IsAbs(tool.programs["sh"]) {
		t.Errorf("programs = %v, want sh as an absolute path and no missing program", tool.programs)
	}
	def := tool.GetToolDefinition()
	b, _ := json.Marshal(def)
	if strings.Contains(string(b), "no-such-program-xyz") {
		t.Errorf("schema offers the missing program: %s", b)
	}

	if _, err := NewRunCommandTool(ws, []string{"no-such-program-xyz"}, nil, 100, &bytes.Buffer{}); !errors.Is(err, ErrNoCommands) {
		t.Errorf("none found: err = %v, want ErrNoCommands", err)
	}
	if _, err := NewRunCommandTool(ws, nil, nil, 100, &bytes.Buffer{}); !errors.Is(err, ErrNoCommands) {
		t.Errorf("empty list: err = %v, want ErrNoCommands", err)
	}
}

// - Verify the schema lists the programs sorted (stable prompt prefix), states the cap in KB and points to read_file
func Test_RunCommand_Definition_SortedEnumAndAdvice(t *testing.T) {
	tool, _ := newRunTool(t, t.TempDir(), []string{"sh", "ls", "echo"}, nil, 128<<10)
	b, _ := json.Marshal(tool.GetToolDefinition())
	text := string(b)
	if !strings.Contains(text, `["echo","ls","sh"]`) {
		t.Errorf("enum not sorted: %s", text)
	}
	for _, want := range []string{"128 KB", "read_file", "not a shell"} {
		if !strings.Contains(text, want) {
			t.Errorf("definition lacks %q: %s", want, text)
		}
	}
}
