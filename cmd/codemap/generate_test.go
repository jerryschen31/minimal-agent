package main

import (
	"bytes"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func Test_Generate_ErrorsNamingThePackageWhenDocGoIsMissing(t *testing.T) {
	root := copyFixture(t)
	if err := os.Remove(filepath.Join(root, "util", "doc.go")); err != nil {
		t.Fatal(err)
	}
	_, err := generate(root, mustLoad(t, root))
	if err == nil {
		t.Fatal("want an error when a package has no doc.go")
	}
	if !strings.Contains(err.Error(), "util") || !strings.Contains(err.Error(), "doc.go") {
		t.Errorf("error does not name the package: %v", err)
	}
}

func Test_Generate_ErrorsWhenDocGoHasNoPackageComment(t *testing.T) {
	root := copyFixture(t)
	os.WriteFile(filepath.Join(root, "util", "doc.go"), []byte("package util\n"), 0o644)
	if _, err := generate(root, mustLoad(t, root)); err == nil {
		t.Fatal("want an error")
	}
}

func Test_Generate_ProducesDocGoPerPackagePlusCodemapMD(t *testing.T) {
	root := copyFixture(t)
	files, err := generate(root, mustLoad(t, root))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"doc.go", "util/doc.go", "shapes/doc.go", "internal/deep/doc.go", "CODEMAP.md"} {
		if _, ok := files[filepath.Join(root, filepath.FromSlash(rel))]; !ok {
			t.Errorf("missing output for %s", rel)
		}
	}
	if len(files) != 5 {
		t.Errorf("got %d files, want 5", len(files))
	}
}

func Test_Generate_DoesNotTouchTheDiskAndIsIdempotentOnceWritten(t *testing.T) {
	root := copyFixture(t)
	before, _ := os.ReadFile(filepath.Join(root, "doc.go"))
	files, err := generate(root, mustLoad(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "doc.go")); !bytes.Equal(before, after) {
		t.Fatal("generate wrote to disk")
	}
	writeGenerated(t, files)
	again, err := generate(root, mustLoad(t, root))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		if !bytes.Equal(again[name], b) {
			t.Errorf("%s changed on the second run", name)
		}
		if strings.HasSuffix(name, "doc.go") {
			if f, err := format.Source(b); err != nil || !bytes.Equal(f, b) {
				t.Errorf("%s is not gofmt-stable (%v)", name, err)
			}
		}
	}
}

func Test_Generate_KeepsHandWrittenOverviewAboveTheSection(t *testing.T) {
	root := copyFixture(t)
	files := generateAndWrite(t, root)
	doc := string(files[filepath.Join(root, "doc.go")])
	if !strings.Contains(doc, "hand-written overview that must survive regeneration") {
		t.Errorf("overview lost:\n%s", doc)
	}
	if strings.Index(doc, "overview") > strings.Index(doc, heading) {
		t.Error("overview must precede the generated section")
	}
}

func Test_Generate_CodemapMDCopiesEachPackageSectionExactlyFromItsDocGo(t *testing.T) {
	root := copyFixture(t)
	files := generateAndWrite(t, root)
	md := string(files[filepath.Join(root, "CODEMAP.md")])

	pkgs := mustLoad(t, root)
	if !strings.Contains(md, "```text\n"+tree("fixture", pkgs)+"```") {
		t.Errorf("CODEMAP.md lacks the directory tree:\n%s", md)
	}
	for _, p := range pkgs {
		doc := files[filepath.Join(root, filepath.FromSlash(p.Dir), "doc.go")]
		lines, ok := extractSection(doc)
		if !ok {
			t.Fatalf("%s: no section in generated doc.go", p.Dir)
		}
		block := strings.Join(codeBlock(lines), "\n")
		if block == "" {
			t.Fatalf("%s: empty code block", p.Dir)
		}
		want := "## " + dirTitle(p) + "\n\n`" + p.ImportPath + "`\n\n```text\n" + block + "\n```\n"
		if !strings.Contains(md, want) {
			t.Errorf("%s: block in CODEMAP.md differs from doc.go section.\nwant:\n%s", p.Dir, want)
		}
	}
}

func Test_Generate_CodemapMDListsPackagesInDirOrderAndNoTestFiles(t *testing.T) {
	root := copyFixture(t)
	md := string(generateAndWrite(t, root)[filepath.Join(root, "CODEMAP.md")])
	prev := -1
	for _, title := range []string{"## (root) package fixture", "## internal/deep (package deep)", "## shapes (package shapes)", "## util (package util)"} {
		i := strings.Index(md, title)
		if i < 0 || i < prev {
			t.Fatalf("%q missing or out of order in:\n%s", title, md)
		}
		prev = i
	}
}

