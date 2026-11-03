//go:build unix

package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// - Verify a new path under a symlink that points outside workDir is refused, and nothing is created outside
func Test_Unit_Workspace_ResolveNewPath_SymlinkOutside_Refused(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	outside := filepath.Join(parent, "outside")
	for _, d := range []string{work, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlink(t, outside, filepath.Join(work, "link"))
	tool := newCreateDirTool(t, work)
	_, err := createDir(tool, "link/new")
	if err == nil || !strings.Contains(err.Error(), "outside the allowed directory") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Error("directory created outside workDir")
	}
}
