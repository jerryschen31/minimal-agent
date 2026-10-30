package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

// stubTool is a minimal Tool for registry tests. Tag lets a test tell two tools with the same
// name apart (e.g. to check which one survived a duplicate registration).
type stubTool struct {
	name string
	Tag  string
}

func (s stubTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(s.name, "stub tool", nil)
}

func (s stubTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return s.Tag, nil
}

// defNames returns the tool names from a slice of definitions, in order.
func defNames(defs []model.ToolDef) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Function.Name
	}
	return names
}

// newRegistry builds a registry from tools, failing the test on error.
func newRegistry(t *testing.T, tools ...Tool) *ToolRegistry {
	t.Helper()
	reg, err := NewToolRegistry(tools)
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	return reg
}

// - Verify that tools registered out of order come back from GetToolDefs sorted by name
// (the order must not depend on registration order or map iteration order)
func Test_ToolRegistry_Register_OutOfOrder_DefsSortedByName(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "zeta"}, stubTool{name: "alpha"}, stubTool{name: "mid"})

	got := strings.Join(defNames(reg.GetToolDefs()), ",")
	if want := "alpha,mid,zeta"; got != want {
		t.Errorf("expected defs %q, got %q", want, got)
	}
}

// - Verify that the defs stay in the same order across repeated calls and rebuilds
// (map iteration is random, so a missing sort would show up as flakiness here)
func Test_ToolRegistry_GetToolDefs_OrderStableAcrossRebuilds(t *testing.T) {
	reg := newRegistry(t)
	for _, n := range []string{"e", "b", "d", "a", "c"} {
		if err := reg.Register(stubTool{name: n}); err != nil {
			t.Fatalf("Register(%q): %v", n, err)
		}
		if names := defNames(reg.GetToolDefs()); !sort.StringsAreSorted(names) {
			t.Fatalf("defs not sorted after registering %q: %v", n, names)
		}
	}
}

// - Verify that registering a duplicate name is an error and the original tool is kept
func Test_ToolRegistry_Register_DuplicateName_ErrorsAndKeepsOriginal(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "dup", Tag: "first"})

	err := reg.Register(stubTool{name: "dup", Tag: "second"})
	if err == nil {
		t.Fatalf("expected an error registering a duplicate name, got nil")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected error to mention 'already registered', got %q", err.Error())
	}
	if n := len(reg.GetToolDefs()); n != 1 {
		t.Errorf("expected 1 def after a rejected duplicate, got %d", n)
	}
	tool, ok := reg.Lookup("dup")
	if !ok {
		t.Fatalf("expected the original tool to still be registered")
	}
	if got, _ := tool.CallTool(context.Background(), nil); got != "first" {
		t.Errorf("expected original tool (tag %q) to be kept, got tag %q", "first", got)
	}
}

// - Verify that NewToolRegistry surfaces a duplicate among its arguments as an error
func Test_ToolRegistry_NewToolRegistry_DuplicateInArgs_ReturnsError(t *testing.T) {
	reg, err := NewToolRegistry([]Tool{stubTool{name: "x"}, stubTool{name: "x"}})
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if reg != nil {
		t.Errorf("expected a nil registry on error, got %+v", reg)
	}
}

// - Verify that a tool with an empty name is rejected and leaves the registry unchanged
func Test_ToolRegistry_Register_EmptyName_Errors(t *testing.T) {
	reg := newRegistry(t)

	err := reg.Register(stubTool{name: ""})
	if err == nil {
		t.Fatalf("expected an error registering an empty name, got nil")
	}
	if n := len(reg.GetToolDefs()); n != 0 {
		t.Errorf("expected no defs after a rejected registration, got %d", n)
	}
}

// - Verify that a nil Tool is rejected with an error, not a panic
// (a typed nil pointer is deliberately not handled; see the note on Register)
func Test_ToolRegistry_Register_NilTool_Errors(t *testing.T) {
	reg := newRegistry(t)

	if err := reg.Register(nil); err == nil {
		t.Errorf("expected an error registering a nil tool, got nil")
	}
	if n := len(reg.GetToolDefs()); n != 0 {
		t.Errorf("expected no defs after a rejected registration, got %d", n)
	}
}

// - Verify that Remove drops both the tool and its definition, keeping the rest in sorted order
func Test_ToolRegistry_Remove_DropsToolAndDef(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "c"}, stubTool{name: "a"}, stubTool{name: "b"})

	if !reg.Remove("b") {
		t.Fatalf("expected Remove(%q) to return true", "b")
	}
	if _, ok := reg.Lookup("b"); ok {
		t.Errorf("expected %q to be gone from Lookup", "b")
	}
	if got, want := strings.Join(defNames(reg.GetToolDefs()), ","), "a,c"; got != want {
		t.Errorf("expected defs %q after Remove, got %q", want, got)
	}
}

