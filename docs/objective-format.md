# The objective file

An objective file says what you want an autonomous run to achieve, in your own words. You write it (by hand, or with
an LLM's help in plan mode); the planner turns it into formal tasks in a separate [plan file](plan-format.md); autonomous
mode runs that plan. When you change the objective, the plan is redone and only what changed is re-run.

> **Status:** the reader (package `objective/`) and the plan file (package `plan/`) are built. Plan mode and
> autonomous mode are not built yet. The whole design: [`PLAN-plan-apply.md`](../PLAN-plan-apply.md).

A complete example: [`examples/objective-example.md`](../examples/objective-example.md), and the plan made from it:
[`examples/objective-example.plan.md`](../examples/objective-example.plan.md).

## The shape

```markdown
# Objective
What to achieve, and why.

# Instructions
How to work, rules and limits, how to tell it's done. Rough steps or an informal task list are fine here.

# Context
Background the agents need.
```

| Section | Required? | What goes in it |
|---|---|---|
| `# Objective` | **yes** | what to achieve and why |
| `# Instructions` | no | how to work: rules, constraints, "done when", rough steps |
| `# Context` | no | facts and background |
| any other section | no | added to Instructions, heading kept (Constraints, Notes, Tasks, …) |

That's all. There is no front matter and no task syntax: formal tasks (IDs, dependencies, "done when") belong to the
plan file, which the planner writes. A one-line file is a valid objective. If you leave out Instructions, the planner
writes down what it had to assume, and you see it before anything runs.

## How it is read

- **Headings** are found at any level (`#`, `##`, `###`, …), in any case, singular or plural, with or without a
  trailing `:`, and even without a space after the `#` (`#Objective`). `## Instruction:` is Instructions.
- **A file with no headings** is all objective. Text before the first heading is the objective too, unless the file
  also has an `# Objective` section (then it is an error: move it under a heading).
- **Other headings.** The level that counts is that of the deepest of the three known headings in the file. Another
  heading at that level or above starts its own section, which is added to Instructions (after Instructions' own text,
  heading kept). A deeper one is a subheading and stays in the section it is in.
- **Code blocks** (` ``` ` or `~~~`) are left alone: a heading inside one is just text.
- **Nothing is rewritten.** Your file is never changed by the reader. Plan mode changes it only when you ask for a
  change in its planning session, and only after you have seen and approved the diff.

For example, this file:

```markdown
## Objective
Build a tiny Go program that prints the first 10 Fibonacci numbers.

## Constraints
- Standard library only.

## Instructions
- go test ./... passes and go run . prints them one per line.

## Tasks
- Write fib.go
- Write fib_test.go
```

is read as:

```markdown
# Objective
Build a tiny Go program that prints the first 10 Fibonacci numbers.

# Instructions
- go test ./... passes and go run . prints them one per line.

## Constraints
- Standard library only.

## Tasks
- Write fib.go
- Write fib_test.go
```

The planner then turns the informal tasks into formal ones in the plan file, keeping their order.

**Errors:** an empty file, larger than 1 MB, or not UTF-8; the same section twice (`## Instruction` and
`### Instructions` count as the same); text before the first heading in a file that has `# Objective`; an empty
objective.

## Changes and the plan

The plan file records a fingerprint (SHA-256) of the objective it was made from. The fingerprint is taken over the
objective as read above, so changing heading levels or spelling, or blank lines around a section, is not a change; a
change to the words is. A changed objective makes the plan stale, and it is redone before the next run.
