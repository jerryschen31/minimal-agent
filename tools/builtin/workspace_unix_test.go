//go:build unix

package builtin

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// - Verify a workDir given as a symlink is stored resolved (rootDir is the link's target), so check compares
// against the real location and a path spelled through the link maps to the same rel
func Test_Unit_NewWorkspace_SymlinkedWorkDir_RootDirResolved(t *testing.T) {
	real := realPath(t, t.TempDir())
	writeFile(t, real, "a.txt", []byte("ALLOWED-A"))
	link := filepath.Join(t.TempDir(), "worklink")
	symlink(t, real, link)

	ws := newWorkspace(t, link, "", DenyList{})
	if ws.rootDir != real {
		t.Errorf("rootDir = %q, want the resolved %q", ws.rootDir, real)
	}
	for _, p := range []string{"a.txt", filepath.Join(link, "a.txt"), filepath.Join(real, "a.txt")} {
		if rel, _, err := ws.resolvePath(p); err != nil || rel != "a.txt" {
			t.Errorf("resolve(%q) = (%q, %v), want (\"a.txt\", nil)", p, rel, err)
		}
	}
}

// - Verify resolve refuses a symlink inside workDir that points outside (it checks the link's target, not its name),
// and that the error names the requested link, not the target
func Test_Unit_Workspace_Resolve_SymlinkOutside_Refused(t *testing.T) {
	work := t.TempDir()
	outside := writeFile(t, t.TempDir(), "secret.txt", []byte("FAKE_SECRET_TOKEN_123"))
	symlink(t, outside, filepath.Join(work, "innocent.txt"))
	ws := newWorkspace(t, work, "", DenyList{})

	rel, _, err := ws.resolvePath("innocent.txt")
	if err == nil || rel != "" || !strings.Contains(err.Error(), "outside the allowed directory") {
		t.Fatalf("resolve(innocent.txt) = (%q, %v), want an outside-the-directory error", rel, err)
	}
	if !strings.HasPrefix(err.Error(), "innocent.txt ") || strings.Contains(err.Error(), "secret.txt") {
		t.Errorf("error should name the requested link, not its target: %q", err)
	}
}

// - Verify a dangling symlink inside workDir gives an error naming only the requested link (not the absolute workDir
// or the link's target path), wrapping fs.ErrNotExist, from resolve, read_file and list_files alike
func Test_Unit_Workspace_Resolve_DanglingSymlink_ErrorHidesWorkDir(t *testing.T) {
	work := t.TempDir()
	symlink(t, "nowhere-target.txt", filepath.Join(work, "dangling"))
	ws := newWorkspace(t, work, "", DenyList{})

	_, _, err := ws.resolvePath("dangling")
	expectErrorHidesWorkDir(t, "resolve(dangling)", err, work, "dangling")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected errors.Is(err, fs.ErrNotExist), got %v", err)
	}
	if strings.Contains(err.Error(), "nowhere-target") {
		t.Errorf("error names the link's target: %q", err)
	}
	_, err = readPath(newTool(t, work), "dangling")
	expectErrorHidesWorkDir(t, "read_file(dangling)", err, work, "dangling")
	_, err = callList(newListTool(t, work, "", DenyList{}), "dangling", "", false)
	expectErrorHidesWorkDir(t, "list_files(dangling)", err, work, "dangling")
}

// - Verify a symlink loop (loop-a -> loop-b -> loop-a), whose error from Go is not a *fs.PathError, still gives an
// error that names the requested link and not the absolute workDir, from resolve, read_file and list_files alike
func Test_Unit_Workspace_Resolve_SymlinkLoop_ErrorHidesWorkDir(t *testing.T) {
	work := t.TempDir()
	symlink(t, "loop-b", filepath.Join(work, "loop-a"))
	symlink(t, "loop-a", filepath.Join(work, "loop-b"))
	ws := newWorkspace(t, work, "", DenyList{})

	rel, _, err := ws.resolvePath("loop-a")
	if rel != "" {
		t.Errorf("expected no rel for a loop, got %q", rel)
	}
	expectErrorHidesWorkDir(t, "resolve(loop-a)", err, work, "loop-a")
	_, err = readPath(newTool(t, work), "loop-a")
	expectErrorHidesWorkDir(t, "read_file(loop-a)", err, work, "loop-a")
	_, err = callList(newListTool(t, work, "", DenyList{}), "loop-a", "", false)
	expectErrorHidesWorkDir(t, "list_files(loop-a)", err, work, "loop-a")
}

//*************************************//
// workDir moved or replaced after NewWorkspace

// movedWorkDirFixture is a workspace on parent/work (containing a.txt) with read_file and list_files sharing it, plus a
// directory outside it holding a secret and a different a.txt, so a test can swap work for something else.
type movedWorkDirFixture struct {
	work, out string // resolved paths
	ws        *Workspace
	read      *ReadFileTool
	list      *ListFilesTool
}

