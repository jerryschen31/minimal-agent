# The plan file

A plan file holds the formal tasks that meet an [objective](objective-format.md), and a word-for-word copy of that
objective. The planner LLM proposes the tasks, Go checks them and writes the file, and auto mode runs it. **The plan is
auto mode's whole input** (like Terraform's saved plan): a run never reads the objective file. You may read a plan,
commit it, tweak it by hand (small mistakes are fixed for you and reported), or write one from scratch, under any name.

> **Status:** the reader and writer (package `plan/`) and plan mode, which makes a first plan, are built; auto mode
> reads and checks a plan (`-mode auto -plan <file>`) but doesn't run tasks yet; updating a plan is not built. Design:
> [`PLAN-auto-mode.md`](../PLAN-auto-mode.md).

## How a plan is made

```sh
minagent -mode plan                          # write a new objective in a chat, /save it, then plan it
minagent -mode plan -objective objective.md  # plan an objective you have (or pass its text instead of a file)
minagent -mode plan -objective goal.md -plan runs/setup.md   # write the plan to a file you name
minagent -mode auto -plan runs/setup.md      # run a plan (any file name)
```

The planner reads the saved objective (and may look at the working directory), then proposes the whole plan. Go checks
it (the checks under "Ready to run" below, and that it reads back the same once written), sends any problems back to
the planner, and shows you a summary:

```
Plan 1 (wordfreq, a word-frequency CLI in Go): 4 to add
  + t1  Create the module and a sample input
  + t2  Count words  (after t1)
  ...
Accept this plan? [Y/n]:
```

**y** writes the plan: to `-plan <file>` if given, else `<objective>.plan.md` next to the objective. A `-plan` file
that exists and isn't a plan (your notes, say) is replaced only if you answer yes to `Replace it? [y/N]`. **n** offers to revise: you say what should change, the
change goes into the objective (in the same chat), and after `/save` the plan is made again. `-yes` accepts without
asking. Running plan mode again on an objective whose plan is up to date just shows the summary; after you edit the
objective, the plan is stale (updating it is not built yet: delete the plan file to plan from scratch).

Example: [`examples/objective-example.plan.md`](../examples/objective-example.plan.md), made from
[`examples/objective-example.md`](../examples/objective-example.md). By default a plan sits next to its objective, named
`<objective>.plan.md`.

## The shape

```markdown
---
format: minagent-plan/v1
plan_id: 7f3a9c1e5b2d8046
objective: objective.md
objective_hash: sha256:3f2a…
plan_version: 4
---

# Plan: Write a review paper on X, Y and Z
An optional summary.

## Objective
### Objective
Write a review paper on X, Y and Z.
### Instructions
- Peer-reviewed sources from 2015 on.
### Context
The audience is grad students.

## Assumptions
- What the planner had to guess because the objective didn't say.

## Tasks

### t1: Survey the literature on X
needs: academic search (a paper-search or web-search tool), PDF reading
done when: notes/x.md lists ≥15 sources, each with a one-line finding

What to do, in as many lines as it takes.

### t9: Submit to the preprint server
depends: t8
effects: external
gate: ask before running (publishes under your name; can't be undone)
needs: a browser or the server's API, logged in as you
done when: the submission ID is in submission.md

What to do.
```

### Header

| Key | Meaning |
|---|---|
| `format` | `minagent-plan/v1`; a file without it is read as v1; an unknown value is an error |
| `plan_id` | the plan's identity: written once by plan mode, kept by updates. Auto mode finds the plan's earlier runs by it, so moving or renaming the file keeps its history. One word (no spaces). A plan without one is known by its path |
| `objective` | the objective file this plan was made from, relative to the plan |
| `objective_hash` | the objective's fingerprint when the plan was made; if the objective's fingerprint changes, plan mode treats the plan as stale |
| `plan_version` | 1 for the first plan, +1 for each update |

All optional. `objective`, `objective_hash` and `plan_version` record where the plan came from, for plan mode; auto
mode ignores them and uses only `plan_id`, and a hand-written plan may
leave the whole header out. An unknown key is reported and ignored.

### Objective

`## Objective` holds the objective the plan was made from, copied word for word by Go (not retold by the model), with
its headings two levels down (`# Instructions` → `### Instructions`). Every worker in a run sees it, so your
instructions and context reach the run without the objective file. Editing this copy by hand is fine: what runs is the
plan. Optional in a hand-written plan. Inside it, headings of level 3 or deeper belong to the copy; `## Objective` itself
is recognized only at level 1 or 2.

### Tasks

Each task is a `### tN: title` heading under `## Tasks`, then optional field lines straight under it, in this order,
then a blank line and the body:

| Field | Meaning |
|---|---|
| `depends: t1, t2` | tasks that must be done first |
| `reason:` | why this plan version added or changed the task (only on those) |
| `effects: external` | the task acts outside the working directory (a trade, an email, a submission): it can't be checked for drift, is never re-run automatically, and each action gets a key so a resumed run can't repeat it |
| `gate:` | why the task must ask you before it runs |
| `needs:` | the capabilities it needs, in plain words: tasks don't name tools |
| `done when:` | how to tell the task is done |

- **IDs are never renumbered or reused.** A task without an ID gets the next free number.
- A task's **definition** is everything except `reason`: changing it marks the task changed for the next run.

## Writing or tweaking one by hand

**Fixed for you** (each fix reported with its line number): a known heading at the wrong level or spelled loosely
(`### Tasks`, `## tasks:`, `#Plan: x`); an unknown level-1 heading (`# Notes` → `## Notes`); `-    item` → `- item`; a
line straight after a bullet, not indented; task headings in other spellings (`## t2: x`, `### T2 - x`, `### Task 2: x`);
bullet tasks under `## Tasks` (become `### tN:` tasks); a task without an ID; field lines in another case, with `-` or
`_` for spaces, or in another order (`Done-When:`, `Depends On: task1`); a code block left open.

**Errors:** an empty file, larger than 1 MB, or not UTF-8; bad or unclosed front matter, an unknown format; the same
section twice; two tasks with the same ID; a task heading outside `## Tasks`; text before the first heading.

**Ready to run** (checked before a plan is saved or run; every problem reported at once): at least one task and no more than the limit; every task has an ID `tN`
(unique) and a title; `effects` is `external` if present; every `depends` entry names another task in the file, and
there are no loops.
