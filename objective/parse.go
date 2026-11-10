package objective

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jerryschen31/minimal-agent/mdscan"
)

// section is one part of the file found by Parse, before it is put into the Objective.
type section struct {
	name    string        // a Heading* constant, or "" for another top-level section (it goes to Instructions)
	heading string        // the heading line as written (kept for a section that goes to Instructions)
	line    int           // line of the heading; 0 for the text before the first heading
	lines   []mdscan.Line // body lines
}

// Parse reads an objective file leniently and returns what it asks for.
//
// Notes:
//   - The three known headings are matched at any level, case-insensitively, singular or plural, with a trailing ":"
//     (sectionName), and also without a space after the "#"s ("#Objective").
//   - Any other heading is compared with the deepest known heading in the file (sectionLevel): at that level or above
//     (a smaller number) it starts a new top-level section, which is added to Instructions after Instructions' own
//     text, with its heading line kept; deeper, it is a subheading, body text of the section it is in. So with
//     "# Objective" and "## Instructions", "## Constraints" goes to Instructions and "### Details" stays put. In a file
//     with none of the three known headings, every heading starts such a section.
//   - Headings inside ``` or ~~~ code blocks are text. A code block left open at the end of a section is closed, so
//     Markdown (which writes the sections in its own order) can't swallow the next one.
//   - Text before the first heading: the objective when the file has no "# Objective" section (a file with no headings
//     is all objective); an error naming its line when it has one.
//   - Errors: the input checks of mdscan.Lines; the same known section twice; an empty objective.
//   - Returns no fixes: Go never rewrites this file, and Markdown's standard form is only for the hash and the planner.
func Parse(data []byte) (Objective, error) {
	lines, err := mdscan.Lines(data, "objective file")
	if err != nil {
		return Objective{}, err
	}
	sections := splitSections(lines, sectionLevel(lines))

	var o Objective
	var extra []string // top-level sections that go to Instructions, in file order
	hasObjective := false
	seen := map[string]int{} // known section name → line of its heading
	for _, s := range sections {
		if s.name != "" {
			if first, dup := seen[s.name]; dup {
				return Objective{}, fmt.Errorf("line %d: a second %q section (the first is on line %d); merge them into one",
					s.line, "# "+s.name, first)
			}
			seen[s.name] = s.line
			hasObjective = hasObjective || s.name == HeadingObjective
		}
		body := sectionText(s.lines, false)
		switch {
		case s.name == HeadingObjective:
			o.ObjText = body
		case s.name == HeadingInstructions:
			o.Instructions = body
		case s.name == HeadingContext:
			o.Context = body
		case s.line > 0:
			if body := sectionText(s.lines, true); body != "" {
				extra = append(extra, s.heading+"\n"+body)
			} else {
				extra = append(extra, s.heading)
			}
		}
	}

	if pre := sections[0]; pre.line == 0 {
		if body := sectionText(pre.lines, false); body != "" {
			if hasObjective {
				return Objective{}, fmt.Errorf("line %d: text before the first heading; move it under a heading "+
					"(for example \"# Context\") or delete it", mdscan.FirstTextLine(pre.lines))
			}
			o.ObjText = body
		}
	}
	o.Instructions = joinNonEmpty(append([]string{o.Instructions}, extra...))
	if strings.TrimSpace(o.ObjText) == "" {
		return Objective{}, errors.New(`the objective is empty: write what you want to achieve under "# Objective" ` +
			`(or as the first lines of the file)`)
	}
	return o, nil
}

// sectionLevel returns the level of the deepest known heading in the file (outside code blocks), or 0 when there is
// none.
//
// Notes:
//   - The deepest, not the first: a file with "# Objective" and "## Instructions" uses level 2 for its sections.
func sectionLevel(lines []mdscan.Line) int {
	deepest, inFence := 0, false
	for _, l := range lines {
		if mdscan.IsCodeBlockFence(l.Text) {
			inFence = !inFence
			continue
		}
		if level, text, ok := mdscan.ReadHeading(l.Text, isKnownName); !inFence && ok && isKnownName(text) {
			deepest = max(deepest, level)
		}
	}
	return deepest
}

// splitSections splits the lines into sections at the headings that start one (see Parse); the first section holds
// the text before the first heading.
//
// Notes:
//   - topLevel is sectionLevel's result: another heading at that level or above starts a section (always, when it is
//     0); a deeper one stays in the current section.
//   - Headings inside code blocks are ignored.
func splitSections(lines []mdscan.Line, topLevel int) []section {
	sections := []section{{}}
	inFence := false
	for _, l := range lines {
		cur := &sections[len(sections)-1]
		if mdscan.IsCodeBlockFence(l.Text) {
			inFence = !inFence
			cur.lines = append(cur.lines, l)
			continue
		}
		if inFence {
			cur.lines = append(cur.lines, l)
			continue
		}
		level, text, isHeading := mdscan.ReadHeading(l.Text, isKnownName)
		switch {
		case !isHeading:
			cur.lines = append(cur.lines, l)
		case isKnownName(text):
			name, _ := sectionName(text)
			sections = append(sections, section{name: name, line: l.Num})
		case topLevel == 0 || level <= topLevel:
			sections = append(sections, section{heading: strings.TrimSpace(l.Text), line: l.Num})
		default:
			cur.lines = append(cur.lines, l) // a subheading inside the current section
		}
	}
	return sections
}

// sectionName maps heading text to a Heading* constant, or reports false for any other heading.
//
// Notes:
//   - Lenient: any case, surrounding spaces, a trailing ":", the singular or plural ("Instruction", "Objectives").
func sectionName(text string) (string, bool) {
	h := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ":")))
	switch h {
	case "objective", "objectives":
		return HeadingObjective, true
	case "instructions", "instruction":
		return HeadingInstructions, true
	case "context":
		return HeadingContext, true
	}
	return "", false
}

// isKnownName reports whether heading text is one of the three known sections.
//
// Notes:
//   - Passed to mdscan.ReadHeading, so "#Objective" counts as a heading and "#hashtag" doesn't.
func isKnownName(text string) bool {
	_, ok := sectionName(text)
	return ok
}

// sectionText joins a section's body lines into text: trailing blank lines dropped, leading ones too unless
// keepLeading, and a code block left open closed with a "```" line.
//
// Notes:
//   - The words are kept as written; nothing else is changed (Go never rewrites this file).
//   - keepLeading is for a section that goes to Instructions with its heading: there the blank lines between heading
//     and body are kept, because when Markdown's output is read again the section may be read as part of the one
//     above it (found by FuzzParse: "# 0" with no body, then "## 0"). All blank → "".
func sectionText(lines []mdscan.Line, keepLeading bool) string {
	var out []string
	inFence := false
	for _, l := range lines {
		if mdscan.IsCodeBlockFence(l.Text) {
			inFence = !inFence
		}
		out = append(out, l.Text)
	}
	start, end := 0, len(out)
	for start < end && strings.TrimSpace(out[start]) == "" {
		start++
	}
	if start == end {
		return ""
	}
	for strings.TrimSpace(out[end-1]) == "" {
		end--
	}
	if keepLeading {
		start = 0
	}
	out = out[start:end]
	if inFence {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

// joinNonEmpty joins the non-empty parts with one blank line between them.
//
// Notes: none.
func joinNonEmpty(parts []string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}
