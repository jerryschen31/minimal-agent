# PLAN: info mode (`-mode info`)

Status: **all steps built**: 1–4 and 6 on 2026-10-23 (`DECISIONS.md § Info mode (-mode info) built`), step 5 (deny rules) on 2026-10-28 (`DECISIONS.md § Approval gate v1 built`). **Built before `PLAN-approval-gates.md`** (decided 2026-10-23): rules are easier to write once you can see the real tool names. Companion to `PLAN-approval-gates.md`: deny rules match on tool names, so you need a way to see the exact names (and what the agent can reach) before writing rules.

## Goal

`go run . -mode info [-config file] [-workdir dir]` prints what the agent *would* have if it started, then exits. No model call, no chat loop, no trace files.

```
minimal-agent v0.1.13 — info
config file:  config.json                   (or "none, defaults only")
working dir:  /Users/jerry/gh/public/minimal-agent  (resolved, absolute)

Configuration
  … same safe allowlist as /config (config.PrintConfig) …
  api key:      OPENAI_API_KEY is set        (yes/no only, never the value)

Built-in tools (1)
  read_file           Read a text file inside the working directory
                      never reads: .env, .env.*, *.pem, *.key, id_rsa*, .ssh/, .agent-trace

MCP servers (2)
  filesystem  stdio  npx (4 args)  connected   server: secure-filesystem-server 0.2.0   14 tools
  github      http                FAILED      connecting: 401 Unauthorized

MCP tools (14)
  filesystem_read_file        read-only     Read the complete contents of a file
  filesystem_write_file                     Create a new file or overwrite …
  filesystem_move_file        destructive   Move or rename files …
  …

Deny rules (once PLAN-approval-gates v1 lands)
  *delete*     (default)    matches: github_delete_file
  *remove*     (default)    matches: nothing
  my_rule      (config)     matches: nothing   ← warning
```

(Labels updated 2026-10-29: the defaults now come from `config.GetDefaultConfig` and a config file's list replaces
them, so "built-in" became "default"; see `DECISIONS.md § Policy out of the core`.)

## What it lists, and why

What you asked for:
1. **Working directory**, resolved to an absolute path. `read_file` is confined to it.
2. **Configuration**, through `config.PrintConfig`, so the credential-safe allowlist is reused, not duplicated.
3. **Built-in tools.**
4. **MCP tools**, after actually connecting to each server and listing its tools.

Suggested additions:
5. **Tool names exactly as the model sees them** (`<server>_<tool>`, e.g. `filesystem_write_file`, not `filesystem:write_file`). These are the strings deny rules match.
6. **First line of each tool description.** Enough to tell what a tool does without flooding the screen.
7. **MCP annotations**: `read-only`, `destructive`, `idempotent`, from the server's tool hints. The fastest way to spot candidates for deny rules. Shown as hints, since a server can claim anything.
8. **Per-server status**: transport, connected or the error, the server's own reported name/version, tool count.
8a. **What each stdio server will run** (guardrail against a tampered config, decided 2026-10-23): the program and the argument count, e.g. `npx (4 args)`. Never the arguments or env themselves: they can hold tokens (`--token=…`). Enough to notice `filesystem` suddenly running `curl` or `sh`. Shown even when the server fails to connect. Downside: a tampered argument with the same program and count looks the same; the first-run trust prompt (`PLAN-approval-gates.md § Later`) is what catches that.
9. **Which config file was loaded**, or that only defaults applied. Explains a surprising value.
10. **Whether the API key env var is set** (yes/no). The most common "why doesn't it work" answer. Never print the value.
11. **`read_file`'s fixed deny list**, so you know which files the model can never read, independent of deny rules.
12. **Deny rules and what each one matches** (after approval-gates v1). A rule that matches no tool is printed with a warning: usually a typo, or a `:` instead of `_`.

Left out on purpose:
- **MCP server `args`/`env`/`headers`/`url`**: they hold tokens (same rule as traces and `/config`). Only the stdio `command` and an argument count are shown (item 8a).
- **Full input schemas**: long; use `-debug` or a later `-mode info -v` if needed.
- **A model connectivity check**: it costs a request and can fail for reasons unrelated to config. Possible later as `-check`.
- **`--json` output**: useful for scripts, not needed to write rules. Later.

