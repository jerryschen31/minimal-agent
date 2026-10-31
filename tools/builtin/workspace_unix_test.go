//go:build unix

package builtin

import (
	"errors"
	"io/fs"
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
