package plan

import (
	"strconv"
	"strings"
)

// Markdown renders p as a plan file in the standard layout; Cleanup reads it back unchanged, with zero fixes.
//
// Notes:
//   - The layout is the one in doc.go: front matter, the title, sections separated by one blank line, the file ending
//     in exactly one "\n".
//   - An empty Format is written as FormatV1 (a Plan built from tool arguments has none).
//   - Empty front-matter values and empty Assumptions are left out; Tasks is written only if there are tasks or task
//     notes.
func (p Plan) Markdown() string {
	return p.frontMatter() + "\n" + p.body()
}

// frontMatter renders the "---" block: format, then objective, objective_hash and plan_version when set.
//
// Notes: none.
func (p Plan) frontMatter() string {
	format := p.Format
	if format == "" {
		format = FormatV1
	}
	s := "---\nformat: " + format + "\n"
	if p.ObjectivePath != "" {
		s += "objective: " + p.ObjectivePath + "\n"
	}
	if p.ObjectiveHash != "" {
		s += "objective_hash: " + p.ObjectiveHash + "\n"
	}
	if p.Version != 0 {
		s += "plan_version: " + strconv.Itoa(p.Version) + "\n"
	}
	return s + "---\n"
}

// body renders everything after the front matter.
//
// Notes:
//   - Ends with exactly one "\n".
//   - The title heading is always written ("# Plan" with no title), so the file always starts the same way.
func (p Plan) body() string {
	blocks := []string{sectionBlock(titleLine(p.Title), p.Summary)}
	if p.Assumptions != "" {
		blocks = append(blocks, sectionBlock(headingLine(HeadingAssumptions), p.Assumptions))
	}
	for _, s := range p.Other {
		blocks = append(blocks, sectionBlock("## "+s.Heading, s.Body))
	}
	if len(p.Tasks) > 0 || p.TaskNotes != "" {
		blocks = append(blocks, sectionBlock(headingLine(HeadingTasks), p.TaskNotes))
		for _, t := range p.Tasks {
			blocks = append(blocks, taskBlock(t))
		}
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// sectionBlock returns a heading line followed by its body, or just the heading when the body is empty.
//
// Notes:
//   - The caller leaves out an empty Assumptions, but writes an Other section even with an empty body: Cleanup keeps
//     it, so leaving it out would break the round trip.
func sectionBlock(heading, body string) string {
	if body == "" {
		return heading
	}
	return heading + "\n" + body
}

// taskBlock returns one "### tN: title" subsection: the heading, its field lines, then a blank line and the body if
// any.
//
// Notes:
//   - Uses taskHeadingLine and fieldLines from cleanup.go, so the reader and the writer share one definition of each
//     line.
func taskBlock(t Task) string {
	lines := append([]string{taskHeadingLine(t)}, fieldLines(t)...)
	s := strings.Join(lines, "\n")
	if t.Body != "" {
		s += "\n\n" + t.Body
	}
	return s
}
