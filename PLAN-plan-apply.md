# PLAN: objective → plan → run, iterated (autonomous mode v2)

Status: **shape agreed 2026-11-08; steps 1 (formats, except `state/`) and 2 (plan mode, first plan) built
2026-11-08/09.** Supersedes `PLAN-plan-mode.md` (whose steps 1–2, the `objective/`
package and the `-mode plan` stub, are built and get refactored here). Decisions: the `DECISIONS.md § 2026-11-08`
entries (redesign; plan mode confirms by default; full governance tabled; simplified plan/run shape agreed).

## Requirements (Jerry, 2026-11-08)

1. **As simple as possible for the user.** Every extra step is a reason not to use the harness.
2. **Tasks are tool-agnostic** but say plainly what capability they need, assuming a well-equipped harness.
3. **Objective files are easy to write**: by hand, with an LLM's help, or anything in between.
4. **Long-running, large, multi-step work**: research, a codebase, a review paper, a study, analysis then a trade,
   multi-agent workflows over databases.
5. **Iteration**: run, read the results, tweak the objective, run again; only what changed is re-run.

## What the files are (and aren't)

The plan and state are **a snapshot and journal of a run**: for provenance, logging, evals, resuming a stalled or
failed run, and continuing with more work. They are **not** a claim to own the environment: humans and other agents may
change things between runs, so state is checked against reality before it is trusted. (A fully governed harness, where
the plan is the only way to change anything, is tabled: `DECISIONS.md § A fully governed harness is tabled`.)

```
myproject/
  objective.md                          you (an LLM may help); the one file you pass around and edit
  objective.plan.md                     planner + Go; updated by diff when objective.md changes
  .minagent/objective/                  one directory per objective file
    lock                                held while a plan or run is in progress (pid, start time)
    runs/0003/state.json                snapshot of run 3, never changed after the run ends
    runs/0003/report.md                 what the run did
    runs/0003/trace.jsonl               the trace (keep out of git: it can hold secrets)
```

Committing `objective.md`, the plan and the reports is your choice; the files are deterministic text, so diffs are small.

## Who does what

| Actor | Does | Never does |
|---|---|---|
| **You** | Write or edit `objective.md`; run one command; read the plan summary and say yes; answer gates; read the report | Write formal tasks, IDs, `depends` |
| **Planner LLM** | Reads the objective (on an update also the previous objective, the previous plan and a state summary; plus the workdir, read-only); calls `propose_plan`; lists its assumptions | Writes files (Go does, after you approve) |
| **Go** | Assigns IDs (`next_id`, never reused); validates; checks the last state against reality and computes the diff; writes the plan; shows the summary; schedules by `depends`; writes the state snapshot; enforces budgets, gates and deny/ask rules | Judges the quality of work |
| **Director LLM** | Hands out ready tasks; judges results against `done when`; retries or fails; may add child tasks (`t3.1`) within a limit; writes the report | Changes the objective or the plan |
| **Worker LLM** | Does one task with the tools the harness has; reports outputs. A changed task's worker gets the old definition, the new one, the reason and the previous result | Sees the whole plan |

## `objective.md` (format v2)

```markdown
# Objective
Write a review paper on X, Y and Z.

# Instructions
- Peer-reviewed sources from 2015 on; at least 40 citations.
- First survey the field, then outline, then draft section by section.     ← informal tasks are fine
- Done when paper.md and refs.bib exist and every claim is cited.

# Context
The audience is grad students in ...
```

- Only `# Objective` is required; a file with no headings is all objective. `# Instructions` and `# Context` are
  optional.
- The three names are matched at any heading level, case-insensitively (singular/plural, trailing `:`). A heading with
  any other name is compared with the deepest known heading in the file: at that level or above it starts a new
  top-level section, which goes to Instructions with its heading kept; deeper, it stays in the section it is in. (So
  `# Objective` + `## Instructions` + `## Constraints` puts Constraints in Instructions.) Headings inside code blocks
  are ignored.
