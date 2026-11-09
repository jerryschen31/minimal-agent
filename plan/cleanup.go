package plan

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jerryschen31/minimal-agent/mdscan"
)

// line is one line of the input with its 1-based line number, so fixes can point at it.
type line = mdscan.Line

// rawSection is one section found by splitSections, before its body is interpreted.
type rawSection struct {
	name    string // a Heading* constant, or "" for an unknown section (goes to Other)
	heading string // the heading text as written (kept for Other)
	title   string // the plan title, for HeadingPlan
	line    int    // line of the heading; 0 for the text before the first heading
	lines   []line // body lines
}

// Cleanup reads a plan file, fixes small mistakes, and returns the Plan and the fixes it made.
//
// Notes:
//   - The one reader of plan files; Markdown is the one writer, and a file it wrote cleans up with zero fixes. It writes
//     nothing: callers print the fixes (warnings) and trace them. Plan files are written by Go, so fixes only come
//     from hand edits.
//   - Fixes: a known section at the wrong level or spelled loosely ("### Tasks", "## tasks:", "#Plan: x"), an unknown
//     level-1 section, extra spaces after a bullet dash, a line that continues the bullet above it, task headings not
//     at "###", bullet tasks turned into "### tN:" tasks, missing task IDs (AssignTaskIDs), field lines that aren't in
//     standard form or order; an unknown front-matter key is a fix too (ignored, Now "").
//   - One fix per changed line; the fixes are sorted by line number.
//   - Errors (it can't decide): too big, not UTF-8 or empty; bad front matter or unknown format; a known section twice;
//     a duplicate task ID; a task heading outside "## Tasks"; text before the first heading.
//   - Not its job: whether the plan is complete or the depends are valid. CheckComplete runs after it.
func Cleanup(data []byte) (Plan, []Fix, error) {
	lines, err := mdscan.Lines(data, "plan file")
	if err != nil {
		return Plan{}, nil, err
	}
	fm, rest, fixes, err := readFrontMatter(lines)
	if err != nil {
		return Plan{}, nil, err
	}
	sections, headingFixes, err := splitSections(rest)
	if err != nil {
		return Plan{}, nil, err
	}
	fixes = append(fixes, headingFixes...)

	p := Plan{Format: fm.format, ObjectivePath: fm.objective, ObjectiveHash: fm.objectiveHash, Version: fm.version}
	seen := map[string]int{} // known section name → line of its heading
	var heads []line         // each task's heading line, for the AssignTaskIDs fixes
	for _, s := range sections {
		if s.name != "" {
			if first, dup := seen[s.name]; dup {
				return Plan{}, nil, fmt.Errorf("line %d: a second %q section (the first is on line %d); merge them into one",
					s.line, headingLine(s.name), first)
			}
			seen[s.name] = s.line
		}

		var f []Fix
		switch s.name {
		case HeadingPlan:
			p.Title = s.title
			p.Summary, f = tidyBody(s.lines)
		case HeadingAssumptions:
			p.Assumptions, f = tidyBody(s.lines)
		case HeadingTasks:
			p.Tasks, p.TaskNotes, heads, f, err = parseTasks(s.lines)
			if err != nil {
				return Plan{}, nil, err
			}
		default: // an unknown section, or (line 0) the text before the first heading
			var body string
			body, f = tidyBody(s.lines)
			switch {
			case s.line > 0:
				p.Other = append(p.Other, Section{Heading: s.heading, Body: body})
			case body != "":
				return Plan{}, nil, fmt.Errorf("line %d: text before the first heading; move it under a heading or "+
					"delete it", mdscan.FirstTextLine(s.lines))
			}
		}
		fixes = append(fixes, f...)
	}

	for _, i := range AssignTaskIDs(p.Tasks, 0) {
		fixes = append(fixes, Fix{Line: heads[i].Num, Was: heads[i].Text, Now: taskHeadingLine(p.Tasks[i])})
	}
	slices.SortStableFunc(fixes, func(a, b Fix) int { return cmp.Compare(a.Line, b.Line) })
	return p, fixes, nil
}

// frontMatter holds the front-matter keys this version knows.
type frontMatter struct {
	format        string // "format:"; FormatV1 when absent
	objective     string // "objective:"
	objectiveHash string // "objective_hash:"
	version       int    // "plan_version:"; 0 when absent
}

