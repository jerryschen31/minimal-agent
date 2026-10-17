# mvp3: autonomous mode, reference build

A copy of the repo-root agent (mvp2, as of commit `2cddfad`) with **autonomous mode** fully
implemented. A second LLM, the **director**, works through the task list in an `objective.md` with the
existing agent as the **worker**. Built by Claude on 2026-10-13 as a run-through and template for Jerry's
own build at the repo root, the same way `mvp1/` came before mvp2.

- Design: `../PLAN-autonomous-mode.md`. Decisions: `../DECISIONS.md § Autonomous, orchestrator and worker
  modes`, including `§ mvp3 reference build`, which lists where this build differs from the plan.
- Separate Go module (`github.com/jerryschen31/minimal-agent/mvp3`), so `go test ./...` from the repo root
  does not touch it.

## Build and test

```sh
cd mvp3
go build -o minagent .
go vet ./... && gofmt -l .
go test -race ./...                       # no network: fake providers drive both director and worker
go test ./agent -run Autonomous -v        # the end-to-end run scenarios
```

## Run

```sh
./minagent -mode autonomous -objective <objective.md> [-config file] [-workdir dir]
           [-maxturnspertask N] [-timeout 30m] [-directormodel name] [-model worker-model]
```

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

## What's new compared to the repo root

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
