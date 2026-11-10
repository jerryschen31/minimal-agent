package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// - Verify the small helper packages import only the standard library: no package of this module and no third-party
// module. That is what lets any package use them without an import cycle. A standard-library import path has no dot
// in its first element ("os/exec"); a module path does ("github.com/...").
func Test_Unit_HelperPackages_ImportOnlyTheStandardLibrary(t *testing.T) {
	for _, dir := range []string{"mdscan", "procutils", "textutils"} {
		pkgs, err := parser.ParseDir(token.NewFileSet(), dir, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		if len(pkgs) == 0 {
			t.Fatalf("%s has no Go files", dir)
		}
		for _, pkg := range pkgs {
			for file, f := range pkg.Files { // test files included: a helper's test may not reach for other packages either
				for _, imp := range f.Imports {
					path := strings.Trim(imp.Path.Value, `"`)
					first, _, _ := strings.Cut(path, "/")
					if strings.Contains(first, ".") {
						t.Errorf("%s imports %q: helper packages may use only the standard library", file, path)
					}
				}
			}
		}
	}
}
