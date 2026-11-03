package builtin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Workspace is the part of the filesystem the file tools may touch: rootDir and everything below it, except
// deny-listed names and the trace directory. File tools in a given session share one workspace; it is not changed
// after NewWorkspace, so sharing is safe.
// Note this is a check the built-in file tools run on every path, and not a true OS sandbox:
// code that doesn't use this workspace (a future run_command, an MCP server) can still reach everything.
type Workspace struct {
	rootDir  string      // absolute path of the allowed directory (config workDir), with symlinks resolved
	traceDir string      // absolute path of the trace directory, with symlinks resolved; "" = none configured
	deny     DenyList    // names never touched (the built-ins plus config denyPaths)
	rootInfo os.FileInfo // rootDir as checked by NewWorkspace; openRoot refuses a directory that isn't this one
}

// NewWorkspace checks and resolves workDir (must be an existing directory) and traceDir (need not exist yet; must not
// be workDir or a parent of it).
//
// Notes:
//   - traceDir is relative to the process's directory, like the trace recorder that creates it (not to workDir);
//     "" = none.
//   - deny is the deny list (NewDenyList); the zero value refuses the built-in names only.
//   - workDir is stored with symlinks resolved, together with its file identity, which openRoot compares later.
func NewWorkspace(workDir, traceDir string, deny DenyList) (*Workspace, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("workDir %q: %w", workDir, err)
	}
	rootDir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workDir %q: %w", workDir, err)
	}
	info, err := os.Stat(rootDir)
	if err != nil {
		return nil, fmt.Errorf("workDir %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workDir %q is not a directory", workDir)
	}
	w := &Workspace{rootDir: rootDir, deny: deny, rootInfo: info}
	if traceDir != "" {
		if w.traceDir, err = resolvePath(traceDir); err != nil {
			return nil, fmt.Errorf("traceDir %q: %w", traceDir, err)
		}
		if isWithin(w.traceDir, w.rootDir) {
			return nil, fmt.Errorf("traceDir %q is workDir or contains it, so the file tools could reach nothing", traceDir)
		}
	}
	return w, nil
}

// resolvePath turns the model's path (relative to rootDir, or absolute) for a file that must already exist into a path
// relative to rootDir, with every symlink followed. The relative file path, info on the file path, and any error encountered are returned.
//
// Notes:
//   - A file tool passes the identity to openFile, which refuses a file that isn't this one.
//   - Lstat, not Stat: the resolved path ends in a real file, so a symlink there now means it was swapped in after the resolve, and the identities won't match.
//   - To prevent error messages revealing the absolute path, use errMsgHidePath to wrap the error with only the requested path.
func (w *Workspace) resolvePath(requested string) (string, os.FileInfo, error) {
	// resolve full candidte path
	candidate := requested
	if !filepath.IsAbs(requested) {
		candidate = filepath.Join(w.rootDir, requested)
	}
	// follow symlinks to get the real path
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// for security, do not reveal absolute file path in the error message
		return "", nil, errMsgHidePath(requested, err)
	}
	// snapshot the file's info - in case the file changes after this point (overwrite, deletion, swap, etc.)
	info, err := os.Lstat(real)
	if err != nil {
		return "", nil, errMsgHidePath(requested, err)
	}
	// check the resolved path against the workspace rules (deny list, trace directory, nothing outside workDir, etc.)
	rel, err := w.checkPath(requested, real)
	if err != nil {
		return "", nil, err
	}
	return rel, info, nil
}

// errMsgHidePath returns an error message that hides the absolute path and only shows the requested path.
//
// Notes:
//   - A *fs.PathError keeps its cause (%w), so errors.Is(err, fs.ErrNotExist) still works; any other error becomes
//     "can't be resolved".
func errMsgHidePath(requested string, err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return fmt.Errorf("%s: %w", requested, pe.Err)
	}
	return fmt.Errorf("%s: can't be resolved", requested)
}

