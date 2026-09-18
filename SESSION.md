# SESSION.md — mvp2 progress (last updated 2026-09-17)

Scratch handoff notes for `mvp2/` (branch `mvp2-do-myself`). Jerry writes the code;
teach-then-let-him-type.

Two tracks live under `mvp2/` right now — don't conflate them:

1. **`mvp2/` root** (`main.go`, `agent/`, `tool/`, `config.json`) — the real agent build,
   following mvp1's architecture. Last touched 2026-09-12; see "Track 1" below.
2. **`mvp2/learn/`** — a separate, standalone sandbox of incremental one-file programs
   building up an OpenAI-compatible chat loop from scratch (no `agent/` package, no
   shared module structure). This is where the last few sessions (2026-09-14 → 09-17)
   actually happened. See "Track 2" below. Full transcripts:
   `mvp2/learn/prompts/20260917-session-1.md`, `20260917-session-2.md`.

## Track 2 — `mvp2/learn/` (most recent work)

```
mvp2/learn/
  chat_simple.go                                    gen 1: chat loop, no memory
  chat_with_simple_memory.go                        gen 2: full history in a slice
  chat_with_sliding_context_window_memory.go         gen 3: fixed-size sliding window
  chat_w_diff_context_window_mgmt_strategies.go      gen 4: pluggable ContextWindow interface
```

Each file is a standalone `package main` snapshot of that learning stage — they all
redeclare the same top-level names (`Provider`, `Config`, `fatal`, etc.), so
**`go build ./...` in `mvp2/learn/` fails on purpose** (redeclaration errors across
files). Build/run one file at a time. Not a bug to fix — it's the point (each file is
a complete, readable stage).

### Current file: `chat_w_diff_context_window_mgmt_strategies.go`

Defines `ContextWindow` interface (`AddMessages`, `GetMessages`, `RemoveLast`) with
four interchangeable strategies, selected via the `WindowStrategy` const and
`createNewChatHistory`'s switch — all four cases are wired up:

- **`OffsetWindow`** — slice, re-slices from the tail when over `maxSize`
  (`messages[len-maxSize:]`). Simplest, but each trim keeps the old backing array
  alive (no realloc needed here since it's a sub-slice, cheap).
- **`InPlaceWindow`** — slice, `copy()`s remaining elements down to index 0 in place
  when over `maxSize`, avoiding letting the backing array grow unbounded across many
  trims.
- **`RingBufferWindow`** — fixed backing array (`messages []ChatMessage`, `maxSize`
  len), `head` = index of *next write slot*, `count` = live message count. O(1)
  writes via `head = (head+1) % maxSize`, no shifting.
- **`LLWindow`** — `container/list.List` (doubly linked). `AddMessages` evicts
  `Front()` before each push once at `maxSize`. O(1) push/evict, no pre-sized backing
  array, but per-node heap allocation vs. contiguous memory.

All four guarded by `sync.Mutex`; `GetMessages` returns a defensive copy.

### Bugs found and fixed this session (don't re-explain unless asked)

- **`RingBufferWindow.GetMessages`** had two candidate index formulas
  (`(head+i)%maxSize` vs. `(head-count+i+maxSize)%maxSize`) — only the second is
  correct. `head` means "next write slot," which only equals "oldest message slot"
  once the buffer has wrapped (`count == maxSize`). Before wrapping, `head` points at
  an empty slot and the oldest live message is still at index 0 — the first formula
  reads garbage during fill-up. Confirmed fixed (commented/dead alternative removed).
- **`RingBufferWindow.RemoveLast`** uses `count >= n` (not `>`) — correctly empties
  the buffer when `n == count`. Flagged as an inconsistency (not a bug) vs.
  `OffsetWindow`/`InPlaceWindow`'s `RemoveLast`, which both use strict `>` and
  silently no-op when `n == len(messages)` — arguably an off-by-one bug in *those*
  two, left unfixed pending Jerry's call.
- **`LLWindow`** (container/list version) reviewed end-to-end, no bugs found:
  evict-before-push keeps the maxSize invariant per-message even across a batch;
  `RemoveLast` guards `Len() > 0` so it can't panic calling `Remove` on nil past the
  front, and (unlike the two slice strategies) correctly empties on `n == Len()`.

### Concepts covered this session (don't re-teach unless asked)

- Ring buffer mental model: fixed array + `head`/`count` instead of shifting data;
  O(1) writes/removal vs. O(n) slice shifting once full.
- `container/list`: doubly linked, sentinel-root internally but behaves as a linear
  list externally (`PushBack`/`PushFront`/`Remove`/`Front`/`Back`, O(1) each).
  `Len()` is O(1) (stored counter, not a walk). `Element.Value` is `any` — needs a
  type assertion (`e.Value.(ChatMessage)`) to get the concrete type back.
- Trade-off named explicitly: ring buffer = fixed size known upfront, contiguous
  memory, best cache locality; linked list = dynamic size, no upfront allocation,
  but per-node heap allocation and pointer-chasing on traversal.

### Next steps for Track 2

- Decide whether to fix the `>` vs `>=` inconsistency in `OffsetWindow`/`InPlaceWindow`.
- `CircularLLWindow` dead code block (commented out near top of file, an earlier
  abandoned attempt) can probably be deleted now that `LLWindow` supersedes it.
- No tests yet for any `ContextWindow` implementation — worth adding before moving on,
  especially for the ring buffer's wraparound edge cases.

## Track 1 — `mvp2/` root (agent build, last touched 2026-09-12)

```
mvp2/
  main.go          flag parsing, Config struct, defaults, fatal(), signal handling, stub run()
  config.json      anthropic + fs MCP server + requires_approval
  tool/builtin.go  Grep tool — DOES NOT COMPILE YET (see below)
  go.mod           module github.com/jerryschen31/minagent
```

`go build ./...` currently fails. Nothing else is written: no `agent/` package,
no `tool/tool.go`, no `llm/`, `memory/`, `harness/`, `hooks/`.

### Concepts covered (don't re-teach unless asked)

- **Defaults pattern**: build `Config` pre-filled, then `json.Unmarshal` onto it —
  absent JSON keys are left untouched. Downside: explicit `0`/`""`/`false` is
  indistinguishable from absent; pointer fields are the fix if that ever matters.
- **Struct tags**: `json:"max_tokens"` is the wire-name adapter. Typos fail silently;
  unexported fields are invisible to `encoding/json`.
- **`fatal()` vs `defer`**: `os.Exit` skips defers. Hence the `main` → `run() error`
  split — `fatal()` only ever called from `main`, all cleanup on `defer` inside `run`.
- **`signal.NotifyContext`**: turns Ctrl-C/SIGTERM into context cancellation.
  `stop()` cancels the ctx *and* restores the signals' prior disposition (not
  necessarily "default"). Second signal is swallowed unless you call `stop()` early.
  `os.Kill`/SIGKILL can't be caught — only useful for *sending*.
