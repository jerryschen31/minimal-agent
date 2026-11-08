# The objective file format (v1)

An objective file describes a piece of work for the agent: what to achieve, how to work, and the list of tasks. It is
the contract between the two halves of a run, like a saved plan in `terraform plan` / `terraform apply`:

- **plan mode** (`-mode plan`) talks the objective through with you and writes a complete objective file;
- **autonomous mode** (`-mode autonomous`) reads that file and works through its tasks without you.

You can also write or edit one by hand. Small mistakes are fixed for you, and each fix is reported (see
[Writing one by hand](#writing-one-by-hand)).

> **Status:** the format and its reader and writer (package `objective/`) are built. Plan mode and autonomous mode are
> not built yet.

A complete example: [`examples/objective-example.md`](../examples/objective-example.md).

## The shape

```markdown
---
format: minagent-objective/v1
---

# Objective
What to achieve, and why.

## Instructions
How to work, and how to tell that the objective is done.

## Constraints
Hard limits: languages, libraries, files not to touch.

## Context
Background the agent needs.

## Tasks
Optional notes about the tasks as a whole.

### t1: First task

What to do, in as many lines as it takes.

### t2: Second task
depends: t1

What to do.
```

| Section | Needed to run? | What goes in it |
|---|---|---|
| `# Objective` | yes | what to achieve and why |
| `## Instructions` | yes | how to work and how to judge "done" (there is no separate "Done when" section) |
| `## Constraints` | no | hard limits |
| `## Context` | no | background |
| `## Tasks` | yes | one or more `### tN: title` tasks |
| any other `## Heading` | no | kept as written, in file order, and passed along |

A draft only needs an objective. A file is **ready to run** only when it has an objective, instructions and at least
one task (see [Ready to run](#ready-to-run)).

## Tasks

Each task is a `###` heading under `## Tasks`:

```markdown
### t3: Pick the top ten
depends: t2

The task's body: as many lines, lists and code blocks as it needs.
```

- **The ID** is `t` and a number: `t1`, `t2`, ... It names the task in `depends` lines, progress reports and traces.
- **IDs are never renumbered.** A task you add later can be `t7` even if it sits between `t2` and `t3`. A task without
  an ID gets the next number after the highest one in the file.
- **Order:** tasks run in file order, except that a task waits for the tasks it depends on.
- **`depends:`** is optional and must be the first line under the heading. It lists IDs separated by commas:
  `depends: t1, t2`. A task can't depend on itself, and dependencies can't form a loop.
- **The body** is everything after that, kept as you wrote it.
- Every task must be under `## Tasks`. A task heading anywhere else is an error.

## The header (front matter)

The block between the two `---` lines at the top is for programs, not people:

| Key | Meaning |
|---|---|
| `format` | the format version, `minagent-objective/v1`. Optional: a file without it is read as v1. |
| `approved` | written by plan mode when you approve the file: a fingerprint of its content. Don't write it by hand. |

- Leave the header out of a hand-written file. Plan mode always writes one.
- An unknown key is reported and ignored, so a file written by a newer version still runs.
- An unknown `format` value is an error. The version changes only when the shape of the file changes, for example a
  new required section.

## Writing one by hand

The reader accepts slightly wrong files and tells you what it fixed. When a file is saved by plan mode, it is
written in the standard form. Autonomous mode never rewrites your file: it fixes what it reads, reports each fix as a
warning, and runs. Run `-mode plan` on the file to save a tidied copy.

**Fixed for you** (each one reported with its line number):

| You wrote | Read as |
|---|---|
| `## Objective`, `#Objective` | `# Objective` |
| `### Constraints`, `  ## Context` | `## Constraints`, `## Context` (any heading level, up to three leading spaces) |
| `## Instruction`, `## tasks`, `## Tasks:` | `## Instructions`, `## Tasks`, `## Tasks` |
| `## Notes ##` | `## Notes` |
| `-    item` | `- item` |
| a line straight after a bullet, not indented | indented under that bullet (Markdown already reads it that way) |
| `## t2: x`, `### T2 - x`, `### Task 2: x` | `### t2: x` |
| `- Write the tests` (bullets under `## Tasks`, before any `###` task) | `### t5: Write the tests` (an ID is added) |
| `### Write the tests` (no ID) | `### t5: Write the tests` |
| `Depends: T1,t2`, `depends on: task1, 3` | `depends: t1, t2`, `depends: t1, t3` |
| a code block left open at the end of a section | closed with a ` ``` ` line |
| an unknown header key | ignored |
| a file with no headings at all | its text becomes the objective |

Also accepted without a warning: Windows line endings and an invisible byte-order mark at the start of the file.

For example, this hand-written file:

```markdown
#Objective
Build a tiny Go program that prints the first 10 Fibonacci numbers.

## Task
- Write fib.go and run it with go run .
- Write fib_test.go and run go test ./...

## Instruction
-   go test ./... passes and go run . prints 0 1 1 2 3 5 8 13 21 34,
one per line.
```

is read as (header left out):

```markdown
# Objective
Build a tiny Go program that prints the first 10 Fibonacci numbers.

## Instructions
- go test ./... passes and go run . prints 0 1 1 2 3 5 8 13 21 34,
  one per line.

## Tasks

### t1: Write fib.go and run it with go run .

### t2: Write fib_test.go and run go test ./...
```

with seven fixes reported, one for each changed line: `#Objective`, `## Task`, the two bullet tasks, `## Instruction`,
the bullet spacing, and the continuation line.

**Errors** (the file is refused, with the line number and what to change):

- the file is empty, larger than 1 MB, or not UTF-8 text;
- the same section twice (`## Instruction` and `### Instructions` count as the same);
- two tasks with the same ID;
- a task heading outside `## Tasks`;
- text before the first heading in a file that also has `# Objective` (move it under a heading);
- a header line that isn't `key: value`, a header with no closing `---`, or an unknown `format`.

**Left alone:** anything inside a code block (` ``` ` or `~~~`), `#hashtags`, and headings of level 3 or deeper whose
name isn't one of the sections above (they are subheadings of the section they're in). Inside `## Tasks`, use level 4
(`####`) for a subheading in a task body, since level 3 starts a new task.

## Ready to run

Before autonomous mode starts, and before plan mode saves a file, the file is checked. Every problem is reported at
once:

- `# Objective` and `## Instructions` are not empty;
- there is at least one task, and no more than the configured limit;
- every task has an ID of the form `tN`, unique, and a title;
- every `depends` entry names a task in the file, not the task itself, and there are no loops.

## Approval

When you approve a plan, plan mode writes `approved:` into the header: a fingerprint (SHA-256) of the content in its
standard form. When autonomous mode starts, it compares:

| State | Meaning | Autonomous mode |
|---|---|---|
| approved | saved by plan mode and unchanged since | runs |
| edited | saved by plan mode, changed since | warns, then runs |
| unplanned | no `approved:` line (written by hand) | warns, then runs |

Changes the reader would fix anyway (spacing, `## Instruction`) don't count as edits. The fingerprint catches edits
nobody re-planned. It is not a security signature: anyone who edits the file can also recompute it.
