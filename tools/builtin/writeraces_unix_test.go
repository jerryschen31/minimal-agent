//go:build unix

package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hardlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
}

// - Verify write_file and edit_file refuse a file that is a hard link to .env, .git/config or the config file, under an
// allowed name, and leave the protected file unchanged. read_file of such a link is still allowed.
func Test_WriteTools_HardLinkToProtectedFile_Refused(t *testing.T) {
	work := t.TempDir()
	env := writeFile(t, work, ".env", []byte("SECRET"))
	gitCfg := writeFile(t, work, ".git/config", []byte("SECRET"))
	cfg := writeFile(t, work, "agent.json", []byte("SECRET"))
	hardlink(t, env, filepath.Join(work, "notes.txt"))
	hardlink(t, gitCfg, filepath.Join(work, "gitcfg.txt"))
	hardlink(t, cfg, filepath.Join(work, "copy.json"))
	ws, err := NewWorkspace(work, "", cfg, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := NewWriteFileTool(ws)
	et, _ := NewEditFileTool(ws)
	for _, alias := range []string{"notes.txt", "gitcfg.txt", "copy.json"} {
		if _, err := writeCall(wt, alias, "PWNED"); err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Errorf("write_file %s: error = %v", alias, err)
		}
		if _, err := editCall(et, alias, "SECRET", "PWNED", false); err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Errorf("edit_file %s: error = %v", alias, err)
		}
	}
	for _, f := range []string{".env", ".git/config", "agent.json", "notes.txt"} {
		if c := readBack(t, work, f); c != "SECRET" {
			t.Errorf("%s changed: %q", f, c)
		}
	}
	// the link count is only a problem for writes: a plain read of the alias still works (an existing, separate gap)
	rt, _ := NewReadFileTool(ws, 4096)
	if _, err := readPath(rt, "gitcfg.txt"); err != nil {
		t.Errorf("read through an alias: %v", err)
	}
}

// - Verify a file with one name is still written and edited, and that a file that gets a second name is refused
func Test_WriteTools_SingleName_Allowed_SecondNameRefused(t *testing.T) {
	work := t.TempDir()
	p := writeFile(t, work, "a.txt", []byte("one"))
	ws := newWorkspace(t, work, "", DenyList{})
	wt, _ := NewWriteFileTool(ws)
	if _, err := writeCall(wt, "a.txt", "two"); err != nil {
		t.Fatalf("single name: %v", err)
	}
	hardlink(t, p, filepath.Join(work, "b.txt"))
	if _, err := writeCall(wt, "a.txt", "three"); err == nil {
		t.Error("two names: want an error")
	}
	if c := readBack(t, work, "a.txt"); c != "two" {
		t.Errorf("a.txt = %q", c)
	}
}

// - Verify openDir refuses a directory above the new file that was swapped, after the approval, for a symlink to .git
// (inside rootDir, so os.Root alone would follow it), for write_file's and create_directory's parent
func Test_OpenDir_ParentSwappedForSymlinkToGit_Refused(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := newWorkspace(t, work, "", DenyList{})
	for _, p := range []string{"a/b/new.txt", "a/b/c/d/new.txt", "a/new.txt"} {
		target, err := ws.resolveNewPath(p)
		if err != nil {
			t.Fatalf("approve %s: %v", p, err)
		}
		// the swap: a (or b) is replaced by a symlink to .git after the approval
		// a RELATIVE link: os.Root itself refuses a link with an absolute target, but follows a relative one that stays
		// inside rootDir, which is the case openDir exists for
		swapped, linkTarget := filepath.Join(work, "a"), ".git"
		if strings.HasPrefix(p, "a/b") {
			swapped, linkTarget = filepath.Join(work, "a", "b"), filepath.Join("..", ".git")
		}
		backup := swapped + ".orig"
		if err := os.Rename(swapped, backup); err != nil {
			t.Fatal(err)
		}
		symlink(t, linkTarget, swapped)

		root, err := ws.openRoot()
		if err != nil {
			t.Fatal(err)
		}
		dir, tail, err := ws.openDir(root, p, target)
		if err == nil {
			dir.Close()
			t.Errorf("%s: openDir accepted a swapped directory (tail %q)", p, tail)
		} else if !strings.Contains(err.Error(), "replaced") {
			t.Errorf("%s: error = %v", p, err)
		}
		root.Close()

		os.Remove(swapped) // undo for the next case
		if err := os.Rename(backup, swapped); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(work, ".git", "hooks")); len(entries) != 0 {
		t.Errorf("something was written into .git/hooks: %v", entries)
	}
}

