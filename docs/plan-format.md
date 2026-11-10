# The plan file

A plan file holds the formal tasks that meet an [objective](objective-format.md). The planner LLM proposes them, Go
checks them and writes the file, and autonomous mode runs it. You normally don't write one; you may read it, commit it,
and tweak it by hand (small mistakes are fixed for you and reported).

> **Status:** the reader and writer (package `plan/`) and plan mode, which makes a first plan, are built; updating a
> plan and running it (autonomous mode) are not yet. Design: [`PLAN-plan-apply.md`](../PLAN-plan-apply.md).

## How a plan is made

```sh
minagent -mode plan                          # write a new objective in a chat, /save it, then plan it
minagent -mode plan -objective objective.md  # plan an objective you have (or pass its text instead of a file)
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

**y** writes `<objective>.plan.md` next to the objective. **n** offers to revise: you say what should change, the
change goes into the objective (in the same chat), and after `/save` the plan is made again. `-yes` accepts without
asking. Running plan mode again on an objective whose plan is up to date just shows the summary; after you edit the
objective, the plan is stale (updating it is not built yet: delete the plan file to plan from scratch).

Example: [`examples/objective-example.plan.md`](../examples/objective-example.plan.md), made from
[`examples/objective-example.md`](../examples/objective-example.md). A plan sits next to its objective, named
`<objective>.plan.md`.

## The shape

```markdown
---
format: minagent-plan/v1
objective: objective.md
objective_hash: sha256:3f2a…
plan_version: 4
---

# Plan: Write a review paper on X, Y and Z
An optional summary.

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
| `objective` | the objective file this plan was made from, relative to the plan |
| `objective_hash` | the objective's fingerprint when the plan was made; if the objective's fingerprint changes, the plan is stale and is redone |
| `plan_version` | 1 for the first plan, +1 for each update |

An unknown key is reported and ignored.

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

**Ready to run** (checked before a plan is saved or run; every problem reported at once): the header names the
objective and its hash and has a version; at least one task and no more than the limit; every task has an ID `tN`
(unique) and a title; `effects` is `external` if present; every `depends` entry names another task in the file, and
there are no loops.
