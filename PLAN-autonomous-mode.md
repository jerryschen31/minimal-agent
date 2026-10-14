# PLAN — autonomous mode (step 1)

Feature plan for `-mode autonomous`, written 2026-10-13 on branch `feat/autonomous-mode`.

- **This file:** the whole feature in one place: what it is, how it works, the build order, and a
  summary of every decision.
- **`DECISIONS.md § Autonomous, orchestrator and worker modes`:** the authoritative decision log,
  including decisions that were later reversed and why. If the two files ever disagree, `DECISIONS.md`
  wins; fix this file.
- **Design discussion:** `prompts/20261012-auto-mode-brainstorming.md` and the 2026-10-13 session.

---

## 1. What autonomous mode is

In chat mode a human types a prompt, reads the reply, decides what to ask next, and decides when to stop.
In autonomous mode a second LLM, the **director**, does that job. It is guided by an `objective.md` file
that says what should be achieved.

```
./minagent -mode autonomous -objective objective.md [-config file] [-maxturnspertask N] [-timeout 30m]
```

- It is still **one agent instance in one process**. The **worker** is the agent you already have:
  same provider config, tools, MCP servers, memory and context window. Its turns run exactly as in
  chat mode; only the author of the prompt changes.
- The director is a second, smaller agent in the same process. It has its own LLM, context and tools.
- The run is a **task list worked through one task at a time.** The code owns the list and the
  "current task" pointer. The director only answers narrow questions such as "does this reply complete
  the current task?"

### Terminology

| Term | Meaning |
|---|---|
| **director** | The second LLM plus its own small `ChatSession`. It writes worker prompts, judges replies and writes the report. |
| **worker** | The existing agent (top-level config). It runs one ReAct turn per prompt. |
| **turn** | One worker ReAct turn: a prompt in, a final reply out. Bounded by `MaxSteps`. |
| **task** | One unit of work from the task list. It may take several worker turns (follow-ups). |
| **phase** | PLAN → WORK → REVIEW. The code moves between them; the director never chooses the phase. |

### Out of scope for step 1

Recorded in `DECISIONS.md § Deferred for Later`, item 12:
- `-json` output;
- a run transcript file;
- re-planning mid-run, and director-added tasks;
- skipping a failed task and continuing;
- a separate verifier LLM;
- human takeover on Ctrl+C;
- token or cost budgets;
- resuming a crashed run;
- everything from step 2 (orchestrator and worker modes, multiple processes, MCP server mode).

---

## 2. The run, end to end

```
setup     parse flags + config → check mode (after flags are applied) → read + parse objective.md
          build worker session (as today) + director session (own provider, context, tools)
          print the warning: worker tool calls run unattended

PLAN      only if objective.md has no ## Tasks
          director (tools: read_file, set_tasks) → set_tasks([...])   1–20 tasks, printed, fixed for the run

WORK      for each task i, in order:
            clear worker context + director context
            director writes the worker prompt for task i            (or it already did, via task_done)
            loop, at most maxTurnsPerTask worker turns:
              worker turn (runTurn) → code wraps the result for the director:
                "Task i of n: <task>
                 Worker reply: <text, capped>
                 Tools called: read_file, fs_write_file   Error: <if any>
                 Is the task complete?"
              director (tools: read_file, task_done, task_failed) →
                task_done(note)       → mark done; next task. The tool result names the next task, and the
                                        director's text reply becomes that task's first worker prompt
                task_failed(reason)   → mark failed; stop WORK
                text reply            → follow-up prompt to the worker, same task
            per-task limit reached     → task failed (reason: "turn limit reached"); stop WORK

REVIEW    after the last task, or after a failed task (best effort)
          director (tools: read_file, finish) → finish(success, summary, gaps)
          judged against # Objective (and ## Done, if present)

REPORT    printed to stdout; exit code 0 / 1 / 130
```

### Stop conditions