func Test_Generate_WithNoPackagesStillBuildsCodemapMD(t *testing.T) {
	root := t.TempDir()
	files, err := generate(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[filepath.Join(root, "CODEMAP.md")]; !ok {
		t.Error("no CODEMAP.md")
	}
}

func Test_Lint_PackageCommentOutsideDocGoIsReportedFromLoadedFixture(t *testing.T) {
	root := copyFixture(t)
	editFile(t, root, "util/util.go", "package util\n", "// Package util is documented twice.\npackage util\n")
	pkgs := mustLoad(t, root)
	if got := lint(pkgs); !contains(got, "util: package comment must be only in doc.go") {
		t.Errorf("got %v", got)
	}
}

func Test_Run_CheckFailsWhenStaleThenPassesAfterWrite(t *testing.T) {
	root := copyFixture(t)
	var out bytes.Buffer
	if err := run([]string{"-root", root, "-check"}, &out); err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("-check on a fresh fixture: err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "CODEMAP.md")); len(b) != 0 {
		t.Fatal("-check wrote CODEMAP.md")
	}
	out.Reset()
	if err := run([]string{"-root", root}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "updated") {
		t.Errorf("no 'updated' lines: %q", out.String())
	}
	if err := run([]string{"-root", root, "-check"}, &out); err != nil {
		t.Errorf("-check after writing: %v", err)
	}
	editFile(t, root, "util/util.go", "return x * 2", "return x * 3")
	// a body change does not change any signature, so the map is still current
	if err := run([]string{"-root", root, "-check"}, &out); err != nil {
		t.Errorf("-check after a body-only edit: %v", err)
	}
	editFile(t, root, "util/util.go", "func Double(x int) int", "func Double(x int, y int) int")
	editFile(t, root, "util/util.go", "return x * 3", "return x * 3 * y")
	if err := run([]string{"-root", root, "-check"}, &out); err == nil {
		t.Error("-check must fail after a signature change")
	}
}

func Test_Run_LintListsProblemsAndFails(t *testing.T) {
	root := copyFixture(t)
	var out bytes.Buffer
	err := run([]string{"-root", root, "-lint"}, &out)
	if err == nil || !strings.Contains(err.Error(), "header problems") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "no header") {
		t.Errorf("output lacks problems: %s", out.String())
	}
}

func Test_Run_LintDoesNotNeedDocGoOrWriteFiles(t *testing.T) {
	root := copyFixture(t)
	os.Remove(filepath.Join(root, "util", "doc.go"))
	var out bytes.Buffer
	err := run([]string{"-root", root, "-lint"}, &out)
	if err == nil || !strings.Contains(out.String(), "util: package comment must be only in doc.go") {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	if _, statErr := os.Stat(filepath.Join(root, "CODEMAP.md")); statErr == nil {
		t.Error("-lint wrote CODEMAP.md")
	}
}

func Test_Run_AckFlagIsGone(t *testing.T) {
	if err := run([]string{"-root", copyFixture(t), "-ack", "x"}, &bytes.Buffer{}); err == nil {
		t.Error("-ack should be an unknown flag")
	}
}

func Test_Generate_SecondRunOutputIsByteIdentical(t *testing.T) {
	root := copyFixture(t)
	first := generateAndWrite(t, root)
	second, err := generate(root, mustLoad(t, root))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range first {
		if !bytes.Equal(second[name], b) {
			t.Errorf("%s differs on the second run", name)
		}
	}
}

func Test_Generate_CodemapMDBlockLinesEqualSignatureLinesOfDocGoSection(t *testing.T) {
	root := copyFixture(t)
	files := generateAndWrite(t, root)
	md := string(files[filepath.Join(root, "CODEMAP.md")])
	for _, p := range mustLoad(t, root) {
		lines, _ := extractSection(files[filepath.Join(root, filepath.FromSlash(p.Dir), "doc.go")])
		var sigs []string
		for _, l := range lines {
			if strings.HasPrefix(l, "\t  func ") {
				sigs = append(sigs, strings.TrimPrefix(l, "\t  "))
			}
		}
		if len(sigs) != len(p.Funcs) {
			t.Fatalf("%s: %d signature lines in doc.go, %d funcs", p.Dir, len(sigs), len(p.Funcs))
		}
		var inMD []string
		for _, l := range strings.Split(md, "\n") {
			if strings.HasPrefix(l, "  func ") {
				inMD = append(inMD, strings.TrimPrefix(l, "  "))
			}
		}
		for _, s := range sigs {
			if !contains(inMD, s) {
				t.Errorf("%s: signature %q missing from CODEMAP.md", p.Dir, s)
			}
		}
	}
}
