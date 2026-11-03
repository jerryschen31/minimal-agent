//go:build unix

package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// - Verify copy and move through a symlink that points outside workDir are refused, as source and as destination, and
// the outside files are untouched
func Test_CopyMove_SymlinkOutside_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	outsideDir := filepath.Join(parent, "outside")
	for _, d := range []string{work, outsideDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := writeFile(t, outsideDir, "victim.txt", []byte("SAFE"))
	writeFile(t, work, "ok.txt", []byte("OK"))
	symlink(t, victim, filepath.Join(work, "link.txt"))
	symlink(t, outsideDir, filepath.Join(work, "linkdir"))
	cp, mv := newCopyTool(t, work, "", ""), newMoveTool(t, work, "", "")
	for _, tc := range []struct{ src, dst string }{{"link.txt", "x.txt"}, {"ok.txt", "link.txt"}, {"ok.txt", "linkdir/new.txt"}} {
		if _, err := copyCall(cp, tc.src, tc.dst); err == nil || !strings.Contains(err.Error(), "outside the allowed directory") {
			t.Errorf("copy %v: error = %v", tc, err)
		}
		if _, err := moveCall(mv, tc.src, tc.dst); err == nil || !strings.Contains(err.Error(), "outside the allowed directory") {
			t.Errorf("move %v: error = %v", tc, err)
		}
	}
	if c := readBack(t, outsideDir, "victim.txt"); c != "SAFE" {
		t.Errorf("outside file changed: %q", c)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); err == nil {
		t.Error("a file was created outside workDir")
	}
	if c := readBack(t, work, "ok.txt"); c != "OK" {
		t.Errorf("ok.txt = %q", c)
	}
}

// - Verify a symlink inside workDir to .env cannot be used as a copy source (a way to read the secret) or a move source,
// and a symlink to .git cannot be a destination
func Test_CopyMove_SymlinkToProtected_Refused(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, ".env", []byte("SECRET"))
	writeFile(t, work, ".git/config", []byte("GIT"))
	writeFile(t, work, "ok.txt", []byte("OK"))
	symlink(t, filepath.Join(work, ".env"), filepath.Join(work, "notes.txt"))
	symlink(t, filepath.Join(work, ".git", "config"), filepath.Join(work, "cfg"))
	cp, mv := newCopyTool(t, work, "", ""), newMoveTool(t, work, "", "")
	if _, err := copyCall(cp, "notes.txt", "leak.txt"); err == nil || !strings.Contains(err.Error(), "deny list") {
		t.Errorf("copy from .env link: error = %v", err)
	}
	if _, err := moveCall(mv, "notes.txt", "leak.txt"); err == nil || !strings.Contains(err.Error(), "deny list") {
		t.Errorf("move from .env link: error = %v", err)
	}
	if _, err := copyCall(cp, "ok.txt", "cfg"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("copy onto .git link: error = %v", err)
	}
	if _, err := moveCall(mv, "ok.txt", "cfg"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("move onto .git link: error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "leak.txt")); err == nil {
		t.Error("leak.txt was created")
	}
	if c := readBack(t, work, ".git/config"); c != "GIT" {
		t.Errorf(".git/config = %q", c)
	}
}

// - Verify hard links: copy refuses a source or an overwritten destination with several names; a move of a hard-linked
// name works but never touches the protected file (rename replaces only the name), and renaming one name of an inode
// onto another name of the same inode is refused
func Test_CopyMove_HardLinks(t *testing.T) {
	work := t.TempDir()
	env := writeFile(t, work, ".env", []byte("SECRET"))
	writeFile(t, work, "ok.txt", []byte("OK"))
	hardlink(t, env, filepath.Join(work, "alias.txt"))
	a := writeFile(t, work, "one.txt", []byte("ONE"))
	hardlink(t, a, filepath.Join(work, "two.txt"))
	cp, mv := newCopyTool(t, work, "", ""), newMoveTool(t, work, "", "")
	if _, err := copyCall(cp, "alias.txt", "leak.txt"); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("copy from an alias of .env: error = %v", err)
	}
	if _, err := copyCall(cp, "ok.txt", "alias.txt"); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("copy over an alias of .env: error = %v", err)
	}
	if c := readBack(t, work, ".env"); c != "SECRET" {
		t.Errorf(".env = %q", c)
	}
	if _, err := moveCall(mv, "ok.txt", "alias.txt"); err != nil { // replaces the NAME alias.txt only
		t.Errorf("move over an alias name: %v", err)
	}
	if c := readBack(t, work, ".env"); c != "SECRET" {
		t.Errorf(".env changed by a rename over its alias: %q", c)
	}
	if c := readBack(t, work, "alias.txt"); c != "OK" {
		t.Errorf("alias.txt = %q", c)
	}
	if _, err := moveCall(mv, "one.txt", "two.txt"); err == nil || !strings.Contains(err.Error(), "same file") {
		t.Errorf("rename onto another name of the same inode: error = %v", err)
	}
}

