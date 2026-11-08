package objective

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

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
//   - One fix per changed line: a task line without an ID gets a single fix, written once its ID is known.
//   - The fixes are sorted by line number.
//   - Errors (it can't decide): too big, not UTF-8 or empty; bad front matter or unknown format; a known section twice;
//     a duplicate task ID; a task heading outside "## Tasks"; text before the first heading when the file also has
//     "# Objective".
//   - A UTF-8 byte-order mark (added by some Windows editors) is dropped without a fix.
//   - Not its job: whether the file is complete or the depends are valid. CheckComplete runs after it.
//   - A file with no "# Objective" heading but text before its first heading is a draft: that text becomes the
//     Objective (one fix). A file of plain text is the simplest case.
func Cleanup(data []byte) (Objective, []Fix, error) {
	if len(data) > MaxBytes {
		return Objective{}, nil, fmt.Errorf("objective file is %d bytes; the limit is %d", len(data), MaxBytes)
	}
	if !utf8.Valid(data) {
		return Objective{}, nil, errors.New("objective file is not UTF-8 text")
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return Objective{}, nil, errors.New("objective file is empty")
	}
	var lines []line
	for i, s := range strings.Split(text, "\n") {
		lines = append(lines, line{num: i + 1, text: s})
	}

	fm, rest, fixes, err := readFrontMatter(lines)
	if err != nil {
		return Objective{}, nil, err
	}
	sections, headingFixes, err := splitSections(rest)
	if err != nil {
		return Objective{}, nil, err
	}
	fixes = append(fixes, headingFixes...)

	o := Objective{Format: fm.format, Approved: fm.approved}
	hasObjective := slices.ContainsFunc(sections, func(s rawSection) bool { return s.name == HeadingObjective })
	seen := map[string]int{} // known section name → line of its heading
	var heads []line         // each task's heading line, for the AssignTaskIDs fixes
	for _, s := range sections {
		if s.name != "" {
			if first, dup := seen[s.name]; dup {
				return Objective{}, nil, fmt.Errorf("line %d: a second %q section (the first is on line %d); merge them into one",
					s.line, headingLine(s.name), first)
			}
			seen[s.name] = s.line
		}

		var f []Fix
		switch s.name {
		case HeadingObjective:
			o.ObjText, f = tidyBody(s.lines)
		case HeadingInstructions:
			o.Instructions, f = tidyBody(s.lines)
		case HeadingConstraints:
			o.Constraints, f = tidyBody(s.lines)
		case HeadingContext:
			o.Context, f = tidyBody(s.lines)
		case HeadingTasks:
			o.Tasks, o.TaskNotes, heads, f, err = parseTasks(s.lines)
			if err != nil {
				return Objective{}, nil, err
			}
		default: // an unknown section, or (line 0) the text before the first heading
			var body string
			body, f = tidyBody(s.lines)
			switch {
			case s.line > 0:
				o.Other = append(o.Other, Section{Heading: s.heading, Body: body})
			case body == "":
				// [agent] nothing before the first heading
			case hasObjective:
				return Objective{}, nil, fmt.Errorf("line %d: text before the first heading; move it under a heading "+
					"(for example \"## Context\") or delete it", firstTextLine(s.lines))
			default:
				// [agent] a draft without "# Objective": its opening text is the objective
				o.ObjText = body
				f = append(f, Fix{Line: firstTextLine(s.lines), Now: headingLine(HeadingObjective)})
			}
		}
		fixes = append(fixes, f...)
	}

	for _, i := range AssignTaskIDs(o.Tasks) {
		fixes = append(fixes, Fix{Line: heads[i].num, Was: heads[i].text, Now: taskHeadingLine(o.Tasks[i])})
	}
	slices.SortStableFunc(fixes, func(a, b Fix) int { return cmp.Compare(a.Line, b.Line) })
	return o, fixes, nil
}

