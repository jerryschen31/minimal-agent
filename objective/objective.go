package objective

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const MaxBytes = 1024 * 1024 // 1 MB

// FormatV1 is the current file format, written in the front matter as "format: minagent-objective/v1".
// A file without front matter (or without a format line) is read as FormatV1.
const FormatV1 = "minagent-objective/v1"

// SupportedFormats lists every format Cleanup reads; an unknown one is an error that names these.
var SupportedFormats = []string{FormatV1}

// Section headings: matched leniently by Cleanup (any level, any case, singular, trailing ":"), written exactly
// like this by Markdown.
const (
	HeadingObjective    = "Objective"
	HeadingInstructions = "Instructions"
	HeadingConstraints  = "Constraints"
	HeadingContext      = "Context"
	HeadingTasks        = "Tasks"
)

// Objective is a cleaned-up objective file.
type Objective struct {
	Format       string    // front matter "format:"; FormatV1 when the file has none
	Approved     string    // front matter "approved:": "sha256:<hex>" of the body when plan mode saved it; "" if never planned
	ObjText      string    // body of "# Objective"
	Instructions string    // body of "## Instructions"
	Constraints  string    // body of "## Constraints"; "" if absent
	Context      string    // body of "## Context"; "" if absent
	TaskNotes    string    // text under "## Tasks" before the first task
	Tasks        []Task    // the "### tN: title" subsections of "## Tasks", in file order; nil when there are none
	Other        []Section // every other "##" section, in file order
}

// Task is one "### tN: title" subsection of "## Tasks".
type Task struct {
	ID      string   // "t1", "t2", …; never renumbered; a task without one gets the highest number + 1 (AssignTaskIDs)
	Title   string   // the heading text after "tN: "
	Depends []string // IDs from the optional "depends: t1, t2" line straight under the heading; nil if none
	Body    string   // the lines under the heading (after depends), newlines kept
}

// Section is one "## Heading" and the text under it.
type Section struct {
	Heading string
	Body    string
}

// Fix is one change Cleanup made, for the warnings plan and autonomous mode print and the trace.
type Fix struct {
	Line int    // 1-based line in the input file; 0 when the fix is not tied to one line
	Was  string // the line as written ("" when something was added)
	Now  string // what Markdown will write instead ("" when the line is dropped, e.g. an unknown front-matter key)
}

// String formats f for a warning line, e.g. `line 7: "### Constraints" → "## Constraints"`.
//
// Notes:
//   - Was "" → `added "…"`; Now "" → `ignored "…"`; leave out "line N: " when Line is 0.
func (f Fix) String() string {
	prefix := ""
	if f.Line > 0 {
		prefix = fmt.Sprintf("line %d: ", f.Line)
	}
	switch {
	case f.Was == "":
		return prefix + fmt.Sprintf("added %q", f.Now)
	case f.Now == "":
		return prefix + fmt.Sprintf("ignored %q", f.Was)
	}
	return prefix + fmt.Sprintf("%q → %q", f.Was, f.Now)
}

// Approval states of a file, from ApprovalStatus; autonomous mode warns on anything but ApprovalApproved.
const (
	ApprovalApproved  = "approved"  // saved by plan mode and unchanged since
	ApprovalEdited    = "edited"    // saved by plan mode, but the body changed since (or the hash is malformed)
	ApprovalUnplanned = "unplanned" // never saved by plan mode (no "approved:" line): hand-written
)

