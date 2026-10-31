//go:build unix

package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// - Verify a symlink to a directory is listed as a link and not followed, even with recursive (its contents never
// appear)
func Test_Unit_ListFiles_SymlinkToDir_ListedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "real/inside.txt", []byte("x"))
	symlink(t, "real", filepath.Join(dir, "linkdir"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	want := lines("linkdir -> (symlink)", "real/", filepath.Join("real", "inside.txt")+" (1 bytes)")
	if got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify a symlink named notes.txt that points at .env is listed only as "notes.txt -> (symlink)": the target's
// name and contents never appear
func Test_Unit_ListFiles_SymlinkToDenyListed_TargetNeverShown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	symlink(t, ".env", filepath.Join(dir, "notes.txt"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	if got != "notes.txt -> (symlink)" {
		t.Errorf(`expected "notes.txt -> (symlink)", got %q`, got)
	}
	if strings.Contains(got, ".env") || strings.Contains(got, "FAKE_SECRET") {
		t.Errorf("symlink target leaked: %q", got)
	}
}

// - Verify a symlink pointing outside workDir (to a file or a directory) is just listed, never followed, and the
// outside path is not shown
func Test_Unit_ListFiles_SymlinkOutside_JustListed(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", []byte("FAKE_SECRET_TOKEN_123"))
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a-file"))
	symlink(t, outside, filepath.Join(dir, "b-dir"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	if want := lines("a-file -> (symlink)", "b-dir -> (symlink)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify listing a path that is a symlink to a directory inside workDir follows it, and the entries are shown
// relative to workDir through the resolved directory (real/..., not linkdir/...)
func Test_Unit_ListFiles_PathIsSymlinkToDirInside_ListsResolvedDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "real/inside.txt", []byte("abc"))
	symlink(t, "real", filepath.Join(dir, "linkdir"))

	got := mustList(t, newListTool(t, dir, "", DenyList{}), "linkdir", "", false)
	if want := filepath.Join("real", "inside.txt") + " (3 bytes)"; got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// - Verify listing a path that is a symlink to a hidden or outside directory is refused (the resolved path is checked)
func Test_Unit_ListFiles_PathIsSymlinkToRefusedDir_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ssh/config", []byte("FAKE_SECRET_TOKEN_123"))
	symlink(t, ".ssh", filepath.Join(dir, "keys"))
	symlink(t, t.TempDir(), filepath.Join(dir, "away"))
	tool := newListTool(t, dir, "", DenyList{})

	for path, want := range map[string]string{"keys": "deny list", "away": "outside the allowed directory"} {
		got, err := callList(tool, path, "", true)
		if err == nil || !strings.Contains(err.Error(), want) || got != "" {
			t.Errorf("%s: expected an error containing %q and no listing, got (%q, %v)", path, want, got, err)
		}
	}
}

// - Verify an unreadable subdirectory is listed itself but its contents are skipped, without failing the call
func Test_Unit_ListFiles_UnreadableSubdir_SkippedNotFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a chmod 000 directory")
	}
	dir := t.TempDir()
	writeFile(t, dir, "locked/inside.txt", []byte("x"))
	writeFile(t, dir, "z.txt", []byte("z"))
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) // so TempDir removal works

	got := mustList(t, newListTool(t, dir, "", DenyList{}), ".", "", true)
	if want := lines("locked/", "z.txt (1 bytes)"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}