// firstTextLine returns the line number of the first non-blank line, or 0 when all are blank.
//
// Notes: none.
func firstTextLine(lines []line) int {
	for _, l := range lines {
		if strings.TrimSpace(l.text) != "" {
			return l.num
		}
	}
	return 0
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
//   - Only flat "key: value" lines (no YAML library yet); blank lines inside the block are skipped.
//   - [agent] a file that opens with a "---" rule followed by prose is read as front matter and fails with the
//     "key: value" error; acceptable, since a rule at the very top of an objective is rare.
func readFrontMatter(lines []line) (fm frontMatter, rest []line, fixes []Fix, err error) {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start].text) == "" {
		start++
	}
	if start == len(lines) || strings.TrimSpace(lines[start].text) != "---" {
		return frontMatter{format: FormatV1}, lines, nil, nil
	}
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l.text)
		switch {
		case t == "---":
			if fm.format == "" {
				fm.format = FormatV1
			}
			return fm, lines[i+1:], fixes, nil
		case t == "":
			continue
		}
		key, value, found := strings.Cut(t, ":")
		if !found {
			return frontMatter{}, nil, nil, fmt.Errorf("line %d: front matter line %q is not \"key: value\"", l.num, l.text)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch key {
		case "format":
			if !slices.Contains(SupportedFormats, value) {
				return frontMatter{}, nil, nil, fmt.Errorf("line %d: unknown format %q; supported: %s",
					l.num, value, strings.Join(SupportedFormats, ", "))
			}
			fm.format = value
		case "approved":
			fm.approved = value
		default:
			fixes = append(fixes, Fix{Line: l.num, Was: l.text})
		}
	}
	return frontMatter{}, nil, nil, fmt.Errorf("line %d: front matter has no closing \"---\"", lines[start].num)
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
//     unknown heading of level 1 or 2 starts an Other section (a fix when it isn't exactly "## <text>"); an unknown
//     level 3+ heading is a body line (a subheading inside the current section).
func splitSections(lines []line) ([]rawSection, []Fix, error) {
	sections := []rawSection{{}} // [agent] the first one holds the text before the first heading (line 0)
	var fixes []Fix
	inFence := false
	for _, l := range lines {
		cur := &sections[len(sections)-1]
		if isCodeBlockFence(l.text) {
			inFence = !inFence
			cur.lines = append(cur.lines, l)
			continue
		}
		var level int
		var text string
		var isHeading bool
		if !inFence {
			level, text, isHeading = readHeading(l.text)
		}
		if !isHeading {
			cur.lines = append(cur.lines, l)
			continue
		}

		if name, known := sectionName(text); known {
			sections = append(sections, rawSection{name: name, heading: name, line: l.num})
			if want := headingLine(name); l.text != want {
				fixes = append(fixes, Fix{Line: l.num, Was: l.text, Now: want})
			}
			continue
		}
		_, _, hasID := taskHeading(text)
		switch {
		case cur.name == HeadingTasks && (hasID || level >= 3):
			cur.lines = append(cur.lines, l) // [agent] parseTasks decides: a task, or a subheading inside one
		case hasID:
			return nil, nil, fmt.Errorf("line %d: %q is a task heading outside \"## Tasks\"; every task must be under "+
				"the \"## Tasks\" section", l.num, l.text)
		case level <= 2:
			sections = append(sections, rawSection{heading: text, line: l.num})
			if want := "## " + text; l.text != want {
				fixes = append(fixes, Fix{Line: l.num, Was: l.text, Now: want})
			}
		default:
			cur.lines = append(cur.lines, l) // a subheading inside the current section
		}
	}
	return sections, fixes, nil
}

// headingLine returns the standard heading line for a known section: "# Objective" or "## <name>".
//
// Notes: none.
func headingLine(name string) string {
	if name == HeadingObjective {
		return "# " + name
	}
	return "## " + name
}

