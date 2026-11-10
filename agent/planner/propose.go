package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
	"github.com/jerryschen31/minimal-agent/tools"
)

// ProposePlan is the propose_plan tool: it turns the model's tasks into a checked plan.Plan for one objective.
type ProposePlan struct {
	objectivePath string     // the objective file, as plan mode names it
	objectiveHash string     // objective.Hash() of the objective being planned
	maxTasks      int        // the most tasks a plan may have (config maxPlanTasks)
	Accepted      *plan.Plan // set by the first proposal that passes the checks; nil until then
}

// proposal is propose_plan's arguments.
type proposal struct {
	Title       string         `json:"title"`
	Summary     string         `json:"summary"`
	Assumptions string         `json:"assumptions"`
	Tasks       []proposedTask `json:"tasks"`
}

// proposedTask is one task in propose_plan's arguments.
type proposedTask struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Depends  []string `json:"depends"`
	Needs    string   `json:"needs"`
	DoneWhen string   `json:"done_when"`
	Effects  string   `json:"effects"`
	Body     string   `json:"body"`
}

// NewProposePlan returns the propose_plan tool for the objective o saved at objectivePath, accepting at most maxTasks
// tasks.
//
// Notes:
//   - The hash is taken now, so the plan records the objective it was made from even if the draft changes later.
//   - maxTasks comes from the config (maxPlanTasks); plan mode's setup checks it is positive.
func NewProposePlan(objectivePath string, o objective.Objective, maxTasks int) *ProposePlan {
	return &ProposePlan{objectivePath: objectivePath, objectiveHash: o.Hash(), maxTasks: maxTasks}
}

