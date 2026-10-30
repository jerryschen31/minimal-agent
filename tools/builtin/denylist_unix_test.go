//go:build unix

package builtin

import (
	"path/filepath"
	"strings"
	"testing"
)

// - Verify a symlink with an innocent name pointing at a file (or a directory) that matches an extra pattern is
// refused, because the deny list is checked on the resolved path
func Test_ReadFile_DenyFileNames_SymlinkInto_Blocked(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "config/credentials.json", []byte("SECRET-FAKE_SECRET_TOKEN_123"))
	writeFile(t, work, "vault.d/token.txt", []byte("SECRET-VAULT"))
	symlink(t, "config/credentials.json", filepath.Join(work, "readme.txt"))
	symlink(t, "vault.d", filepath.Join(work, "docs"))
	tool := newToolWithDeny(t, work, newDenyList(t, "credentials.json", "*.d"))

	for _, p := range []string{"readme.txt", "docs/token.txt"} {
		got, err := readPath(tool, p)
		if err == nil {
			t.Errorf("expected %q to be blocked, got %q", p, got)
			continue
		}
		if !strings.Contains(err.Error(), "deny list") || strings.Contains(err.Error(), "SECRET-") {
			t.Errorf("%q: expected a deny-list error that leaks nothing, got %q", p, err.Error())
		}
	}
}
