package planner

import (
	"strings"

	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
)

// PlanPrompt is the planner run's system prompt: turn a saved objective into tasks with propose_plan.
const PlanPrompt = `You are the planner for an autonomous agent run. You get an objective and turn it into a plan: a 
list of tasks that a director will hand out to worker agents, one task per worker.

Write tasks that:
- each produce something a worker can finish and check on its own, in the order the work naturally goes;
- say what they need in "needs" as capabilities ("read and write files in the working directory", "search the web"),
  never as tool names, since the tools available may change;
- say in "done_when" how anyone can tell the task is done (a command that passes, a file that exists with some content);
- list in "depends" the IDs of the tasks whose results they use;
- set "effects" to "external" only when the task acts outside the working directory (sends, submits, buys, posts);
- put any details a worker needs in "body".
Number the tasks t1, t2, t3, ... in order. Prefer a few solid tasks over many small ones.

You can look at the working directory with your tools before planning. Don't ask the user questions: when you have
to guess, write the guess in "assumptions". When the plan is ready, call propose_plan once with all of it. If it
reports problems, fix them and call propose_plan again.`

// PlanMessage returns the planner run's first message: where the objective is saved, and the objective itself.
//
// Notes:
//   - The objective is given in the standard form (o.Markdown()), the text its hash is taken from.
func PlanMessage(objectivePath string, o objective.Objective) string {
	return "Make a plan for the objective saved in " + objectivePath + ":\n\n" + o.Markdown() + "\nCall propose_plan with the plan."
}

// RevisionNote returns the user message that reopens the objective chat after the user turned plan p down: the plan,
// what the user wants changed, and where changes go.
//
// Notes:
//   - One message, not two: the plan and the change request in separate user messages would be two user turns in a row,
//     which some providers (Anthropic) refuse.
//   - It shows the plan as its file would read and tells the model that changes go into the objective (with
//     update_objective), not into the plan: the plan is made again from the saved objective.
func RevisionNote(p plan.Plan, change string) string {
	return "I did not accept this plan, made from the objective:\n\n" + strings.TrimSuffix(p.Markdown(), "\n") +
		"\n\nWhat I want to change: " + change +
		"\n\nChanges go into the objective with update_objective, not into the plan: after /save the plan is made again " +
		"from the objective."
}
