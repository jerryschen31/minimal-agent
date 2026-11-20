package builtin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryschen31/minimal-agent/textutils"
)

// The write tools (write_file, edit_file, copy_file) never write into the file they change. They write the new content
// to a temporary file next to it and rename that over it, so the file is always either all old or all new: a crash, a
// Ctrl+C or a full disk in the middle leaves the old file as it was (DECISIONS.md § 1g built).
// [agent] A rename within one directory is a single step for the filesystem: the name points at the old file, then at
// the new one, never at half of either. That is why the temporary file is in the same directory, not in /tmp (a rename
// across filesystems is not one step, and fails).

// tempMarker is in every temporary file's name: ".<name>.minagent-<8 hex>.tmp". [agent] Resume (1h) removes the ones a
// crash left next to the files of a cut-off call.
const tempMarker = ".minagent-"

// maxTempBase is how much of the target's name goes into the temporary file's name, so a long name still fits in the
// filesystem's limit (255 bytes on most).
const maxTempBase = 100

// replaceTarget is the file a write tool will replace, as it was checked (checkReplaceTarget).
type replaceTarget struct {
	existing os.FileInfo // nil: nothing is there, a new file will be created
	perm     os.FileMode // the permission bits the new version gets: the existing file's, or the new file's
	verb     string      // "created" or "overwrote"
}

// checkReplaceTarget checks what is at name in dir (the parent directory openDir approved) before a write tool replaces
// it, and returns it with the permission bits the new version will have (newPerm for a new file).
//
// Notes:
//   - Nothing there: a new file, newPerm (the umask still applies, as for any new file). A regular file with one name:
//     replaced, keeping its permission bits. Anything else is refused: a directory, a symlink (even one that points to a
//     file inside workDir), a pipe, a socket, a device, and a file with more than one name (a hard link, which could be a
//     protected file under another name).
//   - It only looks (Lstat, no open). The swap of the target after this check is caught by replaceFile's verify.
func (w *Workspace) checkReplaceTarget(dir *os.Root, requested, name string, newPerm os.FileMode) (replaceTarget, error) {
	info, err := dir.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return replaceTarget{perm: newPerm, verb: "created"}, nil
	case err != nil:
		return replaceTarget{}, errMsgHidePath(requested, err)
	case info.IsDir():
		return replaceTarget{}, fmt.Errorf("%s is a directory, not a file", requested)
	case !info.Mode().IsRegular():
		return replaceTarget{}, fmt.Errorf("%s is not a regular file (it is a %s)", requested, fileKind(info.Mode()))
	}
	if err := refuseHardLink(requested, info); err != nil {
		return replaceTarget{}, err
	}
	return replaceTarget{existing: info, perm: info.Mode().Perm(), verb: "overwrote"}, nil
}

// unchanged returns the check replaceFile runs just before the rename: the target is still what checkReplaceTarget saw.
//
// Notes:
//   - New file: nothing may be there now ("was created while it was being written"); the rename would replace it.
//   - Existing file: the same file (os.SameFile) with the same size and modification time; a swap, a symlink in its
//     place or another writer's change is refused.
func (t replaceTarget) unchanged(dir *os.Root, requested, name string) func() error {
	return func() error {
		now, err := dir.Lstat(name)
		if t.existing == nil {
			if err == nil {
				return fmt.Errorf("%s was created while it was being written; try again", requested)
			}
			return nil
		}
		switch {
		case err != nil || !os.SameFile(now, t.existing):
			return fmt.Errorf("%s was replaced or moved while it was being written; read it again and retry", requested)
		case now.Size() != t.existing.Size() || !now.ModTime().Equal(t.existing.ModTime()):
			return fmt.Errorf("%s changed while it was being written; read it again and retry", requested)
		}
		return nil
	}
}

// refuseHardLink returns an error when info is a file with more than one name (a hard link).
//
// Notes:
//   - The write tools refuse such a file: under another name it may be a protected file (.env, .git/config). Where the
//     link count is unknown (Windows), nothing is refused.
func refuseHardLink(requested string, info os.FileInfo) error {
	if n, known := linkCount(info); known && n > 1 {
		return fmt.Errorf("%s has %d names (hard links), so it may also be a protected file under another name; the file tools won't change it", requested, n)
	}
	return nil
}

// replaceFile writes a new version of name in dir through a temporary file, and renames it into place.
//
// Notes:
//   - dir is the parent directory openDir approved and name a bare name in it, so neither the temporary file nor the
//     rename can land anywhere else. The temporary file is ".<name>.minagent-<8 hex>.tmp", created with O_EXCL.
//   - Steps: create the temporary file; write (write's error is returned as it is, for the caller to word); flush it
//     to disk (Sync); give it t.perm when it replaces an existing file (Chmod, so the umask doesn't change it); verify
//     (nil = no check) just before the rename; rename; flush the directory so the rename survives a power cut.
//   - Any error before the rename removes the temporary file and leaves the target as it was. A directory flush that
//     fails is ignored: the rename is done, and some systems (Windows) can't flush a directory.
//   - w.beforeRename, when set, runs after the temporary file is closed and before verify (tests: look at both files,
//     or swap the target so verify must catch it).
func (w *Workspace) replaceFile(dir *os.Root, requested, name string, t replaceTarget, verify func() error, write func(io.Writer) error) (err error) {
	tmp := tempName(name)
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, t.perm)
	if err != nil {
		return errMsgHidePath(requested, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			f.Close()
			dir.Remove(tmp)
		}
	}()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return errMsgHidePath(requested, err)
	}
	if t.existing != nil {
		if err := f.Chmod(t.perm); err != nil {
			return errMsgHidePath(requested, err)
		}
	}
	if err := f.Close(); err != nil {
		return errMsgHidePath(requested, err)
	}
	if w.beforeRename != nil {
		w.beforeRename()
	}
	if verify != nil {
		if err := verify(); err != nil {
			return err
		}
	}
	if err := dir.Rename(tmp, name); err != nil {
		return errMsgHidePath(requested, err)
	}
	renamed = true
	if d, err := dir.Open("."); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// tempName returns a new temporary file name for name: ".<name>.minagent-<8 hex>.tmp", the name cut to maxTempBase bytes.
//
// Notes:
//   - Random, so two writes of one file never share a temporary file; O_EXCL refuses one that exists anyway.
func tempName(name string) string {
	name, _ = textutils.CutBytes(name, maxTempBase) // never splits a character
	b := make([]byte, 4)
	rand.Read(b)
	return "." + name + tempMarker + hex.EncodeToString(b) + ".tmp"
}

// RemoveTempFiles removes the temporary files replaceFile left next to path (a crash between creating one and renaming
// it), and returns their names.
//
// Notes:
//   - Matches ".<name>.minagent-<8 hex>.tmp" in path's directory, for path's name cut as tempName cuts it. A missing
//     directory is no error (nothing to remove). Called by auto mode's resume for the paths of a cut-off file call.
//   - Compares names with strings, not a glob, so a name with "*" or "[" in it can't match other files.
func RemoveTempFiles(path string) ([]string, error) {
	dir, name := filepath.Split(path)
	name, _ = textutils.CutBytes(name, maxTempBase)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := "." + name + tempMarker
	var removed []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".tmp") && len(n) == len(prefix)+8+len(".tmp") {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
			removed = append(removed, n)
		}
	}
	return removed, nil
}