// - Verify copy and move refuse a destination parent that was swapped, after the approval, for a relative symlink to
// .git, and write nothing into .git
func Test_CopyMove_DestinationParentSwapped_Refused(t *testing.T) {
	for _, name := range []string{"copy", "move"} {
		work := t.TempDir()
		if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(work, "dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, work, "ok.txt", []byte("OK"))
		swap := func() {
			if err := os.Rename(filepath.Join(work, "dir"), filepath.Join(work, "dir.orig")); err != nil {
				panic(err)
			}
			if err := os.Symlink(".git", filepath.Join(work, "dir")); err != nil {
				panic(err)
			}
		}
		var err error
		if name == "copy" {
			tool := newCopyTool(t, work, "", "")
			tool.afterResolve = swap
			_, err = copyCall(tool, "ok.txt", "dir/pre-commit")
		} else {
			tool := newMoveTool(t, work, "", "")
			tool.afterResolve = swap
			_, err = moveCall(tool, "ok.txt", "dir/pre-commit")
		}
		if err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Errorf("%s: error = %v", name, err)
		}
		if entries, _ := os.ReadDir(filepath.Join(work, ".git", "hooks")); len(entries) != 0 {
			t.Errorf("%s: something was written into .git/hooks: %v", name, entries)
		}
		if c := readBack(t, work, "ok.txt"); c != "OK" {
			t.Errorf("%s: ok.txt = %q (a refused move must leave the source)", name, c)
		}
	}
}

// - Verify move refuses a source that was replaced by a different file after the approval, and the other file is
// untouched; and that a source parent swapped for a symlink is refused too
func Test_MoveFile_SourceSwapped_Refused(t *testing.T) {
	work := t.TempDir()
	p := writeFile(t, work, "a.txt", []byte("APPROVED"))
	tool := newMoveTool(t, work, "", "")
	tool.afterResolve = func() {
		if err := os.Remove(p); err != nil {
			panic(err)
		}
		if err := os.WriteFile(p, []byte("OTHER FILE"), 0o644); err != nil {
			panic(err)
		}
	}
	if _, err := moveCall(tool, "a.txt", "b.txt"); err == nil || !strings.Contains(err.Error(), "replaced or moved") {
		t.Errorf("error = %v", err)
	}
	if c := readBack(t, work, "a.txt"); c != "OTHER FILE" {
		t.Errorf("a.txt = %q", c)
	}
	if _, err := os.Stat(filepath.Join(work, "b.txt")); err == nil {
		t.Error("b.txt was created")
	}

	work2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work2, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, work2, "sub/a.txt", []byte("A"))
	tool2 := newMoveTool(t, work2, "", "")
	tool2.afterResolve = func() {
		os.Rename(filepath.Join(work2, "sub"), filepath.Join(work2, "sub.orig"))
		os.Symlink(".git", filepath.Join(work2, "sub"))
	}
	if _, err := moveCall(tool2, "sub/a.txt", "b.txt"); err == nil {
		t.Error("source parent swapped: want an error")
	}
	if c := readBack(t, work2, "sub.orig/a.txt"); c != "A" {
		t.Errorf("the original source = %q", c)
	}
}

// - Verify special files are refused without blocking: a named pipe as copy source, copy destination and move
// destination
func Test_CopyMove_NamedPipe_RefusedWithoutBlocking(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "ok.txt", []byte("OK"))
	if err := syscall.Mkfifo(filepath.Join(work, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	cp, mv := newCopyTool(t, work, "", ""), newMoveTool(t, work, "", "")
	calls := map[string]func() error{
		"copy from a pipe": func() error { _, err := copyCall(cp, "pipe", "x.txt"); return err },
		"copy onto a pipe": func() error { _, err := copyCall(cp, "ok.txt", "pipe"); return err },
		"move onto a pipe": func() error { _, err := moveCall(mv, "ok.txt", "pipe"); return err },
		"move a pipe":      func() error { _, err := moveCall(mv, "pipe", "x.txt"); return err },
	}
	for name, call := range calls {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "named pipe") {
				t.Errorf("%s: error = %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked on a named pipe", name)
		}
	}
	if c := readBack(t, work, "ok.txt"); c != "OK" {
		t.Errorf("ok.txt = %q", c)
	}
}