func newMovedWorkDirFixture(t *testing.T) movedWorkDirFixture {
	t.Helper()
	parent := realPath(t, t.TempDir())
	f := movedWorkDirFixture{work: filepath.Join(parent, "work"), out: filepath.Join(parent, "out")}
	writeFile(t, f.work, "a.txt", []byte("ALLOWED-A"))
	writeFile(t, f.out, "secret.txt", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, f.out, "a.txt", []byte("OUTSIDE-A"))
	f.ws = newWorkspace(t, f.work, "", DenyList{})
	var err error
	if f.read, err = NewReadFileTool(f.ws, testMaxBytes); err != nil {
		t.Fatalf("NewReadFileTool: %v", err)
	}
	if f.list, err = NewListFilesTool(f.ws, 10000, testIgnore(t)); err != nil {
		t.Fatalf("NewListFilesTool: %v", err)
	}
	return f
}

// moveAway renames work to work.old, so something else can be put at its path.
func (f movedWorkDirFixture) moveAway(t *testing.T) {
	t.Helper()
	if err := os.Rename(f.work, f.work+".old"); err != nil {
		t.Fatalf("renaming workDir away: %v", err)
	}
}

// moveBack removes whatever is at work's path and renames work.old back.
func (f movedWorkDirFixture) moveBack(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(f.work); err != nil {
		t.Fatalf("removing the replacement: %v", err)
	}
	if err := os.Rename(f.work+".old", f.work); err != nil {
		t.Fatalf("renaming workDir back: %v", err)
	}
}

// assertRefused checks a tool call failed, returned no output and leaked nothing from out; if wantMsg is set, the
// error must contain it.
func assertRefused(t *testing.T, what, got string, err error, wantMsg ...string) {
	t.Helper()
	if err == nil || got != "" {
		t.Errorf("%s: expected an error and no output, got (%q, %v)", what, got, err)
		return
	}
	// [agent] The error may name the requested path (secret.txt), but never the other directory's file contents.
	for _, leak := range []string{"OUTSIDE-A", "FAKE_SECRET"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("%s: leaked %q from outside workDir: (%q, %v)", what, leak, got, err)
		}
	}
	if len(wantMsg) > 0 && !strings.Contains(err.Error(), wantMsg[0]) {
		t.Errorf("%s: expected the error to contain %q, got %v", what, wantMsg[0], err)
	}
}

// - Verify the control case: with workDir untouched, read_file and list_files work on the fixture
func Test_Unit_Workspace_WorkDirUntouched_ToolsWork(t *testing.T) {
	f := newMovedWorkDirFixture(t)
	if got, err := readPath(f.read, "a.txt"); err != nil || got != "ALLOWED-A" {
		t.Errorf("read_file(a.txt) = (%q, %v), want (\"ALLOWED-A\", nil)", got, err)
	}
	if got := mustList(t, f.list, ".", "", false); !strings.Contains(got, "a.txt") {
		t.Errorf("list_files(.) = %q, want it to list a.txt", got)
	}
}

// - Verify that after workDir is renamed and a symlink to another directory put at its path, read_file and list_files
// refuse every path (by resolve's outside check or openRoot's same-directory check) and nothing from the other
// directory reaches the output
func Test_Unit_Workspace_WorkDirSwappedForSymlink_ToolsRefuse(t *testing.T) {
	f := newMovedWorkDirFixture(t)
	f.moveAway(t)
	symlink(t, f.out, f.work)

	for _, p := range []string{"a.txt", "secret.txt"} {
		got, err := readPath(f.read, p)
		assertRefused(t, "read_file("+p+")", got, err)
	}
	got, err := callList(f.list, ".", "", true)
	assertRefused(t, "list_files(.)", got, err)
}

// - Verify that after workDir is renamed and a new directory created at its path (so every path still resolves inside
// rootDir), read_file and list_files are refused by openRoot with the "moved or replaced" error, which names no path
func Test_Unit_Workspace_WorkDirReplacedByNewDir_ToolsRefuse(t *testing.T) {
	f := newMovedWorkDirFixture(t)
	f.moveAway(t)
	if err := os.Mkdir(f.work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.work, "a.txt", []byte("OUTSIDE-A"))
	writeFile(t, f.work, "secret.txt", []byte("FAKE_SECRET_TOKEN_123"))

	for _, p := range []string{"a.txt", "secret.txt"} {
		got, err := readPath(f.read, p)
		assertRefused(t, "read_file("+p+")", got, err, "moved or replaced")
		if err != nil && strings.Contains(err.Error(), f.work) {
			t.Errorf("read_file(%s): error names the absolute workDir: %v", p, err)
		}
	}
	got, err := callList(f.list, ".", "", true)
	assertRefused(t, "list_files(.)", got, err, "moved or replaced")
}