// readFrontMatter reads an optional front matter block ("---", "key: value" lines, "---") and returns its known keys,
// the lines after it, and a fix for each unknown key.
//
// Notes:
//   - No front matter, or no "format:" line → FormatV1.
//   - An unknown key is a fix with Now "" (warned about and ignored), so a newer file still runs on an older binary.
//   - Errors: an unknown format (lists SupportedFormats); a plan_version that isn't a whole number (at most 9 digits);
//     a line without ":"; no closing "---".
//   - Only flat "key: value" lines (no YAML library); keys in any case; blank lines inside the block are skipped.
func readFrontMatter(lines []line) (fm frontMatter, rest []line, fixes []Fix, err error) {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start].Text) == "" {
		start++
	}
	if start == len(lines) || strings.TrimSpace(lines[start].Text) != "---" {
		return frontMatter{format: FormatV1}, lines, nil, nil
	}
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l.Text)
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
			return frontMatter{}, nil, nil, fmt.Errorf("line %d: front matter line %q is not \"key: value\"", l.Num, l.Text)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch key {
		case "format":
			if !slices.Contains(SupportedFormats, value) {
				return frontMatter{}, nil, nil, fmt.Errorf("line %d: unknown format %q; supported: %s",
					l.Num, value, strings.Join(SupportedFormats, ", "))
			}
			fm.format = value
		case "objective":
			fm.objective = value
		case "objective_hash":
			fm.objectiveHash = value
		case "plan_version":
			if value == "" || len(value) > 9 || strings.Trim(value, "0123456789") != "" {
				return frontMatter{}, nil, nil, fmt.Errorf("line %d: plan_version %q is not a whole number", l.Num, value)
			}
			fm.version, _ = strconv.Atoi(value)
		default:
			fixes = append(fixes, Fix{Line: l.Num, Was: l.Text})
		}
	}
	return frontMatter{}, nil, nil, fmt.Errorf("line %d: front matter has no closing \"---\"", lines[start].Num)
}

// splitSections splits the lines into sections, deciding for each heading whether it starts a section, and returns
// the fixes for headings it rewrote.
//
// Notes:
//   - Headings inside ``` or ~~~ fences are ignored (mdscan.IsCodeBlockFence).
//   - A known section at any level starts that section ("# Plan: <title>", "## Assumptions", "## Tasks"); a fix when
//     the line isn't exactly the standard heading.
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
		if mdscan.IsCodeBlockFence(l.Text) {
			inFence = !inFence
			cur.lines = append(cur.lines, l)
			continue
		}
		var level int
		var text string
		var isHeading bool
		if !inFence {
			level, text, isHeading = mdscan.ReadHeading(l.Text, knownHeading)
		}
		if !isHeading {
			cur.lines = append(cur.lines, l)
			continue
		}

		if title, isTitle := planTitle(text); isTitle {
			sections = append(sections, rawSection{name: HeadingPlan, heading: HeadingPlan, title: title, line: l.Num})
			if want := titleLine(title); l.Text != want {
				fixes = append(fixes, Fix{Line: l.Num, Was: l.Text, Now: want})
			}
			continue
		}
		if name, known := sectionName(text); known {
			sections = append(sections, rawSection{name: name, heading: name, line: l.Num})
			if want := headingLine(name); l.Text != want {
				fixes = append(fixes, Fix{Line: l.Num, Was: l.Text, Now: want})
			}
			continue
		}
		_, _, hasID := taskHeading(text)
		switch {
		case cur.name == HeadingTasks && (hasID || level >= 3):
			cur.lines = append(cur.lines, l) // [agent] parseTasks decides: a task, or a subheading inside one
		case hasID:
			return nil, nil, fmt.Errorf("line %d: %q is a task heading outside \"## Tasks\"; every task must be under "+
				"the \"## Tasks\" section", l.Num, l.Text)
		case level <= 2:
			sections = append(sections, rawSection{heading: text, line: l.Num})
			if want := "## " + text; l.Text != want {
				fixes = append(fixes, Fix{Line: l.Num, Was: l.Text, Now: want})
			}
		default:
			cur.lines = append(cur.lines, l) // a subheading inside the current section
		}
	}
	return sections, fixes, nil
}

// headingLine returns the standard heading line for a known section: "## <name>", or "# Plan" for the title section
// (used in the duplicate-section error; the title line itself comes from titleLine).
//
// Notes: none.
func headingLine(name string) string {
	if name == HeadingPlan {
		return "# " + name
	}
	return "## " + name
}

// titleLine returns the standard title heading: "# Plan: <title>", or "# Plan" when the title is empty.
//
// Notes: none.
func titleLine(title string) string {
	if title == "" {
		return "# " + HeadingPlan
	}
	return "# " + HeadingPlan + ": " + title
}

// planTitle reports whether heading text is the plan's title heading ("Plan", "Plan: <title>", any case) and returns
// the title.
//
// Notes:
//   - The title goes through mdscan.StripClosingHashes ("Plan: #" → ""), since it is written back as a heading.
func planTitle(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if strings.EqualFold(t, "plan") || strings.EqualFold(t, "plan:") {
		return "", true
	}
	if len(t) > 5 && strings.EqualFold(t[:5], "plan:") {
		return mdscan.StripClosingHashes(t[5:]), true
	}
	return "", false
}

