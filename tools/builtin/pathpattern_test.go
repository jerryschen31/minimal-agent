package builtin

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// mustParsePatterns parses raw as denyPaths-style patterns, failing the test on error.
func mustParsePatterns(t *testing.T, raw ...string) []pathPattern {
	t.Helper()
	p, err := parsePathPatterns("test", raw)
	if err != nil {
		t.Fatalf("parsePathPatterns(%q): %v", raw, err)
	}
	return p
}

// pathCase is one row of the matching table: does pattern match rel (a file, or a directory when isDir)?
type pathCase struct {
	pattern string
	rel     string
	isDir   bool
	want    bool
}

// pathCases are Jerry's examples of the .gitignore-style rules (DECISIONS.md § denyPaths and ignorePaths share
// .gitignore-style path patterns), written with "/" between directories.
var pathCases = []pathCase{
	// no "/": a name at any depth
	{".env", ".env", false, true},
	{".env", "secrets/.env", false, true},
	{".env", "secrets/config/.env", false, true},
	{".env", "prod.env", false, false},
	// "*.env" is broader: it also matches prod.env
	{"*.env", ".env", false, true},
	{"*.env", "secrets/.env", false, true},
	{"*.env", "secrets/config/.env", false, true},
	{"*.env", "prod.env", false, true},
	{"*.env", "a/b/prod.env", false, true},
	{"*.env", "prod.envx", false, false},
	// a "/" in the middle anchors at workDir, and "*" never crosses a "/"
	{"secrets/*.env", "secrets/a.env", false, true},
	{"secrets/*.env", "secrets/old/a.env", false, false},
	{"secrets/*.env", "app/secrets/a.env", false, false},
	{"secrets/*.env", "a.env", false, false},
	{"secrets/*.env", "secrets", true, false},
	// a plain name matches a file or a directory anywhere, and a matched directory covers everything below it
	{"secrets", "secrets", false, true},
	{"secrets", "secrets", true, true},
	{"secrets", "x/secrets", false, true},
	{"secrets", "secrets/a", false, true},
	{"secrets", "x/secrets/y/z", false, true},
	{"secrets", "mysecrets", false, false},
	{"secrets", "secrets.txt", false, false},
	// a trailing "/" matches directories only (still at any depth), and everything under them
	{"secrets/", "secrets", true, true},
	{"secrets/", "x/secrets", true, true},
	{"secrets/", "secrets/a", false, true},
	{"secrets/", "x/secrets/y/z", false, true},
	{"secrets/", "secrets", false, false},
	{"secrets/", "x/secrets", false, false},
	// a leading "/" anchors at workDir
	{"/build", "build", true, true},
	{"/build", "build", false, true},
	{"/build", "build/x", false, true},
	{"/build", "src/build", true, false},
	{"/build", "src/build/x", false, false},
	// wildcards in the middle of a name
	{"*_pycache_*", "__pycache__", true, true},
	{"*_pycache_*", "a/__pycache__/x.pyc", false, true},
	{"*_pycache_*", "foo_pycache_bar", false, true},
	{"*_pycache_*", "pycache", true, false},
	// an anchored file pattern
	{"docs/*.tmp", "docs/a.tmp", false, true},
	{"docs/*.tmp", "docs/sub/a.tmp", false, false},
	{"docs/*.tmp", "other/docs/a.tmp", false, false},
	// matching ignores case, in the path and in the pattern
	{"secrets/*.env", "SECRETS/A.ENV", false, true},
	{"Secrets/", "SECRETS/a", false, true},
	{"/BUILD", "build/x", false, true},
	// workDir itself is never matched, even by "*"
	{"*", ".", true, false},
	{"*", "", true, false},
}

// - Verify matchPath follows the .gitignore-style rules on each of Jerry's examples
func Test_Unit_MatchPath_GitignoreRules(t *testing.T) {
	for _, c := range pathCases {
		if got := matchPath(mustParsePatterns(t, c.pattern), c.rel, c.isDir, nil); got != c.want {
			t.Errorf("matchPath(%q, %q, isDir=%v) = %v, want %v", c.pattern, c.rel, c.isDir, got, c.want)
		}
	}
}

// - Verify IgnoreList.Ignores gives the same answers as matchPath on each of Jerry's examples
func Test_Unit_IgnoreList_Ignores_GitignoreRules(t *testing.T) {
	for _, c := range pathCases {
		if got := newIgnoreList(t, c.pattern).Ignores(c.rel, c.isDir); got != c.want {
			t.Errorf("Ignores(%q, isDir=%v) with %q = %v, want %v", c.rel, c.isDir, c.pattern, got, c.want)
		}
	}
}

// - Verify DenyList.IsDenied gives the same answers as matchPath on each of Jerry's examples (or true where a built-in
// name such as .env already denies the path)
func Test_Unit_DenyList_IsDenied_GitignoreRules(t *testing.T) {
	for _, c := range pathCases {
		want := c.want || (DenyList{}).IsDenied(c.rel, c.isDir)
		if got := newDenyList(t, c.pattern).IsDenied(c.rel, c.isDir); got != want {
			t.Errorf("IsDenied(%q, isDir=%v) with %q = %v, want %v", c.rel, c.isDir, c.pattern, got, want)
		}
	}
}

