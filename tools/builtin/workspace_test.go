package builtin

import (
	"errors"
	"io"
	"io/fs"
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

//*************************************//
// resolve error messages (no absolute workDir)
//*************************************//

// expectErrorHidesWorkDir fails the test unless err names wantName and contains neither spelling of the workDir
// (t.TempDir's own and its resolved form: on macOS /var is a symlink to /private/var).
func expectErrorHidesWorkDir(t *testing.T, what string, err error, work, wantName string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error", what)
	}
	if !strings.Contains(err.Error(), wantName) {
		t.Errorf("%s: expected the error to name %q, got %q", what, wantName, err)
	}
	for _, abs := range []string{work, realPath(t, work)} {
		if strings.Contains(err.Error(), abs) {
			t.Errorf("%s: error leaks the absolute workDir %q: %q", what, abs, err)
		}
	}
}

// missingRelPaths are requests for paths that don't exist: a file in workDir, one in an existing subdirectory, and
// one in a subdirectory that doesn't exist either.
var missingRelPaths = []string{"missing.txt", filepath.Join("sub", "missing.txt"), filepath.Join("nodir", "missing.txt")}

// - Verify resolve's error for a missing relative path names only the requested path, never the absolute workDir,
// and still wraps the cause so errors.Is(err, fs.ErrNotExist) holds
func Test_Unit_Workspace_Resolve_Missing_ErrorNamesRequestOnly(t *testing.T) {
	work := t.TempDir()
	mkdir(t, work, "sub")
	ws := newWorkspace(t, work, "", DenyList{})
	for _, p := range missingRelPaths {
		_, err := ws.resolve(p)
		expectErrorHidesWorkDir(t, "resolve("+p+")", err, work, p)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("resolve(%q): expected errors.Is(err, fs.ErrNotExist), got %v", p, err)
		}
	}
}

// - Verify read_file and list_files return the same workDir-free error for a missing path, still matching
// fs.ErrNotExist (this text is what reaches the model, and so the provider)
func Test_Unit_FileTools_Missing_ErrorHidesWorkDir(t *testing.T) {
	work := t.TempDir()
	mkdir(t, work, "sub")
	readFile := newTool(t, work)
	listFiles := newListTool(t, work, "", DenyList{})
	for _, p := range missingRelPaths {
		_, err := readPath(readFile, p)
		expectErrorHidesWorkDir(t, "read_file("+p+")", err, work, p)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("read_file(%q): expected errors.Is(err, fs.ErrNotExist), got %v", p, err)
		}
		_, err = callList(listFiles, p, "", false)
		expectErrorHidesWorkDir(t, "list_files("+p+")", err, work, p)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("list_files(%q): expected errors.Is(err, fs.ErrNotExist), got %v", p, err)
		}
	}
}

// - Verify check treats a path that doesn't exist as a directory, so a directory-only deny pattern ("secrets/") refuses
// it (for future write tools: refusing too much is the safe way to be wrong), while an existing FILE named secrets is
// allowed and an existing directory named secrets is refused
func Test_Unit_Workspace_Check_DirOnlyDeny_NonexistentCountsAsDir(t *testing.T) {
	ws := newWorkspace(t, t.TempDir(), "", newDenyList(t, "secrets/"))
	for _, rel := range []string{"secrets", filepath.Join("new", "secrets")} {
		if got, err := ws.check("REQUESTED-NAME", filepath.Join(ws.rootDir, rel)); err == nil || !strings.Contains(err.Error(), "deny list") {
			t.Errorf("check(nonexistent %q) = (%q, %v), want a deny-list error", rel, got, err)
		}
	}
	writeFile(t, ws.rootDir, "notes/secrets", []byte("x"))
	if rel, err := ws.check("x", filepath.Join(ws.rootDir, "notes", "secrets")); err != nil || rel != filepath.Join("notes", "secrets") {
		t.Errorf("check(file notes/secrets) = (%q, %v), want it allowed", rel, err)
	}
	mkdir(t, ws.rootDir, "app/secrets")
	if got, err := ws.check("x", filepath.Join(ws.rootDir, "app", "secrets")); err == nil || !strings.Contains(err.Error(), "deny list") {
		t.Errorf("check(directory app/secrets) = (%q, %v), want a deny-list error", got, err)
	}
}

//*************************************//
// resolveInfo and openFile (the check-then-open race)
//*************************************//

// lstatInfo returns os.Lstat(path), failing the test on error. Tests use it to take want the way resolveInfo does,
// before a simulated swap.
func lstatInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	return info
}

