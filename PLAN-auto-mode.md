# PLAN: autonomous mode v1 (one serial run, journal, resume, budgets)

Status: **design agreed 2026-11-19; 1a–1g built (2026-11-20).** This is step 3 of `PLAN-plan-apply.md` plus the budgets from its
step 6, with the changes agreed on 2026-11-19 (`DECISIONS.md § 2026-11-19`). Where the two plans differ, this one wins:
the per-run `state.json` snapshot is replaced by an append-only journal.

## What it does

```
minagent -mode auto -plan objective.plan.md        # "-mode autonomous" is the same
```

1. **The plan file is the input** (Terraform's saved plan; `apply` runs exactly what was reviewed). The objective is
   found from the plan's front matter (`objective:`, relative to the plan) and read for context and the stale check. A
   stale plan (the objective changed since) or one that fails `CheckComplete` is refused, with the plan-mode command.
2. Show the plan summary, then `Run? [Y/n]` (`-y` answers it).
3. Take the lock. If the latest run never ended (crash, power cut, kill): **resume** it (below). Otherwise start a new run.
4. Run tasks one at a time, each after the tasks it `depends` on. Per task: a worker (fresh context) does the task, then
   the director judges the result against `done when` (`task_done` / `task_failed`), with a retry limit. A task with a
   `gate:` asks before it runs; `-y` never answers a gate.
5. End with a typed stop reason and a short report: tasks, verdicts, tokens and time (step 2).

Plan mode gets `Run now? [y/N]` after a plan is accepted; it starts the same run.

## Approval in auto mode

Nobody is asked during a run (`DECISIONS.md § 2026-11-01 — Approval levels`): a call matching `denyRules` is refused,
a call matching `askRules` is **denied** (there is nobody to ask; `"approval": "allow-all"` turns asks into runs), and
anything else runs. This holds for `run_command` too: with `run_command` in `builtinTools` and programs in
`runCommands`, a command line that matches no `run_command:` deny or ask rule runs without asking.

`GetDefaultConfig("auto")` uses `askRules: ["*delete*", "*remove*"]`: the global default also lists the write tools,
which in auto mode would deny every file write. The write tools stay contained to `workDir` by `Workspace`.
`runCommands` stays empty by default, so `run_command` is opt-in as in chat.

The only questions are the gate before a gated task and, on a manual resume, the cut-off call that can't be checked
(below). Both happen before work starts, not in the middle of it.

## On disk

```
~/.minagent/                 config "stateDir"; the file tools never touch it
  locks/<key>.lock           one OS file lock per plan (plan_id, else real path); the OS frees it on a crash
  runs/0001/journal.jsonl    the run, append-only; line 1 is the manifest; a resume appends to the same file
  runs/0001/plan.md          the exact plan that ran
  runs/0001/config.json      the run's settings, secrets removed
  traces/<trace-id>.jsonl    auto mode's traces (traceDir is ignored); the manifest's trace_run_id names the run's
```

Chat and plan mode keep `.agent-trace/` in the working directory. (Revised 2026-11-20, `DECISIONS.md § 1d built`.)
**Later sprint:** accepted plans kept in the state folder (`~/.minagent/plans/<plan_id>.md`) and run from there, so the
model can't edit or copy the plan that runs; the work-dir file becomes an editable draft you accept again.

## The journal

One JSON object per line, written and synced to disk before `Append` returns. `state.Read` rebuilds the run's state
from the top and ignores a half-written last line. There is no snapshot file: the journal is the only record.

| Event | Data |
|---|---|
| `manifest` (always first) | harness version + commit, provider, model, config hash (no secrets), prompt hashes per role, objective and plan path + hash, tool names + definitions hash, trace run ID |
| `task_started` | attempt |
| `task_finished` | status (`done` / `failed`), summary, verdict |
| `tool_before` | call ID, tool, arguments hash + preview, idempotency key; file tools also: path(s) and file hash before and after |
| `tool_after` | call ID, failed |
| `resumed` | the cut-off calls found and what was done with each |
| `gate_answered` | `yes` / `no`: a person's answer to a task's gate (1e) |
| `usage` (step 2) | tokens per model call or per task |
| `run_ended` | reason: `done`, `task_failed` (judged not done on every attempt), `declined` (no to a gate), `budget`, `timeout`, `interrupted`, `error`, `waiting` (a gate with nobody to answer); `max_steps` is defined but no longer used (a worker out of steps is a failed attempt) |

**Every tool call** gets `tool_before` and `tool_after`; nothing is filtered. The hook is in `executeToolCall`, after the
approval check (a denied call leaves no open record) and before the call. If `tool_before` cannot be written, the call
does not run. In chat and plan mode the hook is nil.

**Idempotency key:** `minagent/<run>/<task>/<call ID>`, recorded in `tool_before` and sent with every MCP call in the
request's `_meta`. Servers that deduplicate can use it; others ignore it.

## Resume (manual in v1)

You start the same command again. The run finds the journal without `run_ended`, and before any task runs it settles
each **cut-off call** (a `tool_before` with no `tool_after`; at most one or a few per crash):

| The cut-off call's tool | What resume does |
|---|---|
| A built-in read tool (`read_file`, `list_files`, `search_files`), or MCP `readOnlyHint` | Nothing; the task runs again |
| A built-in file tool (`write_file`, `edit_file`, `create_directory`, `copy_file`, `move_file`) | Checks the file(s) against the "before" hashes in `tool_before`: all match → it didn't happen; otherwise → it happened, or something else changed the file, and the worker is told (1f records "after" only in `tool_after`, so a cut-off call has no "after" to compare with) |
| MCP with `idempotentHint` | Re-sends the exact recorded call with the same idempotency key |
| Anything else (other MCP tools, `run_command`) | **Warns and asks**: "`email_send` {to: …} did not complete before the crash. Send it again? [y/N]". **y** re-sends the exact recorded call; **n** treats it as done. `-y` never answers it; with no terminal the run stops with `waiting` |

Then the cut-off task runs again from the start with a fresh worker, which is told what was cut off and how it was
settled. Done tasks are never run again.

**What resume promises:** done tasks are skipped, and a cut-off call is never repeated without a reason (a file
check, a declared hint, or your answer). It cannot stop the model from choosing to repeat an action when the task runs
again; the worker's message is there to prevent that.

**v2 (not now):** the worker checks whether the action happened (e.g. the sent folder) instead of asking; an
`onUnknownOutcome` config policy (`verify` / `rerun` / `skip`); automatic retries with backoff for model calls and
never-sent tool calls; automatic resume at startup with a crash-loop limit.

## Budgets (step 2)

- `maxTokens`: the per-response output limit, sent to the provider (today parsed but unused).
- `runTokenBudget`: total tokens for the run; `runTimeoutMinutes`: **time the run was actually running**, summed over
  its sessions (start to crash, resume to end, …) from the journal, so time spent crashed or waiting to be resumed does
  not count. Checked in the session provider wrapper, so `reActLoop` does not change. Hitting one ends the run with
  `budget` or `timeout`.
- Token usage comes from the provider's response (`usage`), fills the trace's existing `Usage`, and is added up per
  task in the journal and the report. A provider that reports no usage counts as 0, said so in the report.

## Commits

**Step 1: one serial run**

| # | Commit |
|---|---|
| 1a | `-mode auto -plan <file>` (alias `autonomous`): read the plan, find its objective, refuse stale or broken, summary, `Run? [Y/n]`, stop |
| 1b | `state/`: run folders, lock, journal `Writer` (`Append`), `Read` → `RunState` |
| 1c | The manifest as the first event |
| 1d | Serial scheduler + worker: `depends` order, fresh context per task, task events (`task_finished` records the task's `DefinitionHash`), typed stop reasons |
| 1e | Director: `task_done` / `task_failed` against `done when`, retry limit, gates |
| 1f | `tool_before` / `tool_after` for every call; file tools add their before/after hashes; idempotency key in MCP `_meta` |
| 1g | Write tools write via a temporary file + rename, so a crash never leaves half a file (security-sensitive: keeps `openDir`, same-file and hard-link checks) |
| 1h | Resume or continue: decide per task from the last run (table "Which tasks run" below); settle cut-off calls (table above), re-run the cut-off task, skip done ones. **Done test:** a child process is killed (`os.Exit`) inside a changing tool call; resume does not repeat the call and does not re-run done tasks |
| 1i | Plan mode's `Run now? [y/N]`; docs (CLAUDE.md commands, `PLAN-plan-apply.md` status, DECISIONS, SESSION) |

**Which tasks run** (every start, after a crash or after an edit of the plan, by the planner or by hand):

| Task in the plan file | Last run | Runner does |
|---|---|---|
| same `DefinitionHash` | done | skip |
| different hash | done | run again (it changed) |
| new ID | — | run |
| gone from the plan | done | nothing; listed in the report |
| any | failed / cut off | run again |

**Step U: plan update, small** (after step 1; replaces most of `PLAN-plan-apply.md` step 4). Plan mode with an existing
plan and a changed objective: the planner gets the old and new objective, the current plan (with hand edits) and which
tasks are done, and proposes the new plan; unchanged tasks must be copied exactly (Go checks it); the summary marks
tasks new, changed or removed; `Accept? [Y/n]`. Left out: drift check, recorded-reads cascade, `-replace` (edit a task
to force a re-run). **Objective drift:** the plan carries the objective word for word (`## Objective`); a hand edit of
that copy is fine for running. When you plan again from the objective file, plan mode compares the file with the plan's
copy and, if they differ, shows the difference and asks which one is the intent before updating.

**Step 2: budgets** (after step U)

| # | Commit |
|---|---|
| 2a | Token usage from the OpenAI-compatible adapter onto the response; trace `Usage` filled |
| 2b | `maxTokens` sent as the per-response limit |
| 2c | `runTokenBudget`, `runTimeoutMinutes` in the session provider; stop reasons `budget` / `timeout` |
| 2d | Usage per task in the journal; tokens and time per task and in total in the report |

## Files

```
config/flags.go, config.go, doc.go   ModeAuto + alias; GetDefaultConfig("auto"); new keys (step 2)
state/                               NEW (stdlib only): doc.go, dir.go (folders, lock), journal.go (events, Writer),
                                     read.go (Read → RunState), manifest.go
agent/director/                      NEW, like agent/planner: prompts, task_done / task_failed
agent/automode.go                    NEW: autoModeDetails, setupAutoMode, runAutoMode, autoMode, loadRunnablePlan
agent/autorun.go, autoworker.go      NEW (1d): openRun, tasksToRun, runPlan, end; workerPrompt, taskMessage,
                                     runWorker. 1e: runTask (attempts), askGate; autodirector.go judgeTask. 1h resumeRun
agent/react.go, session.go           executeToolCall writes tool_before / tool_after via cs.ToolJournal (nil = off)
agent/planmode.go                    planStep: Run now? [y/N]
mcpconnect/tools.go                  idempotency key in _meta; HintsOf already gives readOnly / idempotent
tools/builtin/ (write tools)         before/after hashes for tool_before; temp file + rename (1g)
model/ (step 2)                      usage on the response; max_tokens
```

## Reference

mvp3 (`mvp3/agent/autonomous.go`, `mvp3/director/`) for the director's tools, fresh worker context per task,
`stopReason` and the report. New here: `state/`, the tool records, resume, reading `plan.Plan`.