| What happens | Report reason | REVIEW call? | Exit code |
|---|---|---|---|
| All tasks done, then `finish` | `finished` | yes | 0 if `success`, else 1 |
| `task_failed`, or per-task limit hit | `task_failed` | yes, best effort (on failure: report built from code state) | 1 |
| Ctrl+C | `interrupted` | no (code state only) | 130 |
| `-timeout` expired | `timeout` | no | 1 |
| Director turn fails (provider error, director's own `MaxSteps`) | `error` | no | 1 |
| SIGTERM | `interrupted` | no | open: 130, or 143 (= 128 + 15, the usual SIGTERM convention) |

A failed **worker** turn (e.g. `ErrMaxSteps` or a provider error) does **not** stop the run. It becomes
part of the wrapped message to the director ("Error: …"), and the director decides: follow up, or
`task_failed`. This is the existing rule "every failure becomes an observation", applied one level up.

There is **no run-wide turn limit.** The run is bounded by `maxTurnsPerTask` × (number of tasks ≤ 20).
The worst case is 100 worker turns, each up to `MaxSteps` LLM calls; `-timeout` is the wall-clock
safety net.

---

## 3. `objective.md`

### Format

```markdown
# Objective
Summarize how MCP servers are configured in this repo, for a new contributor.

## Tasks
- Read README.md and config.default.json
- List every MCP server field and what it does
- Write the summary to notes/mcp-config.md (max 40 lines)

## Done when
- notes/mcp-config.md exists and covers stdio and http servers

## Constraints
- Do not modify any existing file.

## Context
The config loader is config/config.go.
```

### Rules

| Section | Required? | Use |
|---|---|---|
| `# Objective` | **yes**, exactly one H1 | Always in the director's system prompt |
| `## Tasks` | no | If present: the task list, **used exactly as written** (no rewriting or splitting). If absent: the PLAN phase |
| `## Done…` (any `##` heading starting with "Done", case-insensitive) | no | Checked in REVIEW, alongside `# Objective` |
| `## Constraints`, `## Context` | no | Passed into the director's system prompt as text |
| any other `##` section | no | Passed through as text |

- **Task items** are the top-level bullets under `## Tasks` (`-`, `*`, `1.`).
  - A `[ ]`/`[x]` checkbox is stripped and ignored, so every task starts `pending`.
  - Nested bullets and continuation lines belong to their parent task's text.
  - At most 20 tasks; an empty `## Tasks` is an error.
- **The file** must exist, be non-empty and be at most 20 KB. A bad file is a startup error (exit 2,
  like other usage errors).
- `objective.md` is **read-only input owned by the human.** The run never writes to it.
- The worker never sees the objective, only the director's prompts. A task is the unit of text the
  worker gets.

---

## 4. The director

### It's a small ReAct agent: a second `ChatSession`

The director needs `read_file` (to check what the worker produced), so it is more than one LLM call
per decision: it may read a file or two before answering. It therefore gets everything a worker turn
already has, reused:

- its **own provider** (from the `director` config block);
- its **own `ChatContext`**, with auto-compaction on as a safety net. The window settings are package
  constants in `contextwindow/`, the same as the worker's;
- its **own tool registry**, swapped per phase (below);
- **`runTurn`**, the same function a worker turn uses.

A director turn's **final text reply is the worker's next prompt**, passed as a value. `InBuffer` and
`OutBuffer` keep their existing job as the terminal the human watches: stdin is unused in autonomous
mode, and stdout shows `[director]`, `[assistant]` and `[tool]` lines and the report. We do **not** pipe
the worker's `OutBuffer` into the director, because it contains every log line, and the director side
would have to parse the final reply and tool names back out of text.

### Tools per phase

Only the current phase's tools are offered. All of these except `read_file` are "control tools": they
change run state in the code, they don't touch the outside world.

| Phase | Tools | Meaning of a plain text reply |
|---|---|---|
| PLAN | `read_file`, `set_tasks(tasks []string)` | not allowed: nudge "call set_tasks" |
| WORK | `read_file`, `task_done(note)`, `task_failed(reason)` | a prompt for the worker (first prompt or follow-up) |
| REVIEW | `read_file`, `finish(success bool, summary string, gaps []string)` | not allowed: nudge "call finish" |

- `read_file` is the existing built-in, confined to the same `workDir` as the worker, with the same
  deny list.
- Tool definitions are sorted by name, as in every registry, to keep prompts cacheable.
- **Ending a director turn early.** Normally a tool call means "run it and send me the result", which
  would cost one more LLM call just to acknowledge it. `task_done` on the **last** task, `task_failed`
  and `finish` end the director's turn at once; their tool result is never sent. That is safe because
  the director's context is cleared, or the run ends, right afterwards. `task_done` on any other task is
  *not* terminal: its result says *"Recorded. Next is task 3 of 5: <text>. Reply with the worker's
  prompt for it."* The director's next text reply is that prompt, so one director turn judges a task
  and starts the next.
  - *Mechanism (proposed, decide while coding):* a sentinel error, e.g. `tools.ErrEndTurn`, returned by
    a terminal tool's `CallTool`. `reActLoop` sees it (via `errors.Is`) after running that call and
    returns the turn so far instead of calling the LLM again. This is the same pattern as `ErrMaxSteps`.
- **Bad calls are observations.** Wrong-phase or unknown tool, invalid args, an empty prompt, `set_tasks`
  with 0 or more than 20 tasks: each returns an `error: …` tool result, and the director tries again
  within its own `MaxSteps`.

### The director's system prompt (rebuilt before every director turn)

```
<built-in director instructions>          constant text in the code
<director.additionalInstructions>         from config, appended (never replaces the built-in text)
## Objective                              the # Objective text
## Done when / Constraints / Context      whichever sections exist
## Tasks                                  each task with its status and note, the current one marked:
  1. [done]    Read README.md …           note: "config keys: command/url/env/headers/type"
  2. [current] List every MCP server field …
  3. [pending] Write the summary …
## Phase                                  PLAN | WORK (task i of n) | REVIEW, and what's expected now
```

- **The objective stays for the whole run.** Judging a task, honouring constraints and writing good
  worker prompts all need the "why", not just the list.
- **Progress lives in the system prompt**, so compaction can't lose it. The trade-off: changing the
  system prompt breaks provider-side prompt caching for that call. That's acceptable here, and local
  models don't cache.
- **The built-in instructions must tell the director to:**
  - send one clear instruction per prompt;
  - judge replies critically against the *current task*;
  - ask the worker for evidence (e.g. "show the file contents") instead of trusting claims, or check
    the evidence itself with `read_file`;
  - write `task_done` notes that later tasks will need;
  - include in a prompt any context from earlier tasks that the worker needs, because the worker
    forgets between tasks;
  - call `task_failed` rather than repeat a failing instruction;
  - in REVIEW, list anything not achieved as `gaps`.

### Memory

- **At the start of each task, both contexts are cleared:** the worker's `MsgContext` and the director's.
  The worker's `MsgHistory` is still appended to every turn, so a future transcript feature loses
  nothing.
- **Within a task**, both contexts persist, so follow-ups work.
- **Across tasks**, the only carried-over memory is the `task_done` notes in the system prompt, plus
  files in `workDir`. *Downside:* anything not written into a note is gone, though the director can
  `read_file` what the worker produced.

### What the director sees after each worker turn

The code builds this message from a `runTurn` result, not from terminal output:
- the task header ("Task i of n: …");
- the worker's final reply, **capped at about 4,000 characters** and marked as cut when longer;
- the names of the tools called during the turn;
- the error, if the turn failed;
- the question "Is the task complete?"

---

## 5. Config

### Shape

The top level stays the worker's config, so existing chat configs work unchanged. A `"director"` block
is added:

```json
{
  "provider": "openai", "model": "qwen2.5:7b", "baseUrl": "http://127.0.0.1:11434/v1",
  "agentMode": "autonomous",
  "objectiveFile": "objective.md",
  "timeout": "30m",
  "director": {
    "provider": "openai",
    "model": "gpt-4o-mini",
    "baseUrl": "https://api.openai.com/v1",
    "apiKeyName": "OPENAI_API_KEY",
    "maxSteps": 10,
    "maxTurnsPerTask": 5,
    "additionalInstructions": "Prefer small, verifiable steps."
  }
}
```

| Field | Where | Default | Notes |
|---|---|---|---|
| `agentMode` | top | `chat` | `chat \| autonomous \| oneshot` (renamed from `headless`) |
| `objectiveFile` | top | — | Required when the mode is `autonomous` (renamed from `missionFile`) |
| `timeout` | top | none | Go duration string (`"30m"`); flag `-timeout` |
| `director.provider/model/baseUrl/apiKeyName` | director | **default director config** | *Not* inherited from the worker |
| `director.maxSteps` | director | 10 | The director's own ReAct step limit per director turn |
| `director.maxTurnsPerTask` | director | 5 | Must be > 0; flag `-maxturnspertask` |
| `director.additionalInstructions` | director | `""` | Appended to the built-in instructions |

- **Precedence:** flag > config file `director` block > default director config.
  `SetDefaultConfig` already unmarshals the file onto `GetDefaultConfig()`. `encoding/json` only
  overwrites the fields present in the file, including inside a nested struct, so director defaults set
  in `GetDefaultConfig()` "fill in the empty fields" for free.
- **Two gotchas:**
  - The unknown-key warning (`configJSONKeys`) looks at top-level keys only, so a typo inside
    `"director"` is ignored silently. Extend the check one level down, or accept this for now.
  - If the default director config points at OpenAI and the worker is a local Ollama model, a run
    without a `director` block fails on a missing `OPENAI_API_KEY`. That's fine as long as the error is
    clear at startup.
- **Mode checks must run after `FlagsOverlay`** (the existing TODO). Otherwise `"agentMode":
  "autonomous"` in a file, with no `objectiveFile`, gets through.

### Flags

| Flag | Change |
|---|---|
| `-mode autonomous` | replaces `-mode headless` |
| `-objective <file>` | replaces `-mission`; requires the autonomous mode |
| `-maxturnspertask <n>` | new; positive integer |
| `-timeout <duration>` | new; Go duration |
| `--json` | still accepted for `oneshot` only; autonomous `-json` is deferred |

---

## 6. Package layout and types

The dependency direction is unchanged: capability packages never import `agent/`.

```
director/                 NEW capability package (imports model, tools, config — never agent)
  objective.go            Objective type + ParseObjective (pure, no I/O besides the caller's bytes)
  plan.go                 Plan / Task state + phase, the methods the control tools call
  tools.go                set_tasks, task_done, task_failed, finish (each a tools.Tool over *Plan)
  prompt.go               the built-in instructions + SystemPrompt(objective, plan, extra) + WrapTurn(...)
agent/
  chat.go                 runTurn pulled out of handleUserInput
  autonomous.go           NEW: runAutonomous (the phase loop) + report printing
  agent.go                SetupAgent builds the director session when mode == autonomous; RunAgent dispatch
config/                   renames, DirectorConfig, new flags, Validate after overlay
main.go                   run() error + exit codes
```

Type sketches. These are signatures to fill in, not final code:

```go
// director/objective.go
type Objective struct {
	Text     string   // the # Objective body
	Tasks    []string // from ## Tasks; nil = PLAN phase needed
	Sections []Section // Done…, Constraints, Context, others, in file order
}
type Section struct{ Heading, Body string }
func ParseObjective(data []byte) (Objective, error)

// director/plan.go
type TaskStatus string // "pending" | "done" | "failed"
type Task struct {
	Text   string
	Status TaskStatus
	Note   string // task_done note or task_failed reason
}
type Phase string // "plan" | "work" | "review"
type Plan struct {
	Phase   Phase
	Tasks   []Task
	Current int // index into Tasks during WORK
	// filled by finish:
	Success bool
	Summary string
	Gaps    []string
}

// tools/ (or director/): the "end this director turn now" signal
var ErrEndTurn = errors.New("end turn")

// agent/chat.go
type turnResult struct {
	Reply       string   // final assistant text
	ToolsCalled []string // tool names, in call order
}
func runTurn(ctx context.Context, cs *ChatSession, prompt string) (turnResult, error)

// agent/autonomous.go
type runReport struct {
	Reason    string // finished | task_failed | interrupted | timeout | error
	Plan      *director.Plan
	TurnsUsed int
	Err       error
}
func runAutonomous(ctx context.Context, worker, dir *ChatSession, obj director.Objective, plan *director.Plan) runReport
```

---

## 7. Behaviour details

- **Ctrl+C ends the whole run.**
  - The run is wrapped in one `createTurnWatcher(ctx, interrupts)`. A cancelled run prints the report
    from the current code state, then shuts down normally (`ShutdownAgent` closes MCP servers), and
    exits 130.
  - Auto-compaction still uses the root context (an existing invariant).
  - *Add to the CLAUDE.md invariant:* "in autonomous mode the whole run is the foreground work".
- **No slash commands from the director.** Its text always goes to the worker as an ordinary prompt,
  even when it starts with `/`. `runTurn` never parses slash commands; only `handleUserInput` does.
- **Safety warning.** At startup, if the worker has any MCP tools, print one line: worker tool calls
  will run unattended, with no approval. There's no refusal and no opt-in flag (decided). The reason:
  in chat a human reads every reply and can stop; here up to `tasks × maxTurnsPerTask × MaxSteps` tool
  calls run unwatched. That includes calls driven by *prompt injection*, meaning instructions hidden in
  tool output such as a fetched web page.
- **Output.** During the run, `[director] <prompt>`, `[director] task 2/5 done: <note>`, and the
  existing `[assistant]` and `[tool]` lines. `ShowProgress` (the thinking dots) stays off. At the end,
  the report goes to stdout:

```
=== Autonomous run report ===
Result:   finished (success)            | task_failed | interrupted | timeout | error: <err>
Summary:  <director summary, or "no review" when the REVIEW call didn't run or failed>
Tasks:
  1. [done]    Read README.md …                 <note>
  2. [failed]  List every MCP server field …    <reason>
  3. [pending] Write the summary …
Gaps:
  - <gap>
Turns:    7 worker turns
```

- **Gaps** come from `finish(…, gaps)`, one item each, so a later `-json` mode can emit them as an
  array and a later feature can turn them into new tasks.

---

## 8. Implementation slices

Each slice builds, passes `go build ./... && go vet ./... && gofmt -l . && go test -race ./...`, and is
reviewed before the next one. Working style as usual: concepts first, then small examples, then a
skeleton with `// TODO` hints that Jerry fills in.

### Slice 1: Renames, `run() error`, exit codes, validation after overlay

- `headless` → `autonomous`, `mission` → `objective` (`grep -rn -i 'mission\|headless'`: config, flags,
  usage text, tests).
- Add `-maxturnspertask` and `-timeout` (flags + `Config` + `FlagsOverlay`), plus the `DirectorConfig`
  struct with defaults.
- `config.Validate(cfg)` after the overlay: autonomous mode needs `objectiveFile`, and
  `maxTurnsPerTask > 0`.
- `main`: move the lifecycle into `run() error` so the deferred `ShutdownAgent` runs, then `os.Exit`
  with the code. Resolves `DECISIONS.md` Deferred items 3 and 11. Concept to cover first: `os.Exit`
  skips deferred calls.
- **Tests:** flag cases (new flags, renamed flags, autonomous without an objective); `Validate` for a
  mode set from the config file; the default director config merges with a partial `director` block.
- `RunAgent` still returns "unsupported" for autonomous mode at this point.

### Slice 2: `runTurn`

- Pull the non-slash part of `handleUserInput` into `runTurn(ctx, cs, prompt) (turnResult, error)`:
  run the ReAct loop, commit on success only, return the final reply plus the names of tools called
  (from the turn's assistant messages).
- Add `ErrEndTurn` handling to `reActLoop` here or in slice 4. If here: a terminal tool's result ends
  the loop, and the turn messages up to that point are returned.
- **Tests:** existing chat tests unchanged; `runTurn` returns the reply and tool names; a failed turn
  commits nothing; an `ErrEndTurn` tool stops the loop with no further `Chat` call (the `FakeProvider`
  script would fail loudly on an extra call).

### Slice 3: `director/` objective parsing

- `ParseObjective`: the rules in §3. A pure function, table-driven tests.
- **Tests:**
  - missing or duplicate `# Objective`;
  - `## Tasks` with `-`/`*`/`1.` bullets, checkboxes, nested bullets, continuation lines;
  - more than 20 tasks;
  - an empty `## Tasks`;
  - `## Done when` / `## done` / `## Done criteria` matched;
  - unknown sections kept;
  - the size cap.

### Slice 4: `director/` plan, control tools, prompts

- `Plan` and its transitions: `SetTasks`, `TaskDone`, `TaskFailed`, `Finish`, with errors for calls in
  the wrong phase.
- The four control tools as `tools.Tool` over `*Plan`. JSON-schema params, argument validation, and
  `ErrEndTurn` where terminal.
- The built-in instruction text, `SystemPrompt(obj, plan, extra)` and `WrapTurn(task i/n, result, err)`
  with the 4,000-character cap.
- **Tests:** each tool's happy path and bad args; phase checks; the system prompt contains the
  objective, task statuses and notes; `WrapTurn` truncation.

### Slice 5: Director session wiring

- `SetupAgent` for autonomous mode:
  1. read the objective file and `ParseObjective`;
  2. set up the director provider from `cfg.Director`;
  3. give the director its own `ChatContext` and a `ChatSession`;
  4. build the per-phase tool registries (`read_file` + control tools);
  5. print the unattended-tools warning.
- **Tests:** setup with a fake config; a missing or bad objective file errors; the director registry
  has no worker or MCP tools.

### Slice 6: The autonomous loop and the report

- `runAutonomous`: the PLAN → WORK → REVIEW loop (§2), context clearing per task, `maxTurnsPerTask`,
  the best-effort REVIEW after a failure, and the run-wide watcher for Ctrl+C plus `-timeout`.
- `RunAgent` dispatch; report printing; exit-code mapping in `main`.
- **Tests:** two `FakeProvider` scripts, one for the director and one for the worker:
  - a happy path with given `## Tasks`;
  - the PLAN path (`set_tasks`);
  - a follow-up within a task;
  - `task_failed` → REVIEW still runs;
  - the per-task limit → `task_failed`;
  - a worker turn error reaches the director as text;
  - Ctrl+C mid-task → `interrupted`, no REVIEW call;
  - a director provider error → `error`;
  - the contexts are cleared between tasks.

### Slice 7: Real run and docs

- A small `objective.md` (§3), first with the same model in both roles, then with a stronger director.
- Update `CLAUDE.md` (commands, a new invariant for the Ctrl+C scope in autonomous mode), `README.md`,
  `SESSION.md`; mark this plan done in `DECISIONS.md`.

---

## 9. Decision summary

All dates 2026-10-13 unless noted. Full reasoning is in `DECISIONS.md § Autonomous, orchestrator and
worker modes`.

| # | Decision | Notes / trade-off |
|---|---|---|
| D1 | Build in stages: step 1 = autonomous (one instance); step 2 = orchestrator + worker over MCP | Reverses the 2026-10-12 jump straight to multi-process; step 2 ideas parked, not rejected |
| D2 | Rename `headless` → `autonomous`, `mission` → `objective`; no alias | "Headless" only means "no UI" (`oneshot` is too); nothing shipped, so a clean rename |
| D3 | The director is a second `ChatSession` in the same process; turns go through the shared `runTurn` | Reuses context, compaction and tools; no new protocol |
| D4 | The director's final text = the worker's next prompt, passed as a value, not through `InBuffer`/`OutBuffer` | The buffers stay the human's terminal; no parsing of log text |
| D5 | Task-driven run: PLAN (if no `## Tasks`) → WORK one task at a time → REVIEW | The code owns the plan; the LLM makes narrow judgments; easy to test |
| D6 | Only `# Objective` is required; `## Tasks`, `## Done…`, `## Constraints`, `## Context` optional | The author of a given task list is responsible for it covering the objective |
| D7 | Given `## Tasks` are used exactly as written | The user's intent; the director doesn't rewrite them |
| D8 | The objective stays in the director's system prompt for the whole run | Needed to judge, honour constraints and write prompts |
| D9 | Progress (status + notes) lives in the system prompt, rebuilt each director turn | Compaction-proof; breaks prompt caching for that call |
| D10 | Phase-specific director tools: `set_tasks` / `task_done`, `task_failed` / `finish`, plus `read_file` | Replaces the earlier `mark_item` + checklist design |
| D11 | Terminal control tools end the director turn without an extra LLM call | Mechanism (`ErrEndTurn`) to be confirmed while coding |
| D12 | `task_done` on a non-last task returns the next task, and the director's reply is its prompt | Judging one task and starting the next in one director turn |
| D13 | Worker **and** director contexts are cleared per task; notes carry memory forward | Small contexts; anything not noted is lost |
| D14 | The worker never sees the objective; the director passes along earlier results as needed | A task is the worker's unit of work |
| D15 | The director judges task completion and the final objective check | No separate verifier (deferred) |
| D16 | REVIEW: `finish(success, summary, gaps)` against `# Objective` (+ `## Done`) | Gaps are a list, so they can feed `-json` and future follow-up tasks |
| D17 | A failed task stops the run; REVIEW still runs, best effort | On REVIEW failure, the report comes from the code's state |
| D18 | `maxTurnsPerTask` (default 5); **no run-wide turn limit** | Bounded by tasks × limit; `-timeout` is the safety net |
| D19 | `-timeout <duration>`, default none | Reason `timeout` |
| D20 | A failed worker turn is an observation for the director; a failed director turn ends the run (`error`) | No retries yet |
| D21 | Ctrl+C ends the whole run, prints the report, shuts down cleanly | Human takeover deferred |
| D22 | The worker keeps its MCP tools; a startup warning only, no refusal or opt-in | No approvals exist in mvp2; unattended risk is accepted for now |
| D23 | No slash commands from the director | `runTurn` never parses them |
| D24 | `[director]` lines during the run; the report goes to stdout; `-json` deferred | |
| D25 | Report: reason, success, summary, tasks with status + notes, gaps, turns used | |
| D26 | Exit codes: 0 finished and success; 1 not met or error; 130 interrupted; 2 usage / bad objective file | Needs `run() error` in `main` |
| D27 | Config: a `director` block in the existing config; empty fields come from a default director config (not the worker's); built-in instructions + `additionalInstructions` (append only) | Precedence: flag > block > director defaults |
| D28 | `director/` is a capability package; the loop lives in `agent/autonomous.go` | Keeps the dependency direction |
| D29 | `objective.md` parsing rules, 20 tasks max, 20 KB max, a bad file is exit 2 | Claude's defaults, accepted |

### Open while coding (small; decide in the slice)

- The exact `ErrEndTurn` mechanism and where it lives (`tools/` vs `director/`): slice 2 or 4.
- Whether the unknown-key warning should look inside `"director"`: slice 1.
- The exit code for SIGTERM: 130 like Ctrl+C, or the conventional 143. Slice 1.
- How many nudges to allow when the director replies with text in PLAN or REVIEW before the turn
  counts as failed (proposal: rely on the director's own `MaxSteps`): slice 4 or 6.

---

## 10. Step 2 (parked, for context only)

`-mode orchestrator` = autonomous mode where the director delegates tasks to worker instances;
`-mode worker` = an agent that only serves MCP requests (stdio or streamable HTTP). Ideas kept from the
2026-10-12 brainstorm:
- stateless tasks plus a shared workspace;
- the LLM decides *what* to delegate, a pool decides *how many* instances;
- a `workers` config section reusing `McpServerConfig`;
- launchers (local process, existing URL, later Kubernetes);
- Kubernetes "pattern B" first (Deployments + Services, the orchestrator connects by URL);
- hub-and-spoke only, with no worker-to-worker messaging.

What step 1 must leave ready for step 2: the task list and plan state in `director/`, the phases, and
`runTurn` as the one way to run a turn. In step 2, WORK becomes "hand ready tasks to idle workers"
instead of "the next task in-process".
