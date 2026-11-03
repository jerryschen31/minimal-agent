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

// - Verify a write through a symlink that points outside workDir is refused and the outside file is untouched, both
// for an existing outside file and for a new file in an outside directory
func Test_WriteFile_SymlinkOutside_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	outsideDir := filepath.Join(parent, "outside")
	for _, d := range []string{work, outsideDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := writeFile(t, outsideDir, "victim.txt", []byte("SAFE"))
	symlink(t, victim, filepath.Join(work, "link.txt"))
	symlink(t, outsideDir, filepath.Join(work, "linkdir"))
	tool := newWriteTool(t, work, "", "", DenyList{})
	for _, p := range []string{"link.txt", "linkdir/new.txt"} {
		if _, err := writeCall(tool, p, "PWNED"); err == nil || !strings.Contains(err.Error(), "outside the allowed directory") {
			t.Errorf("write %q: error = %v", p, err)
		}
	}
	if c := readBack(t, outsideDir, "victim.txt"); c != "SAFE" {
		t.Errorf("outside file changed: %q", c)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); err == nil {
		t.Error("file created outside workDir")
	}
}

// - Verify a symlink inside workDir to a deny-listed file or to .git cannot be used to write to them (the check runs on
// the resolved path)
func Test_WriteFile_SymlinkToDeniedTargets_Refused(t *testing.T) {
	work := t.TempDir()
	env := writeFile(t, work, ".env", []byte("SECRET"))
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, env, filepath.Join(work, "notes.txt"))
	symlink(t, filepath.Join(work, ".git", "hooks"), filepath.Join(work, "h"))
	tool := newWriteTool(t, work, "", "", DenyList{})
	for p, want := range map[string]string{"notes.txt": "deny list", "h/pre-commit": "read-only"} {
		if _, err := writeCall(tool, p, "x"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("write %q: error = %v, want %q", p, err, want)
		}
	}
	if c := readBack(t, work, ".env"); c != "SECRET" {
		t.Errorf(".env changed: %q", c)
	}
}

// - Verify a symlink inside workDir to another allowed file writes THROUGH to that file (the tool reports the resolved
// name), and a dangling symlink is refused
func Test_WriteFile_SymlinkInsideWorkDir_WritesTarget_DanglingRefused(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "real.txt", []byte("old"))
	symlink(t, filepath.Join(work, "real.txt"), filepath.Join(work, "alias.txt"))
	symlink(t, filepath.Join(work, "nowhere.txt"), filepath.Join(work, "dangling.txt"))
	tool := newWriteTool(t, work, "", "", DenyList{})
	got, err := writeCall(tool, "alias.txt", "via alias")
	if err != nil || got != "overwrote real.txt (9 bytes)" {
		t.Fatalf("(%q, %v)", got, err)
	}
	if c := readBack(t, work, "real.txt"); c != "via alias" {
		t.Errorf("real.txt = %q", c)
	}
	if _, err := writeCall(tool, "dangling.txt", "x"); err == nil {
		t.Error("dangling symlink: want an error")
	}
	if _, err := os.Stat(filepath.Join(work, "nowhere.txt")); err == nil {
		t.Error("the dangling link's target was created")
	}
}

// - Verify writing over a named pipe is refused at once (it must not block) and a socket-like special file is not
// replaced
func Test_WriteFile_NamedPipe_RefusedWithoutBlocking(t *testing.T) {
	work := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(work, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	tool := newWriteTool(t, work, "", "", DenyList{})
	done := make(chan error, 1)
	go func() { _, err := writeCall(tool, "pipe", "x"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "named pipe") {
			t.Errorf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write_file blocked on a named pipe")
	}
}
