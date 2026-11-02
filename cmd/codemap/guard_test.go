package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const guardA = `package p

// Add adds.
func Add(a, b int) int {
	x := a
	y := b
	return x + y
}
`

// guardRepo makes a git repo with a.go and b.go committed and returns its path.
func guardRepo(t *testing.T) string {
	t.Helper()
	needGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(guardA), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package p\n\nfunc B() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o644)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func Test_CodeUnchanged_NothingChangedReportsNothing(t *testing.T) {
	dir := guardRepo(t)
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_CommentOnlyEditsPass(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(strings.Replace(guardA, "// Add adds.", "// Add adds two numbers.\n//\n// Example:\n//\n//\tAdd(1, 2)\n\n// stray comment", 1)), 0o644)
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_TrailingAndBlockCommentsPass(t *testing.T) {
	dir := guardRepo(t)
	src := strings.Replace(guardA, "x := a", "x := a // trailing\n\t/* block */", 1)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_CommentLineBetweenStatementsPasses(t *testing.T) {
	dir := guardRepo(t)
	src := strings.Replace(guardA, "\tx := a\n\ty := b", "\tx := a\n\t// [agent] explains y\n\ty := b", 1)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || len(got) != 0 {
		t.Errorf("REGRESSION: an inline comment line between statements is reported as a code change: %v, %v", got, err)
	}
}

func Test_CodeUnchanged_CodeEditIsReported(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(strings.Replace(guardA, "return x + y", "return x - y", 1)), 0o644)
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_CodeEditHiddenInsideACommentedOutLineStillCounts(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(strings.Replace(guardA, "\ty := b\n", "\t// y := b\n\ty := 0\n", 1)), 0o644)
	if got, _ := codeUnchanged(dir, "HEAD"); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("got %v", got)
	}
}

func Test_CodeUnchanged_DeletedFileIsReported(t *testing.T) {
	dir := guardRepo(t)
	os.Remove(filepath.Join(dir, "b.go"))
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || !reflect.DeepEqual(got, []string{"b.go (deleted)"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_AddedFileWithCodeIsReportedWhetherStagedOrNot(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "new.go"), []byte("package p\n\nfunc New() {}\n"), 0o644)
	want := []string{"new.go (added, contains code)"}
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("untracked: %v, %v", got, err)
	}
	runGit(t, dir, "add", "new.go")
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("staged: %v, %v", got, err)
	}
}

func Test_CodeUnchanged_AddedCommentOnlyFilePassesWhetherStagedOrNot(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "doc.go"), []byte("// Package p does things.\npackage p\n"), 0o644)
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("untracked: %v, %v", got, err)
	}
	runGit(t, dir, "add", "doc.go")
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("staged: %v, %v", got, err)
	}
}

func Test_CodeUnchanged_AddedFileWithOnlyAnImportOrADirectiveIsReported(t *testing.T) {
	for name, src := range map[string]string{
		"imp.go": "package p\n\nimport _ \"embed\"\n",
		"dir.go": "//go:build ignore\n\npackage p\n",
		"bad.go": "package p\n\nfunc (",
	} {
		dir := guardRepo(t)
		os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644)
		got, err := codeUnchanged(dir, "HEAD")
		if err != nil || len(got) != 1 {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
}

func Test_CodeUnchanged_UntrackedFileInAGitIgnoredPathIsNotReported(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("scratch/\n"), 0o644)
	runGit(t, dir, "add", ".gitignore")
	os.MkdirAll(filepath.Join(dir, "scratch"), 0o755)
	os.WriteFile(filepath.Join(dir, "scratch", "x.go"), []byte("package s\n\nfunc X() {}\n"), 0o644)
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_NonGoChangesAreIgnored(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("changed\n"), 0o644)
	if got, err := codeUnchanged(dir, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func Test_CodeUnchanged_ComparesAgainstTheGivenRefNotJustHead(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(strings.Replace(guardA, "return x + y", "return y + x", 1)), 0o644)
	runGit(t, dir, "commit", "-q", "-am", "code change")
	if got, _ := codeUnchanged(dir, "HEAD"); len(got) != 0 {
		t.Errorf("HEAD: %v", got)
	}
	if got, _ := codeUnchanged(dir, "HEAD~1"); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Errorf("HEAD~1: %v", got)
	}
}

