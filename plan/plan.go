package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// FormatV1 is the current plan file format, written in the front matter as "format: minagent-plan/v1".
const FormatV1 = "minagent-plan/v1"

// SupportedFormats lists every format Cleanup reads; an unknown one is an error that names these.
var SupportedFormats = []string{FormatV1}

// Section headings: matched leniently by Cleanup (any level, any case, singular, trailing ":"), written exactly like
// this by Markdown. The title heading is "# Plan: <title>".
const (
	HeadingPlan        = "Plan"
	HeadingAssumptions = "Assumptions"
	HeadingTasks       = "Tasks"
)

// EffectsExternal is the one value of a task's "effects:" line: the task acts outside the workdir (a trade, an email,
// a submission), so it can't be checked for drift, is never re-run automatically, and its actions get idempotency keys.
const EffectsExternal = "external"

// Plan is a cleaned-up plan file: the tasks the planner and Go made from an objective file.
type Plan struct {
	Format        string    // front matter "format:"; FormatV1 when the file has none
	ObjectivePath string    // "objective:" — the objective file this plan was made from, relative to the plan
	ObjectiveHash string    // "objective_hash:" — objective.Hash() when this plan was made; a different hash = stale
	Version       int       // "plan_version:" — 1 for a first plan, +1 per update; 0 when absent
	Title         string    // the text after "# Plan: "
	Summary       string    // text under the title heading; "" if none
	Assumptions   string    // "## Assumptions": what the planner had to guess; "" if none
	TaskNotes     string    // text under "## Tasks" before the first task
	Tasks         []Task    // the "### tN: title" subsections of "## Tasks", in file order; nil when there are none
	Other         []Section // every other "##" section, in file order
}

