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

// writeDeniedNames are read-only for the file tools: they may be read (unless deny-listed) but a write tool never
// creates, changes or deletes them. ".git" matches a directory or a file (a submodule's .git is a file) at any depth.
// [agent] Writing inside .git can plant a hook that runs on the user's next `git commit`, which is code execution
// outside the agent's approval gate.
var writeDeniedNames = []string{".git"}

// Workspace is the part of the filesystem the file tools may touch: rootDir and everything below it, except
// deny-listed names and the trace directory. File tools in a given session share one workspace; it is not changed
// after NewWorkspace, so sharing is safe.
// Note this is a check the built-in file tools run on every path, and not a true OS sandbox:
// code that doesn't use this workspace (a future run_command, an MCP server) can still reach everything.
type Workspace struct {
	rootDir  string   // absolute path of the allowed directory (config workDir), with symlinks resolved
	traceDir string   // absolute path of the trace directory, with symlinks resolved; "" = none configured
	deny     DenyList // names never touched (the built-ins plus config denyPaths)
	// writeDeny: names that may be read but never written, created or deleted by a file tool (see writeDeniedNames)
	writeDeny []pathPattern
	// configFile: absolute path of the config file passed with -config, with symlinks resolved; "" = none. Read-only
	// for the file tools, so the model can't rewrite its own approval rules.
	configFile string
	rootInfo   os.FileInfo // rootDir as checked by NewWorkspace; openRoot refuses a directory that isn't this one
}

