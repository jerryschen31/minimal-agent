# SESSION.md — mvp2 progress (last updated 2026-09-19)

Scratch handoff notes for `mvp2/` (branch `mvp2-do-myself`). Jerry writes the code;
teach-then-let-him-type.

Two tracks live under `mvp2/` right now — don't conflate them:

1. **`mvp2/` root** (`main.go`, `agent/`, `tool/`, `config.json`) — the real agent build,
   following mvp1's architecture. Last touched 2026-09-12; see "Track 1" below.
2. **`mvp2/learn/`** — a separate, standalone sandbox of incremental one-file programs
   building up an OpenAI-compatible chat loop from scratch (no `agent/` package, no
   shared module structure). This is where the recent sessions (2026-09-14 → 09-19)
   happened. See "Track 2" below. Full transcripts in `mvp2/learn/prompts/`
   (latest: `20260919-session-3.md`).

## Track 2 — `mvp2/learn/` (most recent work)

```
mvp2/learn/
  chat_simple.go                                    gen 1: chat loop, no memory
  chat_with_simple_memory.go                        gen 2: full history in a slice
  chat_with_sliding_context_window_memory.go         gen 3: fixed-size sliding window
  chat_w_diff_context_window_mgmt_strategies.go      gen 4: pluggable ContextWindow interface
  chat_w_memory_compaction.go                        gen 5: summarize + compact (CURRENT)
```

Each file is a standalone `package main` snapshot of one learning stage — they all
redeclare the same top-level names, so **`go build ./...` in `mvp2/learn/` fails on
purpose**, and the IDE shows a wall of "redeclared" errors. Build/vet/run one file at a
time: `go vet chat_w_memory_compaction.go`, `go run chat_w_memory_compaction.go`.
(Terminology covered: file = source file/program; package = files in one directory
sharing a `package` line; module = versioned set of packages via `go.mod`.)

### Current file: `chat_w_memory_compaction.go` (gen 5)

State at end of session 3: `go vet` and `gofmt` clean. **Not run interactively, no tests
yet.** Everything below was checked by reading + tracing, not execution. Auto-compaction
(below) is now wired in; the guard has never been exercised under `-race`.

`ContextWindow` interface now: `AddMessages`, `GetMessages`, `RemoveLast`,
`Snapshot() ContextState`, `Compact(state ContextState, summaryMsg ChatMessage) bool`,
`Clear()`. `ContextState{Messages, Gen}`. All four window types implement it
(Offset, InPlace, RingBuffer, LL), each with a `gen` counter.

**Compaction flow** (`compactChatHistory(ctx, p, w)`): `Snapshot()` → summarize
`state.Messages` via the provider (slow LLM call, outside any lock) → wrap the summary
text in a `ChatMessage` (role `user`, fresh `ID`, `Timestamp`) → `w.Compact(state, msg)`.
`Compact` returns `false` if the window was cleared/compacted since the snapshot; the
caller turns that into an error.

**Design decisions made this session (and why):**

- **ID set, not a per-message "replace" flag.** Jerry's idea: mark the messages being
  compacted; anything unmarked is new and survives. Refined to: every `ChatMessage`
  already has a unique `ID`, so build a `map[string]bool` from the snapshot and keep
  window messages whose ID isn't in it. Same semantics, but nothing is mutated on the
  shared state, so a failed summary needs no cleanup and overlapping compactions can't
  confuse each other.
- **ID set is built inside `Compact` from `state.Messages`** (helper `getMessageIDs`),
  not inside `Snapshot()` (keeps `Snapshot` a generic view). Must come from the frozen
  snapshot, never from live `GetMessages()`, or new messages would be dropped.
- **`Gen` is kept.** IDs alone can't tell "cleared mid-summary" from "every compacted
  message was legitimately evicted"; `Gen` can. It also rejects a stale second
  compaction. Cost: one int. Only matters once compaction goes async (loop is
  synchronous today).
