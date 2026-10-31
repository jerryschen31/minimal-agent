package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newWorkspace builds a workspace on workDir with traceDir and deny, failing the test on error.
func newWorkspace(t *testing.T, workDir, traceDir string, deny DenyList) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(workDir, traceDir, deny)
	if err != nil {
		t.Fatalf("NewWorkspace(%q, %q): %v", workDir, traceDir, err)
	}
	return ws
}

// realPath returns p with symlinks resolved (t.TempDir is under a symlink on macOS: /var -> /private/var).
func realPath(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return real
}

//*************************************//
// NewWorkspace
//*************************************//

// - Verify NewWorkspace rejects a workDir that does not exist, and one that is a file ("not a directory")
func Test_Unit_NewWorkspace_InvalidWorkDir_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, dir, "plain.txt", []byte("x"))

	if ws, err := NewWorkspace(filepath.Join(dir, "missing"), "", DenyList{}); err == nil || ws != nil {
		t.Errorf("missing workDir: expected (nil, error), got (%v, %v)", ws, err)
	}
	if ws, err := NewWorkspace(file, "", DenyList{}); err == nil || ws != nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file workDir: expected (nil, 'not a directory' error), got (%v, %v)", ws, err)
	}
}

// - Verify NewWorkspace rejects a traceDir equal to workDir or a parent of it, with the message that names the
// consequence for every file tool (not only read_file)
func Test_Unit_NewWorkspace_TraceDirContainsWorkDir_ReturnsError(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, traceDir := range map[string]string{"equal": work, "parent": parent} {
		t.Run(name, func(t *testing.T) {
			ws, err := NewWorkspace(work, traceDir, DenyList{})
			if err == nil || ws != nil || !strings.Contains(err.Error(), "so the file tools could reach nothing") {
				t.Errorf("expected (nil, 'file tools could reach nothing' error), got (%v, %v)", ws, err)
			}
		})
	}
}

// - Verify NewWorkspace accepts traceDir "" (none) and a traceDir that does not exist yet, and resolves rootDir
// to the real absolute path of workDir
func Test_Unit_NewWorkspace_ValidTraceDir_Accepted(t *testing.T) {
	work := t.TempDir()
	for name, traceDir := range map[string]string{
		"none":              "",
		"not yet existing":  filepath.Join(work, "logs", "trace"),
		"outside, existing": t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			ws := newWorkspace(t, work, traceDir, DenyList{})
			if want := realPath(t, work); ws.rootDir != want {
				t.Errorf("rootDir = %q, want %q", ws.rootDir, want)
			}
			if traceDir == "" && ws.traceDir != "" {
				t.Errorf("traceDir \"\": expected none, got %q", ws.traceDir)
			}
			if traceDir != "" && ws.traceDir == "" {
				t.Errorf("traceDir %q: expected it to be recorded, got none", traceDir)
			}
		})
	}
}

//*************************************//
// check
//*************************************//

// - Verify check returns the path relative to rootDir for rootDir itself ("."), a file and a nested file
func Test_Unit_Workspace_Check_Inside_ReturnsRel(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), "", DenyList{})
	for real, want := range map[string]string{
		ws.rootDir:                                  ".",
		filepath.Join(ws.rootDir, "a.txt"):          "a.txt",
		filepath.Join(ws.rootDir, "sub", "b", "c"):  filepath.Join("sub", "b", "c"),
		filepath.Join(ws.rootDir, "..notdotdot.md"): "..notdotdot.md", // starts with ".." but is a plain name inside rootDir
	} {
		rel, err := ws.check("requested", real)
		if err != nil || rel != want {
			t.Errorf("check(%q) = (%q, %v), want (%q, nil)", real, rel, err, want)
		}
	}
}

