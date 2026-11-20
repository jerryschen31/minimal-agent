package builtin

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// tempFiles returns the names of the replaceFile temporary files in dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), tempMarker) {
			out = append(out, e.Name())
		}
	}
	return out
}

// - Verify tempName is ".<name>.minagent-<8 hex>.tmp", new each time, and cuts a long name so the result stays short
func Test_Unit_TempName(t *testing.T) {
	a, b := tempName("notes.txt"), tempName("notes.txt")
	if !regexp.MustCompile(`^\.notes\.txt\.minagent-[0-9a-f]{8}\.tmp$`).MatchString(a) || a == b {
		t.Errorf("tempName = %q, %q", a, b)
	}
	if long := tempName(strings.Repeat("é", 200)); len(long) > maxTempBase+30 {
		t.Errorf("long name: %d bytes", len(long))
	}
}

//   - Verify each write tool never leaves half a file: just before the rename the target still has its old content (or
//     doesn't exist) and the temporary file next to it has all of the new content; afterwards the target has the new
//     content and no temporary file is left
func Test_WriteTools_NeverHalfAFile(t *testing.T) {
	tests := []struct {
		name, target, old, want string // old "" = the target doesn't exist yet
		run                     func(t *testing.T, work string, hook func()) error
	}{
		{"write_file new", "a.txt", "", "new content", func(t *testing.T, work string, hook func()) error {
			tool := newWriteTool(t, work, "", "", DenyList{})
			tool.ws.beforeRename = hook
			_, err := writeCall(tool, "a.txt", "new content")
			return err
		}},
		{"write_file overwrite", "a.txt", "old content", "new content", func(t *testing.T, work string, hook func()) error {
			tool := newWriteTool(t, work, "", "", DenyList{})
			tool.ws.beforeRename = hook
			_, err := writeCall(tool, "a.txt", "new content")
			return err
		}},
		{"edit_file", "a.txt", "old content", "new content", func(t *testing.T, work string, hook func()) error {
			tool := newEditTool(t, work, "", "")
			tool.ws.beforeRename = hook
			_, err := editCall(tool, "a.txt", "old", "new", false)
			return err
		}},
		{"copy_file overwrite", "a.txt", "old content", "new content", func(t *testing.T, work string, hook func()) error {
			writeFile(t, work, "src.txt", []byte("new content"))
			tool := newCopyTool(t, work, "", "")
			tool.ws.beforeRename = hook
			_, err := copyCall(tool, "src.txt", "a.txt")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := t.TempDir()
			if tt.old != "" {
				writeFile(t, work, tt.target, []byte(tt.old))
			}
			hooked := false
			err := tt.run(t, work, func() {
				hooked = true
				got, err := os.ReadFile(filepath.Join(work, tt.target))
				if tt.old == "" && err == nil || tt.old != "" && string(got) != tt.old {
					t.Errorf("before the rename the target is %q (%v), want %q", got, err, tt.old)
				}
				temps := tempFiles(t, work)
				if len(temps) != 1 {
					t.Fatalf("before the rename: temporary files %v, want one", temps)
				}
				if data, _ := os.ReadFile(filepath.Join(work, temps[0])); string(data) != tt.want {
					t.Errorf("temporary file holds %q, want all of %q", data, tt.want)
				}
			})
			if err != nil || !hooked {
				t.Fatalf("call: %v (hook ran: %v)", err, hooked)
			}
			if got := readBack(t, work, tt.target); got != tt.want {
				t.Errorf("after: %q, want %q", got, tt.want)
			}
			if temps := tempFiles(t, work); len(temps) != 0 {
				t.Errorf("temporary files left: %v", temps)
			}
		})
	}
}