- No front matter. Go never rewrites the file on its own. Plan mode's objective chat keeps the draft in memory (the
  model changes it section by section with `update_objective`, you can change it with `/edit`) and writes the file
  only when you type `/save` (built in step 2; this replaced an earlier `edit_objective` idea of exact replacements).
- The change-detection hash is taken over the normalized text (sections in standard form), so a whitespace-only edit
  doesn't trigger a replan.

## `objective.plan.md` (format `minagent-plan/v1`)

```markdown
---
format: minagent-plan/v1
objective: objective.md
objective_hash: sha256:3f2a…
plan_version: 4
---

# Plan: Write a review paper on X, Y and Z

## Assumptions
- No length was given; aiming for 8,000–10,000 words.

## Tasks

### t1: Survey the literature on X
needs: academic search (a paper-search or web-search tool), PDF reading
done when: notes/x.md lists ≥15 sources, each with a one-line finding

Search for ...

### t11: Add a section on Z
depends: t1
reason: the objective now asks for Z
needs: academic search
done when: paper.md has a "Z" section citing ≥5 sources

...

### t9: Submit to the preprint server
depends: t8
effects: external
gate: ask before running (publishes under your name; can't be undone)
needs: a browser or the server's API, logged in as you
done when: the submission ID is in submission.md
```

- Field lines go straight under a task heading, in this order, each optional: `depends:`, `reason:` (only on a task
  added or changed by this plan version), `effects: external` (the task acts outside the workdir; see Run rules),
  `gate:`, `needs:` (free text), `done when:`. Then a blank line and the body.
- Reuses the built task code: `### tN:` headings, IDs never renumbered, `depends` and its checks, lenient `Cleanup`
  (a hand tweak is still read), `Markdown` with the zero-fix round trip.
- A task's **definition hash** covers every field and the body except `reason`.
- `objective_hash` differs from the objective's hash → the plan is **stale** and is redone before a run.

## `state.json` (one snapshot per run)

Written by Go during the run and frozen when it ends; the next run starts from the latest snapshot. Never holds
secrets.

