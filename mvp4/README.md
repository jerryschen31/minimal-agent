# mvp4: orchestrator mode (serial MVP), reference build

A copy of `mvp3/` (autonomous mode) with **orchestrator mode** added: the director stays in this process,
and the worker runs as a separate `-mode worker` process reached over MCP on stdin/stdout. Tasks still
run one at a time. Built by Claude on 2026-10-15 from `../PLAN-orchestrator-mode-serial.md` (slices A–C);
decisions in `../DECISIONS.md § Step 2 MVP` and `§ mvp4 reference build`. Everything about autonomous
mode below still applies: orchestrator mode is "autonomous mode with the worker moved out of process".

- Separate Go module (`github.com/jerryschen31/minimal-agent/mvp4`).

## Build and test

```sh
cd mvp4
go build -o minagent .
go vet ./... && gofmt -l .
go test -race ./...                       # no network: fake providers drive both director and worker
go test ./agent -run Autonomous -v        # the end-to-end run scenarios
go test ./agent -run 'Worker|Orchestrator' -v   # the worker protocol over in-memory MCP, and a run over it
```

## Run

```sh
./minagent -mode autonomous   -objective <objective.md> [-config file] [-workdir dir]
           [-maxturnspertask N] [-timeout 30m] [-directormodel name] [-model worker-model]
./minagent -mode orchestrator -objective <objective.md> ...same flags...
```

In orchestrator mode this process starts `./minagent -mode worker -config <same file> -workdir <same dir>
-model <m> -maxsteps <n>` and talks to it over MCP (tools `run_task`, `end_task`, `worker_info`). The
top-level config is the worker's; the child's `[worker]` lines arrive on stderr. `-mode worker` is only for
that: its stdout carries MCP messages and nothing else, and it exits at end of input.

**Offline check (no API cost):** point both models at a scripted fake OpenAI-compatible server, as done
on 2026-10-15 (`DECISIONS.md § mvp4 reference build`). With a real provider, any of the smoke tests below
works with `-mode orchestrator` in place of `-mode autonomous`.

The worker's file tools (`read_file`, `write_file`, `edit_file`, `list_dir`) work only inside `-workdir`,
so point it at a scratch directory. `run_command` (`"RunCommand"` in `builtinTools`) runs `go`, `python3`,
`python` or `pytest` there; it is code execution with your permissions, not a sandbox (it can read outside
`-workdir`), so only enable it for objectives you'd be happy to run yourself. Exit codes: 0 objective met · 1 not met or error · 2 usage or bad
objective file · 130 Ctrl+C · 143 SIGTERM.

### Smoke tests

```sh
mkdir -p /tmp/agent-work/{fib,glossary,missing,deepwiki}

# 1. Given tasks, coding: gpt-4o-mini for both roles; the worker runs go itself via run_command.
#    gpt-4o-mini sometimes fails task 2 (its own test doesn't compile); check the result either way:
./minagent -config examples/config.openai.json -mode autonomous \
  -objective examples/objective-fib.md -workdir /tmp/agent-work/fib -timeout 10m
(cd /tmp/agent-work/fib && go test ./... && go run .)

# 2. No ## Tasks, so the director plans first. Claude (via Anthropic's OpenAI-compatible endpoint)
#    as director, gpt-4o-mini as worker. Needs OPENAI_API_KEY and ANTHROPIC_API_KEY.
./minagent -config examples/config.claude-director.json -mode autonomous \
  -objective examples/objective-glossary.md -workdir /tmp/agent-work/glossary

# 3. A task that can't be done: expect task_failed, a review, exit 1, and no invented data.
./minagent -config examples/config.openai.json -mode autonomous \
  -objective examples/objective-missing-input.md -workdir /tmp/agent-work/missing

# 4. Worker with a remote MCP server (DeepWiki): prints the "runs unattended" warning.
./minagent -config examples/config.mcp.json -mode autonomous \
  -objective examples/objective-deepwiki.md -workdir /tmp/agent-work/deepwiki

# 5. Local models only (start Ollama and `ollama pull qwen2.5:7b` first; small models are flaky at tool calls).
./minagent -config examples/config.ollama.json -mode autonomous \
  -objective examples/objective-fib.md -workdir /tmp/agent-work/fib -maxturnspertask 4

# 6. Ctrl+C partway through: expect a report with "interrupted" and exit 130.
# 7. -timeout 20s on example 1: expect "timeout" and exit 1.
# 8. Mistakes are caught before any LLM call:
./minagent -mode autonomous                                   # exit 2: no objective file
./minagent -mode chat -objective examples/objective-fib.md    # exit 2: -objective needs autonomous mode
./minagent -config examples/config.openai.json -mode autonomous -objective examples/objective-fib.md \
  -directormodel gpt-nonexistent                              # exit 1: the 404 reason is shown
```

