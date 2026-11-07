# PLAN: plan mode (`-mode plan`)

Status: **shape agreed 2026-11-07, not built.** Decisions: `DECISIONS.md § 2026-11-07 — Plan mode before autonomous mode`.
Built **before** autonomous mode; autonomous mode then requires a planned objective file.

## Goal

The `terraform plan` / `terraform apply` split for the agent:

```
go run . -mode plan [-objective|-obj draft.md] [-out file.md]   # talk it through → objective-full.md (with ## Tasks)
go run . -mode autonomous -objective objective-full.md          # works the tasks; refuses a file without ## Tasks
```

Plan mode is interactive: a person reviews and approves every plan. It gets read-only tools only, so it raises
none of autonomous mode's "unattended means contained" questions.

## The objective file (the contract between plan and apply)

| Section | Output of plan mode | Notes |
|---|---|---|
| `# Objective` | required | what to achieve and why |
| `## Instructions` | required | how to work and how to judge "done" (the old `## Quality bar` and `## Done when`, merged) |
| `## Constraints` | optional | hard limits: language, libraries, files not to touch |
| `## Context` | optional | background the worker or director needs |
| `## Tasks` | required | plain top-level bullets, 1..`maxTasks`, worked in order |

- Input (a draft) needs only `# Objective`. Headings match case-insensitively.
- *Proposed, not decided:* when reading a draft, `## Quality bar` and `## Done…` are folded into Instructions, so the
  mvp4 examples still load; output only ever writes `## Instructions`.
- mvp4's parser passes unknown sections through to the director, so plan-mode output also runs with
  `mvp4/minagent -mode autonomous` for testing (`## Instructions` arrives as a free-text section).

## Flow

1. Draft given → parse it. No draft → ask "What do you want to build?".
2. Planner conversation (the chat loop, top-level model, its own system prompt). It asks a few clarifying questions
   at a time, may read `workDir` with `read_file` / `list_files` / `search_files`, and **proposes immediately, with
   no questions, when the draft is already clear**. A draft that already has `## Tasks` is reviewed and refined, not
   replaced.
3. The planner calls `propose_objective{objective, instructions[], constraints[], context, tasks[]}`. Go renders the
   markdown, re-parses it, checks it is complete, prints the whole file, and asks `Save to <path>? [y/N]`.
   - **N:** the tool result tells the model the user declined and to ask what to change; the turn ends; the user
     types feedback at the prompt.
   - **y:** choose the path (below), write it, return `tools.ErrEndTurn`; the session ends, exit 0.
4. Ctrl+D or `/quit` before saving: nothing is written.

### Output path

- Default: `<draft-name>-full.md` next to the draft; `objective-full.md` in the current directory without a draft.
  `-out` overrides. (Location is proposed, not decided.)
- **Never overwrite.** If the file exists, ask for a new name with a suggested default
  `<name>-full-<YYYYMMDD-HHMMSS>.md` (Enter accepts it); ask again if that one exists too. The write uses
  `O_CREATE|O_EXCL`, so a file that appears between the check and the write is still not overwritten.
- The model never chooses or writes the path: Go does, so no write tool and no new `Workspace` rule is needed.

## Code shape

| Where | What |
|---|---|
| `objective/` (new capability package; imports nothing from this module) | `Objective{Text, Instructions, Constraints, Context, Tasks, Other}`, `Section`; `Parse(data, maxTasks)`; `(Objective) Markdown()`; `(Objective) CheckComplete(maxTasks)` (Objective + Instructions + 1..maxTasks tasks). Shared with autonomous mode. |
| `planner/` (new) | `Instructions` (interview rules; what a good task is: one checkable outcome, names files, ordered); `SystemPrompt(draft, workerTools)`; `proposeObjectiveTool` (given an asker, the output path chooser and `maxTasks`) |
| `tools/tool.go` | `ErrEndTurn` |
| `agent/react.go` | `reActLoop` stops after a tool returns `ErrEndTurn` (autonomous mode needs it too) |
| `agent/planmode.go` (new) | `setupPlanSession` (read-only built-ins from the same `Workspace` + `propose_objective` + planner prompt); `runPlanMode`, reusing `chatLoop` |
| `agent/chat.go` | `chatLoop` gets one way to stop besides `/quit` (e.g. a `done func() bool` checked after each turn) |
| `config/` | `ModePlan`; `ObjectiveFile` (shared with autonomous); `OutFile`; flags `-objective` + alias `-obj` (one pointer, `nil` when unset), `-out`; `ValidateMode`: plan mode needs a terminal, `-out` only in plan mode; `doc.go` lines |
| `trace/` | `objective_saved` (path, task count, SHA-256 of the file) |

## Build order

1. `objective/`: parse, render, round-trip test over `mvp4/examples/objective-*.md`.
2. `-mode plan` wiring and flags; a stub that prints the parsed draft or asks for the objective.
3. `ErrEndTurn` in `reActLoop`.
4. `propose_objective` + output-path chooser, tested with a fake asker (yes / no / invalid / file exists).
5. Planner prompt, session setup, `chatLoop` stop check. Smoke test: plan here, apply with mvp4's binary.
6. Autonomous mode (`SESSION.md`'s earlier shape), minus `runPlan` / `set_tasks` / the PLAN phase.

## Deferred

- A one-command path (Terraform's `apply` without a saved plan: plan, show, confirm, run) and an auto-approve flag.
- Detecting a stale plan (the workdir changed since planning); the `objective_saved` hash is the hook for it.
- Task IDs and `depends_on` in the file (for parallel tasks).