## Where it sits

```
main.go
  ParseFlags → SetDefaultConfig → FlagsOverlay          (existing)
  if cfg.AgentMode == "info":                            ← new
      agent.PrintInfo(ctx, cfg)   then return            (skips SetupAgent / RunAgent)
  else: SetupAgent → RunAgent → ShutdownAgent            (existing)

agent.PrintInfo(ctx, cfg)                                ← new, agent/info.go
  ├─ config.PrintConfig(cfg) + config file, workdir, api-key-set
  ├─ tools.SetupBuiltinTools(...)  → list GetToolDefinition()
  ├─ for each MCP server (sorted): connect, list tools, print, Close
  └─ (later) deny rules: hooks.DenyRules.Matches(name) per tool
```

Why `PrintInfo` and not `SetupAgent` + a new `RunAgent` branch:
- `SetupAgent` creates the trace recorder (a trace file per run) and the provider. Info mode should leave no files and should work with no API key.
- It reuses the same setup functions (`SetupBuiltinTools`, the MCP connect/list functions), so what info prints matches what chat mode builds. Honest downside: two call sites wire the same setup functions, and they could drift. A test that compares tool names from both paths (with a fake MCP server, as `mcpconnect` tests already use) guards that.

Why in `agent/`: it needs `tools`, `mcpconnect` and `config`, and `agent/` is the package that wires capabilities together. `main.go` only dispatches.

## MCP connection

Done ahead of this plan (2026-10-23, `DECISIONS.md § One failing MCP server no longer drops the others`): `mcpconnect.ConnectMCPServer(ctx, name, cfg)` connects one server, and `SetupMCPConns` skips a failing server instead of dropping all of them. Info mode calls `ConnectMCPServer` once per server (sorted), so each server gets its own status line.

To show annotations and server info, `mcpconnect` needs to expose them. `mcpTool` keeps only `safeToRetry` today. Add a small exported read accessor (e.g. `Hints() (readOnly, destructive, idempotent bool)`) or an `InfoOf(tools.Tool)` helper in `mcpconnect`; `tools.Tool` itself does not change. Server name/version come from the session's initialize result.

Every connection is closed before `PrintInfo` returns (`defer`), and `main` returns normally, so no MCP child process outlives info mode.

## Config and flags

- `ModeInfo = "info"` in `config/flags.go`; `validateMode` accepts it. `-query`, `-mission`, `--json` with `-mode info` are errors (same pattern as the other modes).
- `agentMode: "info"` in a config file also works, though a flag is the normal use.
- Config file path: `SetDefaultConfig` knows which file it loaded; return or record it so info can print it (small change in `config/`).

## Errors and exit code

- A config error fails before info runs, as today (exit 2).
- A failed MCP server is printed in its row; info continues with the other servers.
- Exit code 0 when everything connected, 1 if any MCP server failed, so a script can check `-mode info` as a preflight.

## Build order (top-down)

1. `main.go` dispatch + `ModeInfo` in flags/validation; `agent.PrintInfo` printing workdir, config file, config, API-key-set. Test: `PrintInfo` output contains the workdir and no secret values (reuse the credential-leak style of the `/config` tests).
2. Built-in tools section.
3. The MCP servers and tools sections with status, the command + argument count, and annotations. Test that no argument or env value appears in the output. Test with the existing fake/in-memory MCP server setup; a failing server must not hide a working one; connections closed.
4. Exit code.
5. Deny-rules section, after approval-gates v1 step 2 (so steps 1–4 and 6 ship first, as their own PR).
6. Docs: `DECISIONS.md` entry, `SESSION.md`, README/CLAUDE.md commands line.

## Honest downsides

- **It starts MCP servers.** `-mode info` runs each stdio server's command and connects to each remote one, which is real side effects (processes, network, possibly auth). That's needed to list tools; worth a line in the help text.
- **A snapshot, not a guarantee.** An MCP server can change its tools between `-mode info` and the next run; rules should be reviewed against the names at run time too (approval-gates' "rule matches nothing" warning could also print at chat startup later).
- **A second wiring path** (see "Why PrintInfo"), guarded by a test.
