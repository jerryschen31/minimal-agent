//go:build unix

package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// - Verify an edit through a symlink pointing outside workDir is refused and the outside file is untouched
func Test_EditFile_SymlinkOutside_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := writeFile(t, parent, "victim.txt", []byte("SAFE"))
	symlink(t, victim, filepath.Join(work, "link.txt"))
	tool := newEditTool(t, work, "", "")
	if _, err := editCall(tool, "link.txt", "SAFE", "PWNED", false); err == nil || !strings.Contains(err.Error(), "outside the allowed directory") {
		t.Errorf("error = %v", err)
	}
	if c := readBack(t, parent, "victim.txt"); c != "SAFE" {
		t.Errorf("outside file changed: %q", c)
	}
}

// - Verify a symlink inside workDir to .env or into .git cannot be used to edit them, and a symlink to an allowed file
// edits that file
func Test_EditFile_SymlinkToDeniedTargets_Refused_AllowedTargetEdited(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, ".env", []byte("SECRET"))
	writeFile(t, work, ".git/config", []byte("SECRET"))
	writeFile(t, work, "real.txt", []byte("old"))
	symlink(t, filepath.Join(work, ".env"), filepath.Join(work, "notes.txt"))
	symlink(t, filepath.Join(work, ".git", "config"), filepath.Join(work, "cfg"))
	symlink(t, filepath.Join(work, "real.txt"), filepath.Join(work, "alias.txt"))
	tool := newEditTool(t, work, "", "")
	for p, want := range map[string]string{"notes.txt": "deny list", "cfg": "read-only"} {
		if _, err := editCall(tool, p, "SECRET", "x", false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("edit %q: error = %v, want %q", p, err, want)
		}
	}
	if got, err := editCall(tool, "alias.txt", "old", "new", false); err != nil || got != "edited real.txt (1 replacement, 3 bytes)" {
		t.Errorf("alias: (%q, %v)", got, err)
	}
	for f, want := range map[string]string{".env": "SECRET", ".git/config": "SECRET", "real.txt": "new"} {
		if c := readBack(t, work, f); c != want {
			t.Errorf("%s = %q, want %q", f, c, want)
		}
	}
}
