package planner

import (
	"strings"

	"github.com/jerryschen31/minimal-agent/objective"
	"github.com/jerryschen31/minimal-agent/plan"
)

// PlanPrompt is the planner run's system prompt: turn a saved objective into tasks with propose_plan.
// [agent] The body layout (Approach / Produces / Check) and the weak-vs-strong examples are there because, without
// them, models (gpt-4o-mini especially) wrote generic one-line tasks ("Create the HTML structure"). Two examples, one
// coding and one not, so the layout isn't read as code-only.
const PlanPrompt = `You are the planner for an autonomous agent run. You get an objective and turn it into a plan: a
list of tasks that a director will hand out to worker agents, one task per worker. A worker sees only its own task,
the objective and the results of the tasks it depends on, so each task must say concretely what to do and how.

Write tasks that:
- each produce something a worker can finish and check on its own, in the order the work naturally goes;
- have a specific title that names what gets made or decided ("Build index.html: start button, 3x3 board, status
  line"), not a generic step ("Create the HTML structure");
- say what they need in "needs" as capabilities ("read and write files in the working directory", "search the web"),
  never as tool names, since the tools available may change;
- say in "done_when" how anyone can tell the task is done (a command that passes, a file that exists with some content);
- list in "depends" the IDs of the tasks whose results they use;
- set "effects" to "external" only when the task acts outside the working directory (sends, submits, buys, posts);
- set "gate" to a short reason when a person should approve the task before it runs (anything with external effects,
  or hard to undo); leave it empty otherwise;
- have a "body" in this layout, with concrete names (files, functions, sources, people, numbers) taken from the
  objective or decided by you:
  **Approach:** numbered steps saying how the work is done, including the decisions a worker would otherwise have to guess.
  **Produces:** the files or results the task leaves behind, and where.
  **Check:** how the worker verifies its own result before finishing.
Don't make a task that only glues other tasks together (e.g. "link the script to the page"): fold it into the task
that needs it. Number the tasks t1, t2, t3, ... in order. Prefer a few solid tasks over many small ones, but every
task must be detailed.

Example of a weak task:
  title "Write JavaScript for game logic", body "Handle turns and check for a winner."
The same task done well:
  title "Write game.js: turns, win/draw detection, restart"
  body
  **Approach:**
  1. Keep the board as an array of 9 cells ("", "X" or "O") and the current player, starting with X.
  2. On a click on an empty cell, mark it, then check the 8 winning lines; else a draw when all cells are full.
  3. Update the status line: "Player X: your turn", "Player O wins! Play again?", or "Draw! Play again?".
  4. Play again clears the board and starts with X.
  **Produces:** game.js, loaded by index.html.
  **Check:** open index.html, play a win for each player and a draw, and confirm each status message.

A non-code task done well:
  title "Compare three hosting options on price and limits"
  body
  **Approach:**
  1. Find the current free and lowest paid plan of GitHub Pages, Netlify and Cloudflare Pages on their own pricing pages.
  2. For each, note the price, bandwidth and build limits, custom domain support, and the date checked.
  3. Recommend one for a static single-page site, in two sentences, with the main trade-off.
  **Produces:** hosting-comparison.md with a table (one row per option) and the recommendation.
  **Check:** every cell has a value and a source link.

You can look at the working directory with your tools before planning. Don't ask the user questions: when you have
to guess, write the guess in "assumptions". When the plan is ready, call propose_plan once with all of it; don't
write the plan in your reply (the user is shown it from the call). If it reports problems, fix them and call
propose_plan again.`

// ProposeReminder is the user message that reminds a planner that replied with text instead of calling propose_plan.
// [agent] Plan mode sends it at most a few times (runPlanner); small models often announce a tool call without making it.
const ProposeReminder = "You replied with text but did not call propose_plan, so there is no plan yet. Call propose_plan " +
	"now with the whole plan. Don't reply with text."

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
