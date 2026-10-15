// Package director holds the parts of autonomous mode that don't need the agent loop: parsing
// objective.md, the task plan and its state, the director's control tools, and the text the director
// LLM is shown. It is a capability package: it imports model and tools, never agent.
// See PLAN-autonomous-mode.md.
package director

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	MaxObjectiveBytes = 20 * 1024 // objective.md size limit
)

// Objective is a parsed objective.md.
type Objective struct {
	Text      string    // body of the "# Objective" section (required, non-empty)
	Tasks     []string  // top-level bullets under "## Tasks"; nil when there is no Tasks section
	TaskNotes string    // any non-bullet text under "## Tasks" before the first bullet
	Sections  []Section // every other "##" section, in file order (Done…, Constraints, Context, others)
}

// Section is one "## Heading" and the text under it.
type Section struct {
	Heading string
	Body    string
}

// HasDone reports whether the objective has a "## Done…" section (checked in REVIEW).
func (o Objective) HasDone() bool {
	for _, s := range o.Sections {
		if isDoneHeading(s.Heading) {
			return true
		}
	}
	return false
}

func isDoneHeading(h string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "done")
}

// bulletRe matches a top-level markdown bullet: "- x", "* x", "+ x", "1. x", "1) x".
var bulletRe = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(.*)$`)

// checkboxRe matches a task-list checkbox at the start of a bullet's text: "[ ] ", "[x] ", "[X] ".
var checkboxRe = regexp.MustCompile(`^\[[ xX]\]\s*`)

// ParseObjective parses the contents of objective.md. Rules (PLAN-autonomous-mode.md § 3):
//   - exactly one "# " heading, and it must be "# Objective" with a non-empty body;
//   - "## Tasks" is optional; its top-level bullets are the tasks (checkboxes stripped, nested
//     bullets and indented lines belong to the task above), at most maxTasks (director.maxTasks in the config), and at least one if the
//     section exists;
//   - every other "##" section is kept as text; "###" and deeper are ordinary body text;
//   - lines inside ``` fences are never headings or bullets.
func ParseObjective(data []byte, maxTasks int) (Objective, error) {
	if len(data) > MaxObjectiveBytes {
		return Objective{}, fmt.Errorf("objective file is %d bytes; the limit is %d", len(data), MaxObjectiveBytes)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return Objective{}, errors.New("objective file is empty")
	}

	// [agent] first pass: split the file into (level, heading, body lines) blocks
	type block struct {
		level   int // 1 = "#", 2 = "##"; 0 = text before the first heading
		heading string
		lines   []string
	}
	blocks := []block{{}}
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}
		if !inFence {
			switch {
			case strings.HasPrefix(line, "# "):
				blocks = append(blocks, block{level: 1, heading: strings.TrimSpace(line[2:])})
				continue
			case strings.HasPrefix(line, "## "):
				blocks = append(blocks, block{level: 2, heading: strings.TrimSpace(line[3:])})
				continue
			}
		}
		blocks[len(blocks)-1].lines = append(blocks[len(blocks)-1].lines, line)
	}

	var obj Objective
	seenObjective, seenTasks := false, false
	for _, b := range blocks {
		body := strings.TrimSpace(strings.Join(b.lines, "\n"))
		switch b.level {
		case 0:
			// text before the first heading is ignored (e.g. a note to the human author)
		case 1:
			if !strings.EqualFold(b.heading, "objective") {
				return Objective{}, fmt.Errorf("unexpected top-level heading %q: the only \"# \" heading allowed is \"# Objective\"", b.heading)
			}
			if seenObjective {
				return Objective{}, errors.New(`"# Objective" appears more than once`)
			}
			seenObjective = true
			obj.Text = body
		case 2:
			if strings.EqualFold(b.heading, "tasks") {
				if seenTasks {
					return Objective{}, errors.New(`"## Tasks" appears more than once`)
				}
				seenTasks = true
				tasks, notes := parseTasks(b.lines)
				if len(tasks) == 0 {
					return Objective{}, errors.New(`"## Tasks" has no bullet items; add tasks or remove the section to let the director plan`)
				}
				if len(tasks) > maxTasks {
					return Objective{}, fmt.Errorf(`"## Tasks" has %d items; the limit is %d (director.maxTasks)`, len(tasks), maxTasks)
				}
				obj.Tasks, obj.TaskNotes = tasks, notes
				continue
			}
			obj.Sections = append(obj.Sections, Section{Heading: b.heading, Body: body})
		}
	}
	if !seenObjective {
		return Objective{}, errors.New(`objective file has no "# Objective" section`)
	}
	if obj.Text == "" {
		return Objective{}, errors.New(`"# Objective" section is empty`)
	}
	return obj, nil
}

// parseTasks turns the lines of a "## Tasks" section into task texts. A top-level bullet starts a task;
// indented lines (nested bullets, continuations) are appended to it. Non-bullet text before the first
// bullet is returned as notes; unindented non-bullet text after a bullet continues that task.
func parseTasks(lines []string) (tasks []string, notes string) {
	var noteLines []string
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}
		if trimmed == "" && !inFence {
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		if m := bulletRe.FindStringSubmatch(line); m != nil && !indented && !inFence {
			item := strings.TrimSpace(checkboxRe.ReplaceAllString(m[1], ""))
			if item != "" {
				tasks = append(tasks, item)
			}
			continue
		}
		if len(tasks) == 0 {
			noteLines = append(noteLines, trimmed)
			continue
		}
		tasks[len(tasks)-1] += "\n" + strings.TrimRight(line, " \t")
	}
	return tasks, strings.Join(noteLines, "\n")
}
