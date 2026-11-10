package objective

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// [agent] testdata/: objective-build-*.md are real drafts ("## Constraints" next to "## Instructions");
// objective-deepwiki.md has "## Notes" and an informal "## Tasks" section with "### t1:" subheadings. All of them go
// through the "other top-level sections are added to Instructions" rule.

// fileOf joins its arguments into a file, one per line, ending in "\n", so a test can count line numbers.
func fileOf(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// mustParse parses s and fails the test on an error.
func mustParse(t *testing.T, s string) Objective {
	t.Helper()
	o, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v\ninput:\n%s", err, s)
	}
	return o
}

// checkObjective fails the test unless got equals want, showing both.
func checkObjective(t *testing.T, got, want Objective) {
	t.Helper()
	if got != want {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

// checkRoundTrip fails the test unless Parse(o.Markdown()) gives back o.
func checkRoundTrip(t *testing.T, o Objective) {
	t.Helper()
	md := o.Markdown()
	o2, err := Parse([]byte(md))
	if err != nil {
		t.Fatalf("Parse(Markdown()): %v\nmarkdown:\n%s", err, md)
	}
	if o2 != o {
		t.Errorf("round trip changed the objective:\n got  %#v\n want %#v\nmarkdown:\n%s", o2, o, md)
	}
}

// --- Parse ---

// - Verify a file with no headings is all objective, blank lines around it dropped
func Test_Unit_Parse_NoHeadings(t *testing.T) {
	o := mustParse(t, fileOf("", "Build a CLI.", "It should be fast.", ""))
	checkObjective(t, o, Objective{ObjText: "Build a CLI.\nIt should be fast."})
}

// - Verify the three known headings are found at any level, in any case, singular or plural, with a trailing ":", and
// without a space after the "#"s
func Test_Unit_Parse_KnownHeadingsAnyLevel(t *testing.T) {
	o := mustParse(t, fileOf("## Objectives:", "a", "#Instruction", "b", "  ### CONTEXT", "c"))
	checkObjective(t, o, Objective{ObjText: "a", Instructions: "b", Context: "c"})
}

// - Verify text before the first heading is the objective when there is no "# Objective" section
func Test_Unit_Parse_TextBeforeHeadings(t *testing.T) {
	o := mustParse(t, fileOf("Build a CLI.", "", "## Instructions", "- Go only"))
	checkObjective(t, o, Objective{ObjText: "Build a CLI.", Instructions: "- Go only"})
}

// - Verify a deeper heading of another name stays in the section it is in
func Test_Unit_Parse_SubheadingsStayInSection(t *testing.T) {
	o := mustParse(t, fileOf("# Objective", "a", "## Background", "b", "# Context", "c", "### Details", "d"))
	checkObjective(t, o, Objective{ObjText: "a\n## Background\nb", Context: "c\n### Details\nd"})
}

// - Verify the level that counts is the deepest known heading: with "# Objective" and "## Instructions", "## Constraints"
// is a section (goes to Instructions); with only "# Objective", "## Background" is a subheading of it; with no known
// heading, every heading starts a section
func Test_Unit_Parse_SectionLevelIsTheDeepestKnownHeading(t *testing.T) {
	o := mustParse(t, fileOf("# Objective", "a", "## Constraints", "x", "### Details", "d", "## Instructions", "y"))
	checkObjective(t, o, Objective{ObjText: "a", Instructions: "y\n\n## Constraints\nx\n### Details\nd"})
	checkRoundTrip(t, o)

	o = mustParse(t, fileOf("# Objective", "a", "## Background", "b"))
	checkObjective(t, o, Objective{ObjText: "a\n## Background\nb"})

	o = mustParse(t, fileOf("Build X.", "", "## Steps", "- a", "# More", "m"))
	checkObjective(t, o, Objective{ObjText: "Build X.", Instructions: "## Steps\n- a\n\n# More\nm"})
	checkRoundTrip(t, o)

	o = mustParse(t, fileOf("# Objective", "a", "```", "#### Instructions", "```", "## Notes", "n"))
	checkObjective(t, o, Objective{ObjText: "a\n```\n#### Instructions\n```\n## Notes\nn"})
}

// - Verify another heading at the same level as the current section or above starts a top-level section that is added
// to Instructions after its own text, in file order, heading line kept; also one before any known heading
func Test_Unit_Parse_OtherTopLevelGoesToInstructions(t *testing.T) {
	o := mustParse(t, fileOf(
		"## Notes", "n",
		"## Objective", "a",
		"## Constraints", "- Go only",
		"### Hard limits", "- no network",
		"## Instructions", "- tests pass",
		"# Tasks", "- write it",
		"## Context", "c",
	))
	checkObjective(t, o, Objective{
		ObjText:      "a",
		Instructions: "- tests pass\n\n## Notes\nn\n\n## Constraints\n- Go only\n### Hard limits\n- no network\n\n# Tasks\n- write it",
		Context:      "c",
	})
	checkRoundTrip(t, o)
}

// - Verify headings inside a ``` or ~~~ code block are text, "#hashtag" and a 4-space indented "#" are text, and a
// code block left open at the end of a section is closed
func Test_Unit_Parse_CodeBlocksAndNotHeadings(t *testing.T) {
	o := mustParse(t, fileOf("# Objective", "a", "#hashtag", "    # code", "```", "# Instructions", "```",
		"~~~", "## Context", "~~~", "# Context", "```", "open"))
	checkObjective(t, o, Objective{
		ObjText: "a\n#hashtag\n    # code\n```\n# Instructions\n```\n~~~\n## Context\n~~~",
		Context: "```\nopen\n```",
	})
	checkRoundTrip(t, o)
}

// - Verify what Parse can't decide is an error naming the problem
func Test_Unit_Parse_Errors(t *testing.T) {
	tests := []struct {
		name, input, wantSub string
	}{
		{"empty", "", "objective file is empty"},
		{"not UTF-8", "# Objective\n\xff\n", "not UTF-8"},
		{"two Objective sections", fileOf("# Objective", "a", "## Objectives", "b"),
			`line 3: a second "# Objective" section (the first is on line 1)`},
		{"two Instructions sections", fileOf("# Objective", "a", "## Instruction", "b", "### Instructions", "c"),
			`line 5: a second "# Instructions" section (the first is on line 3)`},
		{"text before the first heading", fileOf("Note to self", "# Objective", "x"), "line 1: text before the first heading"},
		{"empty objective section", fileOf("# Objective", "", "# Instructions", "x"), "the objective is empty"},
		{"only other sections", fileOf("## Notes", "x"), "the objective is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.input)); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("err = %v; want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

// - Verify the testdata drafts read as expected: "## Constraints" and "## Tasks" end up in Instructions
func Test_Unit_Parse_Testdata(t *testing.T) {
	for _, name := range []string{"objective-build-checksum-checker.md", "objective-build-minisearch.md",
		"objective-deepwiki.md"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			o := mustParse(t, string(data))
			if o.ObjText == "" || !strings.Contains(o.Instructions, "\n## ") {
				t.Errorf("ObjText %q; Instructions lacks a folded section:\n%s", o.ObjText, o.Instructions)
			}
		})
	}
	data, _ := os.ReadFile("testdata/objective-deepwiki.md")
	o := mustParse(t, string(data))
	if !strings.Contains(o.Instructions, "## Tasks\n\n### t1: Find the transport code") || o.Context == "" {
		t.Errorf("deepwiki: Instructions:\n%s\nContext %q", o.Instructions, o.Context)
	}
}

// --- Markdown, Hash, ChangedSections ---

// - Verify Markdown writes "# Objective" always and the other sections when not empty, one blank line apart, ending in
// one "\n"
func Test_Unit_Markdown_Layout(t *testing.T) {
	tests := []struct {
		o    Objective
		want string
	}{
		{Objective{ObjText: "a", Instructions: "b", Context: "c"}, "# Objective\na\n\n# Instructions\nb\n\n# Context\nc\n"},
		{Objective{ObjText: "a", Context: "c"}, "# Objective\na\n\n# Context\nc\n"},
		{Objective{ObjText: "a"}, "# Objective\na\n"},
		{Objective{}, "# Objective\n"},
	}
	for _, tt := range tests {
		if got := tt.o.Markdown(); got != tt.want {
			t.Errorf("%#v.Markdown() = %q; want %q", tt.o, got, tt.want)
		}
	}
}

// - Verify Parse(o.Markdown()) gives back o for every testdata file and the example
func Test_Unit_Markdown_RoundTrip(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.md")
	files = append(files, "../examples/objective-example.md")
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			checkRoundTrip(t, mustParse(t, string(data)))
		})
	}
}