// NewWorkspace checks and resolves workDir (must be an existing directory) and traceDir (need not exist yet; must not
// be workDir or a parent of it).
//
// Notes:
//   - traceDir is relative to the process's directory, like the trace recorder that creates it (not to workDir);
//     "" = none.
//   - configFile is the -config path ("" = none). It is relative to the process's directory, need not exist, and is
//     refused for writing (not for reading) if it is inside workDir.
//   - deny is the deny list (NewDenyList); the zero value refuses the built-in names only.
//   - workDir is stored with symlinks resolved, together with its file identity, which openRoot compares later.
func NewWorkspace(workDir, traceDir, configFile string, deny DenyList) (*Workspace, error) {
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
	writeDeny, err := parsePathPatterns("writeDeniedNames", writeDeniedNames)
	if err != nil {
		return nil, err // the names are fixed in the code, so this is a bug
	}
	w := &Workspace{rootDir: rootDir, deny: deny, rootInfo: info, writeDeny: writeDeny}
	if traceDir != "" {
		if w.traceDir, err = resolveSymLinks(traceDir); err != nil {
			return nil, fmt.Errorf("traceDir %q: %w", traceDir, err)
		}
		if isWithin(w.traceDir, w.rootDir) {
			return nil, fmt.Errorf("traceDir %q is workDir or contains it, so the file tools could reach nothing", traceDir)
		}
	}
	if configFile != "" {
		if w.configFile, err = resolveSymLinks(configFile); err != nil {
			return nil, fmt.Errorf("config file %q: %w", configFile, err)
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
	// resolve full candidate path
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
//   - A *fs.PathError keeps its cause (%w), so errors.Is(err, fs.ErrNotExist) still works; "not a directory" (a parent
//     is a file) is kept too; any other error becomes "can't be resolved".
func errMsgHidePath(requested string, err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return fmt.Errorf("%s: %w", requested, pe.Err)
	}
	if errors.Is(err, syscall.ENOTDIR) { // a parent in the path is a file
		return fmt.Errorf("%s: %w", requested, syscall.ENOTDIR)
	}
	return fmt.Errorf("%s: can't be resolved", requested)
}

// newTarget is a path a write tool will create or replace, as resolveNewPath approved it.
type newTarget struct {
	rel     string      // the path relative to rootDir ("." = rootDir itself)
	dirRel  string      // the deepest directory that already existed above rel, relative to rootDir ("." = rootDir)
	dirInfo os.FileInfo // that directory's identity when it was approved; openDir refuses any other directory
}

// resolveNewPath turns the model's path for something a write tool will create (it may not exist yet) into a newTarget:
// the path relative to rootDir, with every symlink in the part that exists followed, and the identity of the deepest
// directory that exists above it.
//
// Notes:
//   - The longest existing part of the path is resolved (resolveSymLinks) and the missing rest is added back, then
//     checkPath runs on the whole, so a new file under a symlink that points outside rootDir is refused.
//   - On top of checkPath's rules, a write is refused for a name in writeDeniedNames (e.g. anything under .git) and
//     for the config file.
//   - A path that doesn't exist yet counts as a directory for patterns ending in "/" (the safe way to be wrong).
//   - Errors name only the requested path, as in resolvePath.
//   - It doesn't create anything. [agent] Its checks are about the path as it is now; the caller must create through
//     openDir, which opens dirRel as a descriptor-bound Root and refuses it unless it is dirInfo. Otherwise another
//     process could swap a directory above the new file for a symlink to .git between this check and the write, and
//     os.Root would follow it (it only stops symlinks that leave rootDir).
func (w *Workspace) resolveNewPath(requested string) (newTarget, error) {
	candidate := requested
	if !filepath.IsAbs(requested) {
		candidate = filepath.Join(w.rootDir, requested)
	}
	real, err := resolveSymLinks(candidate)
	if err != nil {
		return newTarget{}, errMsgHidePath(requested, err)
	}
	rel, err := w.checkPath(requested, real)
	if err != nil {
		return newTarget{}, err
	}
	info, statErr := os.Stat(real)
	if err := w.checkWritable(requested, rel, statErr != nil || info.IsDir()); err != nil {
		return newTarget{}, err
	}
	return w.anchorFor(requested, rel, real)
}

// anchorFor returns the newTarget for rel (relative to rootDir; real is the same path, absolute): rel together with the
// deepest directory that exists above it and that directory's identity.
//
// Notes:
//   - For rootDir itself the anchor is rootDir, with the identity NewWorkspace took.
//   - requested is used only in error messages. An anchor that is not a directory is an error.
func (w *Workspace) anchorFor(requested, rel, real string) (newTarget, error) {
	if rel == "." {
		return newTarget{rel: ".", dirRel: ".", dirInfo: w.rootInfo}, nil
	}
	// the deepest directory that exists above real; rootDir exists, so the walk ends there at the latest
	dir := filepath.Dir(real)
	var dirInfo os.FileInfo
	var err error
	for {
		if dirInfo, err = os.Lstat(dir); err == nil {
			break
		}
		if dir == w.rootDir {
			return newTarget{}, errMsgHidePath(requested, err)
		}
		dir = filepath.Dir(dir)
	}
	if !dirInfo.IsDir() {
		return newTarget{}, fmt.Errorf("%s: a parent in the path is not a directory", requested)
	}
	dirRel, err := filepath.Rel(w.rootDir, dir)
	if err != nil {
		return newTarget{}, errMsgHidePath(requested, err)
	}
	return newTarget{rel: rel, dirRel: dirRel, dirInfo: dirInfo}, nil
}

// openDir opens the directory t.dirRel as a Root of its own and refuses it unless it is the directory resolveNewPath
// approved. It returns that Root and the part of t.rel below it.
//
// Notes:
//   - The caller closes the Root and creates or opens only through it, with the returned path.
//   - [agent] A Root made by OpenRoot is tied to the directory itself, not its path, so a later swap of any directory
//     above it can't redirect the write. The one gap is before the open: comparing it with dirInfo (os.SameFile) catches
//     a swap since the approval, including a symlink to another directory inside rootDir such as .git.
//   - A tail of "." means t.rel is the directory itself (rootDir, or a directory that already exists).
//   - requested names the path in errors.
func (w *Workspace) openDir(root *os.Root, requested string, t newTarget) (*os.Root, string, error) {
	dir, err := root.OpenRoot(t.dirRel)
	if err != nil {
		return nil, "", errMsgHidePath(requested, err)
	}
	info, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, t.dirInfo) {
		dir.Close()
		return nil, "", fmt.Errorf("%s: a directory in the path was replaced while it was being written; try again", requested)
	}
	tail, err := filepath.Rel(t.dirRel, t.rel)
	if err != nil {
		dir.Close()
		return nil, "", errMsgHidePath(requested, err)
	}
	return dir, tail, nil
}

// resolvePathForWrite is resolvePath for a file that must already exist and will be changed or removed: the same
// checks, plus the write-deny checks of resolveNewPath.
//
// Notes:
//   - Returns a newTarget (the path relative to rootDir, and the directory above it with its identity, for openDir) and
//     the file's identity, for openExisting.
//   - Used by tools that edit a file in place (edit_file) or take it away (move_file). A file that doesn't exist is an
//     error, with resolvePath's message.
func (w *Workspace) resolvePathForWrite(requested string) (newTarget, os.FileInfo, error) {
	rel, info, err := w.resolvePath(requested)
	if err != nil {
		return newTarget{}, nil, err
	}
	if err := w.checkWritable(requested, rel, info.IsDir()); err != nil {
		return newTarget{}, nil, err
	}
	target, err := w.anchorFor(requested, rel, filepath.Join(w.rootDir, rel))
	if err != nil {
		return newTarget{}, nil, err
	}
	return target, info, nil
}

// checkWritable refuses a path (relative to rootDir, symlinks already resolved) that the file tools may read but never
// change: a name in writeDeniedNames, or the config file.
//
// Notes:
//   - isDir is about rel itself (for patterns ending in "/").
//   - requested is used only in error messages.
func (w *Workspace) checkWritable(requested, rel string, isDir bool) error {
	if matchPath(w.writeDeny, rel, isDir, nil) {
		return fmt.Errorf("%s is blocked: it is read-only for the file tools (%s)", requested, strings.Join(writeDeniedNames, ", "))
	}
	if w.configFile != "" && strings.EqualFold(filepath.Join(w.rootDir, rel), w.configFile) { // EqualFold: macOS and Windows ignore case
		return fmt.Errorf("%s is blocked: it is the agent's own config file, read-only for the file tools", requested)
	}
	return nil
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
//   - It is openExisting with O_RDONLY; see there for the checks and the reasons.
func (w *Workspace) openFile(root *os.Root, requested, rel string, want os.FileInfo) (*os.File, os.FileInfo, error) {
	return w.openExisting(root, requested, rel, want, os.O_RDONLY)
}

// openExisting opens the regular file rel (relative to rootDir) through root with flag (O_RDONLY, O_WRONLY or O_RDWR,
// never O_TRUNC or O_CREATE), and refuses it unless it is want: the file the workspace check approved.
//
// Notes:
//   - requested names the file in errors. The caller closes the file.
//   - [agent] The check approves a name, the open follows that name again, and os.Root follows symlinks that stay
//     inside rootDir. Something else (another program, an MCP server) can swap a.txt for a symlink to .env in
//     between, and the open would then return .env. Locks can't stop another process, so instead the opened file is
//     compared with the checked one (os.SameFile: same device and inode), as openRoot does for rootDir. The opened
//     file is checked again on its descriptor too: O_NONBLOCK makes opening a named pipe return at once rather than
//     wait for a writer forever.
//   - A file opened for writing (O_WRONLY or O_RDWR) that has more than one name (st_nlink > 1) is refused: the checks are
//     by name, so a hard link to .env or .git/config under an allowed name would otherwise pass them. Reading such a
//     file is still allowed. Skipped where the OS gives no link count (linkCount).
//   - A want that is a directory or not a regular file is refused before the open, so a write open never tries a
//     directory or a pipe. A write tool truncates only after this returns, so a refused file is never emptied.
//   - Remaining gap: a directory above the file swapped in the moment between resolving the name and taking want;
//     both then follow the new directory, so they match. Closing it would need the OS to report the opened file's
//     real path.
func (w *Workspace) openExisting(root *os.Root, requested, rel string, want os.FileInfo, flag int) (*os.File, os.FileInfo, error) {
	if want != nil {
		switch {
		case want.IsDir():
			return nil, nil, fmt.Errorf("%s is a directory, not a file", requested)
		case !want.Mode().IsRegular():
			return nil, nil, fmt.Errorf("%s is not a regular file (it is a %s)", requested, fileKind(want.Mode()))
		}
	}
	f, err := root.OpenFile(rel, flag|syscall.O_NONBLOCK, 0)
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
	case flag&(os.O_WRONLY|os.O_RDWR) != 0:
		if n, known := linkCount(info); known && n > 1 {
			err = fmt.Errorf("%s has %d names (hard links), so it may also be a protected file under another name; the file tools won't change it", requested, n)
		}
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// stillAtPath refuses f unless rel (relative to root's directory) still names the file f has open.
//
// Notes:
//   - [agent] f is a descriptor on one inode. If another process renames the file after it was opened and puts a
//     different file at rel, writing to f would change the old, unlinked inode, report success, and leave rel as it
//     was. Comparing f with what rel names now (os.SameFile) catches that before anything is truncated.
//   - Called just before the truncate; a very small window remains between this check and the truncate (accepted).
//   - requested names the file in errors.
func (w *Workspace) stillAtPath(root *os.Root, requested, rel string, f *os.File) error {
	open, err := f.Stat()
	if err != nil {
		return errMsgHidePath(requested, err)
	}
	cur, err := root.Lstat(rel)
	if err != nil || !os.SameFile(open, cur) {
		return fmt.Errorf("%s was replaced or moved while it was being written; read it again and retry", requested)
	}
	return nil
}

// resolveSymLinks returns p as an absolute path with symlinks resolved, like the paths it is compared with.
//
// Notes:
//   - p need not exist yet (the recorder creates the trace directory when a run starts): the longest existing part is
//     resolved and the rest is added back unchanged.
//   - A path that exists as a symlink to a missing target is refused (see the comment in the body).
func resolveSymLinks(p string) (string, error) {
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