// - Verify check refuses rootDir's parent, a sibling directory, a sibling sharing rootDir's name prefix and an
// unrelated absolute path, and that the error names the requested path, not the resolved one
func Test_Unit_Workspace_Check_Outside_Refused(t *testing.T) {
	parent := realPath(t, t.TempDir())
	work := filepath.Join(parent, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := newWorkspace(t, work, "", DenyList{})
	for _, real := range []string{
		parent,
		filepath.Join(parent, "outside", "secret.txt"),
		filepath.Join(parent, "work-evil", "secret.txt"),
		filepath.Join(string(filepath.Separator), "etc", "hosts"),
	} {
		rel, err := ws.check("REQUESTED-NAME", real)
		if err == nil || rel != "" || !strings.Contains(err.Error(), "outside the allowed directory") {
			t.Errorf("check(%q) = (%q, %v), want an outside-the-directory error", real, rel, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "REQUESTED-NAME ") || strings.Contains(err.Error(), real) {
			t.Errorf("check(%q): error should name the requested path, not the resolved one: %q", real, err)
		}
	}
}

// - Verify check refuses a built-in deny-listed name and a config extra (NewDenyList), naming the requested path
func Test_Unit_Workspace_Check_DenyListed_Refused(t *testing.T) {
	deny, err := NewDenyList([]string{"*.secret"})
	if err != nil {
		t.Fatal(err)
	}
	ws := newWorkspace(t, t.TempDir(), "", deny)
	for _, rel := range []string{".env", filepath.Join("sub", "key.pem"), "creds.secret", filepath.Join("deep", "x", "API.SECRET")} {
		real := filepath.Join(ws.rootDir, rel)
		got, err := ws.check("REQUESTED-NAME", real)
		if err == nil || got != "" || !strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), "deny list") {
			t.Errorf("check(%q) = (%q, %v), want a deny-list error", rel, got, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "REQUESTED-NAME ") || strings.Contains(err.Error(), ws.rootDir) {
			t.Errorf("check(%q): error should name the requested path, not the resolved one: %q", rel, err)
		}
	}
	// [agent] the extra must not leak into a workspace built without it
	plain := newWorkspace(t, t.TempDir(), "", DenyList{})
	if _, err := plain.check("creds.secret", filepath.Join(plain.rootDir, "creds.secret")); err != nil {
		t.Errorf("a workspace without the extra refused creds.secret: %v", err)
	}
}

// - Verify check refuses the trace directory and anything inside it, and allows a neighbour whose name only starts
// with the trace directory's name
func Test_Unit_Workspace_Check_TraceDir_Refused(t *testing.T) {
	work := realPath(t, t.TempDir())
	ws := newWorkspace(t, work, filepath.Join(work, "logs", "trace"), DenyList{})
	for _, real := range []string{
		filepath.Join(work, "logs", "trace"),
		filepath.Join(work, "logs", "trace", "run.jsonl"),
		filepath.Join(work, "LOGS", "Trace", "run.jsonl"), // case-insensitive, like isWithin
	} {
		rel, err := ws.check("REQUESTED-NAME", real)
		if err == nil || rel != "" || !strings.Contains(err.Error(), "trace directory") {
			t.Errorf("check(%q) = (%q, %v), want a trace-directory error", real, rel, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "REQUESTED-NAME ") {
			t.Errorf("check(%q): error should name the requested path: %q", real, err)
		}
	}
	if rel, err := ws.check("x", filepath.Join(work, "logs", "trace-notes.txt")); err != nil || rel != filepath.Join("logs", "trace-notes.txt") {
		t.Errorf("trace-notes.txt: got (%q, %v), want it allowed", rel, err)
	}
}

//*************************************//
// resolve and openRoot
//*************************************//

// - Verify resolve returns an error for a path that does not exist (it is for files that already exist)
func Test_Unit_Workspace_Resolve_Missing_ReturnsError(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), "", DenyList{})
	for _, p := range []string{"missing.txt", filepath.Join(ws.rootDir, "missing.txt"), filepath.Join("nodir", "x")} {
		if rel, err := ws.resolve(p); err == nil || rel != "" {
			t.Errorf("resolve(%q) = (%q, %v), want an error", p, rel, err)
		}
	}
}