// - Verify Hash is sha256 of the standard form: heading spelling, level and blank lines don't change it; any change to
// the words does
func Test_Unit_Hash(t *testing.T) {
	tidy := mustParse(t, fileOf("# Objective", "a", "", "# Instructions", "- b"))
	h := tidy.Hash()
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("Hash = %q; want sha256:<64 hex digits>", h)
	}
	messy := mustParse(t, fileOf("", "#objective:", "", "a", "", "", "### Instruction", "- b", ""))
	if messy.Hash() != h {
		t.Error("a formatting difference changed the hash")
	}
	for _, edited := range []Objective{
		{ObjText: "a!", Instructions: "- b"},
		{ObjText: "a", Instructions: "- b "},
		{ObjText: "a", Instructions: "- b", Context: "c"},
	} {
		if edited.Hash() == h {
			t.Errorf("%#v: the hash did not change", edited)
		}
	}
}

// - Verify ChangedSections names the sections whose text differs, in standard order
func Test_Unit_ChangedSections(t *testing.T) {
	old := Objective{ObjText: "a", Instructions: "b"}
	if got := old.ChangedSections(old); got != nil {
		t.Errorf("unchanged: %v", got)
	}
	now := Objective{ObjText: "a", Instructions: "b2", Context: "c"}
	if got, want := now.ChangedSections(old), []string{"# Instructions", "# Context"}; !slices.Equal(got, want) {
		t.Errorf("ChangedSections = %v; want %v", got, want)
	}
}

