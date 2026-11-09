# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A teaching repo. Jerry is learning how AI agents work from first principles by building a minimal,
extensible ReAct agent in Go. Two generations live side by side:

- `mvp1/` — a complete working reference agent (~1800 lines incl. tests, one third-party dep: the
  Anthropic Go SDK). Written for Jerry to read. Treat it as a reference design for this first building phase. mvp1/README.md is a good summary of the agent implementation.
- `mvp2/` — the in-progress second generation. `mvp2/learn/` contains standalone `package main`
  snapshots used to build the agent incrementally (kept as reference; its big test file is not edited).
  The code being migrated out of it is the **real mvp2 build, at the repo root** (`main.go`, `agent/`,
  `config/`, `model/`, `memory/`, `contextwindow/`, `tools/`, `mcpconnect/` — see "Architecture for
  mvp2" below). Jerry writes this code himself while being tutored step by step, so default behaviour
  is teach-then-let-him-type: explain the concept, show the smallest next slice, and wait for him to
  write/confirm it.
- `mvp3/` — a reference build of **autonomous mode** (a director LLM works through `objective.md`'s
  tasks with the agent as worker), written by Claude on 2026-10-13 as a template for Jerry's own build
  at the root, the way `mvp1/` preceded mvp2. A separate Go module copied from the root code; see
  `mvp3/README.md`, `PLAN-autonomous-mode.md` and `DECISIONS.md § mvp3 reference build`. Like mvp1, it
  is for reading; don't fold it into the root code unasked.
- `mvp4/` — a copy of `mvp3/` with **orchestrator mode** added: `-mode orchestrator` runs the
  director and starts each worker as a `-mode worker` child process over MCP stdio. Written by Claude on
  2026-10-15 (serial) and 2026-10-16 (parallel tasks, `director.maxParallel`; worker types, `"workers"`); see
  `mvp4/README.md`, `PLAN-orchestrator-mode-serial.md`, `PLAN-orchestrator-mode-parallel.md`,
  `PLAN-orchestrator-worker-types.md` and `DECISIONS.md § mvp4 reference build` / `§ Slice D` / `§ Slice E`. Also for reading; mvp3 stays the frozen autonomous-mode reference.
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
  **Both files are chronological (oldest first).** Append new dated entries at the **end** of
  `SESSION.md`'s "Session log" and `DECISIONS.md`'s "Decision log"; never insert in the middle or at
  the top. Reversed decisions get a new entry at the end plus a `superseded by` note on the old one.

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

## Commands for mvp2 (repo root)

```sh
go build ./... && go vet ./... && gofmt -l .     # mvp2/tool/builtin.go has two old syntax errors; ignore
go test -race ./...                              # every package has its own tests; no network (fake provider)
go test ./contextwindow -run OrphanedTool -v     # one test family
go test -run XXX -bench . ./contextwindow        # window-strategy benchmarks
go run . [-config file] [-model m] [-workdir dir] [-debug]  # bare `go run .` = chat mode; read_file confined to -workdir (default .)
                                                 # -debug (or "debug": true): per-turn context dump + MCP protocol log on stderr
go run . -mode info [-config file]               # print config, built-in + MCP tools (model-facing names), then exit;
                                                 # starts each MCP server; exit 1 if one failed, 130 on Ctrl+C
go run . -mode plan [-obj objective.md]          # plan mode (stub so far): prints the objective as the planner will see
                                                 # it, or asks for one; needs a terminal; writes nothing yet
```

`mvp1/` and `mvp2/` are separate Go modules, so `./...` from the root does not touch them.

## Architecture for mvp2 (repo-root code)

The sections above describe mvp1 as a reference design. The code at the repo root uses the **opposite
dependency direction** (see `DECISIONS.md § Package dependency direction` and `§ As built`):

- **Capability packages are independent building blocks and never import `agent/`:**
  `model/` (shared message/tool types, `Provider` interface, OpenAI-compatible adapter),
  `memory/` (chat history), `contextwindow/` (`ChatContext` + the four window strategies +
  summarize/compact), `tools/` (the `Tool` interface, `ToolRegistry`, built-in tool setup) with
  `tools/builtin/` (`ReadFileTool`), `mcpconnect/` (MCP client connections + the adapter that
  wraps an MCP tool as a `tools.Tool`; `conn.go` + `tools.go`, with room for resources/prompts), and
  `hooks/` (the `Hook` interface, `BeforeTool(ctx, call) error`, and `DenyRules`; imports only `model`).
- **Helper packages import only the standard library**, never a package of this module or a third-party module, so any
  package may use them. `procutils/` (start a command in its own process group, kill the group; Unix and Windows files)
  and `textutils/` (`CutRunes`, `CutBytes`, `TrimPartialRune`: cuts that never leave half a character). Each is named for
  what it does, not `utils`/`helpers`. A function moves in when a second package needs it. The rule is checked by
  `Test_Unit_HelperPackages_ImportOnlyTheStandardLibrary` (`utilpackages_test.go`); a new helper package is added to its list.
- **`agent/`** holds the ReAct loop, the chat session and the wiring (`SetupAgent`, `RunAgent`,
  `ShutdownAgent`), and **imports the capability packages**.
- **Shared types** (`ChatMessage`, `ToolCall`, `ToolDef`) live in `model/`, below the capabilities,
  so no capability needs a type defined in `agent/`. A capability that wants an `agent/` type is the
  architectural regression to watch for (the compiler reports it as an import cycle).
  `tools.Tool` lives in `tools/` (not `agent/`), because the registry, the built-ins and
  `mcpconnect` all name it; `mcpconnect` imports `tools`, never the reverse.
- `config/` is a leaf (Config, file loading, flag parsing and overlay); the capability `Setup*`
  functions take a `config.Config`. `config/doc.go` lists every config key with its choices and default;
  a new `Config` field gets a line there. `main.go` imports `agent` and `config`. Setup takes a `Config`
  value — never `os.Args`, `flag.*` or `os.Exit`.
- Config precedence is **flags > config file > defaults**; an unspecified flag is `nil`, never a
  zero value.
- **Test doubles shared across packages** live in `model/modeltest` (a normal package, like
  `net/http/httptest`) because a `_test.go` file can't be imported. The session fixture
  (`newChatSessionFixture`) is `agent`-only.

### Invariants that are easy to break (mvp2)

- **Remote MCP requests never leave the configured origin.** `newRemoteHTTPClient` refuses redirects to a
  different scheme/host/port, and `headerTransport` refuses to send to one, because it re-adds the
  token on every redirect hop (and a 307/308 replays the body). Keep both layers if you touch the HTTP
  client; `http` -> `https` redirects are rejected for now (`DECISIONS.md § Deferred for Later`, item 6).
- **`read_file` stays inside `workDir` and never reads deny-listed names** (`.env`, `.env.*` but not
  `.env.example`, `*.pem`, `*.key`, `id_rsa*`, `.ssh/`, `.agent-trace`), plus any config `denyPaths` patterns (`builtin.DenyList`, .gitignore-style rules in
  `pathpattern.go`: config extends the list, never shrinks it), nor anything inside the configured
  `traceDir` (matched by resolved path, whatever its name). It resolves symlinks first, checks the *resolved*
  path, then opens through `os.Root`; `workDir` defaults to `.` and is set by config or `-workdir`. These rules live
  in `builtin.Workspace` (`tools/builtin/workspace.go`), built once in `SetupBuiltinTools` (even with no file tool
  enabled) and shared by the file tools. A new file tool must take the `*Workspace` and go through its `resolvePath` /
  `checkPath` / `openRoot`, not call `os.Open` on a model-supplied path
  (`DECISIONS.md § read_file is confined to workDir`, `§ File tools slice 0`). A tool that reads a file opens it with
  `Workspace.openFile`, passing the identity taken before the check (`resolvePath`, or a walk entry's `d.Info()`): it
  opens with `O_NONBLOCK` (opening a named pipe blocks forever), accepts only regular files (checked on the opened
  descriptor) and refuses a file swapped in after the check (`os.SameFile`; `DECISIONS.md § PR #31 review`).
  **Write tools** (`create_directory`, `write_file`) use `Workspace.resolveNewPath` (the path may not exist yet), which adds
  a write-deny on top of `checkPath`: `.git` and the `-config` file (`cfg.ConfigFile`) can be read but never written.
  `write_file` opens an existing file without `O_TRUNC`, checks it on the descriptor (`os.SameFile`), and only then
  truncates; a new file uses `O_EXCL`; it never creates parent directories; one call writes at most `maxWriteBytes`
  (1 MB; this is not a limit on total disk use). The approval question shows `approvalPreview`, not the whole content
  (`DECISIONS.md § Slice 4 built`).
  A new file or directory is created only through `Workspace.openDir` (the nearest existing directory above it, opened as its
  own `os.Root` and compared with the identity `resolveNewPath` approved), so a parent swapped for a symlink to `.git`
  after the check can't redirect the write. `openExisting` refuses to open for writing a file with more than one name
  (hard link), and `stillAtPath` refuses to truncate a file that was renamed away after it was opened
  (`DECISIONS.md § PR #35 Copilot review`).
  `copy_file` (source `resolvePath`, destination `resolveNewPath` + `openDir` + `openForWrite`, streamed in 256 KB pieces with
  the context checked, up to `copyFileMaxBytes` (default 100 MB), files only, no hard-linked source) and `move_file`
  (`resolvePathForWrite` + `resolveNewPath`, `openDir` on both parents, then `os.Root.Rename`: a real rename, no size cap,
  files only; across filesystems it fails with a message pointing at `copy_file`) check both paths
  (`DECISIONS.md § Slice 6 built`).
  `edit_file` (`resolvePathForWrite` → `openExisting` read-write, no `O_TRUNC`) replaces an exact `old_string` that must
  match once (or `replace_all`), refuses files over 1 MB or with a NUL byte, builds the result in memory, re-stats the
  file just before writing (refuses if size or mtime changed), and serializes edits of one file. The trace cuts file
  bodies (`trace.shortenArguments`); the approval question is one summary per call (`DECISIONS.md § Slice 5 built`).
- **After a trim, a context window must start at a `user` message.** Windows trim by message count
  and a turn is several messages, so a trim can land mid-turn: on a tool result (providers reject an
  orphaned one, and since a failed turn is not added back, the same bad context is re-sent forever)
  or on an assistant message (Anthropic requires a user-first context; it would also split the
  compaction summary's user/assistant pair). Each window strategy's `AddMessages` drops leading
  messages until a `user` message, and `clampToMax` (compaction) does the same for survivors. A new
  window strategy must do likewise; `Test_ContextWindow_AddMessages_NeverLeavesOrphanedToolMessages`
  runs every strategy. See `DECISIONS.md § Windows start at a user message after a trim`.
- **Ctrl+C stops foreground work, never the program (except during setup); only quitting stops background
  work.** The root context listens for SIGTERM only. `main` registers `os.Interrupt` once, for the whole run, on
  a 1-slot channel passed to `SetupAgent` and `runChatLoop`, so Go's default "Ctrl+C kills the process" never
  returns (that would skip `ShutdownAgent` and orphan MCP servers). **During setup, Ctrl+C aborts the program
  gracefully:** the MCP steps run under `createInterruptWatcher`, `SetupAgent` returns `context.Canceled`, `main`
  exits and its deferred `ShutdownAgent` closes the servers that had connected. At the prompt, the line editor
  treats Ctrl+C as a key and clears the input. Each turn gets `createInterruptWatcher(ctx, interrupts)`: a child
  context cancelled by Ctrl+C; `stop()` cancels it and waits for the watcher to exit, so nothing that outlives the
  turn or setup may keep it. **Stdio MCP servers run in their own process groups** (`procutils/procgroup_*.go`, called from `mcpconnect/conn.go`),
  so the terminal's Ctrl+C never reaches them; `McpConnection.Close` kills the group. `discardStrayInterrupts` runs *before* the watcher
  starts. A cancelled turn prints `[interrupted]` and adds nothing to memory. **Anything long-lived
  started from a turn (auto-compaction) must get the root context**, which is why `maybeStartAutoCompaction`
  is called from the loop, not from `handleUserInput`. Quit = Ctrl+D, `/quit`, `/exit` (or SIGTERM). See
  `DECISIONS.md § Chat loop and Ctrl+C`.
- **Two prompt readers (`agent/input.go`):** `ttyReader` (go-multiline-ny line editor) only when stdin is a
  real terminal; `plainReader` (line by line) for pipes, files and tests. Tests drive the editor through
  `newTTYReaderWith(&auto.Pilot{...}, io.Discard)`; the terminal must be set before the first `BindKey`.
- **Every tool call passes `checkBeforeToolHooks` (`agent/react.go`) before it runs**, after the unknown-tool and
  argument checks. A veto is returned as the tool result (`error: denied by rule "…"`), never by skipping the call,
  because every tool call needs a result. **Three levels** (`hooks.ToolRules`): a `denyRules` match is refused in every
  mode and never asked; an `askRules` match returns an error wrapping `hooks.ErrNeedsApproval`; no match runs. Deny
  wins over ask. **Only an `ErrNeedsApproval` veto can be overridden**: by `cs.AllowAskRules` (approval `allow-all`,
  which chat sets only after the user confirms it at startup in `confirmAllowAll`; it never touches deny rules), or by
  the user. Any other hook error is final. Each veto is traced as `tool_vetoed` (`denied` | `denied_by_user` |
  `allowed_by_user` | `allowed_by_config`). The user is asked only through `cs.PromptAllowDenyCall`, which `chatLoop`
  sets for a `ttyReader` and which reads through the line editor (`AskYesNo`), never a raw stdin read (a second
  reader would steal the next prompt line); `nil` means nobody to ask, so an ask is denied. Model text shown in the
  question is printed with `%q`. A new path that runs tools must go through this check. Rules match model-facing
  names (`<server>_<tool>`, case-insensitive), are built before any MCP server starts, and an invalid rule or an
  unknown `approval` value fails setup. `hooks` holds no rules of its own: the defaults are `GetDefaultConfig()`
  (`denyRules` empty; `askRules` `*delete*`, `*remove*` and the write tools), and a config file's list replaces that
  list's default (`[]` = no rules). See `DECISIONS.md § Approval gate v1 built`, `§ Policy out of the core` and
  `§ Approval levels: deny / ask / run`.
- **`run_command` is opt-in by `runCommands` and is not contained** (`tools/builtin/runcommand.go`; `DECISIONS.md § run_command plan`
  and `§ run_command built`). Enabled only when `run_command` is in `builtinTools` AND `runCommands` (bare program names, resolved
  to absolute paths once at setup, refused inside `workDir`) is non-empty; otherwise setup warns and skips it. No shell, empty
  stdin, environment limited to `DefaultCommandEnv` plus `commandEnv` (extend-only), own process group killed on context end and
  after exit, output cut at `commandMaxOutputBytes` (program killed). A running program bypasses every `Workspace` check; only `dir`
  is checked. Rules: `run_command:<glob>` in `denyRules`/`askRules` match the joined command line (`hooks.ToolRules.LevelFor`);
  rules without that prefix match tool names only; there is no allow list (not matched = runs), so `run_command` is deliberately
  not in the default `askRules`. Unreadable arguments fail closed to ask. A glob over a command line is not a detector.
  On Windows only the direct process is killed when the call ends (no Job Object), and the hard-link checks of the write tools are
  skipped (no link count there); both are accepted gaps with the real fix written down (`DECISIONS.md § PR #38 review`).
  **Best-effort argument guard:** `Workspace.checkCommandArgs` refuses (never asks) a call whose arguments, or `--flag=value` values,
  name a file `checkPath` refuses (deny list, `denyPaths`, trace dir), also via symlinks or `..`. It misses directories (`grep -r .`),
  interpreter-built names and `rev:path`; it is a guard against mistakes, not a boundary (`DECISIONS.md § run_command: arguments are checked`).
- **Timeouts:** `toolTimeoutSeconds` (default 300) bounds every tool call, MCP included (`executeToolCall`); `turnTimeoutSeconds`
  (default 600) bounds a whole turn (`handleUserInput`) and must not be below the tool limit (`checkConfig`). A tool must honour
  its context. Ctrl+C and the turn timeout are told apart from the tool's own deadline by which context ended.
  `mcpConnectTimeoutSeconds` (default 120) bounds each MCP server's connect at startup (`SetupMCPConns`, which connects all
  servers at once); a server that runs out is skipped, not a reason to abort setup. Listing tools (`SetupMCPTools`) is not bounded.
- **`MaxSteps` is the ReAct step limit** (`cfg.MaxSteps`, default 10 from `GetDefaultConfig`, overridable
  by `-maxsteps`; read via `cs.Config.MaxSteps` in `reActLoop`, no constant). It must be positive:
  0 or a negative value from a config file makes every turn fail with `ErrMaxSteps (limit 0)`.
  **`MaxTokens` is parsed and merged but not yet read**; don't assume `-maxtokens` takes effect.

## Code map (root module only)

`CODEMAP.md` is a generated map of every package: a directory tree, then each Go file's function and method
signatures. It is copied from the `# Codemap (auto-generated)` section at the end of each package's `doc.go`, which
`cmd/codemap` writes from the code. **Never edit those sections or `CODEMAP.md` by hand.**

```sh
go run ./cmd/codemap                         # regenerate every doc.go section and CODEMAP.md
go run ./cmd/codemap -check                  # fail if anything is stale (go test ./cmd/codemap runs this too)
go run ./cmd/codemap -lint                   # list functions with a missing or malformed header
go run ./cmd/codemap -code-unchanged HEAD    # fail if anything but comments changed since a git ref
```

- **After adding, removing or changing a function signature, run `go run ./cmd/codemap`** and commit the result.
- **Every function (exported or not) has a header**: a one-liner starting with the function's name, then `Notes:`
  (bullets for what the code does not make obvious; `Notes: none.` if nothing). No `Example:` sections. The format is in
  `cmd/codemap/doc.go`. Long explanations above a function belong in its Notes.
- **Package comments live only in `doc.go`.** A new package needs a `doc.go` with a hand-written overview.
- **Whoever changes a function body (you or an agent) rereads that function's header and updates its one-liner and Notes
  if they no longer match.** Nothing enforces this; Notes drifting is accepted as a minor defect, but do not leave a
  header you know is wrong. Signatures in the map cannot drift: they are generated.
- The `doc-keeper` agent (`.claude/agents/doc-keeper.md`) writes headers and edits comments only. After it runs,
  `-code-unchanged HEAD` must pass. See `DECISIONS.md § Codemap`.
- **Pre-commit hook** (`.githooks/pre-commit`): blocks a commit when the map is stale or a function lacks a header. It
  only checks, never stages. Enable once per clone: `git config core.hooksPath .githooks`. Skip with `--no-verify`.

## Conventions

- Keep explanations clear, concise, accurate but avoid unnecessary technical jargon when possible. Explain things as if I am a generalist mid-level software engineer.
- Prefix explanatory/teaching comments with `[agent]` so Jerry can grep Claude's commentary apart from
  ordinary code comments. Keep ordinary doc comments unprefixed.
- Concise, small files; no framework scaffolding, no dependency added without a reason.
- State the trade-off, including the honest downside, whenever choosing a language, library, or design —
  a bare choice reads as arbitrary here (see `tradeoffs/language-scorecard.md` for the expected form).
- New behaviour should arrive as a plug-in behind one of the five interfaces. If it can't, say why before
  touching `agent/`. (In `mvp1/` that means the kernel stays untouched; in mvp2, `agent/` may wire in
  new capability packages, but the loop itself should still not grow cross-cutting features.)
- When Jerry asks for a block of code, keep it simple: the most direct version that meets the request, with
  no extra helpers, abstractions, options, or tests he didn't ask for. Prefer newer stdlib/language features
  (go.mod targets Go 1.26) over hand-rolled helpers. Mention optional extras in a sentence instead of adding them.