// - Verify moving the original workDir back (same directory, same inode) makes read_file and list_files work again,
// after both a symlink swap and a new-directory swap
func Test_Unit_Workspace_WorkDirMovedBack_ToolsWorkAgain(t *testing.T) {
	f := newMovedWorkDirFixture(t)
	for name, replace := range map[string]func(){
		"symlink": func() { symlink(t, f.out, f.work) },
		"new dir": func() { writeFile(t, f.work, "a.txt", []byte("OUTSIDE-A")) },
	} {
		f.moveAway(t)
		replace()
		if _, err := readPath(f.read, "a.txt"); err == nil {
			t.Fatalf("%s: expected read_file to fail while workDir is replaced", name)
		}
		f.moveBack(t)
		if got, err := readPath(f.read, "a.txt"); err != nil || got != "ALLOWED-A" {
			t.Errorf("%s, moved back: read_file(a.txt) = (%q, %v), want (\"ALLOWED-A\", nil)", name, got, err)
		}
		if got, err := callList(f.list, ".", "", false); err != nil || !strings.Contains(got, "a.txt") {
			t.Errorf("%s, moved back: list_files(.) = (%q, %v), want a.txt listed", name, got, err)
		}
	}
}

// - Verify openRoot itself returns the "moved or replaced" error and a nil root after workDir is swapped for a symlink
// or a new directory, and opens normally once the original is moved back
func Test_Unit_Workspace_OpenRoot_WorkDirSwapped_ReturnsError(t *testing.T) {
	f := newMovedWorkDirFixture(t)
	for name, replace := range map[string]func(){
		"symlink": func() { symlink(t, f.out, f.work) },
		"new dir": func() { writeFile(t, f.work, "a.txt", []byte("OUTSIDE-A")) },
	} {
		f.moveAway(t)
		replace()
		root, err := f.ws.openRoot()
		if err == nil || root != nil || !strings.Contains(err.Error(), "moved or replaced") {
			t.Errorf("%s: openRoot() = (%v, %v), want (nil, a moved-or-replaced error)", name, root, err)
		}
		if root != nil {
			root.Close()
		}
		f.moveBack(t)
		root, err = f.ws.openRoot()
		if err != nil {
			t.Fatalf("%s, moved back: openRoot: %v", name, err)
		}
		root.Close()
	}
}

// - Verify the gap openRoot closes: resolve accepts a.txt, workDir is then swapped for a symlink to another directory
// before the file is opened, and openRoot refuses (without the check, the root would open the other directory and
// read its a.txt)
func Test_Unit_Workspace_OpenRoot_SwapAfterResolve_Refused(t *testing.T) {
	f := newMovedWorkDirFixture(t)
	rel, _, err := f.ws.resolvePath("a.txt")
	if err != nil || rel != "a.txt" {
		t.Fatalf("resolve(a.txt) before the swap = (%q, %v), want (\"a.txt\", nil)", rel, err)
	}
	f.moveAway(t)
	symlink(t, f.out, f.work)

	root, err := f.ws.openRoot()
	if err == nil {
		b, _ := root.ReadFile(rel)
		root.Close()
		t.Fatalf("openRoot after the swap succeeded; reading %s through it gave %q", rel, b)
	}
	if root != nil || !strings.Contains(err.Error(), "moved or replaced") {
		t.Errorf("openRoot() = (%v, %v), want (nil, a moved-or-replaced error)", root, err)
	}
}

// swapForSymlink replaces the file at path with a symlink to target (simulating another program swapping it in
// between the workspace check and the open).
func swapForSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
	symlink(t, target, path)
}

