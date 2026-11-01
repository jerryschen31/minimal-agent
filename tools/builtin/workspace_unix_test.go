//go:build unix

package builtin

import (
	"errors"
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
		if rel, err := ws.resolve(p); err != nil || rel != "a.txt" {
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

	rel, err := ws.resolve("innocent.txt")
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

	_, err := ws.resolve("dangling")
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

	rel, err := ws.resolve("loop-a")
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
	rel, err := f.ws.resolve("a.txt")
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
