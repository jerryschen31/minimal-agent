# Auto mode

Auto mode runs a plan's tasks without you. You write an objective, plan mode turns it into a plan file you review, and
auto mode carries the plan out, one task at a time, writing down everything it does so a crash can be resumed.

```sh
minagent -mode plan -objective goal.md        # make goal.plan.md; "Accept this plan?", then "Run now? [y/N]"
minagent -mode auto -plan goal.plan.md        # run it ("-mode autonomous" is the same); "Run? [Y/n]" first
minagent -mode plan -objective goal.md -y     # plan, accept and run in one go (no questions)
```

## The plan is the only input

Auto mode reads only the plan file, under any name. The plan holds everything: the objective (its `## Objective`
section, copied in by plan mode), the assumptions and the tasks. Edit it by hand if you like; auto mode reads what is
there and tells you about any small slips it fixed while reading (the file itself isn't rewritten). A plan that can't run
(no tasks, a `depends` on a task that doesn't exist, a loop) is refused with what to fix. See
[plan file format](plan-format.md).

## What a run does

1. Shows the plan's summary and asks `Run? [Y/n]` (`-y` answers yes).
2. Takes the plan's lock (one run of a plan at a time) and reads its history: which tasks earlier runs finished.
3. Runs the tasks in `depends` order. A task that is done and unchanged since is skipped; a new, changed, failed or
   interrupted one runs.
4. For each task, a **worker** (the agent, starting from a fresh conversation) does the task. It is told the plan's
   objective, the task, and what the tasks it depends on reported. Then a **director** (a second conversation with the
   model) checks the result against the task's `done when`: it reads files and may run the commands you allowed, then
   says `task_done` or `task_failed`. Not done: a new worker tries again with the director's advice, up to
   `maxTaskAttempts` (default 3) times.
5. A task that fails every attempt stops the run; the tasks after it are not started.
6. Ends with a report: done / skipped / failed / not started, the run folder and the trace file.

```
Run 0001 (~/.minagent/runs/0001)
▶ t1: Write wc.go
    [tool] write_file
    director: [tool] read_file
✓ t1 done — wc.go counts words; checked with the sample file
▶ t2: Add tests
...
Finished: 3 done, 0 skipped.
```

## Nobody is asked during a run

Tool calls follow your rules (`denyRules`, `askRules`, see `config/doc.go`), but there is nobody to ask, so:

| A tool call that matches | In auto mode |
|---|---|
| `denyRules` | refused |
| `askRules` | refused (with `"approval": "allow-all"`: runs) |
| neither | runs |

Auto mode's default `askRules` are `*delete*` and `*remove*`, so the file tools can write (always inside `workDir`).
`run_command` is off unless you list programs in `runCommands`; then any command line that matches no
`run_command:...` rule runs.

The only questions auto mode asks come **before** work starts:
- **Gates.** A task with a `gate:` line asks `Run it? [y/N]` before it starts. `-y` never answers a gate. No answer
  possible (no terminal) stops the run with `waiting`; no stops it with `declined`. Run it again on a terminal: done
  tasks are skipped and the gate is asked again.
- **After a crash** (below).

## Crashes and resuming

Every tool call is written down before it runs and again when it returns, in the run's journal. If minagent dies in
the middle (a crash, a kill, a power cut), run the same command again. Before any task runs, it settles each call the
crash cut off:

| The cut-off call | What happens |
|---|---|
| a read (or anything that changes nothing) | nothing |
| a file write | the file is compared with how it was before the call: unchanged means it didn't happen; leftover temporary files are removed |
| an MCP tool that says it is safe to repeat (`idempotentHint`) | sent again with the same arguments and idempotency key |
| anything else (another MCP tool, `run_command`) | you are asked: `... did not complete before the crash. Send it again? [y/N]` (no = it happened, don't repeat it) |

Then the task that was cut off runs again with a worker that is told what happened to those calls; finished tasks are
skipped. If the plan and config are unchanged, the same run continues (run folder `0003` stays `0003`); otherwise a
new run picks up the history. Files are never left half written: the file tools write a temporary file and rename it
into place.

## Where things are

```
~/.minagent/                 (config "stateDir"; the file tools never read or write it)
  runs/0001/journal.jsonl    every event of the run, one JSON line each; line 1 records what ran it
  runs/0001/plan.md          the exact plan that ran
  runs/0001/config.json      the settings it ran with (secrets removed)
  traces/<id>.jsonl          the full trace (model calls, tool calls), and <id>.digest.md, a readable summary
  locks/                     one lock per plan
```

A plan is recognised by its `plan_id` (plan mode writes one), so moving or renaming the plan file keeps its history.
A copy with the same `plan_id` shares that history and runs with a warning.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | finished, or you said no (`Run?`, `Accept?`) |
| 1 | failed: setup, an error, or a task not done after every attempt |
| 2 | bad flags or config |
| 3 | needs a person: a gate or a crash question nobody could answer, or a declined gate |
| 130 | stopped with Ctrl+C |

## Settings

| Key | Default (auto mode) | |
|---|---|---|
| `maxTaskAttempts` | 3 | tries per task (worker + director) |
| `stateDir` | `~/.minagent` | runs, traces, locks |
| `askRules` | `["*delete*", "*remove*"]` | asked = refused in auto mode |
| `runCommands` | none | programs `run_command` may start |
| `maxSteps` | 10 | model steps per worker (and per director) |

## Current limits

- One task at a time (no parallel tasks yet).
- No token or time budget for a whole run yet (step 2 of `PLAN-auto-mode.md`).
- The director can only judge what it can check: a `done when` it can't verify with reads or your allowed commands is
  judged not done.
- A crash in chat or plan mode isn't resumable (there is no journal there).