// isCodeBlockFence reports whether s opens or closes a code fence: "```" or "~~~" after optional indentation.
//
// Notes:
//   - [agent] simplified: any fence line toggles, so a ~~~ line inside a ``` block would end it. Markdown pairs a fence
//     with the same character; fine to tighten later if a real file needs it.
func isCodeBlockFence(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// readHeading reports whether s is a heading and returns its level (1–6) and text.
//
// Notes:
//   - "# Title"; up to three leading spaces are allowed (four or more is an indented code block in Markdown).
//   - A closing "#" run after a space is dropped ("## Tasks ##" → "Tasks"); "### C#" keeps its "#".
//   - No space after the "#"s counts only when the rest is a known section name or a task ID ("#Objective",
//     "###t2: x"), so "#hashtag" stays text.
//   - Seven or more "#", or a heading with no text ("# "), is not a heading.
func readHeading(s string) (level int, text string, ok bool) {
	trimmed := strings.TrimLeft(s, " ")
	if len(s)-len(trimmed) > 3 {
		return 0, "", false
	}
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := trimmed[level:]

	if rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		text = strings.TrimSpace(rest)
		if t := strings.TrimRight(text, "#"); t == "" {
			text = "" // [agent] "## ##": only hashes, no text
		} else if t != text && (strings.HasSuffix(t, " ") || strings.HasSuffix(t, "\t")) {
			text = strings.TrimSpace(t)
		}
		if text == "" {
			return 0, "", false
		}
		return level, text, true
	}

	text = strings.TrimSpace(rest)
	if _, known := sectionName(text); known {
		return level, text, true
	}
	if _, _, hasID := taskHeading(text); hasID {
		return level, text, true
	}
	return 0, "", false
}

// sectionName maps heading text to a Heading* constant, or reports false for an unknown section.
//
// Notes:
//   - Lenient: any case, surrounding spaces, a trailing ":", the singular or plural ("Instruction", "Objectives").
//   - Exact names otherwise: "Task list" is unknown (goes to Other).
func sectionName(text string) (string, bool) {
	h := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ":")))
	switch h {
	case "objective", "objectives":
		return HeadingObjective, true
	case "instructions", "instruction":
		return HeadingInstructions, true
	case "constraints", "constraint":
		return HeadingConstraints, true
	case "context":
		return HeadingContext, true
	case "tasks", "task":
		return HeadingTasks, true
	}
	return "", false
}

// taskIDRe matches a task heading's ID and title: "t2: Title", "T2 - Title", "t2. Title", "Task 2: Title",
// "task2 Title", "t2". The ID must end at a separator, a space or the end ("t2x" has no ID).
var taskIDRe = regexp.MustCompile(`^(?i:task\s*|t)(\d+)(?:\s*[:.)\-]\s*|\s+|$)(.*)$`)

// taskIDOnlyRe matches a bare task ID as written in a depends line: "t3", "T3", "task3", "Task 3", "3".
var taskIDOnlyRe = regexp.MustCompile(`^(?i:task\s*|t)?(\d+)$`)

// taskHeading splits task heading text into a lower-case ID and a title; hasID is false when there is no ID (then
// the whole text is the title).
//
// Notes:
//   - "Test the API" and "Tasks" have no ID: a digit must follow "t" or "task" (the regexp needs it).
//   - Leading zeros are dropped: "t02" → "t2".
func taskHeading(text string) (id, title string, hasID bool) {
	text = strings.TrimSpace(text)
	m := taskIDRe.FindStringSubmatch(text)
	if m == nil {
		return "", text, false
	}
	return taskIDFromDigits(m[1]), strings.TrimSpace(m[2]), true
}

// taskIDFromDigits builds a task ID from its digits, dropping leading zeros ("03" → "t3", "0" → "t0").
//
// Notes:
//   - Works on the string, so a very long number can't overflow; CheckComplete rejects IDs taskNumber can't read.
func taskIDFromDigits(digits string) string {
	d := strings.TrimLeft(digits, "0")
	if d == "" {
		d = "0"
	}
	return "t" + d
}

// taskHeadingLine returns the standard heading line for a task: "### t3: Title" ("### t3:" when the title is empty).
//
// Notes: none.
func taskHeadingLine(t Task) string {
	if t.Title == "" {
		return "### " + t.ID + ":"
	}
	return "### " + t.ID + ": " + t.Title
}

// dependsLine returns the standard depends line, "depends: t1, t2", or "" when there are none.
//
// Notes: none.
func dependsLine(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return "depends: " + strings.Join(ids, ", ")
}