// - Verify that Remove on an unknown name returns false and changes nothing
func Test_ToolRegistry_Remove_UnknownName_ReturnsFalse(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "a"})

	if reg.Remove("nope") {
		t.Errorf("expected Remove of an unknown name to return false")
	}
	if n := len(reg.GetToolDefs()); n != 1 {
		t.Errorf("expected registry unchanged (1 def), got %d", n)
	}
}

// - Verify that a removed name can be registered again
func Test_ToolRegistry_Remove_ThenRegisterAgain_Succeeds(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "a", Tag: "old"})
	reg.Remove("a")

	if err := reg.Register(stubTool{name: "a", Tag: "new"}); err != nil {
		t.Fatalf("re-registering a removed name: %v", err)
	}
	tool, _ := reg.Lookup("a")
	if got, _ := tool.CallTool(context.Background(), nil); got != "new" {
		t.Errorf("expected the new tool (tag %q), got tag %q", "new", got)
	}
}

// - Verify Lookup finds registered tools and reports (nil, false) for unknown names
func Test_ToolRegistry_Lookup_KnownAndUnknown(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "known"})

	if tool, ok := reg.Lookup("known"); !ok || tool == nil {
		t.Errorf("expected Lookup(%q) to find the tool, got (%v, %v)", "known", tool, ok)
	}
	if tool, ok := reg.Lookup("unknown"); ok || tool != nil {
		t.Errorf("expected Lookup(%q) to return (nil, false), got (%v, %v)", "unknown", tool, ok)
	}
}

// - Verify an empty registry has no defs (so the request's "tools" key is omitted)
func Test_ToolRegistry_GetToolDefs_EmptyRegistry_NoDefs(t *testing.T) {
	reg := newRegistry(t)

	if n := len(reg.GetToolDefs()); n != 0 {
		t.Errorf("expected no defs, got %d", n)
	}
}

