package objective

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
	// TODO: a switch on which of Was / Now is empty; fmt.Sprintf with %q
	return ""
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
	// TODO: sha256.Sum256([]byte(o.body())); "sha256:" + hex.EncodeToString(sum[:])
	return ""
}

// ApprovalStatus reports whether o was saved by plan mode and is unchanged since (ApprovalApproved), changed since
// (ApprovalEdited), or never planned (ApprovalUnplanned).
//
// Notes:
//   - Plan mode sets o.Approved = o.BodyHash() just before it writes the file.
func (o Objective) ApprovalStatus() string {
	// TODO: Approved "" → unplanned; Approved == o.BodyHash() → approved; else edited
	return ""
}

// CheckComplete reports everything that stops o from being run: plan mode's output and autonomous mode's input.
//
// Notes:
//   - Runs after Cleanup, or on an Objective built from the planner's tool arguments (never cleaned up), so it checks
//     task IDs too.
//   - Returns every problem at once (errors.Join): the planner LLM fixes them all in one go.
//   - The wording is read by a person and by the planner LLM: say what to add or change.
func (o Objective) CheckComplete(maxTasks int) error {
	// TODO 1: var errs []error
	// TODO 2: sections: ObjText empty · Instructions empty (strings.TrimSpace)
	// TODO 3: tasks: none · more than maxTasks
	// TODO 4: each task: ID not "t<digits>" (taskNumber) · ID seen before (a map) · Title blank
	// TODO 5: each depends entry: names no task · names the task itself
	// TODO 6: if findCycle(o.Tasks) returns a cycle, one error naming it ("t2 → t3 → t2")
	// TODO 7: return errors.Join(errs...)  (nil when the slice is empty)
	return nil
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
	// TODO 1: highest := 0; for each task with taskNumber(ID) ok → highest = max(highest, n)
	// TODO 2: for i, t := range tasks: if t.ID == "" → highest++; tasks[i].ID = fmt.Sprintf("t%d", highest); remember i
	// Hint: write through tasks[i], not t (t is a copy)
	return nil
}

// taskNumber returns n for an ID "t<n>" (n ≥ 1), and false for anything else.
//
// Notes:
//   - IDs are lower-case here: Cleanup lower-cases "T3" before storing it.
func taskNumber(id string) (int, bool) {
	// TODO: strings.CutPrefix(id, "t"), then strconv.Atoi; reject n < 1 and a leading "+"/"-" (Atoi accepts them)
	return 0, false
}

// findCycle returns one dependency cycle as IDs ("t2", "t3", "t2"), or nil when there is none.
//
// Notes:
//   - Depends entries naming no task are skipped here (CheckComplete reports them separately).
//   - [agent] a depth-first search with three states: unvisited, on the current path, done. Reaching a task that is
//     "on the current path" means a cycle; the path from that task to here is the cycle.
func findCycle(tasks []Task) []string {
	// TODO 1: byID := map[string]Task; state := map[string]int (0 unvisited, 1 on path, 2 done); var path []string
	// TODO 2: a recursive visit(id) func: state 1 → cycle found (cut path from where id first appears, append id);
	//         state 2 → return; else mark 1, push id, visit each depends, pop, mark 2
	// TODO 3: visit every task in file order; return the first cycle
	return nil
}
