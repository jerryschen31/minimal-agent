package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// main runs the tool and turns an error into exit status 1.
//
// Notes: none.
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "codemap:", err)
		os.Exit(1)
	}
}

// readFile reads a whole file; guard.go uses it through this name so main.go owns the os dependency.
//
// Notes: none.
func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

// run executes one invocation of the tool with the given arguments, writing messages to stdout.
//
// Notes:
//   - Modes are exclusive and checked in this order: -code-unchanged, then -lint, then write or -check.
//     -lint and -check never write; plain runs write only the files whose content changed.
func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("codemap", flag.ContinueOnError)
	root := fs.String("root", ".", "module root")
	check := fs.Bool("check", false, "write nothing; fail if any generated file is out of date")
	doLint := fs.Bool("lint", false, "report missing or malformed function headers and misplaced package comments")
	unchanged := fs.String("code-unchanged", "", "fail if any .go file changed since this git ref differs in more than comments")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *unchanged != "" {
		bad, err := codeUnchanged(*root, *unchanged)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			return fmt.Errorf("code changed (not only comments) in: %s", strings.Join(bad, ", "))
		}
		fmt.Fprintln(stdout, "only comments changed since", *unchanged)
		return nil
	}

	pkgs, err := load(*root)
	if err != nil {
		return err
	}
	if *doLint {
		problems := lint(pkgs)
		for _, p := range problems {
			fmt.Fprintln(stdout, p)
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d header problems", len(problems))
		}
		return nil
	}

	files, err := generate(*root, pkgs)
	if err != nil {
		return err
	}

	var stale []string
	for name, want := range files {
		got, _ := os.ReadFile(name)
		if bytes.Equal(got, want) {
			continue
		}
		stale = append(stale, name)
		if !*check {
			if err := os.WriteFile(name, want, 0o644); err != nil {
				return err
			}
		}
	}
	slices.Sort(stale)
	if *check && len(stale) > 0 {
		return fmt.Errorf("out of date (run go run ./cmd/codemap): %s", strings.Join(stale, ", "))
	}
	for _, s := range stale {
		fmt.Fprintln(stdout, "updated", s)
	}
	return nil
}

// generate computes the new contents of every generated file (each package's doc.go and CODEMAP.md), keyed by path.
//
// Notes:
//   - CODEMAP.md is built from the sections as they will be on disk (read back out of the new doc.go text), never from the
//     in-memory packages, so the two cannot disagree.
//   - A package without a doc.go with a package comment is an error: the hand-written overview is not generated.
func generate(root string, pkgs []Pkg) (map[string][]byte, error) {
	files := map[string][]byte{}
	sections := map[string][]string{}
	for i := range pkgs {
		p := &pkgs[i]
		docPath := filepath.Join(root, filepath.FromSlash(p.Dir), "doc.go")
		src, err := os.ReadFile(docPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (every package needs a doc.go with a package comment)", p.Dir, err)
		}
		out, err := replaceSection(src, sectionLines(*p))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", docPath, err)
		}
		files[docPath] = out
		sections[p.Dir], _ = extractSection(out)
	}
	module := "module"
	if len(pkgs) > 0 {
		module = moduleOf(pkgs)
	}
	files[filepath.Join(root, "CODEMAP.md")] = []byte(codemapMD(module, pkgs, sections))
	return files, nil
}

// moduleOf returns the module path, which is the import path of the root package.
//
// Notes: none.
func moduleOf(pkgs []Pkg) string {
	for _, p := range pkgs {
		if p.Dir == "." {
			return p.ImportPath
		}
	}
	return pkgs[0].ImportPath
}