// - Verify copy-on-write: a slice handed out earlier is not changed by later Register/Remove calls
// (this is what lets GetToolDefs return its slice without copying it)
func Test_ToolRegistry_GetToolDefs_EarlierSliceUnaffectedByLaterChanges(t *testing.T) {
	reg := newRegistry(t, stubTool{name: "a"}, stubTool{name: "c"})
	before := reg.GetToolDefs()

	if err := reg.Register(stubTool{name: "b"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.Remove("a")

	if got, want := strings.Join(defNames(before), ","), "a,c"; got != want {
		t.Errorf("earlier slice changed: expected %q, got %q", want, got)
	}
	if got, want := strings.Join(defNames(reg.GetToolDefs()), ","), "b,c"; got != want {
		t.Errorf("expected current defs %q, got %q", want, got)
	}
}

// - Verify concurrent Register, GetToolDefs and Lookup are safe (run with -race) and that
// every registration lands
func Test_ToolRegistry_ConcurrentRegisterAndRead_RaceFree(t *testing.T) {
	const writers = 20
	reg := newRegistry(t)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(2)
		name := fmt.Sprintf("tool_%02d", i)
		go func() {
			defer wg.Done()
			if err := reg.Register(stubTool{name: name}); err != nil {
				t.Errorf("Register(%q): %v", name, err)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reg.GetToolDefs()
				reg.Lookup(name)
			}
		}()
	}
	wg.Wait()

	names := defNames(reg.GetToolDefs())
	if len(names) != writers {
		t.Errorf("expected %d defs, got %d", writers, len(names))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("expected defs sorted by name, got %v", names)
	}
}

// - Verify SetupBuiltinTools builds the read_file tool for an existing workDir, and fails for a bad workDir
// or an unknown tool name, so a misconfigured agent stops at startup instead of at the first file read
func Test_SetupBuiltinTools_ReadFile_WorkDir(t *testing.T) {
	built, err := SetupBuiltinTools([]string{"ReadFile"}, t.TempDir(), "", 4096, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(built) != 1 || built[0].GetToolDefinition().Function.Name != "read_file" {
		t.Errorf("expected one read_file tool, got %v", defNames([]model.ToolDef{built[0].GetToolDefinition()}))
	}

	if _, err := SetupBuiltinTools([]string{"ReadFile"}, filepath.Join(t.TempDir(), "missing"), "", 4096, nil); err == nil {
		t.Errorf("expected an error for a workDir that does not exist")
	}
	if _, err := SetupBuiltinTools([]string{"NoSuchTool"}, t.TempDir(), "", 4096, nil); err == nil {
		t.Errorf("expected an error for an unknown built-in tool")
	}
}

// - Verify SetupBuiltinTools passes traceDir through to read_file: a file in a trace directory under workDir is refused
func Test_SetupBuiltinTools_ReadFile_TraceDir(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "logs", "trace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "logs", "trace", "run.jsonl"), []byte("fake-trace"), 0o644); err != nil {
		t.Fatal(err)
	}
	built, err := SetupBuiltinTools([]string{"ReadFile"}, work, filepath.Join(work, "logs", "trace"), 4096, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := built[0].CallTool(context.Background(), json.RawMessage(`{"path":"logs/trace/run.jsonl"}`))
	if err == nil || !strings.Contains(err.Error(), "trace directory") {
		t.Errorf("expected a trace-directory error, got (%q, %v)", got, err)
	}
	if _, err := SetupBuiltinTools([]string{"ReadFile"}, work, work, 4096, nil); err == nil {
		t.Errorf("expected an error for a traceDir equal to workDir")
	}
}

// - Verify SetupBuiltinTools accepts both "read_file" (the model-facing name, the config default) and the old
// "ReadFile" spelling, each giving one tool named read_file; an unknown name still errors
func Test_SetupBuiltinTools_ReadFile_BothSpellings(t *testing.T) {
	for _, name := range []string{"read_file", "ReadFile"} {
		built, err := SetupBuiltinTools([]string{name}, t.TempDir(), "", 4096, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if len(built) != 1 || built[0].GetToolDefinition().Function.Name != "read_file" {
			t.Errorf("%s: expected one read_file tool, got %d tools", name, len(built))
		}
	}
	if _, err := SetupBuiltinTools([]string{"Read_File"}, t.TempDir(), "", 4096, nil); err == nil {
		t.Errorf("expected an error for an unknown built-in tool name")
	}
}

// - Verify listing both spellings builds two read_file tools, and the duplicate is caught when they are registered
// (NewToolRegistry), not by SetupBuiltinTools
func Test_SetupBuiltinTools_BothSpellingsTogether_DuplicateAtRegistration(t *testing.T) {
	built, err := SetupBuiltinTools([]string{"read_file", "ReadFile"}, t.TempDir(), "", 4096, nil)
	if err != nil {
		t.Fatalf("SetupBuiltinTools: unexpected error: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(built))
	}
	if _, err := NewToolRegistry(built); err == nil {
		t.Errorf("expected NewToolRegistry to reject the duplicate read_file")
	}
}

// - Verify a bad denyFileNames pattern fails SetupBuiltinTools, with no tools enabled as well as with read_file,
// so a typo in the config stops startup instead of silently protecting nothing
func Test_SetupBuiltinTools_DenyFileNames_BadPattern_Fails(t *testing.T) {
	for _, bad := range []string{"[abc", "sub/creds.json", ""} {
		for _, toolList := range [][]string{nil, {}, {"read_file"}} {
			built, err := SetupBuiltinTools(toolList, t.TempDir(), "", 4096, []string{"*.secret", bad})
			if err == nil || !strings.Contains(err.Error(), "denyFileNames") {
				t.Errorf("pattern %q, tools %v: expected a denyFileNames error, got (%d tools, %v)", bad, toolList, len(built), err)
			}
		}
	}
}

// - Verify SetupBuiltinTools passes denyFileNames through to read_file: a matching file is refused (the error names the
// pattern), a non-matching one still reads, and the built-ins stay denied
func Test_SetupBuiltinTools_DenyFileNames_Enforced(t *testing.T) {
	work := t.TempDir()
	for name, body := range map[string]string{"creds.json": "FAKE_SECRET_TOKEN_123", "notes.txt": "ALLOWED-NOTES", ".env": "FAKE_SECRET_TOKEN_123"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	built, err := SetupBuiltinTools([]string{"read_file"}, work, "", 4096, []string{"Creds.JSON"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call := func(p string) (string, error) {
		return built[0].CallTool(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, p)))
	}

	for _, p := range []string{"creds.json", ".env"} {
		got, err := call(p)
		if err == nil || !strings.Contains(err.Error(), "deny list") || !strings.Contains(err.Error(), "creds.json") {
			t.Errorf("%s: expected a deny-list error naming creds.json, got (%q, %v)", p, got, err)
		}
		if strings.Contains(got, "FAKE_SECRET") {
			t.Errorf("%s: secret leaked: %q", p, got)
		}
	}
	if got, err := call("notes.txt"); err != nil || got != "ALLOWED-NOTES" {
		t.Errorf("notes.txt: expected ALLOWED-NOTES, got (%q, %v)", got, err)
	}
}