// sectionName maps heading text to HeadingAssumptions or HeadingTasks, or reports false for an unknown section.
//
// Notes:
//   - Lenient: any case, surrounding spaces, a trailing ":", the singular or plural ("Task", "Assumption").
//   - The title heading is matched by planTitle, not here.
func sectionName(text string) (string, bool) {
	h := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ":")))
	switch h {
	case "assumptions", "assumption":
		return HeadingAssumptions, true
	case "tasks", "task":
		return HeadingTasks, true
	}
	return "", false
}

// knownHeading reports whether heading text written without a space after the "#"s still counts as a heading: the
// title, a known section name or a task ID ("#Plan: x", "#Tasks", "###t2: x"), so "#hashtag" stays text.
//
// Notes:
//   - Passed to mdscan.ReadHeading.
func knownHeading(text string) bool {
	if _, isTitle := planTitle(text); isTitle {
		return true
	}
	if _, known := sectionName(text); known {
		return true
	}
	_, _, hasID := taskHeading(text)
	return hasID
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
//   - The title goes through mdscan.StripClosingHashes ("t2:#" → title ""), since it is written back after "### t2: ".
func taskHeading(text string) (id, title string, hasID bool) {
	text = strings.TrimSpace(text)
	m := taskIDRe.FindStringSubmatch(text)
	if m == nil {
		return "", text, false
	}
	return taskIDFromDigits(m[1]), mdscan.StripClosingHashes(m[2]), true
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

// Field keys, in the order Markdown writes them under a task heading.
const (
	fieldDepends  = "depends"
	fieldReason   = "reason"
	fieldEffects  = "effects"
	fieldGate     = "gate"
	fieldNeeds    = "needs"
	fieldDoneWhen = "done when"
)

// fieldLines returns t's field lines in standard order, leaving out empty fields.
//
// Notes:
//   - "depends: t1, t2", "reason: …", "effects: external", "gate: …", "needs: …", "done when: …".
func fieldLines(t Task) []string {
	var out []string
	if len(t.Depends) > 0 {
		out = append(out, fieldDepends+": "+strings.Join(t.Depends, ", "))
	}
	for _, f := range []struct{ key, value string }{
		{fieldReason, t.Reason},
		{fieldEffects, t.Effects},
		{fieldGate, t.Gate},
		{fieldNeeds, t.Needs},
		{fieldDoneWhen, t.DoneWhen},
	} {
		if f.value != "" {
			out = append(out, f.key+": "+f.value)
		}
	}
	return out
}

// readField reads one field line ("Done-When: x", "depends on: task1, 3") and returns its standard key and value;
// ok is false when s isn't a field line.
//
// Notes:
//   - The key is matched in any case, with "-" or "_" for spaces; "depends on" is "depends", "effect" is "effects".
//   - A line with an empty value ("needs:") is not a field line: it stays in the body as written (the rule FuzzCleanup
//     found for "depends:", which would otherwise let a second such line move up and be read as a field next time).
//   - The depends value goes through parseDepends; the effects value is lower-cased.
//   - [agent] a task body whose first line happens to start "Needs: …" is read as a field line; harmless, and the
//     fix shows it.
func readField(s string) (key, value string, depends []string, ok bool) {
	k, v, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found {
		return "", "", nil, false
	}
	k = strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(k))
	k = strings.Join(strings.Fields(k), " ")
	v = strings.TrimSpace(v)
	switch k {
	case "depends", "depends on":
		ids, ok := parseDepends(v)
		return fieldDepends, v, ids, ok
	case "reason", "gate", "needs", "done when":
		return k, v, nil, v != ""
	case "effects", "effect":
		return fieldEffects, strings.ToLower(v), nil, v != ""
	}
	return "", "", nil, false
}

// parseFields reads the field lines straight under a task heading into t and returns the body lines after them and
// the fixes it made.
//
// Notes:
//   - The block starts at the first non-blank line and ends at the first line that isn't a field line (readField), a
//     key seen already in this block, or a blank line. With no field lines, every line is body.
//   - One fix per block line that isn't what Markdown writes there: a non-standard key or spacing, or fields out of
//     order (the block is written back in standard order, so the lines are compared position by position).
func parseFields(body []line, t *Task) (rest []line, fixes []Fix) {
	i := 0
	for i < len(body) && strings.TrimSpace(body[i].Text) == "" {
		i++
	}
	start := i
	seen := map[string]bool{}
	for ; i < len(body); i++ {
		key, value, ids, ok := readField(body[i].Text)
		if !ok || seen[key] {
			break
		}
		seen[key] = true
		switch key {
		case fieldDepends:
			t.Depends = ids
		case fieldReason:
			t.Reason = value
		case fieldEffects:
			t.Effects = value
		case fieldGate:
			t.Gate = value
		case fieldNeeds:
			t.Needs = value
		case fieldDoneWhen:
			t.DoneWhen = value
		}
	}
	if i == start {
		return body, nil
	}
	want := fieldLines(*t)
	for j, l := range body[start:i] {
		if l.Text != want[j] {
			fixes = append(fixes, Fix{Line: l.Num, Was: l.Text, Now: want[j]})
		}
	}
	return body[i:], fixes
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
//   - The field lines under each heading are read by parseFields; the body keeps its newlines (tidyBody); a duplicate
//     ID → error naming both lines.
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
		if want := taskHeadingLine(Task{ID: id, Title: title}); hasID && l.Text != want {
			fixes = append(fixes, Fix{Line: l.Num, Was: l.Text, Now: want})
		}
	}

	inFence, sawHeading := false, false
	for _, l := range lines {
		if mdscan.IsCodeBlockFence(l.Text) {
			inFence = !inFence
			addLine(l)
			continue
		}
		if inFence {
			addLine(l)
			continue
		}
		level, text, isHeading := mdscan.ReadHeading(l.Text, knownHeading)
		_, _, hasID := taskHeading(text)
		switch {
		case isHeading && (level == 3 || hasID):
			sawHeading = true
			startTask(l, text, false)
		case !isHeading && !sawHeading && bulletRe.MatchString(l.Text):
			m := bulletRe.FindStringSubmatch(l.Text)
			startTask(l, mdscan.StripClosingHashes(checkboxRe.ReplaceAllString(m[1], "")), true)
		default:
			addLine(l)
		}
	}

	seenIDs := map[string]int{} // task ID → line of its heading
	for _, r := range raw {
		if r.id != "" {
			if first, dup := seenIDs[r.id]; dup {
				return nil, "", nil, nil, fmt.Errorf("line %d: task ID %s is used twice (first on line %d); give each "+
					"task its own ID", r.head.Num, r.id, first)
			}
			seenIDs[r.id] = r.head.Num
		}
		body := r.body
		if r.bullet {
			body = dedent(body)
		}
		t := Task{ID: r.id, Title: r.title}
		body, f := parseFields(body, &t)
		fixes = append(fixes, f...)
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
//   - Returns the lines unchanged if dedenting would turn one of them into a heading.
func dedent(lines []line) []line {
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l.Text) == "" {
			continue
		}
		n := len(l.Text) - len(strings.TrimLeft(l.Text, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return lines
	}
	out := make([]line, len(lines))
	for i, l := range lines {
		if len(l.Text) >= indent {
			l.Text = l.Text[indent:]
		} else {
			l.Text = ""
		}
		if _, _, isHeading := mdscan.ReadHeading(l.Text, knownHeading); isHeading {
			return lines // [agent] "    # x" is code; dedented it would become a heading (found by FuzzCleanup)
		}
		out[i] = l
	}
	return out
}

// parseDepends reads the value of a depends line ("t1, t2", "task1, 3") into task IDs and reports whether it holds
// any.
//
// Notes:
//   - Items split on ","; each goes through normalizeTaskID, so "task1, 3" gives t1, t3. An item that isn't one ID
//     ("t1 t2") is split on spaces and used if every part is an ID.
//   - An item that still isn't an ID is kept as written: CheckComplete reports it as naming no task.
func parseDepends(value string) (ids []string, ok bool) {
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
	return ids, len(ids) > 0
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
//   - Outside fences only (mdscan.IsCodeBlockFence): "-    x" → "- x" (at any indentation); a non-blank, unindented,
//     non-bullet line straight after a bullet item (Markdown reads it as part of that item) is indented by two spaces
//     so it visibly belongs there. Lines starting with "#", ">" or "|" are not treated as such a continuation.
//   - A code fence still open at the end is closed with a "```" line (a fix on the last non-blank line).
//   - Leading and trailing blank lines are dropped; newlines inside are kept.
//   - Makes no fix on its own output, so a file Markdown wrote cleans up with zero fixes.
func tidyBody(lines []line) (string, []Fix) {
	var out []string
	var fixes []Fix
	inFence, inItem := false, false // inItem: the line above belongs to a bullet item
	for _, l := range lines {
		text := l.Text
		switch {
		case mdscan.IsCodeBlockFence(text):
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
		if text != l.Text {
			fixes = append(fixes, Fix{Line: l.Num, Was: l.Text, Now: text})
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
	out = out[start:end]
	if inFence {
		// [agent] close a fence left open, or Markdown (which writes sections in its own order) would put the next
		// section inside it (found by FuzzCleanup)
		out = append(out, "```")
		fixes = append(fixes, Fix{Line: mdscan.LastTextLine(lines), Now: "```"})
	}
	return strings.Join(out, "\n"), fixes
}
