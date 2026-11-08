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
	// TODO: o.frontMatter() + "\n" + o.body()
	return ""
}

// frontMatter renders the "---" block: format, then approved if set.
//
// Notes: none.
func (o Objective) frontMatter() string {
	// TODO: "---\nformat: …\n" + ("approved: …\n" if set) + "---\n"
	return ""
}

// body renders everything after the front matter; BodyHash hashes exactly this.
//
// Notes:
//   - Ends with exactly one "\n".
func (o Objective) body() string {
	// TODO 1: "# Objective" + ObjText
	// TODO 2: writeSection for Instructions, Constraints, Context, then each Other section
	// TODO 3: if tasks or notes: "## Tasks", TaskNotes, then writeTask for each task
	// Hint: build a list of blocks and strings.Join(blocks, "\n\n") + "\n": easier than tracking blank lines
	return ""
}

// writeSection writes "## heading" and body.
//
// Notes:
//   - Leave Instructions, Constraints and Context out when empty (the caller checks), but write an Other section even
//     with an empty body: Cleanup keeps it, so leaving it out would break the round trip.
func writeSection(b *strings.Builder, heading, body string) {
	// TODO
}

// writeTask writes one "### tN: title" subsection: heading, the depends line if any, a blank line, the body.
//
// Notes: none.
func writeTask(b *strings.Builder, t Task) {
	// TODO: taskHeadingLine(t); dependsLine(t.Depends) if non-empty; a blank line and the body if non-empty
	// Hint: use the helpers in cleanup.go, so the reader and the writer share one definition of each line
}