// - Verify a relative request and the matching absolute request (through the unresolved workDir spelling too)
// resolve to the same rel
func Test_Unit_Workspace_Resolve_RelativeAndAbsolute_SameRel(t *testing.T) {
	work := t.TempDir() // [agent] on macOS this is the /var spelling, while rootDir is the resolved /private/var one
	writeFile(t, work, "sub/b.txt", []byte("ALLOWED-B"))
	ws := newWorkspace(t, work, "", DenyList{})
	want := filepath.Join("sub", "b.txt")
	for _, p := range []string{
		filepath.Join("sub", "b.txt"),
		filepath.Join(".", "sub", "..", "sub", "b.txt"),
		filepath.Join(ws.rootDir, "sub", "b.txt"),
		filepath.Join(work, "sub", "b.txt"),
	} {
		if rel, err := ws.resolve(p); err != nil || rel != want {
			t.Errorf("resolve(%q) = (%q, %v), want (%q, nil)", p, rel, err, want)
		}
	}
}

// - Verify openRoot opens rootDir: a known file can be found through it, and ".." is refused by the root
func Test_Unit_Workspace_OpenRoot_OpensRootDir(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "sub/known.txt", []byte("ALLOWED"))
	ws := newWorkspace(t, work, "", DenyList{})

	root, err := ws.openRoot()
	if err != nil {
		t.Fatalf("openRoot: %v", err)
	}
	defer root.Close()
	if info, err := root.Stat(filepath.Join("sub", "known.txt")); err != nil || info.Size() != int64(len("ALLOWED")) {
		t.Errorf("Stat(sub/known.txt) through the root = (%v, %v)", info, err)
	}
	if _, err := root.Stat(filepath.Join("..", filepath.Base(work))); err == nil {
		t.Errorf("expected the root to refuse a path through ..")
	}
}

//*************************************//
// NewReadFileTool and sharing a workspace
//*************************************//

// - Verify NewReadFileTool refuses a nil workspace with an error instead of building a tool that panics later
func Test_Unit_NewReadFileTool_NilWorkspace_ReturnsError(t *testing.T) {
	tool, err := NewReadFileTool(nil, testMaxBytes)
	if err == nil || tool != nil || !strings.Contains(err.Error(), "needs a workspace") {
		t.Errorf("expected (nil, 'needs a workspace' error), got (%v, %v)", tool, err)
	}
}

// - Verify two read_file tools on one workspace give the same answers, also when called concurrently
// (run with -race: the workspace is read-only after NewWorkspace, so sharing it must be safe)
func Test_Unit_Workspace_SharedByTwoTools_SameResultsConcurrently(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("ALLOWED-A"))
	writeFile(t, work, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, work, "logs/trace/run.jsonl", []byte("FAKE_SECRET_TOKEN_123"))
	ws := newWorkspace(t, work, filepath.Join(work, "logs", "trace"), DenyList{})
	var toolsOnWs []*ReadFileTool
	for _, n := range []int{testMaxBytes, 4} { // different caps: the cap belongs to the tool, the workspace is shared
		tool, err := NewReadFileTool(ws, n)
		if err != nil {
			t.Fatal(err)
		}
		toolsOnWs = append(toolsOnWs, tool)
	}

	var wg sync.WaitGroup
	for range 20 {
		for i, tool := range toolsOnWs {
			wg.Go(func() {
				if got, err := readPath(tool, "a.txt"); err != nil || !strings.HasPrefix(got, "ALLO") {
					t.Errorf("tool %d a.txt: got (%q, %v)", i, got, err)
				}
				for _, p := range []string{".env", "logs/trace/run.jsonl", "../x"} {
					if got, err := readPath(tool, p); err == nil || strings.Contains(got, "FAKE_SECRET") {
						t.Errorf("tool %d %s: expected refused, got (%q, %v)", i, p, got, err)
					}
				}
			})
		}
	}
	wg.Wait()
}