// [agent] tedious pattern, from mvp3/director/objective.go: bulletRe matches a top-level bullet ("- x", "* x", "+ x",
// "1. x", "1) x") and checkboxRe a "[ ] " / "[x] " box at the start of a bullet's text.
var (
	bulletRe   = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(.*)$`)
	checkboxRe = regexp.MustCompile(`^\[[ xX]\]\s*`)
)

// bulletSpacingRe splits a bullet line (at any indentation) into indentation, marker, the space after the marker, and
// the text, so tidyBody can fix "-    x".
var bulletSpacingRe = regexp.MustCompile(`^([ \t]*)([-*+]|\d+[.)])([ \t]+)(\S.*)$`)

// parseTasks turns the body lines of "## Tasks" into tasks, and returns each task's heading line (for AssignTaskIDs
// fixes) and the fixes it made.
//
// Notes:
//   - A level-3 heading, or a heading with a task ID at any level, starts a task; a fix when it has an ID and the line
//     isn't "### tN: title" (wrong level, "T2 - x"). A task without an ID gets its fix later, from Cleanup, once
//     AssignTaskIDs has given it one.
//   - Before the first task heading, a top-level bullet starts a task too (a hand-written bullet list): its text is
//     the title (checkbox stripped), the lines under it (dedented) the body. After the first task heading, bullets
//     are body text.
//   - Other text before the first task is TaskNotes.
//   - The first non-blank body line "depends: …" or "depends on: …" fills Depends (parseDepends); a fix when it
//     isn't exactly "depends: t1, t2" (an empty list is dropped).
//   - The body keeps its newlines (tidyBody); a duplicate ID → error naming both lines.
func parseTasks(lines []line) (tasks []Task, notes string, heads []line, fixes []Fix, err error) {
	type rawTask struct {
		head   line
		id     string
		title  string
		bullet bool // started by a bullet: its body lines are dedented
		body   []line
	}
	var raw []rawTask
	var noteLines []line
	addLine := func(l line) {
		if len(raw) == 0 {
			noteLines = append(noteLines, l)
			return
		}
		raw[len(raw)-1].body = append(raw[len(raw)-1].body, l)
	}
	startTask := func(l line, text string, bullet bool) {
		id, title, hasID := taskHeading(text)
		raw = append(raw, rawTask{head: l, id: id, title: title, bullet: bullet})
		if want := taskHeadingLine(Task{ID: id, Title: title}); hasID && l.text != want {
			fixes = append(fixes, Fix{Line: l.num, Was: l.text, Now: want})
		}
	}

	inFence, sawHeading := false, false
	for _, l := range lines {
		if isCodeBlockFence(l.text) {
			inFence = !inFence
			addLine(l)
			continue
		}
		if inFence {
			addLine(l)
			continue
		}
		level, text, isHeading := readHeading(l.text)
		_, _, hasID := taskHeading(text)
		switch {
		case isHeading && (level == 3 || hasID):
			sawHeading = true
			startTask(l, text, false)
		case !isHeading && !sawHeading && bulletRe.MatchString(l.text):
			m := bulletRe.FindStringSubmatch(l.text)
			startTask(l, strings.TrimSpace(checkboxRe.ReplaceAllString(m[1], "")), true)
		default:
			addLine(l)
		}
	}

	seenIDs := map[string]int{} // task ID → line of its heading
	for _, r := range raw {
		if r.id != "" {
			if first, dup := seenIDs[r.id]; dup {
				return nil, "", nil, nil, fmt.Errorf("line %d: task ID %s is used twice (first on line %d); give each "+
					"task its own ID", r.head.num, r.id, first)
			}
			seenIDs[r.id] = r.head.num
		}
		body := r.body
		if r.bullet {
			body = dedent(body)
		}
		t := Task{ID: r.id, Title: r.title}

		// [agent] the depends line is the first non-blank line under the heading
		i := 0
		for i < len(body) && strings.TrimSpace(body[i].text) == "" {
			i++
		}
		if i < len(body) {
			if ids, ok := parseDepends(body[i].text); ok {
				t.Depends = ids
				if want := dependsLine(ids); body[i].text != want {
					fixes = append(fixes, Fix{Line: body[i].num, Was: body[i].text, Now: want})
				}
				body = body[i+1:]
			}
		}

		var f []Fix
		t.Body, f = tidyBody(body)
		fixes = append(fixes, f...)
		tasks = append(tasks, t)
		heads = append(heads, r.head)
	}

	notes, f := tidyBody(noteLines)
	fixes = append(fixes, f...)
	return tasks, notes, heads, fixes, nil
}

// dedent removes the indentation that every non-blank line shares (the lines under a bullet task).
//
// Notes:
//   - Blank lines shorter than that indentation become empty.
func dedent(lines []line) []line {
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l.text) == "" {
			continue
		}
		n := len(l.text) - len(strings.TrimLeft(l.text, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return lines
	}
	out := make([]line, len(lines))
	for i, l := range lines {
		if len(l.text) >= indent {
			l.text = l.text[indent:]
		} else {
			l.text = ""
		}
		out[i] = l
	}
	return out
}

// parseDepends reads a "depends: t1, t2" line and reports whether s is one.
//
// Notes:
//   - Key "depends" or "depends on", any case. Items split on ","; each goes through normalizeTaskID, so
//     "depends on: task1, 3" gives t1, t3. An item that isn't one ID ("t1 t2") is split on spaces and used if every
//     part is an ID.
//   - An item that still isn't an ID is kept as written: CheckComplete reports it as naming no task.
//   - An empty list is still a depends line (Depends nil).
//   - [agent] a task body whose first line happens to start "Depends: …" is read as a depends line; CheckComplete then
//     reports the words as unknown tasks, which tells the author.
func parseDepends(s string) (ids []string, ok bool) {
	key, value, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found {
		return nil, false
	}
	key = strings.Join(strings.Fields(strings.ToLower(key)), " ")
	if key != "depends" && key != "depends on" {
		return nil, false
	}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if id, isID := normalizeTaskID(item); isID {
			ids = append(ids, id)
			continue
		}
		parts := strings.Fields(item)
		var partIDs []string
		for _, p := range parts {
			if id, isID := normalizeTaskID(p); isID {
				partIDs = append(partIDs, id)
			}
		}
		if len(partIDs) == len(parts) {
			ids = append(ids, partIDs...)
		} else {
			ids = append(ids, item)
		}
	}
	return ids, true
}

// normalizeTaskID turns "t3", "T3", "task3", "Task 3", "3" or "t03" into "t3", and reports false for anything else.
//
// Notes: none.
func normalizeTaskID(s string) (string, bool) {
	m := taskIDOnlyRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", false
	}
	return taskIDFromDigits(m[1]), true
}

// tidyBody joins a section's (or task's) body lines into text, fixing bullets, and returns the fixes.
//
// Notes:
//   - Outside fences only (isCodeBlockFence): "-    x" → "- x" (at any indentation); a non-blank, unindented,
//     non-bullet line straight after a bullet item (Markdown reads it as part of that item) is indented by two spaces
//     so it visibly belongs there. Lines starting with "#", ">" or "|" are not treated as such a continuation.
//   - Leading and trailing blank lines are dropped; newlines inside are kept.
//   - Makes no fix on its own output, so a file Markdown wrote cleans up with zero fixes.
//   - [agent] a "* * *" rule straight before a paragraph counts as a bullet, so that paragraph gets indented; rare.
func tidyBody(lines []line) (string, []Fix) {
	var out []string
	var fixes []Fix
	inFence, inItem := false, false // inItem: the line above belongs to a bullet item
	for _, l := range lines {
		text := l.text
		switch {
		case isCodeBlockFence(text):
			inFence, inItem = !inFence, false
		case inFence:
			// code is kept exactly as written
		case strings.TrimSpace(text) == "":
			inItem = false
		default:
			if m := bulletSpacingRe.FindStringSubmatch(text); m != nil {
				if m[3] != " " {
					text = m[1] + m[2] + " " + m[4]
				}
				inItem = true
			} else if text[0] == ' ' || text[0] == '\t' {
				// already indented: part of the item above, or ordinary indented text
			} else if inItem && !strings.ContainsAny(text[:1], "#>|") {
				text = "  " + text
			} else {
				inItem = false
			}
		}
		if text != l.text {
			fixes = append(fixes, Fix{Line: l.num, Was: l.text, Now: text})
		}
		out = append(out, text)
	}

	start, end := 0, len(out)
	for start < end && strings.TrimSpace(out[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(out[end-1]) == "" {
		end--
	}
	return strings.Join(out[start:end], "\n"), fixes
}