func Test_CodeUnchanged_WorksWhenRootIsASubdirectoryOfTheRepo(t *testing.T) {
	needGit(t)
	top := t.TempDir()
	sub := filepath.Join(top, "sub")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "a.go"), []byte(guardA), 0o644)
	os.WriteFile(filepath.Join(top, "outside.go"), []byte("package q\n\nfunc O() {}\n"), 0o644)
	runGit(t, top, "init", "-q")
	runGit(t, top, "add", "-A")
	runGit(t, top, "commit", "-q", "-m", "base")

	os.WriteFile(filepath.Join(sub, "a.go"), []byte(strings.Replace(guardA, "// Add adds.", "// Add adds numbers.", 1)), 0o644)
	if got, err := codeUnchanged(sub, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("comment edit: %v, %v", got, err)
	}
	os.WriteFile(filepath.Join(sub, "a.go"), []byte(strings.Replace(guardA, "return x + y", "return 0", 1)), 0o644)
	got, err := codeUnchanged(sub, "HEAD")
	if err != nil || len(got) != 1 || !strings.HasSuffix(got[0], "a.go") {
		t.Fatalf("code edit: %v, %v", got, err)
	}
}

func Test_CodeUnchanged_BuildConstraintChangeIsACodeChange(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("//go:build linux\n\npackage p\n\nfunc A() {}\n"), 0o644)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("//go:build windows\n\npackage p\n\nfunc A() {}\n"), 0o644)
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || len(got) != 1 {
		t.Errorf("BUG: changing //go:build (stripped as a comment) passes the comment-only guard: %v, %v", got, err)
	}
}

func Test_CodeUnchanged_DirectiveChangeIsACodeChange(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\n//go:noinline\nfunc A() {}\n"), 0o644)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc A() {}\n"), 0o644)
	got, err := codeUnchanged(dir, "HEAD")
	if err != nil || len(got) != 1 {
		t.Errorf("BUG: removing //go:noinline passes the comment-only guard: %v, %v", got, err)
	}
}

func Test_CodeUnchanged_SyntaxErrorInNewVersionIsReported(t *testing.T) {
	dir := guardRepo(t)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\nfunc ("), 0o644)
	if got, _ := codeUnchanged(dir, "HEAD"); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("got %v", got)
	}
}

func Test_CodeUnchanged_UnknownRefReturnsError(t *testing.T) {
	dir := guardRepo(t)
	if _, err := codeUnchanged(dir, "no-such-ref"); err == nil {
		t.Fatal("want an error")
	}
}

func Test_CodeUnchanged_OutsideAGitRepositoryReturnsError(t *testing.T) {
	needGit(t)
	if _, err := codeUnchanged(t.TempDir(), "HEAD"); err == nil {
		t.Fatal("want an error")
	}
}

func Test_Stripped_DropsCommentsButKeepsCode(t *testing.T) {
	a, err := stripped([]byte("package p\n\n// c\nfunc F() { /* x */ }\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := stripped([]byte("package p\nfunc F() {}\n"))
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
	if _, err := stripped([]byte("nope")); err == nil {
		t.Error("want a parse error")
	}
}

func Test_Run_CodeUnchangedFlag(t *testing.T) {
	dir := guardRepo(t)
	var out bytes.Buffer
	if err := run([]string{"-root", dir, "-code-unchanged", "HEAD"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "only comments changed") {
		t.Errorf("output %q", out.String())
	}
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(strings.Replace(guardA, "return x + y", "return 1", 1)), 0o644)
	if err := run([]string{"-root", dir, "-code-unchanged", "HEAD"}, &out); err == nil || !strings.Contains(err.Error(), "a.go") {
		t.Errorf("err = %v", err)
	}
}
