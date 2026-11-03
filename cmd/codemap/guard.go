package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
)

// codeUnchanged returns the .go files changed since a git ref whose code differs from the version at the ref, ignoring comments.
//
// Notes:
//   - Deleted files are reported as "name (deleted)".
//   - A file added since the ref, staged or not, counts as a code change if it holds anything but a package clause
//     (reported as "name (added, contains code)"); a new comment-only file such as a doc.go passes.
//   - Renames are treated as a delete plus an add (--no-renames), so a renamed file is reported.
//   - A file that fails to parse is reported.
func codeUnchanged(root, ref string) ([]string, error) {
	out, err := git(root, "diff", "--name-status", "--no-renames", ref, "--", "*.go")
	if err != nil {
		return nil, err
	}
	var entries [][2]string // {status, path relative to the repository top}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if status, name, ok := strings.Cut(line, "\t"); ok {
			entries = append(entries, [2]string{status, name})
		}
	}
	// git diff never lists files that are not tracked yet, so a new file nobody has staged would slip through.
	// -z: names are separated by NUL and never quoted, so a name with a space (or a non-ASCII character) stays one name
	untracked, err := git(root, "ls-files", "--others", "--exclude-standard", "--full-name", "-z", "--", "*.go")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(untracked, "\x00") {
		if name != "" {
			entries = append(entries, [2]string{"A", name})
		}
	}
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	top = strings.TrimSpace(top)

	var bad []string
	for _, e := range entries {
		status, name := e[0], e[1]
		if status == "D" {
			bad = append(bad, name+" (deleted)")
			continue
		}
		cur, err := readFile(filepath.Join(top, name))
		if err != nil {
			return nil, err
		}
		if status == "A" {
			if !onlyPackageClause(cur) {
				bad = append(bad, name+" (added, contains code)")
			}
			continue
		}
		old, err := git(root, "show", ref+":"+name)
		if err != nil {
			return nil, err
		}
		a, err1 := stripped([]byte(old))
		b, err2 := stripped(cur)
		if err1 != nil || err2 != nil || a != b {
			bad = append(bad, name)
		}
	}
	return bad, nil
}

// onlyPackageClause reports whether a Go source file holds nothing but comments and a package clause.
//
// Notes:
//   - Imports, declarations and compiler directives all count as code; a file that does not parse does too.
func onlyPackageClause(src []byte) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	return err == nil && len(f.Decls) == 0 && directives(src) == ""
}

// stripped prints a Go source file with its comments dropped, so two files can be compared by code alone.
//
// Notes:
//   - Compiler directives (//go:build, //go:noinline, ...) are kept, because they change behaviour; see directives.
//   - Printing normalizes formatting, and squash drops blank lines and collapses runs of spaces, so whitespace-only
//     edits compare equal too (a header inserted between two adjacent one-line methods adds a blank line and removes
//     the column alignment their bodies had).
func stripped(src []byte) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0) // no ParseComments: comments are dropped
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		return "", err
	}
	return squash(buf.String()) + "\n" + directives(src), nil
}

// squash removes what the printer keeps from the source layout: blank lines (a comment line between two statements
// leaves one) and runs of spaces (adjacent one-line functions are padded to line up).
//
// Notes: none.
func squash(s string) string {
	var kept []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// directives lists the compiler directive comments of a source file, each with the first code line after it, so a
// moved, added or removed //go:build or //go:noinline counts as a code change even though comments are dropped.
//
// Notes:
//   - Recognized prefixes are //go:, //line , //export and //extern; ordinary comments are ignored.
func directives(src []byte) string {
	lines := strings.Split(string(src), "\n")
	var out []string
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "//go:") && !strings.HasPrefix(l, "//line ") && !strings.HasPrefix(l, "//export ") && !strings.HasPrefix(l, "//extern ") {
			continue
		}
		next := ""
		for _, n := range lines[i+1:] {
			if n = strings.TrimSpace(n); n != "" && !strings.HasPrefix(n, "//") {
				next = n
				break
			}
		}
		out = append(out, l+" => "+strings.Join(strings.Fields(next), " "))
	}
	return strings.Join(out, "\n")
}

// git runs a git command in dir and returns its standard output.
//
// Notes: none.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