Rounds: set `"maxIterations": 3` in the `"director"` block (default 1). When a review says the objective
is not met and lists gaps, the gaps are planned as new tasks, worked and reviewed again, up to that many
rounds in total.

## What's new compared to mvp3

| Where | What |
|---|---|
| `worker/` (new package) | `worker.go`: the `Worker` interface, `Result` (`status` set by Go: completed / step_limit / error), `Info`. `mcp.go`: the protocol types, `MCPWorker` (MCP client, 10-minute `CallTimeout`), `StartMCPWorker` / `childArgs` |
| `tasks/` (new package) | `Manager` (the TaskManager): task IDs, `depends_on`, worker type and ID, status, per-task turn history; `Set` checks IDs, dependencies and cycles; `Next` = first ready task in list order |
| `agent/localworker.go` (new) | `localWorker`: the `ChatSession` behind `Worker`; one conversation per task ID |
| `agent/workerserver.go` (new) | `-mode worker`: `run_task`, `end_task`, `worker_info` on an MCP server over stdin and the real stdout |
| `agent/autonomous.go` | the run holds `[]worker.Worker`; `runTask` starts the next ready task, calls `RunTurn`, records history, ends the task; `pickWorker`; report shows IDs |
| `agent/agent.go` | orchestrator mode starts the MCP worker and the director only; worker mode serves the local worker; `ShutdownAgent` closes workers |
| `director/` | `Plan` keeps phase, current task and review, with `Tasks *tasks.Manager`; `set_tasks` takes `{id, task, depends_on, worker_type}`; prompts show IDs, dependencies, the worker section and a `Status:` line |
| `config/` | modes `orchestrator` and `worker`; `UsesDirector`; runtime fields `ConfigFile`, `ProtocolOut` |
| `main.go` | for `-mode worker`, `os.Stdout` points at stderr and the real stdout is kept for MCP |

## What mvp3 added compared to the repo root

| Where | What |
|---|---|
| `director/` (new package) | `objective.go`: `ParseObjective` → `Objective`. `plan.go`: `Plan`, `Task`, `Phase` and the phase transitions. `tools.go`: the control tools `set_tasks`, `task_done`, `task_failed`, `finish`. `prompt.go`: the built-in `Instructions`, `SystemPrompt`, the per-phase prompts, `WrapTurn`, the nudges. |
| `agent/autonomous.go` (new) | `setupAutonomous`, `autonomousRun` (`run` → `runPlan` / `runTask` / `runReview`), `RunReport`, `printReport`, `runAutonomous` (Ctrl+C and the timeout), `ErrBadObjective` / `ErrObjectiveNotMet` / `ErrRunInterrupted` |
| `agent/chat.go` | `runTurn` + `turnResult`, pulled out of `handleUserInput` |
| `agent/agent.go` | `runToolCallEnd`: `reActLoop` stops on `tools.ErrEndTurn`; `RunAgent` dispatches autonomous mode |
| `agent/session.go` | `ChatSession.Name` → `[worker]` / `[director]` output labels |
| `tools/tool.go` | `ErrEndTurn`; built-ins `WriteFile`, `EditFile`, `ListDir`, `RunCommand` |
| `tools/builtin/runcommand.go` (new) | `run_command`: runs `go` / `python3` / `python` / `pytest` (no shell, timeout, output cap, process-group kill, filtered environment) |
| `tools/builtin/writefile.go` (new) | `write_file`, `edit_file`, `list_dir`, all confined like `read_file`; `readRegularFile` shared with `read_file` |
| `config/` | `DirectorConfig` + `GetDefaultDirectorConfig`, `ObjectiveFile`, `Timeout`, `Validate`; flags `-objective`, `-maxturnspertask`, `-timeout`, `-directormodel`; unknown-key warnings inside `"director"`; `headless`/`mission` renamed |
| `main.go` | `run() int` + exit codes; `Validate` after the flag overlay |
| `model/openai.go` | an HTTP error includes the start of the response body |