- **Cancellation is cooperative**: the last thing debugged. `time.Sleep` ignores ctx.
  Anything blocking must take a ctx or be raced against `ctx.Done()` in a `select`.

### Decisions made

- **`requires_approval`** (renamed from mvp1's `approve`) = tools that need a human
  `[y/N]`. It is a *restriction* list, not an allowlist. Empty list ⇒ hook never
  installed ⇒ nothing gated. `["*"]` ⇒ everything gated.
- **MCP tool names are prefixed** `<server>_<tool>` (mvp1 `tool/mcp.go:97`), so
  approval entries must be `fs_write_file`, `fs_edit_file`, `fs_move_file`,
  `fs_create_directory`. No collision with builtins.
- **Grep is a Go-native `tool.Func`, not an MCP server.** Probed the alternatives:
  no official MCP server does content search (`filesystem`'s `search_files` is
  filename-glob only); npm's `mcp-ripgrep` still needs `rg` on PATH and pipes
  model-controlled strings through `spawn(..., {shell: true})` with hand-rolled
  escaping. Pure Go = zero deps, zero build tags, cross-platform.
- **Simplified Grep over the hardened version.** The guard that actually matters is
  the NUL-byte binary check: measured 24KB vs 2.99MB of output on this repo (124×).
  `Truncate` does not save you — the garbage comes first. Skip-list, streaming
  reads, and `ToSlash` deferred until they bite.

### Open items

1. **`tool/builtin.go` won't compile** — three fixes:
   - L29 `struct{ Pattern string, Path string }` → `struct{ Pattern, Path string }`
   - L82 missing trailing `,` after the `}` closing the `Fn:` field
   - missing import `github.com/jerryschen31/minagent/agent`
2. **Grep schema bug**: `"required"` is nested *inside* `"properties"` — move it out,
   or nothing is required and `regexp.Compile("")` matches every line.
3. **Missing prerequisites** for Grep: `agent` package (`agent.Tool`), `tool/tool.go`
   (`Func`, `Args`, `Truncate`), `confine` + `resolveExisting`, `const maxOutput = 16 << 10`.
   Port from mvp1 `tool/tool.go` and `tool/builtin.go:152`.
4. **`api_key_name` vs `api_key_env`** — struct tag and `config.json` disagree; the
   default masks it. Pick one.
5. **`config.json` is currently mandatory** in mvp2 (mvp1 treats the default path as
   optional). Confirm that's intended.
6. **Stub `run()` ignores `ctx`** — wrap the sleep in `select { case <-time.After(…):
   case <-ctx.Done(): return ctx.Err() }`.
7. `gofmt -w` — mixed tabs/spaces in `tool/builtin.go`.

### Next phase (Jerry's stated plan)

Tests and simple evals before building out the loop. `run(ctx, cfg) error` is the
seam: testable with `context.WithCancel`, no model needed. mvp1 drives its loop with
a scripted fake LLM (`go test ./agent -run TestReActLoop`) — same approach here.
