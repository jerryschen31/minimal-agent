package objective

import "strings"

// Markdown renders o as an objective file in the standard layout; Cleanup reads it back unchanged, with zero fixes.
//
// Notes:
//   - The layout is the one in doc.go: sections separated by one blank line, the file ending in exactly one "\n".
//   - Empty optional sections are left out; Tasks is written only if there are tasks or task notes; the depends line
//     only if Depends is non-empty.
//   - An empty Format is written as FormatV1 (an Objective built from tool arguments has none).
//   - The "approved:" line is written only when o.Approved is set.
func (o Objective) Markdown() string {
	return o.frontMatter() + "\n" + o.body()
}

// frontMatter renders the "---" block: format, then approved if set.
//
// Notes: none.
func (o Objective) frontMatter() string {
	format := o.Format
	if format == "" {
		format = FormatV1
	}
	s := "---\nformat: " + format + "\n"
	if o.Approved != "" {
		s += "approved: " + o.Approved + "\n"
	}
	return s + "---\n"
}

// body renders everything after the front matter; BodyHash hashes exactly this.
//
// Notes:
//   - Ends with exactly one "\n".
//   - "# Objective" is always written, even with no text, so the file has the heading CheckComplete asks to fill in.
func (o Objective) body() string {
	blocks := []string{sectionBlock(headingLine(HeadingObjective), o.ObjText)}
	for _, s := range []Section{
		{HeadingInstructions, o.Instructions},
		{HeadingConstraints, o.Constraints},
		{HeadingContext, o.Context},
	} {
		if s.Body != "" {
			blocks = append(blocks, sectionBlock(headingLine(s.Heading), s.Body))
		}
	}
	for _, s := range o.Other {
		blocks = append(blocks, sectionBlock("## "+s.Heading, s.Body))
	}
	if len(o.Tasks) > 0 || o.TaskNotes != "" {
		blocks = append(blocks, sectionBlock(headingLine(HeadingTasks), o.TaskNotes))
		for _, t := range o.Tasks {
			blocks = append(blocks, taskBlock(t))
		}
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// sectionBlock returns a heading line followed by its body, or just the heading when the body is empty.
//
// Notes:
//   - The caller leaves out an empty Instructions, Constraints or Context, but writes an Other section even with an
//     empty body: Cleanup keeps it, so leaving it out would break the round trip.
func sectionBlock(heading, body string) string {
	if body == "" {
		return heading
	}
	return heading + "\n" + body
}

// taskBlock returns one "### tN: title" subsection: the heading, the depends line if any, then a blank line and the
// body if any.
//
// Notes:
//   - Uses taskHeadingLine and dependsLine from cleanup.go, so the reader and the writer share one definition of each
//     line.
func taskBlock(t Task) string {
	s := taskHeadingLine(t)
	if d := dependsLine(t.Depends); d != "" {
		s += "\n" + d
	}
	if t.Body != "" {
		s += "\n\n" + t.Body
	}
	return s
}
