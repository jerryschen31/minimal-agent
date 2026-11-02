package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Func is one function or method of a package, as the map shows it.
type Func struct {
	Key    string // "Recv.Name" for a method, "Name" for a function; repeated names (init) repeat
	Name   string // the bare function or method name
	File   string // base name of the file it is declared in
	Sig    string // full signature on one line, from "func" up to (not including) the body
	Header string // doc comment text with the // markers removed; "" when there is none
}

// Pkg is one package of the module.
type Pkg struct {
	ImportPath  string
	Name        string
	Dir         string   // slash path relative to the module root; "." for the root package
	Files       []string // non-test .go files, sorted
	Funcs       []Func   // in file-name order, then source order
	PkgDocFiles []string // files that carry a package comment (should be only doc.go)
}

// load parses every package under root (tests excluded) and returns them sorted by directory.
//
// Notes:
//   - Only syntax is loaded, not types: the map holds signatures as written, so a type error elsewhere does not stop it.
//     A syntax error in any package fails the whole run.
//   - Packages in git-ignored directories (such as scratch/) are skipped.
//   - Files that only compile on another OS (such as *_windows.go on a Mac) are not in the map.
func load(root string) ([]Pkg, error) {
	cfg := &packages.Config{
		Dir:  root,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax,
	}
	loaded, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []Pkg
	for _, lp := range loaded {
		if len(lp.Errors) > 0 {
			return nil, fmt.Errorf("package %s: %v", lp.PkgPath, lp.Errors[0])
		}
		p, err := buildPkg(lp, absRoot)
		if err != nil {
			return nil, err
		}
		if gitIgnored(absRoot, p.Dir) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// gitIgnored reports whether git ignores dir, so scratch directories stay out of the map.
//
// Notes:
//   - Outside a git repository, or if git is missing, nothing counts as ignored.
func gitIgnored(root, dir string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", dir)
	cmd.Dir = root
	return cmd.Run() == nil
}

// buildPkg turns one loaded package into a Pkg: its files, package-comment locations and one Func per declared function.
//
// Notes:
//   - Files are sorted by name so the output does not depend on the loader's order.
func buildPkg(lp *packages.Package, absRoot string) (Pkg, error) {
	p := Pkg{ImportPath: lp.PkgPath, Name: lp.Name}
	type file struct {
		name string
		ast  *ast.File
	}
	var files []file
	for _, f := range lp.Syntax {
		abs := lp.Fset.Position(f.Pos()).Filename
		files = append(files, file{filepath.Base(abs), f})
		if p.Dir == "" {
			rel, err := filepath.Rel(absRoot, filepath.Dir(abs))
			if err != nil {
				return p, err
			}
			p.Dir = filepath.ToSlash(rel)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, f := range files {
		p.Files = append(p.Files, f.name)
		if f.ast.Doc != nil {
			p.PkgDocFiles = append(p.PkgDocFiles, f.name)
		}
		for _, d := range f.ast.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			fx := Func{Key: funcName(fn), Name: fn.Name.Name, File: f.name}
			fx.Sig = oneLine(printNode(lp.Fset, withoutBody(fn)))
			if fn.Doc != nil {
				fx.Header = fn.Doc.Text()
			}
			p.Funcs = append(p.Funcs, fx)
		}
	}
	return p, nil
}

// funcName returns "Recv.Name" for a method (receiver type without * or type arguments) and "Name" for a function.
//
// Notes: none.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name + "." + fn.Name.Name
		default:
			return fn.Name.Name
		}
	}
}

// withoutBody copies a function declaration minus its doc comment and body, so printing the copy gives only the signature.
//
// Notes:
//   - It copies the struct; the original declaration is not modified.
func withoutBody(fn *ast.FuncDecl) *ast.FuncDecl {
	c := *fn
	c.Doc, c.Body = nil, nil
	return &c
}

// printNode prints an AST node as Go source, or "" if printing fails.
//
// Notes:
//   - Comments are not printed: they hang off the file, not the node.
func printNode(fset *token.FileSet, n any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return ""
	}
	return buf.String()
}

// oneLine collapses all runs of whitespace, newlines included, into single spaces, and removes the space that a line
// break leaves just inside a bracket or before a closing one.
//
// Notes:
//   - A multi-line parameter list ends in a trailing comma before its closing bracket; that comma is dropped too.
//   - Only the shapes line breaks produce are handled ("( ", " )", ", )", "[ ", " ]", ", ]"); this is for signatures,
//     not arbitrary text.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return oneLineFixes.Replace(s)
}

var oneLineFixes = strings.NewReplacer(", )", ")", ", ]", "]", "( ", "(", " )", ")", "[ ", "[", " ]", "]")