// BodyHash returns "sha256:<hex>" of the body as Markdown writes it (everything after the front matter).
//
// Notes:
//   - Hashes the standard layout, not the file's bytes: an edit that Cleanup tidies away (spacing, "## Instruction")
//     is not a change; any change to the content is.
//   - The front matter is left out, so writing the hash into it doesn't change the hash.
//   - [agent] a fingerprint, not a signature: whoever edits the file can recompute it. It catches edits nobody
//     re-planned, not someone set on hiding them.
func (o Objective) BodyHash() string {
	sum := sha256.Sum256([]byte(o.body()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ApprovalStatus reports whether o was saved by plan mode and is unchanged since (ApprovalApproved), changed since
// (ApprovalEdited), or never planned (ApprovalUnplanned).
//
// Notes:
//   - Plan mode sets o.Approved = o.BodyHash() just before it writes the file.
func (o Objective) ApprovalStatus() string {
	switch o.Approved {
	case "":
		return ApprovalUnplanned
	case o.BodyHash():
		return ApprovalApproved
	}
	return ApprovalEdited
}

// CheckComplete reports everything that stops o from being run: plan mode's output and autonomous mode's input.
//
// Notes:
//   - Runs after Cleanup, or on an Objective built from the planner's tool arguments (never cleaned up), so it checks
//     task IDs too.
//   - Returns every problem at once (errors.Join): the planner LLM fixes them all in one go.
//   - The wording is read by a person and by the planner LLM: say what to add or change.
//   - A task is named by its ID, or by its position and title when the ID is the problem.
func (o Objective) CheckComplete(maxTasks int) error {
	var errs []error
	if strings.TrimSpace(o.ObjText) == "" {
		errs = append(errs, errors.New(`"# Objective" is missing or empty: say what to achieve and why`))
	}
	if strings.TrimSpace(o.Instructions) == "" {
		errs = append(errs, errors.New(`"## Instructions" is missing or empty: say how to work and how to tell `+
			`that the objective is done`))
	}
	switch {
	case len(o.Tasks) == 0:
		errs = append(errs, errors.New(`"## Tasks" has no tasks: add at least one "### t1: <title>" task`))
	case len(o.Tasks) > maxTasks:
		errs = append(errs, fmt.Errorf("there are %d tasks; the limit is %d: merge some", len(o.Tasks), maxTasks))
	}

	ids := map[string]bool{}
	for i, t := range o.Tasks {
		if _, ok := taskNumber(t.ID); !ok {
			errs = append(errs, fmt.Errorf("task %d (%q): ID %q is not t<number> (t1, t2, …)", i+1, t.Title, t.ID))
		} else if ids[t.ID] {
			errs = append(errs, fmt.Errorf("task ID %s is used by more than one task: give each task its own ID", t.ID))
		}
		ids[t.ID] = true
		if strings.TrimSpace(t.Title) == "" {
			errs = append(errs, fmt.Errorf("task %s has no title: write one after \"### %s: \"", t.ID, t.ID))
		}
	}
	for _, t := range o.Tasks {
		for _, d := range t.Depends {
			switch {
			case d == t.ID:
				errs = append(errs, fmt.Errorf("task %s depends on itself: remove %s from its depends line", t.ID, d))
			case !ids[d]:
				errs = append(errs, fmt.Errorf("task %s depends on %q, which is not a task in this file", t.ID, d))
			}
		}
	}
	if cycle := findCycle(o.Tasks); cycle != nil {
		errs = append(errs, fmt.Errorf("tasks depend on each other in a loop (%s): remove one of these dependencies",
			strings.Join(cycle, " → ")))
	}
	return errors.Join(errs...)
}

// AssignTaskIDs gives every task without an ID the next number after the highest existing one, in file order, and
// returns the indexes of the tasks it changed.
//
// Notes:
//   - Never renumbers: a task that has an ID keeps it, even with gaps or out of order (t1, t5, t2).
//   - With no IDs at all the tasks become t1, t2, …; with t1, t5 and two without, those become t6, t7.
//   - Only "t<digits>" counts when finding the highest; other IDs (from tool arguments) are left for CheckComplete.
//   - Shared by Cleanup and plan mode's propose_objective, so a draft and a proposal follow the same rule.
func AssignTaskIDs(tasks []Task) []int {
	highest := 0
	for _, t := range tasks {
		if n, ok := taskNumber(t.ID); ok {
			highest = max(highest, n)
		}
	}
	var changed []int
	for i := range tasks {
		if tasks[i].ID == "" {
			highest++
			tasks[i].ID = "t" + strconv.Itoa(highest)
			changed = append(changed, i)
		}
	}
	return changed
}

// taskNumber returns n for an ID "t<n>" (1 ≤ n, at most 9 digits), and false for anything else.
//
// Notes:
//   - IDs are lower-case here: Cleanup lower-cases "T3" before storing it.
//   - Digits only (strconv.Atoi alone would accept "+3"); the 9-digit cap keeps AssignTaskIDs' "highest + 1" from
//     overflowing on a silly ID. A longer number is not an ID CheckComplete accepts.
func taskNumber(id string) (int, bool) {
	digits, ok := strings.CutPrefix(id, "t")
	if !ok || digits == "" || len(digits) > 9 || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// findCycle returns one dependency cycle as IDs ("t2", "t3", "t2"), or nil when there is none.
//
// Notes:
//   - Depends entries naming no task, and a task depending on itself, are skipped here (CheckComplete reports them
//     separately).
//   - [agent] a depth-first search with three states: unvisited, on the current path, done. Reaching a task that is
//     "on the current path" means a cycle; the path from that task to here is the cycle.
func findCycle(tasks []Task) []string {
	byID := map[string]Task{}
	for _, t := range tasks {
		if _, dup := byID[t.ID]; !dup {
			byID[t.ID] = t // [agent] with a duplicate ID the first task wins; CheckComplete reports the duplicate
		}
	}
	const (
		unvisited = iota
		onPath
		done
	)
	state := map[string]int{}
	var path, cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		switch state[id] {
		case onPath:
			cycle = append(slices.Clone(path[slices.Index(path, id):]), id)
			return true
		case done:
			return false
		}
		state[id] = onPath
		path = append(path, id)
		for _, d := range byID[id].Depends {
			if _, exists := byID[d]; !exists || d == id {
				continue
			}
			if visit(d) {
				return true
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return false
	}
	for _, t := range tasks {
		if visit(t.ID) {
			return cycle
		}
	}
	return nil
}
