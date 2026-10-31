package builtin

import (
	"strings"
	"testing"
)

// newIgnoreList builds an IgnoreList from patterns, failing the test on error.
func newIgnoreList(t *testing.T, patterns ...string) IgnoreList {
	t.Helper()
	l, err := NewIgnoreList(patterns)
	if err != nil {
		t.Fatalf("NewIgnoreList(%q): %v", patterns, err)
	}
	return l
}

// - Verify NewIgnoreList refuses an empty or whitespace-only name, a "/" anywhere but the end, a "\" and a malformed
// pattern, each with an error that starts with "ignorePaths:" and names the pattern
func Test_Unit_NewIgnoreList_BadPattern_ReturnsError(t *testing.T) {
	for _, bad := range []string{"", " ", "/", "  /", "//", "a/b", "/x", "a/b/", `a\b`, `\`, "[x", "[x/"} {
		l, err := NewIgnoreList([]string{"*.log", bad})
		if err == nil || !strings.HasPrefix(err.Error(), "ignorePaths:") {
			t.Errorf("pattern %q: expected an error starting with ignorePaths:, got %v", bad, err)
			continue
		}
		if !strings.Contains(err.Error(), `"`+strings.ReplaceAll(bad, `\`, `\\`)+`"`) {
			t.Errorf("pattern %q: expected the error to quote the pattern, got %v", bad, err)
		}
		if l.String() != "" {
			t.Errorf("pattern %q: expected an empty list with the error, got %q", bad, l.String())
		}
	}
}

// - Verify NewIgnoreList accepts plain names, wildcards and a trailing "/", and nil or empty input gives a list that
// ignores nothing
func Test_Unit_NewIgnoreList_ValidPatterns_Accepted(t *testing.T) {
	for _, ok := range [][]string{nil, {}, {".git/"}, {"*.pyc", "node_modules/", "build?", "[abc]*", ".env*"}} {
		if _, err := NewIgnoreList(ok); err != nil {
			t.Errorf("patterns %q: unexpected error %v", ok, err)
		}
	}
	if l := newIgnoreList(t); l.Ignores("anything", false) || l.Ignores("anything", true) {
		t.Errorf("expected an empty list to ignore nothing")
	}
}

// - Verify a pattern ending in "/" matches directories only, and one without it matches files and directories
func Test_Unit_IgnoreList_Ignores_DirOnlyAndBoth(t *testing.T) {
	l := newIgnoreList(t, "node_modules/", "*.pyc")
	cases := []struct {
		name  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"node_modules", false, false}, // a file named like a directory-only pattern is not ignored
		{"x.pyc", false, true},
		{"x.pyc", true, true}, // no trailing / means directories too
		{"x.py", false, false},
		{"node_modules2", true, false},
		{"my_node_modules", true, false},
	}
	for _, c := range cases {
		if got := l.Ignores(c.name, c.isDir); got != c.want {
			t.Errorf("Ignores(%q, isDir=%v) = %v, want %v", c.name, c.isDir, got, c.want)
		}
	}
}

// - Verify matching ignores case, both in the name and in the configured pattern
func Test_Unit_IgnoreList_Ignores_CaseInsensitive(t *testing.T) {
	l := newIgnoreList(t, "Node_Modules/", "*.PYC")
	for _, c := range []struct {
		name  string
		isDir bool
	}{
		{"node_modules", true}, {"NODE_MODULES", true}, {"x.pyc", false}, {"X.Pyc", false},
	} {
		if !l.Ignores(c.name, c.isDir) {
			t.Errorf("expected Ignores(%q, isDir=%v) to be true", c.name, c.isDir)
		}
	}
}

// - Verify the zero value ignores nothing and prints as ""
func Test_Unit_IgnoreList_ZeroValue_IgnoresNothing(t *testing.T) {
	var l IgnoreList
	if l.Ignores(".git", true) || l.Ignores("x.pyc", false) {
		t.Errorf("expected the zero IgnoreList to ignore nothing")
	}
	if s := l.String(); s != "" {
		t.Errorf("expected an empty String(), got %q", s)
	}
}

// - Verify String lists the patterns in order, lowercased, with a directory pattern's "/" restored
func Test_Unit_IgnoreList_String_LowercasedWithSlash(t *testing.T) {
	l := newIgnoreList(t, ".Git/", "*.PYC", "Build")
	if got, want := l.String(), ".git/, *.pyc, build"; got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}
