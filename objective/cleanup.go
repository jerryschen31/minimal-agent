package objective

import "regexp"

// line is one line of the input with its 1-based line number, so fixes can point at it.
type line struct {
	num  int
	text string
}

// rawSection is one section found by splitSections, before its body is interpreted.
type rawSection struct {
	name    string // a Heading* constant, or "" for an unknown section (goes to Other)
	heading string // the heading text as written (kept for Other)
	line    int    // line of the heading; 0 for the text before the first heading
	lines   []line // body lines
}

// Cleanup reads an objective file, fixes small mistakes, and returns the Objective and the fixes it made.
//
// Notes:
//   - The one reader of objective files (there is no strict Parse); Markdown is the one writer, and a file it wrote
//     cleans up with zero fixes. It writes nothing: callers print the fixes (warnings) and trace them.
//   - Fixes: a known section at the wrong level ("## Objective", "### Constraints"), no space after "#"
//     ("#Objective"), a singular or "Name:" heading ("## Instruction"), extra spaces after a bullet dash, a line that
//     continues the bullet above it, task headings not at "###", bullet tasks turned into "### tN:" tasks, missing
//     task IDs (AssignTaskIDs), a non-standard "depends" line; an unknown front-matter key is a fix too (ignored, Now "").
//   - Errors (it can't decide): too big or empty file; bad front matter or unknown format; a known section twice;
//     a duplicate task ID; a task heading outside "## Tasks"; text before the first heading when the file also has
//     "# Objective".
//   - A UTF-8 byte-order mark (added by some Windows editors) is dropped without a fix.
//   - Not its job: whether the file is complete or the depends are valid. CheckComplete runs after it.
//   - A file with no headings at all is a one-paragraph draft: its text becomes the Objective (one fix).
func Cleanup(data []byte) (Objective, []Fix, error) {
	// TODO 1: len(data) > MaxBytes → error naming both sizes
	// TODO 2: strings.TrimPrefix(text, "\uFEFF") (the BOM); \r\n → \n; empty after TrimSpace → error;
	//         number the lines (1-based) into []line
	// TODO 3: fm, rest, fixes, err := readFrontMatter(lines); o.Format, o.Approved = fm.format, fm.approved
	// TODO 4: sections, more, err := splitSections(rest)
	// TODO 5: for each section, by name (a map of names already seen → error naming both line numbers):
	//   "" with line 0 (text before the first heading):
	//       ignore if blank · no Objective section in the file → it becomes ObjText, Fix{Now: "# Objective"}
	//       · otherwise → error ("move it under a heading")
	//   HeadingObjective / Instructions / Constraints / Context → field = tidyBody(lines) (append its fixes)
	//   HeadingTasks → o.Tasks, o.TaskNotes, heads, taskFixes, err := parseTasks(lines)
	//   "" (unknown) → o.Other = append(o.Other, Section{heading as written, tidyBody(lines)})
	// TODO 6: for each index AssignTaskIDs(o.Tasks) returns → Fix{Line: heads[i], Now: the "### tN: title" line}
	// TODO 7: return o, fixes, nil
	// Hint: error text is read by a person (bad file) and by the planner LLM: say what to change
	return Objective{}, nil, nil
}

// frontMatter holds the front-matter keys this version knows.
type frontMatter struct {
	format   string // "format:"; FormatV1 when absent
	approved string // "approved:"; "" when absent
}

// readFrontMatter reads an optional front matter block ("---", "key: value" lines, "---") and returns its known keys,
// the lines after it, and a fix for each unknown key.
//
// Notes:
//   - No front matter, or no "format:" line → FormatV1 (a hand-written file never needs one).
//   - An unknown key is a fix with Now "" (warned about and ignored), so a newer file still runs on an older binary;
//     the version changes only when the shape of the file changes (DECISIONS.md).
//   - Errors: an unknown format (lists SupportedFormats); a line without ":"; no closing "---".
//   - Only flat "key: value" lines (no YAML library yet).
func readFrontMatter(lines []line) (fm frontMatter, rest []line, fixes []Fix, err error) {
	// TODO 1: skip leading blank lines; if the first line is not exactly "---" → frontMatter{format: FormatV1}, lines
	// TODO 2: read until the next "---" line; each line strings.Cut(l, ":") → key (lower-cased), value (trimmed)
	// TODO 3: switch key: "format" (slices.Contains(SupportedFormats, v) or error) · "approved" · default → fix
	// TODO 4: no closing "---" → error; fm.format still "" → FormatV1
	return frontMatter{format: FormatV1}, lines, nil, nil
}