// Task is one "### tN: title" subsection of "## Tasks".
type Task struct {
	ID       string   // "t1", "t2", …; never renumbered or reused (AssignTaskIDs)
	Title    string   // the heading text after "tN: "
	Depends  []string // "depends: t1, t2"; nil if none
	Reason   string   // "reason:" — why this plan version added or changed the task; "" otherwise
	Effects  string   // "effects:" — EffectsExternal or ""
	Gate     string   // "gate:" — why the task must ask before running; "" = no gate
	Needs    string   // "needs:" — the capabilities it needs, free text (tool-agnostic)
	DoneWhen string   // "done when:" — how to tell the task is done
	Body     string   // the lines after the field lines, newlines kept
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

// String formats f for a warning line, e.g. `line 7: "### Tasks" → "## Tasks"`.
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

// External reports whether t acts outside the workdir ("effects: external").
//
// Notes: none.
func (t Task) External() bool {
	return t.Effects == EffectsExternal
}

// DefinitionHash returns "sha256:<hex>" of t as Markdown writes it, without its reason line.
//
// Notes:
//   - Reason is left out: a new reason alone is not a change. Every other field, the title, the ID and the body count.
//   - Hashes the standard form, so a change Cleanup tidies away (spacing, "Depends:T1") is not a change.
//   - State stores it per task; a different hash on the next plan means the task changed.
func (t Task) DefinitionHash() string {
	t.Reason = ""
	sum := sha256.Sum256([]byte(taskBlock(t)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IsStale reports whether p was made from a different objective than the one whose hash is objectiveHash.
//
// Notes:
//   - A plan without an objective hash is stale: nothing ties it to the objective.
func (p Plan) IsStale(objectiveHash string) bool {
	return p.ObjectiveHash == "" || p.ObjectiveHash != objectiveHash
}

// CheckComplete reports everything that stops p from being run: the planner's output and autonomous mode's input.
//
// Notes:
//   - Runs after Cleanup, or on a Plan built from the planner's tool arguments (never cleaned up), so it checks the
//     front matter and task IDs too.
//   - Returns every problem at once (errors.Join): the planner LLM fixes them all in one go.
//   - The wording is read by a person and by the planner LLM: say what to add or change.
//   - A task is named by its ID, or by its position and title when the ID is the problem.
func (p Plan) CheckComplete(maxTasks int) error {
	var errs []error
	if p.Format != "" && !slices.Contains(SupportedFormats, p.Format) {
		errs = append(errs, fmt.Errorf("format %q is not supported; supported: %s", p.Format,
			strings.Join(SupportedFormats, ", ")))
	}
	if strings.TrimSpace(p.ObjectivePath) == "" {
		errs = append(errs, errors.New(`"objective:" is missing: name the objective file this plan was made from`))
	}
	if strings.TrimSpace(p.ObjectiveHash) == "" {
		errs = append(errs, errors.New(`"objective_hash:" is missing: the hash of the objective this plan was made from`))
	}
	if p.Version < 1 {
		errs = append(errs, fmt.Errorf(`"plan_version:" is %d: it must be 1 or more`, p.Version))
	}
	switch {
	case len(p.Tasks) == 0:
		errs = append(errs, errors.New(`"## Tasks" has no tasks: add at least one "### t1: <title>" task`))
	case len(p.Tasks) > maxTasks:
		errs = append(errs, fmt.Errorf("there are %d tasks; the limit is %d: merge some", len(p.Tasks), maxTasks))
	}

	ids := map[string]bool{}
	for i, t := range p.Tasks {
		if _, ok := taskNumber(t.ID); !ok {
			errs = append(errs, fmt.Errorf("task %d (%q): ID %q is not t<number> (t1, t2, …)", i+1, t.Title, t.ID))
		} else if ids[t.ID] {
			errs = append(errs, fmt.Errorf("task ID %s is used by more than one task: give each task its own ID", t.ID))
		}
		ids[t.ID] = true
		if strings.TrimSpace(t.Title) == "" {
			errs = append(errs, fmt.Errorf("task %s has no title: write one after \"### %s: \"", t.ID, t.ID))
		}
		if t.Effects != "" && t.Effects != EffectsExternal {
			errs = append(errs, fmt.Errorf("task %s: effects %q is not %q, the only value (leave the line out otherwise)",
				t.ID, t.Effects, EffectsExternal))
		}
	}
	for _, t := range p.Tasks {
		for _, d := range t.Depends {
			switch {
			case d == t.ID:
				errs = append(errs, fmt.Errorf("task %s depends on itself: remove %s from its depends line", t.ID, d))
			case !ids[d]:
				errs = append(errs, fmt.Errorf("task %s depends on %q, which is not a task in this file", t.ID, d))
			}
		}
	}
	if cycle := findCycle(p.Tasks); cycle != nil {
		errs = append(errs, fmt.Errorf("tasks depend on each other in a loop (%s): remove one of these dependencies",
			strings.Join(cycle, " → ")))
	}
	return errors.Join(errs...)
}

// AssignTaskIDs gives every task without an ID the next free number, in file order, and returns the indexes of the
// tasks it changed.
//
// Notes:
//   - The next number is the larger of nextID and the highest existing ID + 1, so a removed task's ID is never reused:
//     nextID comes from the last state snapshot; 0 = none (Cleanup passes 0).
//   - Never renumbers: a task that has an ID keeps it, even with gaps or out of order (t1, t5, t2).
//   - Only "t<digits>" counts when finding the highest; other IDs (from tool arguments) are left for CheckComplete.
func AssignTaskIDs(tasks []Task, nextID int) []int {
	highest := max(nextID-1, 0)
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

// PathFor returns where the plan for the objective file objectivePath lives: next to it, ".md" replaced by ".plan.md".
//
// Notes:
//   - "dir/objective.md" -> "dir/objective.plan.md". The ".md" match ignores case ("X.MD" -> "X.plan.md").
//   - A name without ".md" keeps it whole: "notes.txt" -> "notes.txt.plan.md", "objective" -> "objective.plan.md".
func PathFor(objectivePath string) string {
	base := objectivePath
	if ext := filepath.Ext(base); strings.EqualFold(ext, ".md") {
		base = strings.TrimSuffix(base, ext)
	}
	return base + ".plan.md"
}