// - Verify openDir accepts the directory that was approved, returns the right tail, and works for rootDir itself
func Test_OpenDir_ApprovedDirectory_Accepted(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := newWorkspace(t, work, "", DenyList{})
	for p, wantTail := range map[string]string{"a/b/new.txt": "new.txt", "a/x/y/new.txt": filepath.Join("x", "y", "new.txt"), "top.txt": "top.txt", "a/b": "b", ".": "."} {
		target, err := ws.resolveNewPath(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		root, _ := ws.openRoot()
		dir, tail, err := ws.openDir(root, p, target)
		if err != nil || tail != wantTail {
			t.Errorf("%s: (tail %q, %v), want tail %q", p, tail, err, wantTail)
		}
		if dir != nil {
			dir.Close()
		}
		root.Close()
	}
}

// - Verify edit_file refuses (and changes neither file) when the file is renamed away and a different file is put at its
// path after it was opened: the edit must not be applied to the unlinked old file while reporting success
func Test_EditFile_RenamedAfterOpen_Refused(t *testing.T) {
	work := t.TempDir()
	p := writeFile(t, work, "a.txt", []byte("original text"))
	tool := newEditTool(t, work, "", "")
	tool.afterRead = func() {
		if err := os.Rename(p, p+".moved"); err != nil {
			panic(err)
		}
		if err := os.WriteFile(p, []byte("replacement text"), 0o644); err != nil {
			panic(err)
		}
	}
	_, err := editCall(tool, "a.txt", "text", "EDITED", false)
	if err == nil || !strings.Contains(err.Error(), "replaced or moved") {
		t.Fatalf("error = %v", err)
	}
	if c := readBack(t, work, "a.txt"); c != "replacement text" {
		t.Errorf("the file at the path = %q", c)
	}
	if c := readBack(t, work, "a.txt.moved"); c != "original text" {
		t.Errorf("the moved file was written: %q", c)
	}
}

// - Verify stillAtPath, as write_file's overwrite uses it: true for the file at its path, an error once the path names a
// different file or nothing
func Test_StillAtPath(t *testing.T) {
	work := t.TempDir()
	p := writeFile(t, work, "a.txt", []byte("x"))
	ws := newWorkspace(t, work, "", DenyList{})
	root, _ := ws.openRoot()
	defer root.Close()
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := ws.stillAtPath(root, "a.txt", "a.txt", f); err != nil {
		t.Fatalf("same file: %v", err)
	}
	if err := os.Rename(p, p+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := ws.stillAtPath(root, "a.txt", "a.txt", f); err == nil {
		t.Error("path names nothing: want an error")
	}
	writeFile(t, work, "a.txt", []byte("other"))
	if err := ws.stillAtPath(root, "a.txt", "a.txt", f); err == nil || !strings.Contains(err.Error(), "replaced or moved") {
		t.Errorf("path names another file: error = %v", err)
	}
}

// - Verify move_file refuses a source that is a hard link to .env, .git/config or the config file under an allowed name
// (a rename would keep the protected inode under a readable name), and leaves everything where it was; a file with one
// name still moves
func Test_MoveFile_HardLinkedSource_Refused(t *testing.T) {
	work := t.TempDir()
	env := writeFile(t, work, ".env", []byte("SECRET"))
	gitCfg := writeFile(t, work, ".git/config", []byte("SECRET"))
	cfg := writeFile(t, work, "agent.json", []byte("SECRET"))
	hardlink(t, env, filepath.Join(work, "notes.txt"))
	hardlink(t, gitCfg, filepath.Join(work, "gitcfg.txt"))
	hardlink(t, cfg, filepath.Join(work, "copy.json"))
	writeFile(t, work, "plain.txt", []byte("OK"))
	tool := newMoveTool(t, work, "", cfg)
	for _, alias := range []string{"notes.txt", "gitcfg.txt", "copy.json"} {
		_, err := moveCall(tool, alias, "moved-"+alias)
		if err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Errorf("move %s: error = %v, want a hard-link refusal", alias, err)
		}
		if _, statErr := os.Stat(filepath.Join(work, "moved-"+alias)); statErr == nil {
			t.Errorf("%s was moved", alias)
		}
		if _, statErr := os.Stat(filepath.Join(work, alias)); statErr != nil {
			t.Errorf("%s is gone: %v", alias, statErr)
		}
	}
	if got, err := moveCall(tool, "plain.txt", "plain2.txt"); err != nil || !strings.Contains(got, "moved plain.txt -> plain2.txt") {
		t.Errorf("single-name file: (%q, %v)", got, err)
	}
}
