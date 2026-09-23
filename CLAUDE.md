# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A teaching repo. Jerry is learning how AI agents work from first principles by building a minimal,
extensible ReAct agent in Go. Two generations live side by side:

- `mvp1/` — a complete working reference agent (~1800 lines incl. tests, one third-party dep: the
  Anthropic Go SDK). Written for Jerry to read. Treat it as a reference design for this first building phase. mvp1/README.md is a good summary of the agent implementation.
- `mvp2/` — empty, on branch `mvp2-do-myself`. This is where **Jerry writes the code himself** while
  being tutored step by step. Default behaviour here is teach-then-let-him-type: explain the concept,
  show the smallest next slice, wait for him to write/confirm it before moving on. Do not dump a full
  implementation into `mvp2/` unless he explicitly asks for it — that defeats the purpose of the branch.
- `mvp1/tradeoffs/language-scorecard.md` — the worked argument for Go over TS/Python/Rust. Extend this
  rather than re-arguing from scratch; it sets the expected depth for design justifications here.
- `prompts/`, `mvp1/prompts/`, `mvp1/output/` — transcripts of prior sessions and real run logs
  (`prompts/` is gitignored). Useful for recovering earlier reasoning.
- `SESSION.md` (repo root) — scratch handoff notes for `mvp2/`: current state, what's in
  progress, next steps, open TODOs. Read top-to-bottom at the start of a session. Written to be
  disposable/superseded as work moves — don't treat it as a historical record.
- `DECISIONS.md` (repo root) — the durable design-decision log for `mvp2/`: what was decided,
  why, when, which files it affects, and — importantly — decisions that were later reversed,
  with both the original and the revised reasoning kept. This is where "why does the code look
  like this" answers live long-term; `SESSION.md` should point into a `DECISIONS.md` section
  (`see DECISIONS.md § ...`) rather than re-explaining a settled decision inline. Update
  `DECISIONS.md` whenever a real decision is made or changed while working with Jerry, not just
  at the end of a session — same continuous-update discipline as `SESSION.md` already follows.
  A TODO or an open question stays in `SESSION.md` until it resolves into a decision, at which
  point it moves here.

## Commands for Reference Agent

`mvp1/` is the module root for the reference implementation.

```sh
cd mvp1
go build -o minagent .            # binary name `minagent` is gitignored; `mvp1` is not
go test ./...                     # unit tests; a scripted fake LLM drives the loop, no network
go test ./agent -run TestReActLoop -v
go test -race ./...               # tool calls fan out on goroutines — run this after touching act/hooks/memory
go vet ./... && gofmt -l .
MCP_E2E=1 go test ./tool -run MCP -v   # real MCP server over stdio; needs npx, skipped otherwise

./minagent -q "…"                                     # one-shot, config.json (Ollama, local)
./minagent                                            # REPL; file memory persists across turns
./minagent -config config.anthropic.json -q "…"       # Claude + filesystem MCP server
```

File memory writes `.agent-memory.json` in the working directory — delete it to reset a REPL session.

## Architecture for Reference Agent

`agent/` is the kernel and **imports nothing else from the module**; every other package imports it.
`main.go` is the only file that knows about concrete implementations, so any swap is a change there.
Keep that direction: a `tool`/`llm`/`hooks` import inside `agent/` is the architectural regression to
watch for.

Five interfaces in `agent/agent.go` are the five extension axes, and every cross-cutting feature is
supposed to land on one of them rather than in the loop:

| Interface | Answers | Implementations |
|---|---|---|
| `Provider` | which model? | `llm.OpenAI` (any OpenAI-compatible server), `llm.Anthropic` |
| `Tool` | what can it do? | `tool.Func` (any Go closure), `tool.Shell/ReadFile/WriteFile`, `tool.MCP`, `agent.Subagent` |
| `Memory` | what happened? | `memory.InMemory`, `memory.File` |
| `ContextBuilder` | what does the model see *this step*? | `harness.Full`, `harness.Window` |
| `Hook` | observe / veto | `hooks.Logger`, `hooks.Allowlist`, `hooks.Approval` |

So: tracing, cost metering and eval are Hooks; authorization is `Hook.BeforeTool` returning an error;
sandboxing is a replacement `Tool`, not a change to the loop; a new model is ~100 lines of `Chat`.

`Agent.Run` is the whole loop: append user turn → per step, read **all** of memory, let
`ContextBuilder` derive the view, `Chat`, append the response, return its text if there are no tool
calls, else run every tool call concurrently and append the results as `role: tool` observations.
Memory is the truth; context is a view over it — never trim inside `Memory`.

### Invariants that are easy to break

- **Every failure becomes an observation, not a crash.** Unknown tool, bad args, hook denial, tool
  error → `Message{Role: tool, IsError: true}` fed back so the model can recover. `Run` returns an
  error only for LLM/memory failures and `ErrMaxSteps`.
- **`ToolCall.Args` must be a JSON object before a tool runs** (`isObject` in `agent.go`). Sloppy local
  models emit invalid JSON; `llm/openai.go` deliberately stores it as a JSON *string* so the transcript
  still marshals and replays, and the kernel rejects it rather than letting a tool decode zero values.
- **`Message.Native`** carries a provider's own copy of an assistant turn (Anthropic thinking blocks +
  signatures) and is replayed verbatim by the adapter that owns it; other adapters ignore it. Anthropic
  also needs consecutive tool results batched into one user message — see the `flush` buffer.
- **Context must never cut between an assistant tool call and its results** — providers reject orphaned
  tool results. `harness.Window` widens backwards to a user turn for exactly this reason; any new
  builder must preserve it.
- **Policy hooks are shared with subagents** (`main.go` builds `policy` once), so delegation can't route
  around approval. Only the logger differs per agent, and each subagent gets a fresh `InMemory`.
- **Tool specs are sorted by name** so the prompt prefix stays cacheable.
- **Nothing a tool spawns outlives the call.** `tool.Shell` runs `sh` in its own process group, killed on
  timeout and again after exit; output is capped *while streaming*; `cmd.WaitDelay` abandons a pipe held
  open by a background child. MCP servers are closed via `defer` in `run()` — `main` returns errors
  through `run` instead of calling `os.Exit`, which would orphan them.

## Conventions

- Keep explanations clear, concise, accurate but avoid unneccessary technical jargon when possible. Explain things as if I am a generalist mid-level software engineer. 
- Prefix explanatory/teaching comments with `[agent]` so Jerry can grep Claude's commentary apart from
  ordinary code comments. Keep ordinary doc comments unprefixed.
- Concise, small files; no framework scaffolding, no dependency added without a reason.
- State the trade-off, including the honest downside, whenever choosing a language, library, or design —
  a bare choice reads as arbitrary here (see `tradeoffs/language-scorecard.md` for the expected form).
- New behaviour should arrive as a plug-in behind one of the five interfaces. If it can't, say why before
  touching `agent/`.
