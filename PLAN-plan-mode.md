# PLAN: plan mode (`-mode plan`)

Status: **shape agreed 2026-11-07, not built.** Decisions: `DECISIONS.md § 2026-11-07 — Plan mode before autonomous mode`,
`§ objective/ package`, `§ Objective file format v1: version, task sections, Cleanup`,
`§ Objective format v1: open points settled`,
`§ Objective format v1: review pass`.
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
| `## Instructions` | required | how to work and how to judge "done" (replaces mvp4's `## Quality bar`; no separate `## Done when`) |
| `## Constraints` | optional | hard limits: language, libraries, files not to touch |
| `## Context` | optional | background the worker or director needs |
| `## Tasks` | required | 1..`maxTasks` task subsections `### t1: <title>`, `### t2: …` (IDs never renumbered; a missing ID gets highest + 1), each with an optional `depends: t1, …` line and a body; worked in order, respecting `depends` |

- **Version** (decided 2026-11-07): YAML-style front matter `format: minagent-objective/v1`. Hand-written files may
  leave it out (missing = v1); `Markdown()` always writes it; an unknown version is an error listing the supported ones.
  A later v2 never reinterprets a file without a header. Hint when a header-less file seems to use newer syntax.
  Unknown front-matter keys are warned about and ignored; the version changes only when the shape of the file does.
- **Approval hash** (decided 2026-11-07): plan mode writes `approved: sha256:<hex>` (the body as approved, in the
  standard layout). Autonomous mode warns, and still runs, when the file was edited since (`edited`) or never went
  through plan mode (`unplanned`).
- **Cleanup** (decided 2026-11-07): `objective.Cleanup` reads a slightly malformed file, returns the `Objective` plus the
  list of fixes it made, and refuses only what it can't decide. `CheckComplete` runs only after Cleanup. The tidy text
  is `o.Markdown()`; a canonical file cleans up with zero fixes.
- Input (a draft) needs only `# Objective`. Headings match case-insensitively.
- No folding of legacy headings (decided 2026-11-07): `## Quality bar` and `## Done…` are ordinary unknown sections,
  kept in `Other`. The mvp4 examples are not a format to support.
- mvp4's parser passes unknown sections through to the director, so plan-mode output also runs with
  `mvp4/minagent -mode autonomous` for testing (`## Instructions` arrives as a free-text section).

## Flow

1. Draft given → `Cleanup` it (fixes are shown). No draft → ask "What do you want to build?".
2. Planner conversation (the chat loop, top-level model, its own system prompt). It asks a few clarifying questions
   at a time, may read `workDir` with `read_file` / `list_files` / `search_files`, and **proposes immediately, with
   no questions, when the draft is already clear**. A draft that already has `## Tasks` is reviewed and refined, not
   replaced.
3. The planner calls `propose_objective{objective, instructions, constraints, context, tasks[{title, body, depends}]}`.
   Go assigns task IDs, checks it is complete, renders the markdown, checks that `Cleanup` of it makes zero fixes, prints the whole file, and asks `Save to <path>? [y/N]`.
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
| `objective/` (new capability package; imports nothing from this module) | `objective.go`: `Objective{Format, ObjText, Instructions, Constraints, Context, TaskNotes, Tasks []Task, Other}`, `Task{ID, Title, Depends, Body}`, `Section`, `Fix`, `(Objective) CheckComplete(maxTasks)` (after Cleanup: required sections, 1..maxTasks tasks, depends name existing tasks, no cycles; every problem at once). `cleanup.go`: `Cleanup(data) (Objective, []Fix, error)` (replaces the syntax-only `Parse`). `render.go`: `(Objective) Markdown()`. Shared with autonomous mode. |
| `planner/` (new) | `Instructions` (interview rules; what a good task is: one checkable outcome, names files, ordered); `SystemPrompt(draft, workerTools)`; `proposeObjectiveTool` (given an asker, the output path chooser and `maxTasks`) |
| `tools/tool.go` | `ErrEndTurn` |
| `agent/react.go` | `reActLoop` stops after a tool returns `ErrEndTurn` (autonomous mode needs it too) |
| `agent/planmode.go` (new) | `setupPlanSession` (read-only built-ins from the same `Workspace` + `propose_objective` + planner prompt); `runPlanMode`, reusing `chatLoop` |
| `agent/chat.go` | `chatLoop` gets one way to stop besides `/quit` (e.g. a `done func() bool` checked after each turn) |
| `config/` | `ModePlan`; `ObjectiveFile` (shared with autonomous); `OutFile`; flags `-objective` + alias `-obj` (one pointer, `nil` when unset), `-out`; `ValidateMode`: plan mode needs a terminal, `-out` only in plan mode; `doc.go` lines |
| `trace/` | `objective_saved` (path, task count, SHA-256 of the file); later autonomous mode: `objective_loaded` (path, hash, fixes, sections) |

## Build order

1. `objective/`: Cleanup, render, CheckComplete, BodyHash/ApprovalStatus; tests: each fix, round trip
   (`Cleanup(o.Markdown())` = o with zero fixes) over `objective/testdata/`, `FuzzCleanup`. Then the user-facing format
   reference and an example file (`docs/objective-format.md`, `examples/objective-example.md`). **Done 2026-11-07**;
   `Test_Unit_ExampleFile` and `Test_Unit_DocHandWrittenExample` keep both in step with the code.
2. `-mode plan` wiring and flags; a stub that prints the parsed draft or asks for the objective.
3. `ErrEndTurn` in `reActLoop`.
4. `propose_objective` + output-path chooser, tested with a fake asker (yes / no / invalid / file exists).
5. Planner prompt, session setup, `chatLoop` stop check. Smoke test: plan here, apply with mvp4's binary.
6. Autonomous mode (`SESSION.md`'s earlier shape), minus `runPlan` / `set_tasks` / the PLAN phase. It runs `Cleanup` +
   `CheckComplete` on the given file, writes no file, warns when fixes were made and when `ApprovalStatus` isn't
   `approved`, and traces the sections, fixes and approval status.

## Deferred

- A one-command path (Terraform's `apply` without a saved plan: plan, show, confirm, run) and an auto-approve flag.
- Detecting a stale plan (the workdir changed since planning); the `objective_saved` hash is the hook for it.
- Later-step items from the 2026-11-07 review (see `SESSION.md`): depends-aware scheduling and a `blocked` status;
  which worker tools the planner sees; a no-LLM save when the draft is complete; a diff against the draft; capping
  printed fixes; `x-full.md` input naming; using the same `workDir` for plan and apply.
- Running tasks in parallel (the file already carries `depends`; serial runs follow file order where dependencies allow).
