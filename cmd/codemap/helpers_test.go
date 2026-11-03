package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var parallelOnce sync.Map

// copyFixture copies testdata/fixture into a fresh temp dir and returns it.
func copyFixture(t *testing.T) string {
	t.Helper()
	// Every test that loads a fixture is slow (go list), so let them overlap. Parallel may be called once per test.
	if _, again := parallelOnce.LoadOrStore(t.Name(), true); !again {
		t.Parallel()
	}
	dst := t.TempDir()
	src := filepath.Join("testdata", "fixture")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func mustLoad(t *testing.T, root string) []Pkg {
	t.Helper()
	pkgs, err := load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return pkgs
}

func pkgByDir(t *testing.T, pkgs []Pkg, dir string) Pkg {
	t.Helper()
	for _, p := range pkgs {
		if p.Dir == dir {
			return p
		}
	}
	t.Fatalf("no package with dir %q", dir)
	return Pkg{}
}

func funcByKey(t *testing.T, p Pkg, key string) Func {
	t.Helper()
	for _, f := range p.Funcs {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("package %q has no func with key %q", p.Dir, key)
	return Func{}
}

// writeGenerated writes the map returned by generate to disk.
func writeGenerated(t *testing.T, files map[string][]byte) {
	t.Helper()
	for name, b := range files {
		if err := os.WriteFile(name, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// generateAndWrite loads root, generates, writes the result and returns the files.
func generateAndWrite(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files, err := generate(root, mustLoad(t, root))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	writeGenerated(t, files)
	return files
}

// editFile replaces old with new (which must occur exactly once) in root/rel.
func editFile(t *testing.T, root, rel, old, new string) {
	t.Helper()
	p := filepath.Join(root, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), old) != 1 {
		t.Fatalf("%s: %q occurs %d times, want 1", rel, old, strings.Count(string(b), old))
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// runGit runs git in dir with a hermetic identity and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func contains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