- **`Compact` is per-window mechanics only; policy stays in `compactChatHistory`.** The
  window can't call the LLM and must not hold its mutex across a network call. Smarter
  policies (keep last N verbatim, tiers) = a different message/ID subset chosen by the
  caller. Downside noted: passing a subset `ContextState` stops it being a literal
  snapshot.
- **`Compact` takes `ContextState`** (Jerry's call) rather than separate ids + gen args,
  so Messages and Gen can't come from different snapshots.
- **`Snapshot()` returns no error** — in-memory copy can't fail. Revisit only for a
  file/DB-backed window.
- **`clampToMax(msgs, maxSize)`** shared helper, called in all four `Compact`s. Keeps
  summary at `[0]` + the newest `maxSize-1` survivors. Guards the overflow case: window
  full, all compacted messages evicted during the summary call → `1 + maxSize` messages
  (would panic the ring buffer, and leave the LL stuck at `maxSize+1`).
- **Summary role is `user`**, not `system`, for portability (a second `system` message
  mid-history breaks Anthropic/some Ollama templates). Cost: consecutive `user`
  messages after compaction — fine for OpenAI/Ollama, needs merging for Anthropic.

**Slash-command parsing in `runLoop`** (reviewed, correct): `TrimSpace` the line, then
`strings.Cut(line, " ")` → `lineFirst`/`lineRest`; `switch lineFirst` for `/exit`,
`/clear`, `/summary`|`/summarize` (read-only, prints summary), `/compact` (rewrites
history), `default` → "Unrecognized command" + `continue`. After the switch, one shared
tail trims `lineRest` and sends it as a normal prompt if non-empty (`/clear hi` works).
`/clearing` no longer matches as `/clear` (old `HasPrefix` did).

### Auto-compaction (session 3)

`runLoop` now compacts in the background when the window nears full. Pieces:

- **Window setup moved to `runAgent`** (step 2, "setup memory and context") and passed into
  `runLoop`. `MaxContextWindow` is a package const. `runAgent` should `return err` rather
  than `fatal(err)` (defers) — Jerry said he fixed this; re-verify.
- **`ContextWindow` gained `GetSize()` and `GetMaxSize()`** (Jerry's call: "size" is generic,
  so a token-based window can implement it later). Computed on demand, not cached in a
  field — `len(w.messages)` / `w.count` (ring buffer; its backing array is always `maxSize`
  long so `len()` would be wrong) / `w.messages.Len()` (list, O(1)). A cached counter would
  duplicate state the four `AddMessages`/`Compact` paths must keep in sync.
  `GetMessages()` copy was never the concern (≤ ~20 structs, next to a multi-second LLM call).
- **Threshold** = `int(float64(chatHistory.GetMaxSize()) * AutoCompactThresholdFrac)`,
  frac currently 0.9 (earlier note said 0.8 — deliberate choice either way). Based on the
  window's real max size, not `MaxContextWindow`, because `buffer` is **kept** in
  `createNewChatHistory` (Jerry's decision; headroom for system prompt + pending user
  message), so the window is smaller than the const.
- **Fire-on-demand, not a ticker**: after `AddMessages` each turn, check
  `GetSize() >= threshold && compactInProgress.CompareAndSwap(false, true)`, then `go func`
  with `defer compactInProgress.Store(false)` as its first line. Size check first so the
  flag is only claimed when work will start.
- **`compactInProgress atomic.Bool`** is a local in `runLoop` (not global, not per-window),
  shared by the auto path and `/compact`. Must not be copied (pass `*atomic.Bool`).
  Per-window flag rejected: repeated 4x, and "a compaction is running" is policy, which
  stays out of the windows.
- **`/compact` claims the same flag**: on `CompareAndSwap` failure prints "already in
  progress" and `continue`s (skip, not wait — waiting needs a WaitGroup/channel). Release
  is an explicit `Store(false)` right after `compactChatHistory` and *before* the error
  check; `defer` can't be used in a `case` because it fires when `runLoop` returns.
  Fragile if anything is later inserted between claim and release.
- `Gen` still rejects the loser if `/clear` or a second compaction lands mid-summary.

### Bugs found and fixed this session (don't re-explain unless asked)

Session 3: `GetSize`/`GetMaxSize` pasted onto the wrong receivers (duplicate on
`InPlaceWindow`/`LLWindow`, none on Offset/RingBuffer) → compile error; `int(intVal *
0.9)` doesn't compile (convert to `float64` first); threshold equal to real capacity
(fired only when full); no single-flight guard (every turn re-launched a compaction).

Session 2:

- `map[int]struct{}` for string IDs; `Compact` with no `Gen` check; `InPlaceWindow`
  signature left half-updated; interface method with mixed named/unnamed params.
- Clamp attempts: `newMessages[:maxSize]` (panics when shorter, drops newest) →
  `newMessages[len-maxSize:]` (drops the summary) → correct summary-first version.
- Parsing: `lineFirst[0] == "/"` (byte vs string), unused `lineHasSpace`, normal chat
  lines sent untrimmed with `\n`, `default:` falling through to the LLM,
  `summarizeChatHistory` given the window instead of `GetMessages()`.

### Concepts covered (don't re-teach unless asked)

- `sync/atomic`: why check-then-set on a plain `bool` races (two steps + data race);
  `atomic.Bool` (Go 1.19+, zero value usable) with `Load`/`Store`/`Swap`/`CompareAndSwap`;
  CAS = one indivisible "flip false→true, tell me if I won"; `Load` is for display, never
  for deciding to start work; `defer` inside a goroutine's func literal fires when the
  goroutine ends, `defer` in a `switch case` fires at function end.
- Go hash set: `map[K]struct{}` (zero-byte value; `struct{}{}` = the type + a value of
  it) vs `map[K]bool`; two-value lookup `_, ok := m[k]`.
- "Missing field" in Go = zero value (`m.ID == ""`); options: skip / error / panic.
- `strings.Cut` (first separator; not found → `(s, "", false)`), `strings.Fields`
  (any whitespace run; empty slice on blank), `strings.TrimSpace` (both ends).
- `continue` inside a `switch` targets the enclosing `for`; `break` would not.
- Passing an interface value (`w ContextWindow`), not a pointer to it.

### Open items / next steps for Track 2

1. **Tests first.** For `Compact`: normal (snapshot, add 2, compact → `[summary, m5,
   m6]`), stale `Gen` after `Clear()`, overlapping compactions (second returns false),
   and the overflow case (full window, all compacted messages evicted) — run with
   `-race`. Plus a table test for `clampToMax` (under max, exactly max, max+1 with
   summary still `[0]`, `maxSize == 1`). Run each of the four window types.
2. ~~Nothing triggers compaction automatically~~ — **done in session 3** (see above).
   Still to do: a `-race` test proving the guard. Fake `Provider` (a struct whose `Chat`
   sleeps and bumps an `atomic` counter), fill window to threshold, fire the trigger twice,
   assert one provider call. Also `/compact` while auto is running → "in progress".
   Small polish left: the auto path doesn't print when skipped (good); goroutine prints
   can land mid-line over the `> ` prompt (cosmetic; a channel drained by `runLoop` is the
   later fix); a failing provider retries every turn while over threshold (add backoff if
   annoying); `/clear` mid-compaction prints "compaction failed" (use the sentinel below).
3. **Wire format.** `ChatMessage`'s JSON tags send `id`/`timestamp` to the API. Ollama
   ignores them; I believe OpenAI rejects unknown message fields (unverified — default
   config is OpenAI). Fix = small `{role, content}` wire struct converted just before
   the request. Non-2xx errors also drop the response body, which hides this.
4. Sentinel error (`ErrCompactionStale`) instead of a bare `fmt.Errorf` once callers
   need to distinguish "discarded, retry" from real failures.
5. Stale comment above `compactChatHistory` ("single system message" — it's `user` now);
   it also prints from inside the function.
6. Carried over: `>` vs `>=` in `OffsetWindow`/`InPlaceWindow.RemoveLast` (silently
   no-ops when `n == len`); dead `CircularLLWindow` block in gen 4 if still present.

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