//   - Verify the check just before the rename refuses, removes the temporary file and leaves what is at the path alone
//     when the target changed after it was checked: another file put in its place, its content changed in place, a file
//     created at a new file's path, and (not Windows) a symlink to a file outside workDir put in its place
func Test_WriteFile_TargetChangedBeforeRename_Refused(t *testing.T) {
	outside := writeFile(t, t.TempDir(), "outside.txt", []byte("outside"))
	tests := []struct {
		name, old, want string // old "" = a new file
		swap            func(p string)
		wantAtPath      string // "" = a symlink check instead
	}{
		{"replaced by another file", "old", "replaced or moved", func(p string) {
			os.WriteFile(p+".other", []byte("other"), 0o644)
			os.Rename(p+".other", p)
		}, "other"},
		{"changed in place", "old", "changed while it was being written", func(p string) {
			os.WriteFile(p, []byte("another writer"), 0o644)
		}, "another writer"},
		{"created at a new path", "", "was created while it was being written", func(p string) {
			os.WriteFile(p, []byte("first"), 0o644)
		}, "first"},
		{"replaced by a symlink out", "old", "replaced or moved", func(p string) {
			os.Remove(p)
			os.Symlink(outside, p)
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantAtPath == "" && runtime.GOOS == "windows" {
				t.Skip("symlinks need extra rights on Windows")
			}
			work := t.TempDir()
			p := filepath.Join(work, "a.txt")
			if tt.old != "" {
				writeFile(t, work, "a.txt", []byte(tt.old))
			}
			tool := newWriteTool(t, work, "", "", DenyList{})
			tool.ws.beforeRename = func() { tt.swap(p) }
			if _, err := writeCall(tool, "a.txt", "MINE"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if tt.wantAtPath != "" {
				if got := readBack(t, work, "a.txt"); got != tt.wantAtPath {
					t.Errorf("at the path: %q, want %q", got, tt.wantAtPath)
				}
			} else if data, _ := os.ReadFile(outside); string(data) != "outside" {
				t.Errorf("the file outside workDir was written: %q", data)
			}
			if temps := tempFiles(t, work); len(temps) != 0 {
				t.Errorf("temporary files left: %v", temps)
			}
		})
	}
}

// - Verify a copy cancelled part-way (Ctrl+C) over an existing destination leaves it as it was, with no temporary file
func Test_CopyFile_CancelledPartWay_DestinationUnchanged(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "big.bin", []byte(strings.Repeat("x", 3*copyChunk)))
	writeFile(t, work, "dest.bin", []byte("OLD"))
	tool := newCopyTool(t, work, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	tool.afterOpenDest = cancel // the copy starts, then stops at the first check of the context
	if _, err := tool.CallTool(ctx, twoPathArgs("big.bin", "dest.bin")); err == nil {
		t.Fatal("want an error")
	}
	if got := readBack(t, work, "dest.bin"); got != "OLD" {
		t.Errorf("destination = %d bytes, want OLD", len(got))
	}
	if temps := tempFiles(t, work); len(temps) != 0 {
		t.Errorf("temporary files left: %v", temps)
	}
}

//   - Verify an overwrite keeps the old file's permission bits (0600 stays 0600, for write_file and edit_file) and is a
//     new file (another inode): the accepted cost of the rename
func Test_WriteTools_Overwrite_KeepsPermissionsNewFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no 0600")
	}
	work := t.TempDir()
	p := writeFile(t, work, "secret.txt", []byte("one"))
	os.Chmod(p, 0o600)
	before, _ := os.Stat(p)
	if _, err := writeCall(newWriteTool(t, work, "", "", DenyList{}), "secret.txt", "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := editCall(newEditTool(t, work, "", ""), "secret.txt", "two", "three", false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if after.Mode().Perm() != 0o600 || readBack(t, work, "secret.txt") != "three" {
		t.Errorf("mode %v, content %q; want 0600, three", after.Mode().Perm(), readBack(t, work, "secret.txt"))
	}
	if os.SameFile(before, after) {
		t.Errorf("same inode after an overwrite; replaceFile should have renamed a new file into place")
	}
}

// - Verify RemoveTempFiles removes only the temporary files of that path (not another file's, not a look-alike name)
// - Verify a missing directory is not an error
func Test_Unit_RemoveTempFiles(t *testing.T) {
	work := t.TempDir()
	mine := tempName("a.txt")
	for _, n := range []string{mine, tempName("b.txt"), ".a.txt.minagent-notahexx.tmp.bak", "a.txt"} {
		writeFile(t, work, n, []byte("x"))
	}
	removed, err := RemoveTempFiles(filepath.Join(work, "a.txt"))
	if err != nil || len(removed) != 1 || removed[0] != mine {
		t.Errorf("removed %v, %v; want only %s", removed, err, mine)
	}
	if left, _ := os.ReadDir(work); len(left) != 3 {
		t.Errorf("%d files left, want 3", len(left))
	}
	if removed, err := RemoveTempFiles(filepath.Join(work, "gone", "a.txt")); err != nil || len(removed) != 0 {
		t.Errorf("missing directory: %v, %v", removed, err)
	}
}