// splitSections splits the lines into sections, deciding for each heading whether it starts a section, and returns
// the fixes for headings it rewrote.
//
// Notes:
//   - Headings inside ``` or ~~~ fences are ignored (isCodeBlockFence).
//   - A known section name at any level starts that section; a fix when the line isn't exactly "# Objective" or
//     "## <Name>".
//   - Inside Tasks: a level-3 heading, or any heading with a task ID ("## t2: …"), stays a body line of Tasks
//     (parseTasks makes it a task); level 4 and deeper without an ID stays a body line too (a subheading in a task).
//   - Outside Tasks: a heading with a task ID (any level) is an error: every task must be under "## Tasks". An
//     unknown heading of level 1 or 2 starts an Other section (level 1 gets a fix to "##"); an unknown level 3+
//     heading is a body line (a subheading inside the current section).
func splitSections(lines []line) ([]rawSection, []Fix, error) {
	// TODO 1: sections := []rawSection{{}} (the text before the first heading); inFence := false
	// TODO 2: for each line: isCodeBlockFence flips inFence; inside a fence → append, continue
	// TODO 3: level, text, ok := readHeading(l.text); !ok → append to the last section, continue
	// TODO 4: name, known := sectionName(text) → known: new section (+ fix if l.text != canonical heading)
	// TODO 5: else apply the Tasks / Other / body-line / error rules above
	//   (_, _, hasID := taskHeading(text) tells a task heading); the error names the line and says to move the task
	//   under "## Tasks"
	return nil, nil, nil
}

// isCodeBlockFence reports whether s opens or closes a code fence: "```" or "~~~" after optional indentation.
//
// Notes:
//   - [agent] simplified: any fence line toggles, so a ~~~ line inside a ``` block would end it. Markdown pairs a fence
//     with the same character; fine to tighten later if a real file needs it.
func isCodeBlockFence(s string) bool {
	// TODO: t := strings.TrimSpace(s); HasPrefix "```" or "~~~"
	return false
}

// readHeading reports whether s is a heading and returns its level (1–6) and text.
//
// Notes:
//   - "# Title", and a closing "#" run is dropped ("## Tasks ##" → "Tasks").
//   - No space after the "#"s counts only when the rest is a known section name or a task ID ("#Objective",
//     "###t2: x"), so "#hashtag" stays text.
//   - Seven or more "#" is not a heading (Markdown's rule).
func readHeading(s string) (level int, text string, ok bool) {
	// TODO 1: count leading '#' (stop at 7); 0 or 7 → not a heading
	// TODO 2: rest := s[level:]; starts with a space → text = trimmed rest (drop a trailing " #…" run)
	// TODO 3: no space → a heading only if sectionName(rest) is known or taskHeading(rest) has an ID
	return 0, "", false
}

// sectionName maps heading text to a Heading* constant, or reports false for an unknown section.
//
// Notes:
//   - Lenient: any case, surrounding spaces, a trailing ":", the singular ("Instruction", "Constraint", "Task").
//   - Exact names otherwise: "Task list" is unknown (goes to Other).
func sectionName(text string) (string, bool) {
	// TODO: h := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ":")))
	//   switch h { case "objective": … case "instructions", "instruction": … } ; default → "", false
	return "", false
}

// taskIDRe matches a task heading's ID and title: "t2: Title", "T2 - Title", "t2. Title", "Task 2: Title",
// "task2 Title".
var taskIDRe = regexp.MustCompile(`^(?i:task\s*|t)(\d+)\s*[:.)\-]?\s*(.*)$`)

// taskHeading splits task heading text into a lower-case ID and a title; hasID is false when there is no ID (then
// the whole text is the title).
//
// Notes:
//   - "Test the API" and "Tasks" have no ID: a digit must follow "t" or "task" (the regexp needs it).
func taskHeading(text string) (id, title string, hasID bool) {
	// TODO: m := taskIDRe.FindStringSubmatch(strings.TrimSpace(text)); m == nil → "", text, false
	//   else "t" + m[1], strings.TrimSpace(m[2]), true
	return "", text, false
}

