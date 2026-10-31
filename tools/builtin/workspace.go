package builtin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Workspace is the part of the filesystem the file tools may touch: rootDir and everything below it, except
// deny-listed names and the trace directory. File tools in a given session share one workspace; it is not changed
// after NewWorkspace, so sharing is safe.
// [agent] It is a check the tools run on every path, not an OS sandbox: code that doesn't ask it (a future
// run_command, an MCP server) can still reach everything.
type Workspace struct {
	rootDir  string   // absolute path of the allowed directory (config workDir), with symlinks resolved
	traceDir string   // absolute path of the trace directory, with symlinks resolved; "" = none configured
	deny     DenyList // names never touched (the built-ins plus config denyFileNames)
}

// NewWorkspace checks and resolves workDir (must be an existing directory) and traceDir (need not exist yet; must not be workDir or a parent of it).
// traceDir is relative to the process's directory, like the trace recorder that creates it (not to workDir); "" = none.
// deny is the deny list (NewDenyList); the zero value refuses the built-in names only.
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
	w := &Workspace{rootDir: rootDir, deny: deny}
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

// resolve turns the model's path (relative to rootDir, or absolute) for a file that must already exist into a path relative to rootDir, with every symlink followed.
// It returns an error if the path doesn't exist or check refuses it. The checks run on the resolved path, so a
// symlink named notes.txt that points at .env is refused too.
func (w *Workspace) resolve(requested string) (string, error) {
	candidate := requested
	if !filepath.IsAbs(requested) {
		candidate = filepath.Join(w.rootDir, requested)
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	return w.check(requested, real)
}

// check refuses a resolved path that is outside rootDir, deny-listed, or inside traceDir. requested is used only in error messages.
// real is absolute with symlinks resolved; the result is real relative to rootDir.
func (w *Workspace) check(requested, real string) (rel string, err error) {
	rel, err = filepath.Rel(w.rootDir, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the allowed directory", requested)
	}
	if w.deny.IsDenied(rel) {
		return "", fmt.Errorf("%s is blocked: it matches the sensitive-file deny list (%s; .env.example is allowed)", requested, strings.Join(w.deny.Patterns(), ", "))
	}
	if w.traceDir != "" && isWithin(w.traceDir, real) {
		return "", fmt.Errorf("%s is blocked: it is inside the trace directory (the agent's own audit log)", requested)
	}
	return rel, nil
}

// openRoot opens rootDir as an *os.Root, the second line of defence: it refuses ".." and symlink escapes when the file is opened. The caller closes it.
func (w *Workspace) openRoot() (*os.Root, error) {
	return os.OpenRoot(w.rootDir)
}

// resolvePath returns p as an absolute path with symlinks resolved, like the paths it is compared with.
// p need not exist yet (the recorder creates the trace directory when a run starts):
// the longest existing part is resolved and the rest is added back unchanged.
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

// isWithin reports whether path p is dir or inside it. Both are absolute and symlink-resolved. The comparison ignores
// case, because macOS and Windows filesystems do (".AGENT-TRACE" is the same directory there); on Linux that can
// refuse a path that differs only in case, which errs on the safe side.
func isWithin(dir, p string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
