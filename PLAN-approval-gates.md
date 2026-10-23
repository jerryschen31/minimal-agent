# PLAN: approval gates

Status: **plan only, nothing implemented**. Build-order step 2 in `DECISIONS.md § Build order: tracing core and approval before the autonomous port`. Follows tracing (PR #20) and the L1 digest.

- 2026-10-22: first full design (allow + deny lists, prompts, session grants, MCP trust). Kept below under "Later".
- 2026-10-23: **cut to v1.** The goal is to learn how hooks are built and to stop dangerous calls without bothering the user, not a complete approval system. Companion plan: `PLAN-info-mode.md` (lists the exact tool names that deny rules match against).

## Goals for v1

1. Learn the hook pattern: one interface, a list of hooks, the first veto wins.
2. Stop potentially dangerous tool calls as seamlessly as possible. Everything not on a short deny list runs without a prompt. The user is asked only when a call was denied, and only in chat.

## Where it sits

```
runToolCall(call)
  ├─ lookup tool, check args are an object                      (existing)
  ├─ for each hook in session.Hooks:                            ← new
  │     err := hook.BeforeTool(ctx, call)
  │     if err != nil:
  │         chat mode → "allow once? [y/N]"  (ask-on-deny)
  │             y → run the tool
  │             N / Enter / Ctrl+C / no terminal → denied
  │         other modes → denied
  │     denied → error observation "denied by rule …"; skip the tool
  ├─ callToolSafely                                              (existing)
  └─ record tool_finished                                        (existing; a denial is a failed call)
```

## The interface

```go
// package hooks (capability package: never imports agent/)
type Hook interface {
    BeforeTool(ctx context.Context, call model.ToolCall) error // non-nil = veto, the error text is the reason
}
```

- **A list, run in order, the first veto wins.** Same as mvp1 and most middleware. A new policy is a new `Hook`; `runToolCall` doesn't change again.
- **A veto is an `error`.** That fits the invariant that every failure becomes an observation the model can recover from.
- **No `AfterTool` yet.** Nothing needs it. Tracing already lives in `runToolCall` and the `TracedProvider` decorator.

## The one implementation: `DenyRules`

- A rule is a glob on the **tool name the model sees** (`path.Match` syntax), e.g. `filesystem_write_file`, `filesystem_*`, `*_delete_*`. MCP tools are named `<server>_<tool>` (`mcpconnect/tools.go`); `-mode info` prints these names.
- Once `run_command` exists, a rule may also name the program: `run_command:rm`. Not built in v1 (no `run_command` yet); the parser rejects the `:` form until then, so nobody writes a rule that silently matches nothing.
- **Built-in default list in code**, plus config `denyRules []string`, which **adds to** the defaults and never replaces them. With no `run_command` yet, the defaults target destructive MCP tool names (`*_delete_*`, `*_remove_*`, `*_move_*`, …); the list grows with `run_command`.
- An invalid pattern is a startup error (fail closed).
- The veto reason names the rule: `denied by rule "*_delete_*"`, so the model picks another route instead of retrying.

## Ask-on-deny (chat only)

- Shown **only for a denied call**, so prompts stay rare: `filesystem_delete_file was denied by rule "*_delete_*". Allow once? [y/N]`.
- Default is No. Enter, `n`, Ctrl+C and EOF all mean deny. Ctrl+C denies the call and does not end the turn (the model sees the denial and can adapt).
- "Once" means this one call. No session grants in v1.
- Only when stdin is a real terminal (the same check `ttyReader` uses). Pipes, tests, autonomous and worker modes never ask: denied.
- Parallel tool calls (later) must serialize prompts with a mutex; v1 runs tools in order, so not needed yet.
- How the prompt is wired: the asker is a small function on the session (`AskOnDeny func(call, reason) bool`), nil outside chat. It is not a `Hook`, because asking changes the outcome of a veto rather than issuing one. `hooks/` stays free of terminal code.

## Tracing and digest

- **No new event in v1.** A denial goes through the existing `tool_finished` with an error: `denied by rule "…"`. The digest already renders it as `(failed: denied by rule …)`.
- **An approved-once override gets a `tool_approved {call_id, rule}` event** (decided 2026-10-23), recorded only for asked-and-approved calls, before the tool runs. `tool_finished` has no field for it, and putting it in the tool result would show it to the model. Digest: `(approved once, rule …)`, plus a footer count, because an override of a safety rule is exactly what an auditor wants to see.

## Config

| Field | Flag | Default | Notes |
|---|---|---|---|
| `denyRules` | none in v1 | `[]` (added to the built-in list) | printed by `/config` and `-mode info` |

## Build order (top-down)

1. `runToolCall`: the hook loop and the denial → error observation, against a stub hook. Test: a vetoing hook means the tool never runs and the model gets the error.
2. `hooks/`: the `Hook` interface and `DenyRules` (glob match, defaults + config, invalid pattern = error). Table tests.
3. Wire it: `SetupAgent` builds the hook list; config field; `/config` prints the rules.
4. Ask-on-deny in chat: `AskOnDeny` on the session, the terminal prompt, Ctrl+C/EOF = deny; test through the plain reader that `y` runs and anything else denies.
5. The `tool_approved` event and its digest line.
6. Docs: `DECISIONS.md` entry, `SESSION.md`, `trace/doc.go` if an event is added.

## Honest downsides

- **No in-the-moment escape outside chat.** Autonomous mode just denies; you change the config and rerun. Fine for a short, truly dangerous list; annoying for a broad one, so keep it short.
- **Name and program rules are easy to dodge.** Once `run_command` exists: `sh -c 'rm …'`, `find -delete`, `git clean -fdx`, `python -c "shutil.rmtree()"`. This stops mistakes, not a determined model. A real sandbox is still deferred.
- **Until `run_command` lands, there is little to block.** Today only MCP tools are risky. The gate earns its keep in build-order step 3.
- **Name globs on MCP tools depend on servers naming tools honestly.** A server that calls its delete tool `tidy` won't match. `-mode info` shows MCP annotations (`destructiveHint`) to help write rules; a hook that denies on `destructiveHint` is a possible later addition (hints are claims by the server, not guarantees).

## Later (from the 2026-10-22 design; each is a new `Hook` or an add-on, no interface change)

- **Allow-list hook**, combinable with deny, deny wins. The real control for `run_command` programs.
- **Rule files by flag**: `-deny file`, `-allow file`; one rule per line, `#` comments.
- **Ask on unmatched calls** in chat, with `[a]lways this session` grants (narrow: exact program + subcommand; memory only, never written back).
- **MCP trust at a server's first call**: `[y]es once / [n]o / [t]his tool always / [s]erver always`; persistent form `mcpServers.<name>.autoApprove`. Never show or trace MCP `command`/`args`/`env`/`headers`.
- **`-approve allow-all` / `"approval": "allow-all"`**: skip asks and the allow list; deny rules still apply; recorded in `run_start` and as `source: "allow-all"`.
- **`tool_decided` event** for every call: `{call_id, verdict, source, rule}`; digest counts.
- **Autonomous / orchestrator**: same gate, no asker, unmatched = deny unless allow-all; each worker enforces its own policy from its config, never trusting the director's decision. Settle at the port.