// GetToolDefinition returns propose_plan's name, description and parameters.
//
// Notes: none.
func (t *ProposePlan) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(
		"propose_plan",
		fmt.Sprintf("Proposes the whole plan for the objective. Go checks it: task IDs t1, t2, ... each used once, depends naming "+
			"other tasks with no loops, at most %d tasks, effects empty or \"external\", one-line fields. If there are problems they "+
			"are listed and you call it again with the whole plan fixed; if it passes, the user is shown the plan and decides.", t.maxTasks),
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "A short name for the plan"},
				"summary": {"type": "string", "description": "One or two sentences on the approach"},
				"assumptions": {"type": "string", "description": "What you had to guess, as a Markdown list; empty if nothing"},
				"tasks": {
					"type": "array",
					"description": "The tasks, in order",
					"items": {
						"type": "object",
						"properties": {
							"id": {"type": "string", "description": "t1, t2, t3, ... in order"},
							"title": {"type": "string", "description": "What the task does, in a few words"},
							"depends": {"type": "array", "items": {"type": "string"}, "description": "IDs of the tasks whose results this one uses"},
							"needs": {"type": "string", "description": "Capabilities it needs, not tool names (one line)"},
							"done_when": {"type": "string", "description": "How to tell it is done (one line)"},
							"effects": {"type": "string", "enum": ["", "external"], "description": "\"external\" if it acts outside the working directory"},
							"body": {"type": "string", "description": "Details for the worker, in Markdown; don't start lines with #"}
						},
						"required": ["id", "title"]
					}
				}
			},
			"required": ["title", "tasks"]
		}`),
	)
}

// CallTool checks the proposed plan; when it passes, it keeps it in Accepted and ends the turn.
//
// Notes:
//   - Bad JSON, a field that should be one line but isn't, a plan that doesn't read back as written (readBack) or one
//     that fails plan.CheckComplete returns an error listing the problems: the model sees it and calls again.
//   - On success it returns tools.ErrEndTurn, so the planner's turn ends with no further model call; plan mode shows
//     the plan and asks the user.
func (t *ProposePlan) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	var prop proposal
	if err := json.Unmarshal(args, &prop); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p, err := prop.toPlan(t.objectivePath, t.objectiveHash)
	if err != nil {
		return "", err
	}
	p, err = readBack(p)
	if err != nil {
		return "", err
	}
	if err := p.CheckComplete(t.maxTasks); err != nil {
		return "", fmt.Errorf("the plan has problems; fix them all and call propose_plan again:\n%w", err)
	}
	t.Accepted = &p
	return "The plan passed the checks. The user is shown it and decides whether to accept it.", tools.ErrEndTurn
}

// toPlan turns a proposal into a version-1 plan for the objective at objectivePath.
//
// Notes:
//   - The plan's "objective:" is the objective's file name only: the plan file sits next to the objective
//     (plan.PathFor), and the path is relative to the plan.
//   - Every text is trimmed. Title, depends, needs, done_when and effects must be one line each: a line break there
//     would change the plan file's structure, so it is an error naming the task.
//   - IDs and depends are put in standard form with plan.NormalizeTaskID ("T1", "task 1", "t01" -> "t1") and effects
//     is lower-cased, as plan.Cleanup does when the file is read back, so readBack doesn't mistake that for a broken
//     plan. An ID that isn't a task ID ("x1") is an error here: Cleanup would give that task a new ID, and readBack's
//     message would point at the wrong cause. A depends item that isn't one is kept for CheckComplete to report.
func (prop proposal) toPlan(objectivePath, objectiveHash string) (plan.Plan, error) {
	p := plan.Plan{
		Format:        plan.FormatV1,
		ObjectivePath: filepath.Base(objectivePath),
		ObjectiveHash: objectiveHash,
		Version:       1,
		Title:         strings.TrimSpace(prop.Title),
		Summary:       strings.TrimSpace(prop.Summary),
		Assumptions:   strings.TrimSpace(prop.Assumptions),
	}
	var errs []error
	if strings.ContainsAny(p.Title, "\r\n") {
		errs = append(errs, errors.New("title must be one line"))
	}
	for i, pt := range prop.Tasks {
		task := plan.Task{
			ID:       standardID(pt.ID),
			Title:    strings.TrimSpace(pt.Title),
			Needs:    strings.TrimSpace(pt.Needs),
			DoneWhen: strings.TrimSpace(pt.DoneWhen),
			Effects:  strings.ToLower(strings.TrimSpace(pt.Effects)),
			Body:     strings.TrimSpace(pt.Body),
		}
		for _, d := range pt.Depends {
			task.Depends = append(task.Depends, standardID(d))
		}
		name := task.ID
		if name == "" {
			name = fmt.Sprintf("task %d", i+1)
		}
		if _, ok := plan.NormalizeTaskID(task.ID); !ok {
			errs = append(errs, fmt.Errorf("task %d (%q): id %q is not t<number> (t1, t2, ...)", i+1, task.Title, task.ID))
		}
		for _, f := range []struct{ field, value string }{
			{"title", task.Title}, {"depends", strings.Join(task.Depends, ",")}, {"needs", task.Needs},
			{"done_when", task.DoneWhen}, {"effects", task.Effects},
		} {
			if strings.ContainsAny(f.value, "\r\n") {
				errs = append(errs, fmt.Errorf("%s: %s must be one line", name, f.field))
			}
		}
		p.Tasks = append(p.Tasks, task)
	}
	if len(errs) > 0 {
		return plan.Plan{}, fmt.Errorf("the plan has problems; fix them all and call propose_plan again:\n%w", errors.Join(errs...))
	}
	return p, nil
}

// standardID returns id trimmed and in standard form ("T1" -> "t1"), or just trimmed if it isn't a task ID.
//
// Notes: none.
func standardID(id string) string {
	id = strings.TrimSpace(id)
	if std, ok := plan.NormalizeTaskID(id); ok {
		return std
	}
	return id
}

// readBack writes p as its plan file would be and reads it back with plan.Cleanup, returning what was read.
//
// Notes:
//   - The plan is saved with Markdown and read with Cleanup, so a plan that doesn't survive that would change once
//     saved. Cleanup may tidy the text (bullet spacing, the indent of a line continuing a bullet), and the tidied plan
//     is what is returned and later saved.
//   - Anything more than tidying is an error: a body line that reads as a heading (an extra section, or tasks lost or
//     merged) or a task whose ID, title, depends, needs, done_when or effects came back different.
func readBack(p plan.Plan) (plan.Plan, error) {
	back, _, err := plan.Cleanup([]byte(p.Markdown()))
	if err != nil {
		return plan.Plan{}, fmt.Errorf("the plan can't be saved as written: %w; don't start lines with # in the summary, "+
			"assumptions or task bodies", err)
	}
	var problems []string
	if back.Title != p.Title {
		problems = append(problems, "the title changed")
	}
	for _, s := range back.Other {
		problems = append(problems, fmt.Sprintf("a line reads as a section heading (%q)", "## "+s.Heading))
	}
	if len(back.Tasks) != len(p.Tasks) {
		problems = append(problems, fmt.Sprintf("%d tasks were given but %d read back (a body line reads as a task heading?)",
			len(p.Tasks), len(back.Tasks)))
	} else {
		for i, a := range p.Tasks {
			b := back.Tasks[i]
			if a.ID != b.ID || a.Title != b.Title || !slices.Equal(a.Depends, b.Depends) || a.Needs != b.Needs ||
				a.DoneWhen != b.DoneWhen || a.Effects != b.Effects {
				problems = append(problems, fmt.Sprintf("task %s reads back differently (a body line that looks like a field "+
					"or heading?)", a.ID))
			}
		}
	}
	if len(problems) > 0 {
		return plan.Plan{}, fmt.Errorf("the plan doesn't read back as written: %s. Keep fields to one line and don't start "+
			"lines with # in the summary, assumptions or task bodies", strings.Join(problems, "; "))
	}
	return back, nil
}