- **Run:** number, start and end time, outcome; objective path + hash; plan path + hash + version; `next_id`.
- **Per task** (planned tasks and the director's children, `t3.1`): `kind` (`task`; `service` later), definition
  hash, status (`pending`, `running`, `done`, `failed`, `interrupted`, `removed`), attempts, the run that last ran it,
  **files it read and wrote, with hashes** (recorded by our file tools), summary, the director's verdict, and its
  external effects.
- **External effects** (tasks with `effects: external`): each action gets an **idempotency key** before it is taken and
  is recorded as `started`, then `done` with its reference (order ID, message ID). On resume, an effect left `started`
  is checked ("did key X go through?") before anything is retried, so a crash can't place a trade twice.
- **Environment (minimal):** harness version and build commit; provider, model and system-prompt hash per role; a hash
  of the tool list (names + definitions).

## Commands

`-objective` takes **a file or text**. A value naming an existing file is the file; one word ending in `.md` that
doesn't exist is an error (a typo, most likely); anything else is the objective's text, which is first saved as an
objective file (`objective.md`, or `objective-<YYYYMMDD-HHMMSS>.md` if that exists; never overwritten), because
iteration needs a file.

| Command | What it does |
|---|---|
| `minagent -mode autonomous -objective <file\|text>` | **The one command.** Plan if there is no plan or it is stale → summary → `Run? [Y/n/c]` → run. `c` opens the planning session, then returns to `Run?`. After a crash it resumes; after an edit it re-runs only what changed. |
| `minagent -mode plan -objective <file\|text>` | Plan if needed → summary → `Accept this plan? [Y/n]`; **n** → `Enter chat mode to discuss and revise the plan (Y/n)?` → the objective chat → `/save` → plan again. If the plan is up to date: summary only; if it is stale: an error until step 4. *(Built in step 2. Still to come: `Run now? [y/N]` after saving, step 3.)* |
| `minagent -mode plan` (no objective) | Asks for the new objective file's name (`default: objective.md`; an existing file asks `Overwrite (y/N)?`, **N** asks the name again), then `What is your objective? :`, then the objective chat; `/save` writes it and the plan is made as above. *(Built in step 2.)* |
| `/plan [text \| file.md]` in chat | The planning session inside chat; "run now" starts the run as a background child process. |
| `-yes`, `-y` | Answers the plan and run questions (for scripts; with `-objective` no terminal is needed). **Never answers a gate.** |
| `-replace t3` | Re-run t3 even if it looks done. |

### The planning session (as built in step 2)

Plan mode runs through `SetupAgent` and `RunAgent` like chat, with its own defaults (`GetDefaultConfig("plan")`: the
objective-writing system prompt and the read-only tools `read_file`, `list_files`, `search_files`); `-config` and
`-model` work as in chat, so a planning config can add MCP servers.

- **The objective chat.** The chat loop with plan mode's system prompt and one extra tool, `update_objective`: it
  changes the in-memory draft (an `objective.Objective`, held in `planModeDetails` on the session) one section at a time
  and never writes a file. After each change Go prints the draft and "Type /save to save it and start planning, /edit
  to change it yourself, or tell me what to change." `/save` writes the draft (and is the only thing that does);
  `/edit` fills the prompt with the draft for you to change in place (no model call; the model is told what changed);
  `/exit` or Ctrl+D leaves with nothing written.
- **The planner run.** One turn on a copy of the session: `PlanPrompt` as the system message, no chat history (the
  planner sees only the saved objective), the read-only and MCP tools plus `propose_plan`. Go checks the proposal
  (one-line fields; it must read back the same through `plan.Cleanup`; `CheckComplete`), lists every problem back to
  the model, and ends the turn (`tools.ErrEndTurn`) when it passes.
- **Accept.** The summary, then `Accept this plan? [Y/n]` (`-yes` answers it). **y** writes the plan (the old plan
  file is replaced only here). **n** → `Enter chat mode to discuss and revise the plan (Y/n)?`: **y** → "What would you
  like to change in the plan?", the turned-down plan is added to the chat, and the objective chat reopens; after
  `/save` the plan is made again. **n** prints the command to plan again.

**A change you ask for goes into `objective.md` first, then the plan is redone from it**, so the objective stays the
one place your intent lives, and the diff between objective versions drives the next plan. The planner writes what it
had to guess as Assumptions.

### `propose_plan`: a first plan vs an update

- **First plan:** a list of tasks (all adds).
- **Update** (a plan already exists): a list of **operations**: `keep tN`, `change tN {fields, reason}`,
  `add {fields, reason}`, `remove tN {reason}`. For `keep`, Go copies the old task exactly, so the planner can't reword
  an unchanged task into a needless re-run. Every existing task must be named once; Go refuses a proposal that leaves
  one out.

## The plan summary (every time, the first plan too)

```
Objective changed since plan 3:  # Instructions changed
Checked against the last run (run 3):  1 drifted

Plan 4:  2 to add, 1 to change, 1 to remove, 1 to re-run, 5 unchanged (done)
  + t11  Add a section on Z                (the objective now asks for Z)
  + t12  Check every citation resolves
  ~ t3   Survey sources                    (instructions now require 2018+ sources)
  ↻ t4   Outline                           (depends on changed t3)
  ~ t6   Build the bibliography            drifted: refs.bib changed since run 3
  - t7   Draft the appendix                (no longer asked for; its files stay: appendix.md)
  ? t9   Submit to the preprint server     external: done in run 3 (ID 2611.0042), not redone
Capability gaps:  t12 needs "a tool that resolves DOIs"; nothing loaded looks like one
Run? [Y/n/c]
```

A first plan shows every task as `+`. A large re-run says so ("re-runs 14 tasks").

## Run rules

- **Checked against reality first:** for each task the last state calls done, Go compares the hashes of the files it
  wrote with the files now. A difference marks it **drifted** (`~`), and the planner decides whether it re-runs. Tasks
  with external effects can't be checked: shown as `?`, never redone automatically.
- `+` new: a fresh worker. `~` changed: a worker that gets the old definition, the new one, the reason and the previous
  result.
- `↻` re-run: unchanged, but a task it depends on runs now, **or it read a file a re-run task writes** (from the
  recorded reads, which catch dependencies the planner didn't list). The planner may mark a dependent "unaffected", with
  a reason shown in the summary.
- `-` removed: marked removed; nothing is undone; its files are listed in the report.
- Unchanged and done: skipped. `failed` or `interrupted`: run again (the worker is told which files it may have left
  half-written).
- **External effects:** never re-run automatically (always the gate again); resume checks idempotency keys first.
- A **gate** pauses only the tasks that depend on it; independent tasks keep running.
- The director's child tasks (`t3.1`) live in state only; changing or removing `t3` covers its children.
- **Lock:** one plan or run per objective at a time; a second one stops with "a run is in progress (pid, started at)".
- Budgets for the whole run (time, tokens or cost, number of tasks) plus the per-task step limit.
- **Capability preflight:** the director compares each task's `needs` with the loaded tools and reports gaps (it warns,
  it doesn't block).

## What changes in the code already built

| Built | Becomes |
|---|---|
| `objective/` (sections, tasks, depends, IDs, cycles, Cleanup, Markdown, approval hash) | **`objective/`** shrinks to the v2 reader (three sections, lenient, no front matter, normalized hash). **`plan/`** (new) takes `Task`, `Fix`, `AssignTaskIDs`, the task checks, task parsing and rendering, front matter, the round trip and `FuzzCleanup`, plus the new fields. `Approved`, `BodyHash` and `ApprovalStatus` go. |
| Heading and code-fence helpers in `cleanup.go` | **`mdscan/`** (new helper package, standard library only): input checks and line splitting, `ReadHeading`, `IsCodeBlockFence`, `StripClosingHashes`. Both `objective/` and `plan/` use it. |
| — | **`state/`** (new): snapshot types, load, save, latest, run directories, lock, file hashes. |
| `docs/objective-format.md`, `examples/objective-example.md` | Rewritten for v2 (much shorter); add `docs/plan-format.md` and an example plan. |
| `-mode plan` stub, `-objective`/`-obj`, `-out` | `-objective` stays and accepts text (step 2). `-out` is dropped (step 2). The stub reads with `objective.Parse` (step 1). |

## Build order

1. **Formats.** `mdscan/`, `plan/` and `objective/` v2 **built 2026-11-08** (`DECISIONS.md § mdscan/ and plan/ built`,
   `§ objective/ v2 built`), with docs (`docs/objective-format.md`, `docs/plan-format.md`) and examples (an objective
   and the plan made from it); the stub reads with `objective.Parse`. Still to do: `state/` (types, load/save/latest,
   run dirs, lock, file hashes).
2. **First plan.** **Built 2026-11-09** (`DECISIONS.md § Step 2 built: plan mode`): `ErrEndTurn`; `propose_plan`
   (first-plan form) and `update_objective` in `agent/planner/`; per-mode defaults; plan mode through
   `SetupAgent`/`RunAgent`; the objective chat (`/save`, `/edit`); `Accept? [Y/n]` and revise; `-objective` file or
   text; `-yes`/`-y`; `-out` dropped. This took in most of step 5 (the session with no objective, revising after **n**).
3. **One complete run, serial.** Lock, summary, `Run?`, director and workers in `depends` order (mvp3 is the
   reference), file reads/writes recorded by the file tools, state snapshot, report. **Resume** from the last snapshot
   works here. External effects with idempotency keys.
4. **Iteration.** `propose_plan` operations; drift check; `~ ↻ - ?` in the summary; recorded-reads cascade; "what
   changed" for `~` workers; `-replace`.
5. **The planning session.** Mostly built in step 2. Left: `Run now?` (after step 3), and `/plan` in chat with a
   background run.
6. Gates (pausing only what depends on them), capability preflight, budgets.

## Deferred

- Long-lived services (`kind: service`).
- Parallel tasks (mvp4 is the reference).
- Runs on their own git branch or worktree; checkpoints inside a task.
- Governance (managed resources, drift policy, `adopt`, a hash-chained journal, policy as code): tabled.
