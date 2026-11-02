package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func Test_Load_ReturnsPackagesSortedByDirWithRootAsDot(t *testing.T) {
	pkgs := mustLoad(t, copyFixture(t))
	var dirs []string
	for _, p := range pkgs {
		dirs = append(dirs, p.Dir)
	}
	want := []string{".", "internal/deep", "shapes", "util"}
	if !reflect.DeepEqual(dirs, want) {
		t.Fatalf("dirs = %v, want %v", dirs, want)
	}
	root := pkgByDir(t, pkgs, ".")
	if root.ImportPath != "example.com/fixture" || root.Name != "fixture" {
		t.Errorf("root = %q / %q", root.ImportPath, root.Name)
	}
	if want := []string{"alpha.go", "doc.go", "types.go"}; !reflect.DeepEqual(root.Files, want) {
		t.Errorf("root files = %v, want %v", root.Files, want)
	}
	if !reflect.DeepEqual(root.PkgDocFiles, []string{"doc.go"}) {
		t.Errorf("PkgDocFiles = %v", root.PkgDocFiles)
	}
}

func Test_Load_FuncsAreInFileNameThenSourceOrder(t *testing.T) {
	root := pkgByDir(t, mustLoad(t, copyFixture(t)), ".")
	var keys []string
	for _, f := range root.Funcs {
		keys = append(keys, f.File+":"+f.Key)
	}
	if keys[0] != "alpha.go:Documented" || keys[1] != "alpha.go:Undocumented" || keys[2] != "types.go:Multi" {
		t.Errorf("funcs not in file-name then source order: %v", keys[:3])
	}
	last := ""
	for _, f := range root.Funcs {
		if f.File < last {
			t.Fatalf("file %s after %s", f.File, last)
		}
		last = f.File
	}
}

func Test_Load_ExcludesTestFiles(t *testing.T) {
	root := copyFixture(t)
	os.WriteFile(filepath.Join(root, "util", "util_test.go"), []byte("package util\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"), 0o644)
	p := pkgByDir(t, mustLoad(t, root), "util")
	for _, f := range p.Files {
		if strings.HasSuffix(f, "_test.go") {
			t.Errorf("test file %s listed", f)
		}
	}
	for _, f := range p.Funcs {
		if f.Name == "TestX" {
			t.Error("test function listed")
		}
	}
}

func Test_Load_ReturnsErrorOnSyntaxError(t *testing.T) {
	root := copyFixture(t)
	editFile(t, root, "util/util.go", "return x * 2", "return x * (")
	if _, err := load(root); err == nil {
		t.Fatal("want an error for a package with a syntax error")
	}
}

func Test_Load_TypeErrorAloneDoesNotFailLoad(t *testing.T) {
	root := copyFixture(t)
	editFile(t, root, "util/util.go", "return x * 2", "return x * undefinedName")
	p, err := load(root)
	if err != nil {
		t.Fatalf("load failed on a type error: %v", err)
	}
	funcByKey(t, pkgByDir(t, p, "util"), "Double")
}

func Test_Signatures_AreOnOneLine(t *testing.T) {
	root := pkgByDir(t, mustLoad(t, copyFixture(t)), ".")
	tests := []struct{ key, sig string }{
		{"Unnamed", "func Unnamed(x int) (int, string)"},
		{"Box.Ptr", "func (b *Box) Ptr() int"},
		{"Box.Val", "func (b Box) Val() int"},
		{"Box.NoName", "func (Box) NoName()"},
		{"Pair.Set", "func (p *Pair[K, V]) Set(k K, v V)"},
		{"Pair.Get", "func (p Pair[K, V]) Get() (K, V)"},
		{"List.Add", "func (l *List[T]) Add(x T)"},
		{"List.Len", "func (l List[T]) Len() int"},
		{"Generic", "func Generic[T ~int | ~string](x T, ys ...T) []T"},
		{"init", "func init()"},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			got := funcByKey(t, root, tc.key).Sig
			if got != tc.sig {
				t.Errorf("Sig = %q, want %q", got, tc.sig)
			}
			if strings.Contains(got, "\n") {
				t.Error("signature has a newline")
			}
		})
	}
}

func Test_Keys_RepeatedNamesAreNotNumbered(t *testing.T) {
	root := pkgByDir(t, mustLoad(t, copyFixture(t)), ".")
	n := 0
	for _, f := range root.Funcs {
		if f.Name == "init" {
			n++
			if f.Key != "init" {
				t.Errorf("init key = %q, want %q", f.Key, "init")
			}
		}
		if strings.Contains(f.Key, "#") {
			t.Errorf("key %q is numbered", f.Key)
		}
	}
	if n != 3 {
		t.Errorf("found %d init funcs, want 3", n)
	}
}

func Test_Keys_MethodsUseReceiverTypeNameWithoutPointerOrTypeParams(t *testing.T) {
	root := pkgByDir(t, mustLoad(t, copyFixture(t)), ".")
	for _, key := range []string{"Box.Ptr", "Box.Val", "Box.NoName", "Pair.Set", "Pair.Get", "List.Add", "List.Len"} {
		funcByKey(t, root, key)
	}
}

func Test_Load_SkipsGitIgnoredDirectories(t *testing.T) {
	needGit(t)
	root := copyFixture(t)
	runGit(t, root, "init", "-q")
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("scratch/\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "scratch"), 0o755)
	os.WriteFile(filepath.Join(root, "scratch", "s.go"), []byte("package scratch\n\nfunc S() {}\n"), 0o644)

	for _, p := range mustLoad(t, root) {
		if p.Dir == "scratch" {
			t.Fatal("git-ignored scratch/ was loaded")
		}
	}
	// without the ignore rule it is loaded, proving the skip is what removed it
	os.WriteFile(filepath.Join(root, ".gitignore"), nil, 0o644)
	pkgByDir(t, mustLoad(t, root), "scratch")
}

func Test_Load_IgnoresNothingOutsideAGitRepository(t *testing.T) {
	if gitIgnored(copyFixture(t), "util") {
		t.Error("gitIgnored true outside a git repository")
	}
}

func Test_Signatures_MultiLineSignatureIsOneLineWithoutPrinterLeftovers(t *testing.T) {
	got := funcByKey(t, pkgByDir(t, mustLoad(t, copyFixture(t)), "."), "Multi").Sig
	if strings.Contains(got, "\n") {
		t.Fatalf("signature has a newline: %q", got)
	}
	// REGRESSION GUARD (was cosmetic): joining the printer's multi-line output with strings.Fields leaves "( a" and ", )".
	if want := "func Multi(a int, b string, rest ...float64) (n int, err error)"; got != want {
		t.Errorf("Sig = %q, want %q", got, want)
	}
}

func Test_Load_FunctionWithoutBodyDoesNotPanic(t *testing.T) {
	// Assembly-backed declaration: `func ext() int` has a nil Body. buildPkg passes it to printNode(fset, fn.Body),
	// a typed nil *ast.BlockStmt, which panics inside go/printer (load.go buildPkg).
	root := copyFixture(t)
	os.WriteFile(filepath.Join(root, "util", "ext.go"), []byte("package util\n\nfunc ext() int\n\nfunc Use() int { return ext() }\n"), 0o644)
	os.WriteFile(filepath.Join(root, "util", "ext_stub.s"), []byte("// empty\n"), 0o644)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BUG: load panics on a function without a body: %v", r)
		}
	}()
	p, err := load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	f := funcByKey(t, pkgByDir(t, p, "util"), "ext")
	if f.Sig != "func ext() int" {
		t.Errorf("ext = %+v", f)
	}
}