// [agent] tedious pattern, from mvp3/director/objective.go: bulletRe matches a top-level bullet ("- x", "* x", "+ x",
// "1. x", "1) x") and checkboxRe a "[ ] " / "[x] " box at the start of a bullet's text.
var (
	bulletRe   = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(.*)$`)
	checkboxRe = regexp.MustCompile(`^\[[ xX]\]\s*`)
)

// parseTasks turns the body lines of "## Tasks" into tasks, and returns each task's heading line (for AssignTaskIDs
// fixes) and the fixes it made.
//
// Notes:
//   - A heading (any level here; splitSections only passes on task headings) starts a task; a fix when the line isn't
//     "### tN: title" (wrong level, "T2 - x", no ID yet: the ID fix comes later from AssignTaskIDs).
//   - Before the first task heading, a top-level bullet starts a task too (a hand-written bullet list): its text is
//     the title, the lines under it (dedented) the body; a fix "- x" → "### x". After the first task heading,
//     bullets are body text.
//   - Other text before the first task is TaskNotes.
//   - The first non-blank body line "depends: …" or "depends on: …" fills Depends (parseDepends); a fix when it
//     isn't exactly "depends: t1, t2".
//   - The body keeps its newlines (tidyBody); a duplicate ID → error naming both lines.
func parseTasks(lines []line) (tasks []Task, notes string, heads []int, fixes []Fix, err error) {
	// TODO 1: walk lines tracking fences (isCodeBlockFence); collect per task: heading line, title, ID, body lines; plus notes lines
	// TODO 2: per task: pull a "depends" line off the top of the body (parseDepends), then tidyBody(rest)
	// TODO 3: duplicate IDs: a map ID → heading line
	// Hint: strip the checkbox with checkboxRe on bullet tasks; dedent bullet continuation lines by their common indent
	return nil, "", nil, nil, nil
}

// parseDepends reads a "depends: t1, t2" line and reports whether s is one.
//
// Notes:
//   - Key "depends" or "depends on", any case. Items split on ","; each goes through normalizeTaskID, so
//     "depends on: task1, 3" gives t1, t3. An item that isn't one ID ("t1 t2") is split on spaces and tried again.
//   - An item that still isn't an ID is kept as written: CheckComplete reports it as naming no task.
//   - An empty list is still a depends line (Depends nil).
func parseDepends(s string) (ids []string, ok bool) {
	// TODO 1: key, value, found := strings.Cut(s, ":"); key (trimmed, lower) must be "depends" or "depends on"
	// TODO 2: for each strings.Split(value, ","): trim; skip ""; normalizeTaskID or the space-split retry
	return nil, false
}

// normalizeTaskID turns "t3", "T3", "task3", "Task 3" or "3" into "t3", and reports false for anything else.
//
// Notes: none.
func normalizeTaskID(s string) (string, bool) {
	// TODO: a regexp like `^(?i:task\s*|t)?(\d+)$` on strings.TrimSpace(s); "t" + the digits (strconv.Atoi drops
	//   leading zeros: "t03" → "t3")
	return "", false
}

// tidyBody joins a section's (or task's) body lines into text, fixing bullets, and returns the fixes.
//
// Notes:
//   - Outside fences only (isCodeBlockFence): "-    x" → "- x"; a non-blank, unindented, non-bullet line straight after a bullet
//     item (Markdown reads it as part of that item) is indented by two spaces so it visibly belongs there.
//   - Leading and trailing blank lines are dropped; newlines inside are kept.
//   - Must make no fix on its own output (Markdown writes bodies as they are), or the round trip fails.
func tidyBody(lines []line) (string, []Fix) {
	// TODO 1: walk lines tracking fences (isCodeBlockFence) and whether the previous line was part of a bullet item
	// TODO 2: bulletRe match with more than one space after the marker → rewrite, Fix{l.num, was, now}
	// TODO 3: continuation rule above → "  " + text, Fix
	// TODO 4: strings.TrimSpace on the joined result trims too much (it also strips a first line's indentation):
	//         trim blank lines from both ends instead
	return "", nil
}
