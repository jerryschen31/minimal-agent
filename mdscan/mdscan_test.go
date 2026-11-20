package mdscan

import (
	"slices"
	"strings"
	"testing"
)

// - Verify Lines numbers the lines, drops a BOM, reads "\r\n" and a lone "\r" as "\n", and refuses empty, too big and
// non-UTF-8 input, naming the file
func Test_Unit_Lines(t *testing.T) {
	lines, err := Lines([]byte("\xef\xbb\xbfa\r\nb\rc\n"), "test file")
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{1, "a"}, {2, "b"}, {3, "c"}, {4, ""}}
	if !slices.Equal(lines, want) {
		t.Errorf("Lines = %#v; want %#v", lines, want)
	}

	for _, tt := range []struct {
		name, input, wantErr string
	}{
		{"empty", "", "test file is empty"},
		{"whitespace only", " \n\t\n", "test file is empty"},
		{"only a byte-order mark", "\xef\xbb\xbf", "test file is empty"},
		{"too big", strings.Repeat("x", MaxBytes+1), "the limit is"},
		{"not UTF-8", "a\n\xff\xfe\n", "test file is not UTF-8 text"},
	} {
		if _, err := Lines([]byte(tt.input), "test file"); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v; want it to contain %q", tt.name, err, tt.wantErr)
		}
	}
}

// - Verify FirstTextLine and LastTextLine skip blank lines and return 0 when every line is blank
func Test_Unit_FirstLastTextLine(t *testing.T) {
	lines := []Line{{1, ""}, {2, " a"}, {3, "b"}, {4, "  "}}
	if got := FirstTextLine(lines); got != 2 {
		t.Errorf("FirstTextLine = %d; want 2", got)
	}
	if got := LastTextLine(lines); got != 3 {
		t.Errorf("LastTextLine = %d; want 3", got)
	}
	blank := []Line{{1, ""}, {2, " "}}
	if FirstTextLine(blank) != 0 || LastTextLine(blank) != 0 || FirstTextLine(nil) != 0 {
		t.Error("all-blank input should give 0")
	}
}

// - Verify IsCodeBlockFence accepts ``` and ~~~ (with a language, indented) and nothing else
func Test_Unit_IsCodeBlockFence(t *testing.T) {
	for _, s := range []string{"```", "```go", "  ```", "~~~", "~~~~ text"} {
		if !IsCodeBlockFence(s) {
			t.Errorf("IsCodeBlockFence(%q) = false", s)
		}
	}
	for _, s := range []string{"``", "a ```", "", "- ```"} {
		if IsCodeBlockFence(s) {
			t.Errorf("IsCodeBlockFence(%q) = true", s)
		}
	}
}

// - Verify ReadHeading's level and text, and that a heading without a space after the "#"s counts only when the
// caller's known check accepts its text
func Test_Unit_ReadHeading(t *testing.T) {
	known := func(text string) bool { return text == "Objective" || strings.HasPrefix(text, "t2") }
	tests := []struct {
		in        string
		wantLevel int
		wantText  string
		wantOK    bool
	}{
		{"# x", 1, "x", true},
		{"###### six", 6, "six", true},
		{"####### seven", 0, "", false},
		{"## Tasks ##", 2, "Tasks", true},
		{"### C#", 3, "C#", true},
		{"#\tTabbed", 1, "Tabbed", true},
		{"   # three spaces", 1, "three spaces", true},
		{"    # four spaces", 0, "", false},
		{"#Objective", 1, "Objective", true},
		{"#Objective ##", 1, "Objective", true},
		{"###t2: x", 3, "t2: x", true},
		{"#hashtag", 0, "", false},
		{"#", 0, "", false},
		{"# ", 0, "", false},
		{"## ##", 0, "", false},
		{"# # #", 0, "", false},
		{"## x # #", 2, "x", true},
		{"# ## x", 1, "## x", true},
		{"plain", 0, "", false},
	}
	for _, tt := range tests {
		level, text, ok := ReadHeading(tt.in, known)
		if level != tt.wantLevel || text != tt.wantText || ok != tt.wantOK {
			t.Errorf("ReadHeading(%q) = %d, %q, %v; want %d, %q, %v", tt.in, level, text, ok,
				tt.wantLevel, tt.wantText, tt.wantOK)
		}
	}
	if _, _, ok := ReadHeading("#Objective", nil); ok {
		t.Error("a nil known check should accept no heading without a space")
	}
}

// - Verify StripClosingHashes drops closing "#" runs after a space, repeatedly, and keeps "C#"
func Test_Unit_StripClosingHashes(t *testing.T) {
	for in, want := range map[string]string{
		"Tasks ##": "Tasks",
		"x # #":    "x",
		"C#":       "C#",
		"  a  ":    "a",
		"###":      "",
		"# #":      "",
		"a\t#":     "a",
	} {
		if got := StripClosingHashes(in); got != want {
			t.Errorf("StripClosingHashes(%q) = %q; want %q", in, got, want)
		}
	}
}
