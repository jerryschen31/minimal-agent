package main

import (
	"bytes"
	"os"
	"testing"
)

// Test_Repo_CodemapIsFresh fails when any package's doc.go section or CODEMAP.md no longer matches the code. The fix is
// `go run ./cmd/codemap`.
func Test_Repo_CodemapIsFresh(t *testing.T) {
	root := "../.."
	pkgs, err := load(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := generate(root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date: run go run ./cmd/codemap", name)
		}
	}
}

// Test_Repo_HeadersPassLint fails when a function has no header, a malformed one, or a package comment sits outside
// doc.go. The fix is to run the doc-keeper on the listed functions.
func Test_Repo_HeadersPassLint(t *testing.T) {
	pkgs, err := load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range lint(pkgs) {
		t.Error(p)
	}
}
