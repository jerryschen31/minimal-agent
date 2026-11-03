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

	"github.com/jerryschen31/minimal-agent/config"
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
	built, err := SetupBuiltinTools(builtinCfg([]string{"ReadFile"}, t.TempDir(), "", 4096, 1000, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(built) != 1 || built[0].GetToolDefinition().Function.Name != "read_file" {
		t.Errorf("expected one read_file tool, got %v", defNames([]model.ToolDef{built[0].GetToolDefinition()}))
	}

	if _, err := SetupBuiltinTools(builtinCfg([]string{"ReadFile"}, filepath.Join(t.TempDir(), "missing"), "", 4096, 1000, nil, nil)); err == nil {
		t.Errorf("expected an error for a workDir that does not exist")
	}
	if _, err := SetupBuiltinTools(builtinCfg([]string{"NoSuchTool"}, t.TempDir(), "", 4096, 1000, nil, nil)); err == nil {
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
	built, err := SetupBuiltinTools(builtinCfg([]string{"ReadFile"}, work, filepath.Join(work, "logs", "trace"), 4096, 1000, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := built[0].CallTool(context.Background(), json.RawMessage(`{"path":"logs/trace/run.jsonl"}`))
	if err == nil || !strings.Contains(err.Error(), "trace directory") {
		t.Errorf("expected a trace-directory error, got (%q, %v)", got, err)
	}
	if _, err := SetupBuiltinTools(builtinCfg([]string{"ReadFile"}, work, work, 4096, 1000, nil, nil)); err == nil {
		t.Errorf("expected an error for a traceDir equal to workDir")
	}
}

// - Verify SetupBuiltinTools accepts both "read_file" (the model-facing name, the config default) and the old
// "ReadFile" spelling, each giving one tool named read_file; an unknown name still errors
func Test_SetupBuiltinTools_ReadFile_BothSpellings(t *testing.T) {
	for _, name := range []string{"read_file", "ReadFile"} {
		built, err := SetupBuiltinTools(builtinCfg([]string{name}, t.TempDir(), "", 4096, 1000, nil, nil))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if len(built) != 1 || built[0].GetToolDefinition().Function.Name != "read_file" {
			t.Errorf("%s: expected one read_file tool, got %d tools", name, len(built))
		}
	}
	if _, err := SetupBuiltinTools(builtinCfg([]string{"Read_File"}, t.TempDir(), "", 4096, 1000, nil, nil)); err == nil {
		t.Errorf("expected an error for an unknown built-in tool name")
	}
}

// - Verify listing both spellings builds two read_file tools, and the duplicate is caught when they are registered
// (NewToolRegistry), not by SetupBuiltinTools
func Test_SetupBuiltinTools_BothSpellingsTogether_DuplicateAtRegistration(t *testing.T) {
	built, err := SetupBuiltinTools(builtinCfg([]string{"read_file", "ReadFile"}, t.TempDir(), "", 4096, 1000, nil, nil))
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

// - Verify a bad denyPaths pattern fails SetupBuiltinTools, with no tools enabled as well as with read_file,
// so a typo in the config stops startup instead of silently protecting nothing
func Test_SetupBuiltinTools_DenyPaths_BadPattern_Fails(t *testing.T) {
	for _, bad := range []string{"[abc", "sub//creds.json", "../creds.json", " creds.json", ""} {
		for _, toolList := range [][]string{nil, {}, {"read_file"}} {
			built, err := SetupBuiltinTools(builtinCfg(toolList, t.TempDir(), "", 4096, 1000, []string{"*.secret", bad}, nil))
			if err == nil || !strings.Contains(err.Error(), "denyPaths") {
				t.Errorf("pattern %q, tools %v: expected a denyPaths error, got (%d tools, %v)", bad, toolList, len(built), err)
			}
		}
	}
}

// - Verify a denyPaths or ignorePaths pattern with "**" fails SetupBuiltinTools, whichever tools are enabled, so a rule
// that looks recursive but would miss nested files stops startup instead of protecting less than it appears to
func Test_SetupBuiltinTools_DoubleStarPattern_Fails(t *testing.T) {
	for _, toolList := range [][]string{nil, {"read_file"}, {"read_file", "list_files"}} {
		built, err := SetupBuiltinTools(builtinCfg(toolList, t.TempDir(), "", 4096, 1000, []string{"**/*.secret"}, nil))
		if err == nil || !strings.Contains(err.Error(), "denyPaths") || !strings.Contains(err.Error(), `"**" is not supported`) {
			t.Errorf("denyPaths, tools %v: expected a denyPaths \"**\" error, got (%d tools, %v)", toolList, len(built), err)
		}
		built, err = SetupBuiltinTools(builtinCfg(toolList, t.TempDir(), "", 4096, 1000, nil, []string{"build/**"}))
		if err == nil || !strings.Contains(err.Error(), "ignorePaths") || !strings.Contains(err.Error(), `"**" is not supported`) {
			t.Errorf("ignorePaths, tools %v: expected an ignorePaths \"**\" error, got (%d tools, %v)", toolList, len(built), err)
		}
	}
}

// - Verify SetupBuiltinTools passes denyPaths through to read_file: a matching file is refused (the error names the
// pattern), a non-matching one still reads, and the built-ins stay denied
func Test_SetupBuiltinTools_DenyPaths_Enforced(t *testing.T) {
	work := t.TempDir()
	for name, body := range map[string]string{"creds.json": "FAKE_SECRET_TOKEN_123", "notes.txt": "ALLOWED-NOTES", ".env": "FAKE_SECRET_TOKEN_123"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	built, err := SetupBuiltinTools(builtinCfg([]string{"read_file"}, work, "", 4096, 1000, []string{"Creds.JSON"}, nil))
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

// - Verify SetupBuiltinTools builds the workspace even when no file tool is enabled: with an empty (or nil) tool list,
// a missing workDir, a workDir that is a file, or a traceDir equal to workDir still fails setup, with no tools
func Test_SetupBuiltinTools_NoTools_BadWorkspace_Fails(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(work, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, workDir, traceDir, want string
	}{
		{"missing workDir", filepath.Join(work, "missing"), "", "workDir"},
		{"workDir is a file", file, "", "not a directory"},
		{"traceDir is workDir", work, work, "is workDir or contains it"},
	}
	for _, tc := range cases {
		for _, toolList := range [][]string{nil, {}} {
			built, err := SetupBuiltinTools(builtinCfg(toolList, tc.workDir, tc.traceDir, 4096, 1000, nil, nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(built) != 0 {
				t.Errorf("%s, tools %v: expected an error containing %q and no tools, got (%d tools, %v)", tc.name, toolList, tc.want, len(built), err)
			}
		}
	}
}

// - Verify SetupBuiltinTools builds list_files: ["list_files"] gives one tool named list_files, and
// ["read_file","list_files"] gives both, in that order
func Test_SetupBuiltinTools_ListFiles(t *testing.T) {
	built, err := SetupBuiltinTools(builtinCfg([]string{"list_files"}, t.TempDir(), "", 4096, 1000, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(built) != 1 || built[0].GetToolDefinition().Function.Name != "list_files" {
		t.Fatalf("expected one list_files tool, got %d tools", len(built))
	}

	built, err = SetupBuiltinTools(builtinCfg([]string{"read_file", "list_files"}, t.TempDir(), "", 4096, 1000, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	names := make([]model.ToolDef, len(built))
	for i, b := range built {
		names[i] = b.GetToolDefinition()
	}
	if got, want := strings.Join(defNames(names), ","), "read_file,list_files"; got != want {
		t.Errorf("expected tools %q, got %q", want, got)
	}
}

// - Verify SetupBuiltinTools builds search_files: ["search_files"] gives one tool named search_files that searches
// workDir, and ["list_files","search_files"] gives both, in that order
func Test_SetupBuiltinTools_SearchFiles(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hit"), 0o644); err != nil {
		t.Fatal(err)
	}
	built, err := SetupBuiltinTools(builtinCfg([]string{"search_files"}, work, "", 4096, 1000, nil, nil))
	if err != nil || len(built) != 1 || built[0].GetToolDefinition().Function.Name != "search_files" {
		t.Fatalf("expected one search_files tool, got (%d tools, %v)", len(built), err)
	}
	if got, err := built[0].CallTool(context.Background(), json.RawMessage(`{"query":"hit"}`)); err != nil || got != "a.txt:1: hit" {
		t.Errorf(`expected "a.txt:1: hit", got (%q, %v)`, got, err)
	}

	built, err = SetupBuiltinTools(builtinCfg([]string{"list_files", "search_files"}, t.TempDir(), "", 4096, 1000, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	names := make([]model.ToolDef, len(built))
	for i, b := range built {
		names[i] = b.GetToolDefinition()
	}
	if got, want := strings.Join(defNames(names), ","), "list_files,search_files"; got != want {
		t.Errorf("expected tools %q, got %q", want, got)
	}
}

// - Verify SetupBuiltinTools passes maxSearchFiles and ignorePaths to search_files: maxSearchFiles 0 fails setup with
// "must be positive", and an ignored directory's files are not searched
func Test_SetupBuiltinTools_SearchFiles_MaxSearchFilesAndIgnore(t *testing.T) {
	if built, err := SetupBuiltinTools(builtinCfg([]string{"search_files"}, t.TempDir(), "", 4096, 0, nil, nil)); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("maxSearchFiles 0: expected a 'must be positive' error, got (%d tools, %v)", len(built), err)
	}

	work := t.TempDir()
	for _, rel := range []string{"a.txt", filepath.Join("vendor", "b.txt")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, rel), []byte("hit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	built, err := SetupBuiltinTools(builtinCfg([]string{"search_files"}, work, "", 4096, 1000, nil, []string{"vendor/"}))
	if err != nil || len(built) != 1 {
		t.Fatalf("expected one search_files tool, got (%d tools, %v)", len(built), err)
	}
	if got, err := built[0].CallTool(context.Background(), json.RawMessage(`{"query":"hit"}`)); err != nil || got != "a.txt:1: hit" {
		t.Errorf(`expected only "a.txt:1: hit", got (%q, %v)`, got, err)
	}
}

// - Verify SetupBuiltinTools with a valid workDir and an empty tool list succeeds with zero tools
func Test_SetupBuiltinTools_NoTools_ValidWorkDir_NoTools(t *testing.T) {
	for _, toolList := range [][]string{nil, {}} {
		built, err := SetupBuiltinTools(builtinCfg(toolList, t.TempDir(), "", 4096, 1000, nil, nil))
		if err != nil || len(built) != 0 {
			t.Errorf("tools %v: expected (0 tools, nil), got (%d tools, %v)", toolList, len(built), err)
		}
	}
}

// - Verify SetupBuiltinTools passes maxSearchFiles to list_files: 0 fails setup with "must be positive", and a limit
// of 2 makes a listing of three files stop after two with the stopped note
func Test_SetupBuiltinTools_ListFiles_MaxSearchFiles(t *testing.T) {
	if built, err := SetupBuiltinTools(builtinCfg([]string{"list_files"}, t.TempDir(), "", 4096, 0, nil, nil)); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("maxSearchFiles 0: expected a 'must be positive' error, got (%d tools, %v)", len(built), err)
	}

	work := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	built, err := SetupBuiltinTools(builtinCfg([]string{"list_files"}, work, "", 4096, 2, nil, nil))
	if err != nil || len(built) != 1 {
		t.Fatalf("expected one list_files tool, got (%d tools, %v)", len(built), err)
	}
	got, err := built[0].CallTool(context.Background(), json.RawMessage(`{}`))
	if err != nil || strings.Contains(got, "c.txt") || !strings.Contains(got, "[stopped after looking at 2 entries") {
		t.Errorf("expected a listing stopped after 2 entries without c.txt, got (%q, %v)", got, err)
	}
}

// - Verify maxSearchFiles is checked only by the tool that uses it: with only read_file enabled, 0 does not fail setup
// (as readFileMaxBytes is checked only by read_file)
func Test_SetupBuiltinTools_ReadFileOnly_MaxSearchFilesNotChecked(t *testing.T) {
	built, err := SetupBuiltinTools(builtinCfg([]string{"read_file"}, t.TempDir(), "", 4096, 0, nil, nil))
	if err != nil || len(built) != 1 {
		t.Errorf("expected (1 tool, nil), got (%d tools, %v)", len(built), err)
	}
}

// - Verify a bad ignorePaths pattern fails SetupBuiltinTools with an ignorePaths error, even with no tools enabled
func Test_SetupBuiltinTools_IgnorePaths_BadPattern_Fails(t *testing.T) {
	for _, bad := range []string{"[abc", "sub//x", "./x", "x ", ""} {
		for _, toolList := range [][]string{nil, {}, {"list_files"}} {
			built, err := SetupBuiltinTools(builtinCfg(toolList, t.TempDir(), "", 4096, 1000, nil, []string{"*.tmp", bad}))
			if err == nil || !strings.Contains(err.Error(), "ignorePaths") || len(built) != 0 {
				t.Errorf("pattern %q, tools %v: expected an ignorePaths error and no tools, got (%d tools, %v)", bad, toolList, len(built), err)
			}
		}
	}
}

// - Verify SetupBuiltinTools passes ignorePaths to list_files: ["*.log"] leaves out a .log file, and nil ignores
// nothing (the contents of .git are listed)
func Test_SetupBuiltinTools_ListFiles_IgnorePaths(t *testing.T) {
	work := t.TempDir()
	for _, rel := range []string{"app.log", "main.go", filepath.Join(".git", "HEAD")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := func(ignorePaths []string) string {
		t.Helper()
		built, err := SetupBuiltinTools(builtinCfg([]string{"list_files"}, work, "", 4096, 1000, nil, ignorePaths))
		if err != nil || len(built) != 1 {
			t.Fatalf("ignorePaths %q: expected one list_files tool, got (%d tools, %v)", ignorePaths, len(built), err)
		}
		got, err := built[0].CallTool(context.Background(), json.RawMessage(`{"recursive": true}`))
		if err != nil {
			t.Fatalf("ignorePaths %q: list_files: %v", ignorePaths, err)
		}
		return got
	}

	if got := list([]string{"*.log"}); strings.Contains(got, "app.log") || !strings.Contains(got, "main.go") {
		t.Errorf(`ignorePaths ["*.log"]: expected main.go without app.log, got %q`, got)
	}
	if got := list(nil); !strings.Contains(got, "app.log") || !strings.Contains(got, "HEAD") {
		t.Errorf("ignorePaths nil: expected app.log and .git/HEAD to be listed, got %q", got)
	}
}

// builtinCfg builds the config SetupBuiltinTools reads, from the settings these tests vary.
func builtinCfg(toolList []string, workDir, traceDir string, readFileMaxBytes, maxSearchFiles int, denyPaths, ignorePaths []string) config.Config {
	cfg := config.GetDefaultConfig()
	cfg.BuiltinTools, cfg.WorkDir, cfg.TraceDir = toolList, workDir, traceDir
	cfg.ReadFileMaxBytes, cfg.MaxSearchFiles = readFileMaxBytes, maxSearchFiles
	cfg.DenyPaths, cfg.IgnorePaths = denyPaths, ignorePaths
	return cfg
}

// - Verify SetupBuiltinTools builds create_directory and write_file, in the order listed, and that the config file path
// reaches the workspace: write_file refuses to overwrite it and still writes another file
func Test_SetupBuiltinTools_WriteTools_ConfigFileProtected(t *testing.T) {
	work := t.TempDir()
	cfgFile := filepath.Join(work, "agent.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := builtinCfg([]string{"create_directory", "write_file"}, work, "", 4096, 1000, nil, nil)
	cfg.ConfigFile = cfgFile
	built, err := SetupBuiltinTools(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(built) != 2 || built[0].GetToolDefinition().Function.Name != "create_directory" || built[1].GetToolDefinition().Function.Name != "write_file" {
		t.Fatalf("unexpected tools: %d", len(built))
	}
	ctx := context.Background()
	if _, err := built[1].CallTool(ctx, json.RawMessage(`{"path":"agent.json","content":"pwned"}`)); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Errorf("overwriting the config file: error = %v", err)
	}
	if got, err := built[1].CallTool(ctx, json.RawMessage(`{"path":"notes.txt","content":"hi"}`)); err != nil || got != "created notes.txt (2 bytes)" {
		t.Errorf("write_file = (%q, %v)", got, err)
	}
	if got, _ := os.ReadFile(cfgFile); string(got) != "{}" {
		t.Errorf("config file changed: %q", got)
	}
}

// - Verify SetupBuiltinTools builds edit_file, and that the config file path reaches it: an edit of the config file is
// refused while another file is edited
func Test_SetupBuiltinTools_EditFile_ConfigFileProtected(t *testing.T) {
	work := t.TempDir()
	cfgFile := filepath.Join(work, "agent.json")
	for f, c := range map[string]string{cfgFile: `{"a":1}`, filepath.Join(work, "n.txt"): "old"} {
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := builtinCfg([]string{"edit_file"}, work, "", 4096, 1000, nil, nil)
	cfg.ConfigFile = cfgFile
	built, err := SetupBuiltinTools(cfg)
	if err != nil || len(built) != 1 || built[0].GetToolDefinition().Function.Name != "edit_file" {
		t.Fatalf("(%v, %v)", built, err)
	}
	ctx := context.Background()
	if _, err := built[0].CallTool(ctx, json.RawMessage(`{"path":"agent.json","old_string":"1","new_string":"2"}`)); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Errorf("editing the config file: error = %v", err)
	}
	if got, err := built[0].CallTool(ctx, json.RawMessage(`{"path":"n.txt","old_string":"old","new_string":"new"}`)); err != nil || got != "edited n.txt (1 replacement, 3 bytes)" {
		t.Errorf("edit = (%q, %v)", got, err)
	}
	if b, _ := os.ReadFile(cfgFile); string(b) != `{"a":1}` {
		t.Errorf("config file changed: %q", b)
	}
}

// - Verify SetupBuiltinTools builds copy_file and move_file and the config file path reaches them: copying onto or
// moving away the config file is refused, and an ordinary copy and move work
func Test_SetupBuiltinTools_CopyAndMoveFile(t *testing.T) {
	work := t.TempDir()
	cfgFile := filepath.Join(work, "agent.json")
	for f, c := range map[string]string{cfgFile: "{}", filepath.Join(work, "a.txt"): "A"} {
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := builtinCfg([]string{"copy_file", "move_file"}, work, "", 4096, 1000, nil, nil)
	cfg.ConfigFile = cfgFile
	built, err := SetupBuiltinTools(cfg)
	if err != nil || len(built) != 2 || built[0].GetToolDefinition().Function.Name != "copy_file" || built[1].GetToolDefinition().Function.Name != "move_file" {
		t.Fatalf("(%v, %v)", built, err)
	}
	ctx := context.Background()
	call := func(i int, src, dst string) (string, error) {
		args, _ := json.Marshal(map[string]string{"source": src, "destination": dst})
		return built[i].CallTool(ctx, args)
	}
	if _, err := call(0, "a.txt", "agent.json"); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Errorf("copy onto the config file: error = %v", err)
	}
	if _, err := call(1, "agent.json", "b.txt"); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Errorf("move the config file: error = %v", err)
	}
	if got, err := call(0, "a.txt", "copy.txt"); err != nil || got != "copied a.txt -> copy.txt (1 bytes)" {
		t.Errorf("copy = (%q, %v)", got, err)
	}
	if got, err := call(1, "copy.txt", "moved.txt"); err != nil || got != "moved copy.txt -> moved.txt (1 bytes)" {
		t.Errorf("move = (%q, %v)", got, err)
	}
	if b, _ := os.ReadFile(cfgFile); string(b) != "{}" {
		t.Errorf("config file changed: %q", b)
	}
}

// - Verify copyFileMaxBytes reaches copy_file: a file over the configured limit is refused, one within it is copied,
// and a non-positive limit fails setup (only when copy_file is enabled)
func Test_SetupBuiltinTools_CopyFile_MaxBytes(t *testing.T) {
	work := t.TempDir()
	for f, n := range map[string]int{"small.txt": 10, "big.txt": 11} {
		if err := os.WriteFile(filepath.Join(work, f), []byte(strings.Repeat("x", n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := builtinCfg([]string{"copy_file"}, work, "", 4096, 1000, nil, nil)
	cfg.CopyFileMaxBytes = 10
	built, err := SetupBuiltinTools(cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := func(src, dst string) error {
		args, _ := json.Marshal(map[string]string{"source": src, "destination": dst})
		_, err := built[0].CallTool(context.Background(), args)
		return err
	}
	if err := call("small.txt", "s2.txt"); err != nil {
		t.Errorf("within the limit: %v", err)
	}
	if err := call("big.txt", "b2.txt"); err == nil || !strings.Contains(err.Error(), "11 bytes") {
		t.Errorf("over the limit: error = %v", err)
	}
	cfg.CopyFileMaxBytes = 0
	if _, err := SetupBuiltinTools(cfg); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Errorf("zero limit with copy_file enabled: error = %v", err)
	}
	cfg.BuiltinTools = []string{"read_file"}
	if _, err := SetupBuiltinTools(cfg); err != nil {
		t.Errorf("zero limit without copy_file: %v", err)
	}
}
