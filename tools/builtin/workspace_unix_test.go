//go:build unix

package builtin

import (
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