// - Verify the race openFile closes: want is a.txt's lstat (what resolvePath approved), a.txt is then swapped for a
// symlink to .env, and openFile refuses instead of returning .env's contents (os.Root alone would follow the link,
// since .env is inside workDir)
func Test_Unit_Workspace_OpenFile_SwappedForSymlinkToDenyListed_Refused(t *testing.T) {
	work := t.TempDir()
	path := writeFile(t, work, "a.txt", []byte("ALLOWED-A"))
	writeFile(t, work, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	ws := newWorkspace(t, work, "", DenyList{})
	rel, want, err := ws.resolvePath("a.txt")
	if err != nil {
		t.Fatalf("resolvePath(a.txt) before the swap: %v", err)
	}
	swapForSymlink(t, path, ".env")

	f, info, err := ws.openFile(openRootT(t, ws), "a.txt", rel, want)
	expectOpenRefused(t, "openFile(a.txt -> .env)", f, info, err, work, "a.txt", "a.txt was replaced while it was being opened; try again")
	if err != nil && strings.Contains(err.Error(), "FAKE_SECRET") {
		t.Errorf("error leaks the secret: %q", err)
	}
}

// - Verify the same race into the trace directory: a.txt swapped for a symlink to a trace file after the check is
// refused at open
func Test_Unit_Workspace_OpenFile_SwappedForSymlinkIntoTraceDir_Refused(t *testing.T) {
	work := t.TempDir()
	path := writeFile(t, work, "a.txt", []byte("ALLOWED-A"))
	writeFile(t, work, "traces/run.jsonl", []byte("FAKE_SECRET_TOKEN_123"))
	ws := newWorkspace(t, work, filepath.Join(work, "traces"), DenyList{})
	rel, want, err := ws.resolvePath("a.txt")
	if err != nil {
		t.Fatalf("resolvePath(a.txt) before the swap: %v", err)
	}
	swapForSymlink(t, path, filepath.Join("traces", "run.jsonl"))

	f, info, err := ws.openFile(openRootT(t, ws), "a.txt", rel, want)
	expectOpenRefused(t, "openFile(a.txt -> traces/run.jsonl)", f, info, err, work, "a.txt", "was replaced while it was being opened")
}

// - Verify resolvePath on a symlink (notes.txt -> real.txt) returns the resolved rel and real.txt's info, not the
// link's, so openFile (which follows the link) accepts it
func Test_Unit_Workspace_ResolveInfo_Symlink_ReturnsTargetInfo(t *testing.T) {
	work := t.TempDir()
	target := writeFile(t, work, "real.txt", []byte("ALLOWED-REAL"))
	symlink(t, "real.txt", filepath.Join(work, "notes.txt"))
	ws := newWorkspace(t, work, "", DenyList{})

	rel, info, err := ws.resolvePath("notes.txt")
	if err != nil || rel != "real.txt" {
		t.Fatalf("resolvePath(notes.txt) = (%q, %v), want (\"real.txt\", nil)", rel, err)
	}
	if info == nil || info.Mode()&fs.ModeSymlink != 0 || !os.SameFile(info, lstatInfo(t, target)) {
		t.Fatalf("expected real.txt's info, got %v", info)
	}
	f, _, err := ws.openFile(openRootT(t, ws), "notes.txt", rel, info)
	if err != nil {
		t.Fatalf("openFile(notes.txt) with resolvePath's info: %v", err)
	}
	f.Close()
}

// - Verify resolvePath refuses a symlink to a deny-listed file with a nil info and an error naming only the link
func Test_Unit_Workspace_ResolveInfo_SymlinkToDenyListed_Refused(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	symlink(t, ".env", filepath.Join(work, "notes.txt"))
	ws := newWorkspace(t, work, "", DenyList{})

	rel, info, err := ws.resolvePath("notes.txt")
	if rel != "" || info != nil || err == nil || !strings.Contains(err.Error(), "deny list") {
		t.Fatalf("resolvePath(notes.txt -> .env) = (%q, %v, %v), want a deny-list error", rel, info, err)
	}
	expectErrorHidesWorkDir(t, "resolvePath(notes.txt)", err, work, "notes.txt")
}

// - Verify a KNOWN LIMIT (documented on openFile, not a bug this test asks to fix): if a directory above the file is
// swapped in the moment between resolving the name and taking want, want and the open both follow the new directory,
// so they match and openFile opens the swapped-in file. Simulated by taking want after the swap (d -> .ssh). If the
// gap is ever closed, this test should be changed to expect a refusal.
func Test_Unit_Workspace_OpenFile_KnownLimit_DirSwappedBeforeWantTaken_NotDetected(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "d/a.txt", []byte("ALLOWED-A"))
	writeFile(t, work, ".ssh/a.txt", []byte("FAKE_SECRET_TOKEN_123"))
	ws := newWorkspace(t, work, "", DenyList{})
	rel, _, err := ws.resolvePath(filepath.Join("d", "a.txt")) // the name is approved
	if err != nil {
		t.Fatalf("resolve(d/a.txt): %v", err)
	}
	if err := os.Rename(filepath.Join(work, "d"), filepath.Join(work, "d-old")); err != nil {
		t.Fatal(err)
	}
	symlink(t, ".ssh", filepath.Join(work, "d"))
	want := lstatInfo(t, filepath.Join(work, "d", "a.txt")) // the Lstat follows the swapped-in directory too

	f, _, err := ws.openFile(openRootT(t, ws), rel, rel, want)
	if err != nil {
		t.Fatalf("expected the known gap (the swap goes undetected), but openFile refused: %v; update this test", err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); string(b) != "FAKE_SECRET_TOKEN_123" {
		t.Errorf("expected the swapped-in file to be opened (the known gap), read %q", b)
	}
}