// openRootT opens ws's root, failing the test on error, and closes it when the test ends.
func openRootT(t *testing.T, ws *Workspace) *os.Root {
	t.Helper()
	root, err := ws.openRoot()
	if err != nil {
		t.Fatalf("openRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// expectOpenRefused fails the test unless openFile returned (nil, nil, an error containing wantMsg that names
// requested and hides workDir).
func expectOpenRefused(t *testing.T, what string, f *os.File, info os.FileInfo, err error, work, requested, wantMsg string) {
	t.Helper()
	if f != nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("%s: openFile succeeded and the file read %q; want it refused", what, b)
	}
	if info != nil {
		t.Errorf("%s: expected a nil info on error, got %v", what, info.Name())
	}
	expectErrorHidesWorkDir(t, what, err, work, requested)
	if err != nil && !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("%s: expected an error containing %q, got %q", what, wantMsg, err)
	}
}

// - Verify openFile opens an untouched file when want is that file's lstat, and returns the opened file's info
func Test_Unit_Workspace_OpenFile_Unswapped_OpensAndReturnsInfo(t *testing.T) {
	work := t.TempDir()
	path := writeFile(t, work, "a.txt", []byte("ALLOWED-A"))
	ws := newWorkspace(t, work, "", DenyList{})

	f, info, err := ws.openFile(openRootT(t, ws), "a.txt", "a.txt", lstatInfo(t, path))
	if err != nil {
		t.Fatalf("openFile(a.txt) = %v, want it opened", err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); string(b) != "ALLOWED-A" {
		t.Errorf("read %q, want %q", b, "ALLOWED-A")
	}
	if info == nil || !os.SameFile(info, lstatInfo(t, path)) || info.Size() != int64(len("ALLOWED-A")) {
		t.Errorf("expected the returned info to describe a.txt, got %v", info)
	}
}

// - Verify openFile refuses when want is nil (no approved file to compare with), even for an untouched regular file
func Test_Unit_Workspace_OpenFile_NilWant_Refused(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("ALLOWED-A"))
	ws := newWorkspace(t, work, "", DenyList{})

	f, info, err := ws.openFile(openRootT(t, ws), "a.txt", "a.txt", nil)
	expectOpenRefused(t, "openFile(a.txt, want nil)", f, info, err, work, "a.txt", "was replaced while it was being opened")
}

// - Verify openFile refuses a directory with "is a directory, not a file", naming the requested path only
func Test_Unit_Workspace_OpenFile_Directory_Refused(t *testing.T) {
	work := t.TempDir()
	mkdir(t, work, "sub")
	ws := newWorkspace(t, work, "", DenyList{})

	f, info, err := ws.openFile(openRootT(t, ws), "sub", "sub", lstatInfo(t, filepath.Join(work, "sub")))
	expectOpenRefused(t, "openFile(sub)", f, info, err, work, "sub", "sub is a directory, not a file")
}

// - Verify openFile refuses a.txt when another regular file was renamed over it after want was taken (same name,
// different file), and never returns the new file's contents
func Test_Unit_Workspace_OpenFile_RenamedOver_Refused(t *testing.T) {
	work := t.TempDir()
	path := writeFile(t, work, "a.txt", []byte("ALLOWED-A"))
	other := writeFile(t, work, "b.txt", []byte("SWAPPED-IN"))
	ws := newWorkspace(t, work, "", DenyList{})
	want := lstatInfo(t, path)
	if err := os.Rename(other, path); err != nil {
		t.Fatalf("renaming b.txt over a.txt: %v", err)
	}

	f, info, err := ws.openFile(openRootT(t, ws), "a.txt", "a.txt", want)
	expectOpenRefused(t, "openFile(a.txt renamed over)", f, info, err, work, "a.txt", "was replaced while it was being opened; try again")
}

// - Verify resolveInfo returns the rel path and an info that is the same file as the one requested (by relative and
// absolute path), and resolve still returns the same rel
func Test_Unit_Workspace_ResolveInfo_RegularFile_SameFile(t *testing.T) {
	work := t.TempDir()
	path := writeFile(t, work, "sub/a.txt", []byte("ALLOWED-A"))
	ws := newWorkspace(t, work, "", DenyList{})
	wantRel := filepath.Join("sub", "a.txt")

	for _, p := range []string{wantRel, path} {
		rel, info, err := ws.resolveInfo(p)
		if err != nil || rel != wantRel {
			t.Fatalf("resolveInfo(%q) = (%q, %v), want (%q, nil)", p, rel, err, wantRel)
		}
		if info == nil || !os.SameFile(info, lstatInfo(t, path)) || !info.Mode().IsRegular() {
			t.Errorf("resolveInfo(%q): expected the info of sub/a.txt, got %v", p, info)
		}
		if r, err := ws.resolve(p); err != nil || r != rel {
			t.Errorf("resolve(%q) = (%q, %v), want the same rel as resolveInfo (%q)", p, r, err, rel)
		}
	}
}

// - Verify resolveInfo refuses what resolve refuses (missing, deny-listed, outside workDir, inside traceDir), with
// the same error, a nil info, and no absolute workDir in the message
func Test_Unit_Workspace_ResolveInfo_Refused_SameErrorsAsResolve(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	writeFile(t, work, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, work, "traces/run.jsonl", []byte("FAKE_SECRET_TOKEN_123"))
	outside := writeFile(t, parent, "outside.txt", []byte("OUTSIDE"))
	ws := newWorkspace(t, work, filepath.Join(work, "traces"), DenyList{})

	for p, wantMsg := range map[string]string{
		"missing.txt":                        "no such file",
		".env":                               "deny list",
		outside:                              "outside the allowed directory",
		filepath.Join("..", "outside.txt"):   "outside the allowed directory",
		filepath.Join("traces", "run.jsonl"): "inside the trace directory",
	} {
		rel, info, err := ws.resolveInfo(p)
		if rel != "" || info != nil || err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("resolveInfo(%q) = (%q, %v, %v), want (\"\", nil, an error containing %q)", p, rel, info, err, wantMsg)
			continue
		}
		expectErrorHidesWorkDir(t, "resolveInfo("+p+")", err, work, p) // an absolute request is named as given; outside.txt's path doesn't contain workDir
		if _, rerr := ws.resolve(p); rerr == nil || rerr.Error() != err.Error() {
			t.Errorf("resolve(%q) error = %v, want the same as resolveInfo's %q", p, rerr, err)
		}
	}
}

// - Verify read_file on a subdirectory (relative path) still says "<path> is a directory, not a file" and does not
// leak the absolute workDir
func Test_ReadFile_Subdirectory_IsADirectoryError(t *testing.T) {
	work := t.TempDir()
	mkdir(t, work, "sub")

	got, err := readPath(newTool(t, work), "sub")
	if got != "" {
		t.Errorf("expected no output, got %q", got)
	}
	expectErrorHidesWorkDir(t, "read_file(sub)", err, work, "sub is a directory, not a file")
}