// checkPath refuses a resolved path that is outside rootDir, deny-listed, or inside traceDir.
//
// Notes:
//   - requested is used only in error messages.
//   - real is absolute with symlinks resolved; the result is real relative to rootDir.
//   - A path that can't be stat'ed counts as a directory for patterns ending in "/" (see the comment in the body).
func (w *Workspace) checkPath(requested, real string) (rel string, err error) {
	rel, err = filepath.Rel(w.rootDir, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the allowed directory", requested)
	}
	// A pattern ending in "/" matches directories only. A path that can't be stat'ed (most often one that doesn't exist
	// yet, a file about to be written) counts as a directory here, so such a pattern refuses it too: refusing too much
	// is the safe way to be wrong.
	info, statErr := os.Stat(real)
	isDir := statErr != nil || info.IsDir()
	if w.deny.IsDenied(rel, isDir) {
		return "", fmt.Errorf("%s is blocked: it matches the sensitive-file deny list (%s; .env.example is allowed)", requested, strings.Join(w.deny.Patterns(), ", "))
	}
	if w.traceDir != "" && isWithin(w.traceDir, real) {
		return "", fmt.Errorf("%s is blocked: it is inside the trace directory (the agent's own audit log)", requested)
	}
	return rel, nil
}

// openRoot opens rootDir as an *os.Root, the second line of defence: it refuses ".." and symlink escapes when the file
// is opened.
//
// Notes:
//   - The caller closes it.
//   - It also checks that the directory it opened is the one NewWorkspace checked.
//   - [agent] os.OpenRoot finds rootDir by its path, and a path can be made to point elsewhere after startup: another
//     program (or an MCP filesystem server's move_file, or a future run_command) renames workDir and puts a symlink to
//     / in its place. Without this check, os.Root would then contain the tool to the wrong tree. Once opened, the root
//     is tied to the directory itself, not its path, so comparing them (os.SameFile: same device and inode) closes the
//     gap.
func (w *Workspace) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(w.rootDir)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err == nil && !os.SameFile(info, w.rootInfo) {
		err = fmt.Errorf("the working directory was moved or replaced after the agent started; restart the agent")
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// openFile opens the regular file rel (relative to rootDir) through root for reading, and refuses it unless it is
// want: the file the workspace check approved, from resolvePath or a walk entry's Info.
//
// Notes:
//   - requested names the file in errors. The caller closes the file.
//   - [agent] The check approves a name, the open follows that name again, and os.Root follows symlinks that stay
//     inside rootDir. Something else (another program, an MCP server) can swap a.txt for a symlink to .env in
//     between, and the open would then return .env. Locks can't stop another process, so instead the opened file is
//     compared with the checked one (os.SameFile: same device and inode), as openRoot does for rootDir. The opened
//     file is checked again on its descriptor too: O_NONBLOCK makes opening a named pipe return at once rather than
//     wait for a writer forever.
//   - Remaining gap: a directory above the file swapped in the moment between resolving the name and taking want;
//     both then follow the new directory, so they match. Closing it would need the OS to report the opened file's
//     real path.
func (w *Workspace) openFile(root *os.Root, requested, rel string, want os.FileInfo) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat() // fstat on the descriptor we actually opened
	switch {
	case err != nil:
	case info.IsDir():
		err = fmt.Errorf("%s is a directory, not a file", requested)
	case !info.Mode().IsRegular():
		err = fmt.Errorf("%s is not a regular file (it is a %s)", requested, fileKind(info.Mode()))
	case want == nil || !os.SameFile(info, want):
		err = fmt.Errorf("%s was replaced while it was being opened; try again", requested)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// resolvePath returns p as an absolute path with symlinks resolved, like the paths it is compared with.
//
// Notes:
//   - p need not exist yet (the recorder creates the trace directory when a run starts): the longest existing part is
//     resolved and the rest is added back unchanged.
//   - A path that exists as a symlink to a missing target is refused (see the comment in the body).
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		real, err := filepath.EvalSymlinks(abs)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(abs)
		if !errors.Is(err, fs.ErrNotExist) || parent == abs {
			return "", err
		}
		if _, lerr := os.Lstat(abs); lerr == nil {
			// [agent] abs exists but EvalSymlinks says "not found": a symlink to a missing target. Protecting the
			// link's own path would miss where the recorder actually writes, so refuse it.
			return "", fmt.Errorf("%s is a symlink to a path that doesn't exist", abs)
		}
		rest = filepath.Join(filepath.Base(abs), rest)
		abs = parent
	}
}

// isWithin reports whether path p is dir or inside it.
//
// Notes:
//   - Both are absolute and symlink-resolved.
//   - The comparison ignores case, because macOS and Windows filesystems do (".AGENT-TRACE" is the same directory
//     there); on Linux that can refuse a path that differs only in case, which errs on the safe side.
func isWithin(dir, p string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
