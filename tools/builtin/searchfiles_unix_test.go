//go:build unix

package builtin

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// searchWithTimeout runs the tool in a goroutine and fails the test if it does not return in time, which is how a
// blocked open of a named pipe shows up.
func searchWithTimeout(t *testing.T, tool *SearchFilesTool, args searchArgs) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := callSearch(tool, args)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(3 * time.Second):
		t.Fatalf("search_files did not return within 3s for %v: it is blocked", args)
		return "", nil
	}
}

// - Verify a symlink inside workDir that points at .env, at a file outside workDir or at a directory outside it is
// never searched, so the secret behind it is never returned
func Test_Unit_SearchFiles_Symlinks_NotFollowed(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, dir, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, outside, "secret.txt", []byte("FAKE_SECRET_TOKEN_123"))
	symlink(t, ".env", filepath.Join(dir, "notes.txt"))
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dir, "a-file"))
	symlink(t, outside, filepath.Join(dir, "b-dir"))
	writeFile(t, dir, "real.txt", []byte("FAKE_SECRET_TOKEN_123 in a plain file"))

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "FAKE_SECRET_TOKEN_123"})
	if want := match("real.txt", 1, "FAKE_SECRET_TOKEN_123 in a plain file"); got != want {
		t.Errorf("expected only %q, got\n%s", want, got)
	}
}

// - Verify a start path that is a symlink to an outside directory or to a deny-listed file is refused, and one to a
// file inside workDir is searched and shown by its real path
func Test_Unit_SearchFiles_PathIsSymlink_ResolvedAndChecked(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, ".env", []byte("FAKE_SECRET_TOKEN_123"))
	writeFile(t, dir, "real/inside.txt", []byte("FAKE_SECRET_TOKEN_123 ok"))
	symlink(t, outside, filepath.Join(dir, "away"))
	symlink(t, ".env", filepath.Join(dir, "env-link"))
	symlink(t, filepath.Join("real", "inside.txt"), filepath.Join(dir, "in-link"))
	tool := newSearchTool(t, dir, "", DenyList{})

	for path, want := range map[string]string{"away": "outside the allowed directory", "env-link": "deny list"} {
		got, err := callSearch(tool, searchArgs{"query": "FAKE", "path": path})
		if err == nil || !strings.Contains(err.Error(), want) || got != "" {
			t.Errorf("%s: expected an error containing %q and no output, got (%q, %v)", path, want, got, err)
		}
	}
	if got, want := mustSearch(t, tool, searchArgs{"query": "FAKE", "path": "in-link"}), match("real/inside.txt", 1, "FAKE_SECRET_TOKEN_123 ok"); got != want {
		t.Errorf("in-link: expected %q, got %q", want, got)
	}
}

// - Verify an unreadable file and an unreadable directory are counted as "couldn't be read" without failing the call,
// and the readable file's match is still returned
func Test_Unit_SearchFiles_Unreadable_CountedNotFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a chmod 000 file")
	}
	dir := t.TempDir()
	locked := writeFile(t, dir, "a-locked.txt", []byte("hit"))
	writeFile(t, dir, "b-dir/inner.txt", []byte("hit"))
	writeFile(t, dir, "z.txt", []byte("hit"))
	lockedDir := filepath.Join(dir, "b-dir")
	for _, p := range []string{locked, lockedDir} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o755) }) // so TempDir removal works
	}

	got := mustSearch(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit"})
	if want := lines(match("z.txt", 1, "hit"), "[not searched: 2 file(s) or directories that couldn't be read]"); got != want {
		t.Errorf("expected\n%s\ngot\n%s", want, got)
	}
}

// - Verify a named pipe in the tree is never opened (the search returns at once instead of blocking) and the other
// files are searched
func Test_Unit_SearchFiles_FifoInTree_NotOpened(t *testing.T) {
	dir := t.TempDir()
	makeFifo(t, filepath.Join(dir, "a-pipe"))
	writeFile(t, dir, "b.txt", []byte("hit"))

	got, err := searchWithTimeout(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit"})
	if err != nil || got != match("b.txt", 1, "hit") {
		t.Errorf("expected only the b.txt match, got (%q, %v)", got, err)
	}
}

// - Verify a path that is a named pipe is refused as "not a regular file or directory" without blocking
func Test_Unit_SearchFiles_PathIsFifo_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	makeFifo(t, filepath.Join(dir, "pipe"))

	got, err := searchWithTimeout(t, newSearchTool(t, dir, "", DenyList{}), searchArgs{"query": "hit", "path": "pipe"})
	if err == nil || !strings.Contains(err.Error(), "pipe is not a regular file or directory") || got != "" {
		t.Errorf("expected a 'not a regular file or directory' error, got (%q, %v)", got, err)
	}
	expectErrorHidesWorkDir(t, "search_files(pipe)", err, dir, "pipe")
}

// - Verify searchFile itself never blocks on a named pipe (a regular file swapped for a pipe after the walk saw it):
// it opens without waiting for a writer and counts the pipe as unreadable
func Test_Unit_SearchFiles_SearchFile_Fifo_NotBlockedCountedUnreadable(t *testing.T) {
	dir := t.TempDir()
	makeFifo(t, filepath.Join(dir, "pipe"))
	tool := newSearchTool(t, dir, "", DenyList{})
	root, err := tool.ws.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	done := make(chan searchResult, 1)
	go func() {
		var r searchResult
		tool.searchFile(context.Background(), root, "pipe", regexp.MustCompile("hit"), 0, &r)
		done <- r
	}()
	select {
	case r := <-done:
		if r.unreadable != 1 || len(r.lines) != 0 {
			t.Errorf("expected the pipe counted as unreadable with no matches, got %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("searchFile blocked opening a named pipe")
	}
}