// - Verify a path built with the OS separator (filepath.Join) matches the same as one written with "/"
func Test_Unit_MatchPath_OSSeparator(t *testing.T) {
	for _, c := range pathCases {
		rel := filepath.FromSlash(c.rel)
		if got := matchPath(mustParsePatterns(t, c.pattern), rel, c.isDir, nil); got != c.want {
			t.Errorf("matchPath(%q, %q, isDir=%v) = %v, want %v", c.pattern, rel, c.isDir, got, c.want)
		}
	}
	if !matchPath(mustParsePatterns(t, "secrets/*.env"), filepath.Join("secrets", "a.env"), false, nil) {
		t.Errorf("expected secrets/*.env to match %q", filepath.Join("secrets", "a.env"))
	}
}

// - Verify matchPath with several patterns matches when any one does, and skip exempts only the last name of the path
// from every pattern, so what is under a skipped directory name still matches
func Test_Unit_MatchPath_AnyPatternAndSkip(t *testing.T) {
	patterns := mustParsePatterns(t, "*.tmp", "/build")
	if !matchPath(patterns, "a/x.tmp", false, nil) || !matchPath(patterns, "build/y", false, nil) {
		t.Errorf("expected either pattern to match")
	}
	if matchPath(patterns, "a/x.txt", false, nil) {
		t.Errorf("expected a/x.txt to match neither pattern")
	}
	skipKeep := func(name string) bool { return name == "keep.tmp" }
	if matchPath(patterns, "a/keep.tmp", false, skipKeep) {
		t.Errorf("expected the skipped name keep.tmp not to match")
	}
	if !matchPath(patterns, "keep.tmp/x.tmp", false, skipKeep) {
		t.Errorf("expected x.tmp under a skipped directory name still to match")
	}
}

// - Verify both keys refuse each pattern that would silently match nothing, with an error that starts with
// `<key>: "<pattern>": `
func Test_Unit_PathPatterns_Invalid_ReturnsError(t *testing.T) {
	bad := []string{
		" *.pyc", "*.pyc ", "\t.env", // spaces at the start or end
		`a\b`, `\`, // a backslash
		"", "/", "//", "a//b", "./x", "a/..", "..", ".", "/./x", "x/.", // empty, "." or ".." parts
		"[x", "a/[x", // malformed
	}
	newers := map[string]func([]string) error{
		"denyPaths":   func(p []string) error { _, err := NewDenyList(p); return err },
		"ignorePaths": func(p []string) error { _, err := NewIgnoreList(p); return err },
	}
	for key, newList := range newers {
		for _, raw := range bad {
			err := newList([]string{"*.ok", raw})
			if want := fmt.Sprintf("%s: %q: ", key, raw); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("%s %q: expected an error starting with %q, got %v", key, raw, want, err)
			}
		}
	}
}

// - Verify both keys accept anchored, directory-only and wildcard patterns, and spaces inside a name
func Test_Unit_PathPatterns_Valid_Accepted(t *testing.T) {
	good := []string{".env", "*.env", "secrets/*.env", "secrets", "secrets/", "/build", "/build/", "a/b/c.txt",
		"*_pycache_*", "docs/*.tmp", "my file.txt", "[abc]*"}
	if _, err := NewDenyList(good); err != nil {
		t.Errorf("NewDenyList(%q): unexpected error %v", good, err)
	}
	if _, err := NewIgnoreList(good); err != nil {
		t.Errorf("NewIgnoreList(%q): unexpected error %v", good, err)
	}
}

// - Verify a pattern keeps its configured text (lowercased) for display, with its leading and trailing "/"
func Test_Unit_PathPatterns_RawKeptForDisplay(t *testing.T) {
	if got, want := newIgnoreList(t, "/Build/", "Docs/*.TMP").String(), "/build/, docs/*.tmp"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// - Verify both keys refuse any pattern containing "**", wherever it is, with an error that starts with
// `<key>: "<pattern>": ` and says "**" is not supported (path.Match would read it as two "*" that never cross "/", so
// "**/*.secret" would look recursive but miss a/b/x.secret)
func Test_Unit_PathPatterns_DoubleStar_Refused(t *testing.T) {
	bad := []string{"**", "**/*.secret", "secrets/**", "a/**/b", "x**y", "/**", "secrets/**/", "***"}
	newers := map[string]func([]string) error{
		"denyPaths":   func(p []string) error { _, err := NewDenyList(p); return err },
		"ignorePaths": func(p []string) error { _, err := NewIgnoreList(p); return err },
	}
	for key, newList := range newers {
		for _, raw := range bad {
			err := newList([]string{"*.ok", raw})
			want := fmt.Sprintf("%s: %q: ", key, raw)
			if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), `"**" is not supported`) {
				t.Errorf("%s %q: expected an error starting with %q and saying \"**\" is not supported, got %v", key, raw, want, err)
			}
		}
	}
}

// - Verify a single "*" in any position is still accepted, so the "**" refusal doesn't catch ordinary wildcards
func Test_Unit_PathPatterns_SingleStar_StillAccepted(t *testing.T) {
	good := []string{"*", "*/x", "a/*/b", "*.secret", "x*y*z", "secrets/*"}
	if _, err := NewDenyList(good); err != nil {
		t.Errorf("NewDenyList(%q): unexpected error %v", good, err)
	}
	if _, err := NewIgnoreList(good); err != nil {
		t.Errorf("NewIgnoreList(%q): unexpected error %v", good, err)
	}
}