// --- Documentation ---

// - Verify examples/objective-example.md parses and is already in the standard form
func Test_Unit_ExampleFile(t *testing.T) {
	data, err := os.ReadFile("../examples/objective-example.md")
	if err != nil {
		t.Fatal(err)
	}
	o := mustParse(t, string(data))
	if got := o.Markdown(); got != string(data) {
		t.Errorf("the example is not in the standard form:\n%s", got)
	}
}

// - Verify the before/after example in docs/objective-format.md is what Parse does
func Test_Unit_DocHandWrittenExample(t *testing.T) {
	o := mustParse(t, fileOf(
		"## Objective",
		"Build a tiny Go program that prints the first 10 Fibonacci numbers.",
		"",
		"## Constraints",
		"- Standard library only.",
		"",
		"## Instructions",
		"- go test ./... passes and go run . prints them one per line.",
		"",
		"## Tasks",
		"- Write fib.go",
		"- Write fib_test.go",
	))
	want := fileOf(
		"# Objective",
		"Build a tiny Go program that prints the first 10 Fibonacci numbers.",
		"",
		"# Instructions",
		"- go test ./... passes and go run . prints them one per line.",
		"",
		"## Constraints",
		"- Standard library only.",
		"",
		"## Tasks",
		"- Write fib.go",
		"- Write fib_test.go",
	)
	if got := o.Markdown(); got != want {
		t.Errorf("Markdown:\n%s\nwant:\n%s", got, want)
	}
}

// --- Fuzzing ---

// FuzzParse: for any input, Parse never panics, and when it succeeds, Parse(o.Markdown()) gives back o.
// Run: go test ./objective -run XXX -fuzz FuzzParse -fuzztime 30s
func FuzzParse(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.md")
	for _, p := range files {
		if data, err := os.ReadFile(p); err == nil {
			f.Add(data)
		}
	}
	for _, s := range []string{
		"",
		"Build it.\n",
		"## Notes\nn\n## Objective\na\n### Sub\n## Tasks\n- x\n",
		"# Objective\na\n# Context\n```\nopen\n# Instructions\n",
		"#objective\n#Instructions:\n  ## Context\n####### x\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		o, err := Parse(data)
		if err != nil {
			return // rejecting an input is fine; panicking is not
		}
		checkRoundTrip(t, o)
	})
}
