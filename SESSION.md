# SESSION.md — mvp2 progress (last updated 2026-09-22)

Scratch handoff notes for `mvp2/` (branch `mvp2-do-myself`). Jerry writes the code;
teach-then-let-him-type. This file is written so a **fresh session with no memory of prior
conversations** can pick up exactly where things left off — read top to bottom before doing
anything. A full transcript of the session that produced this file:
`mvp2/learn/prompts/20260921-session-refactoring.md`.

## How to work in this session (interaction style — read this first)

Jerry asked explicitly that this be captured, not just the code state. This is how the last
several sessions have worked well; keep doing it this way.

- **Teach-then-let-him-type is the default.** Explain the concept, give the smallest next
  slice (a struct shape, a method signature, an old→new translation table), then stop and
  wait for Jerry to write it and share it back for review. Don't write whole functions/files
  unless he explicitly says "fix this for me" / "do this for me" — that's the exception, not
  the norm, and it happened rarely (e.g. a batch of small, already-agreed-upon fixes).
- **Verify against the actual file before reviewing anything — don't trust memory of what a
  struct/function looked like a few turns ago.** The code changes fast, often in ways not
  narrated. Use Read/Grep/Bash to check current signatures. Before claiming something compiles
  or is broken, actually run it and quote the real output:
  ```sh
  cd mvp2/learn
  go build -o /dev/null chat_w_memory_compaction_refactor.go
  go vet chat_w_memory_compaction_refactor.go
  gofmt -l chat_w_memory_compaction_refactor.go
  ```
  **Never `go build ./...` or trust the IDE's whole-directory diagnostics** — every file in
  `mvp2/learn/` is a standalone `package main` snapshot that intentionally redeclares the same
  top-level names (see "Two tracks" below), so a directory-wide build/lint always fails with a
  wall of `redeclared` errors. That's expected noise, not a signal. Only single-file builds are
  meaningful here.
- **Separate "expected, not-yet-due" build errors from genuinely new bugs when reporting.**
  Mid-refactor, most build errors are just downstream code that hasn't been rewired yet (a
  planned later step). Naming which errors are already-tracked and which are new keeps Jerry
  from re-chasing something that's already on the list.
- **For multi-call-site renames/refactors, give an old→new mapping table, not prose.** This
  scaled well for translating whole functions (e.g. moving `runLoop`'s body into
  `handleUserInput`) — a table of exact old code → exact new code, one row per change.
- **Every design recommendation comes with the honest trade-off, and where relevant, the
  concrete trigger that would flip the recommendation.** Never a bare "do X." E.g.: "don't
  extract the window types into a package yet, but here's exactly what would make that the
  right call" (Track 1 resuming and needing them).
- **When something sounds appealing but has a subtle correctness problem, trace it through
  concretely** — don't hand-wave "that could be risky." Walking the actual index arithmetic
  of `RingBufferWindow`'s wraparound, or the exact interleaving that loses a message during a
  decomposed `Compact()`, is what caught real bugs and built trust. Do the same level of rigor
  before recommending a design change.
- **`AskUserQuestion` (the structured multi-choice tool) was tried once for a real
  architecture fork; Jerry declined it in favor of free-form discussion.** Default to laying
  out options with trade-offs in prose and asking an open question at the end; only reach for
  the structured tool again for a genuinely blocking multi-way decision, and expect it may be
  declined.
- **Update this file continuously, right after each resolved decision or completed step** —
  not saved up for the end of a session.
- **When reviewing code Jerry wrote:** read the whole relevant function, not just a pasted
  excerpt (excerpts can be stale relative to the file on disk). Check the build. List
  correctness/compile-breaking bugs before style/consistency notes. Compliment genuine
  improvements Jerry makes beyond what was suggested — several of his own design choices in
  this refactor (e.g. centralizing the compaction "announce + release lock" logic inside
  `compactChatContext` itself) were better than the original suggestion; say so specifically.
- **Tone/format:** concise, headers and small tables over long prose, avoid unnecessary
  jargon (per `CLAUDE.md`). The `[agent]` comment prefix convention is for comments *inside
  code Claude writes*, not for chat responses or this file.

## Current state

Working file: **`mvp2/learn/chat_w_memory_compaction_refactor.go`**. This is a from-scratch
refactor of the predecessor file `chat_w_memory_compaction.go` (still present, untouched, kept
as the last-known-good reference — see "Track 2 history" below), splitting the single
`ContextWindow`-does-everything design into separate history/context/session types.

**Milestone reached 2026-09-22: the refactor file compiles clean, standalone.**
`go build`/`go vet`/`gofmt -l` all pass, and it links into a real binary (verified, not
assumed). The full `ChatHistory` / `ChatContext` / `ChatSession` / `handleUserInput` /
`runLoop` / `runAgent` chain is wired together and working.

**Not yet done:**
- An actual interactive run (needs a live provider API key + real stdin — better done by
  Jerry directly than from here: `cd mvp2/learn && go run chat_w_memory_compaction_refactor.go`).
- A small dead-comment cleanup left in `runAgent` (an old commented-out
  `createNewChatHistory(...)` block, fully superseded).
- **The test suite — this is the explicit next step Jerry said he'll pick up.** See
  "Next up" below. This was the original goal of the whole multi-session refactor: split
  history from context specifically to make the code testable, then write the tests. That
  part is finally unblocked.

## Architecture reference (current, verified shape — read before touching the code)

Four types, each answering a different question:

| Type | Kind | Answers | Key methods |
|---|---|---|---|
| `ContextWindow` | interface | how are messages stored/evicted? (pure data structure) | `AddMessages`, `GetMessages`, `RemoveLast`, `Clear`, `GetSize`, `GetMaxSize` — **6 methods**, nothing compaction-related |
| `OffsetWindow`/`InPlaceWindow`/`RingBufferWindow`/`LLWindow` | structs | four different storage strategies implementing `ContextWindow` | each has its own `mu sync.Mutex`, own `maxSize` |
| `ChatHistory` | interface | what was ever said? (durable, append-only, single source of truth) | `GetMessages() []ChatMessage`, `Append(msgs []ChatMessage)` — **no `Clear()`**, deliberately (history survives compaction) |
| `InMemoryChatHistory` | struct | in-memory `ChatHistory` impl | `mu sync.Mutex`, pointer receivers throughout |
| `ChatContext` | struct | what does the model see *right now*? (compactable cache over history) | see below |
| `ChatSession` | struct | everything needed to run one turn | `Provider`, `MsgHistory ChatHistory`, `MsgContext *ChatContext`, `SystemMsg ChatMessage` (built once at construction via `createSystemMessage`), `UserID string`, `InBuffer io.Reader`, `OutBuffer io.Writer` |

**`ChatContext`** is the one with real complexity, all load-bearing:
```go
type ChatContext struct {
	mu                   sync.Mutex     // guards window + gen together
	window               ContextWindow  // unexported — nothing outside ChatContext may touch it directly
	gen                  int            // generation counter, moved here from the window types
	autoCompactThreshold int
	compactInProgress    atomic.Bool
}
```
Methods: pass-throughs to `window` under `c.mu` (`AddMessages`, `GetMessages`, `RemoveLast`,
`GetSize`; `GetMaxSize`/`GetWindow` deliberately skip the lock — `maxSize` is immutable after
construction, so there's nothing to protect); `IsAutoCompactionNeeded() bool`
(`window.GetSize() >= autoCompactThreshold`); `ShouldStartCompaction() bool`
(`compactInProgress.CompareAndSwap(false, true)` — **must be checked**, it has a side effect,
see the "why `Window` is hidden" note below); `EndCompaction()`
(`compactInProgress.Store(false)`); `Snapshot() ContextState`; `Compact(state ContextState,
summaryMsg ChatMessage) bool`.

**Why `Snapshot`/`Compact`/`gen` moved off `ContextWindow` onto `ChatContext`, and why `window`
is unexported** (this was the single hardest piece of design work this session — read this
before changing anything here): `Snapshot`'s `Messages` field was provably redundant with
`GetMessages()` (verified against `RingBufferWindow` — literally the same traversal loop), so
`Snapshot` could move up cleanly. `Compact`, however, needs "read current state, check `gen`
hasn't changed, write new state" to be **one atomic operation** relative to any concurrent
`AddMessages` call (e.g. the main loop adding a message while a background auto-compaction is
mid-summarization — this is a real, designed-for scenario, not hypothetical). Decomposing
`Compact` into separate `GetMessages`+`Clear`+`AddMessages` calls only stays safe if **nothing
can reach the window except through `ChatContext`'s own lock** — which is exactly why `window`
is unexported and every window-touching operation funnels through `ChatContext.mu`. Each window
type also keeps its own internal `mu` regardless (not redundant — needed so each type can still
be tested standalone, and it's cheap defense-in-depth; nests harmlessly under `ChatContext`'s
lock).

**`compactChatContext(ctx, cs *ChatSession, compactionType string) (ChatMessage, error)`**
(function, not a method) does the actual compaction: `defer cs.MsgContext.EndCompaction()` up
top (guarantees release on every path), prints "triggered..." (message depends on
`compactionType`, one of the named consts `CompactionManual`/`CompactionAuto`), snapshots,
summarizes via `summarizeChatContext`, builds the summary message, calls `cs.MsgContext.Compact`.
Callers must call `cs.MsgContext.ShouldStartCompaction()` themselves first and only proceed into
`compactChatContext` if it returns `true` — it does *not* guard itself.

**History vs. Context — the resolved design question:** `ChatHistory` is the single source of
truth; `ChatContext` is a rebuildable cache over it, not a second independent source of truth
(named pattern: event-sourcing/CQRS — durable log + a derived, disposable read-model). Every
turn writes `cs.MsgHistory.Append(...)` **first** (durable), then
`cs.MsgContext.AddMessages(...)` (cache) — see `handleUserInput`. If the cache write ever fails,
that's a degraded turn, not data loss, since `Context` can be rebuilt by replaying `History`
through a fresh window. No special error-handling exists for this today — both are in-memory,
neither call can currently fail; the ordering is a forward-looking discipline for when
persistence lands, not a live fix.

**Deferred alternative, logged not built:** a "pure derive-context-from-history" design was
seriously considered and explicitly deferred — see "Open decisions" below for the reasoning.

## Next up: write the test suite

This is what Jerry said he'll do next. The plan (sections C and D) was built from 3 independent
sub-agent reviews of `chat_w_memory_compaction_test.go`'s test-case outline; "majority" means at
least 2 of 3 agreed. Walk through Go testing step by step — Jerry is new to it.

### C. Test scaffolding
- [ ] C1. `windowFactories` map (`offset`, `in-place`, `ring-buffer`, `linked-list`) +
      `forEachWindow(t, size, func(t, w))` helper. Failures show as `TestX/ring-buffer`.
- [ ] C2. `fakeProvider`: scripted reply/err, mutex-guarded record of the messages per call,
      optional `block chan struct{}` to hold a compaction in flight without `time.Sleep`.
      Return `ctx.Err()` to test cancellation.
- [ ] C3. `msgs(n)` / `msg(role, id, content)` helper with deterministic IDs (`Compact` matches
      on ID; `getMessageIDs` skips empty IDs, so tests that forget IDs fail confusingly).
- [ ] C4. Stub every outline bullet as its own `Test…` function starting with `t.Skip("TODO")`,
      bullet text in a comment/skip message. Prefixed names, e.g.
      `TestWindow_AddToFull_EvictsOldest`, `TestCompaction_EmptySummary_LeavesHistoryUnchanged`.
      Table-driven only for eviction (1 vs N) and failure kinds. One function per bullet
      (majority pick over one function per outline group with subtests).

### D. Order of attack for filling in the stubs
- [ ] D1. Window-only tests: add, order, `RemoveLast`, eviction, `Clear`, copy-safety of
      `GetMessages`. (`Snapshot`/`Compact` are on `ChatContext` now, not the windows — test
      them there instead, see D2.)
- [ ] D2. `ChatContext.Compact`/`Snapshot` and `clampToMax` directly with hand-built
      `ContextState`.
- [ ] D3. `summarizeChatContext` / `compactChatContext` with the fake provider.
- [ ] D4. Concurrency tests, run with `go test -race`. Include: `ShouldStartCompaction`
      single-flight (call it twice, assert only the first returns `true`), and a message added
      via `AddMessages` during an in-flight `Compact` survives correctly.
- [ ] D5. `handleUserInput`-level bullets (system prompt first, slash commands, failed
      responses, the History-then-Context write ordering).
- [ ] D6. Benchmarks (`Benchmark*`, `b.Run` per window type; maybe a separate
      `_bench_test.go`). Performance bullets are benchmarks, not tests.

## Key design decisions

Moved to `DECISIONS.md` (repo root) — see § "Data structures & type organization",
§ "Naming decisions", § "Concurrency & locking", and § "Compaction design". That file now
holds the full why, including the ones that changed mid-session (e.g. `window`'s visibility on
`ChatContext` flipped twice on 2026-09-21 before landing where it is now).

Two decisions worth restating here because they're easy to forget mid-edit, not because they
need re-explaining (full reasoning in `DECISIONS.md`):
- **`fmt.Errorf` vs `fmt.Fprintln`**: only two legitimate output boundaries print directly —
  `fatal()` in `main`, and `handleUserInput` (returns `bool`, not `error`, by design). Every
  other function propagates errors by return.
- **All user-facing output goes through `cs.OutBuffer`**, never bare `fmt.Println`/`os.Stderr`
  — that's what makes it swappable for a test.

## Open decisions / known issues

Resolved decisions moved to `DECISIONS.md` § "Compaction design" (summary-in-`ChatContext`-only,
failed requests excluded from `ChatHistory`) and § "Concurrency & locking". Still-open items
(unresolved, not yet built) live in `DECISIONS.md` § "Deferred / open decisions" — that's now
the single list; don't duplicate items here as they come up, add them there and link back.

Currently open, most relevant to what's being worked on right now:
- [ ] `RemoveLast` (undo) on `ChatHistory`, not just the window — undecided.
- [ ] `RemoveLast(n)` with `n > len`: inconsistent across the four window types (see
  `DECISIONS.md`) — the D1 test suite should surface and force a pick.
- [ ] `Test_ContextCompaction_CompactingEmptyHistoryReturnsError` and
  `Test_ContextCompaction_EmptyOrWhitespaceSummaryTreatedAsFailure` still say "history" in
  name/comment/skip text but should say "context" — flagged 2026-09-22, not yet renamed (see
  `DECISIONS.md` § "Test suite decisions").

## Two tracks live under `mvp2/` — don't conflate them

1. **`mvp2/` root** (`main.go`, `agent/`, `tool/`, `config.json`) — the real agent build,
   following `mvp1`'s architecture. Last touched 2026-09-12; see "Track 1" below. Dormant,
   not part of the current session's work.
2. **`mvp2/learn/`** — a separate, standalone sandbox of incremental one-file programs
   building up an OpenAI-compatible chat loop from scratch (no `agent/` package, no shared
   module structure — deliberately). This is where all current work happens. See "Track 2"
   below.

## Track 2 — `mvp2/learn/`

```
mvp2/learn/
  chat_simple.go                                     gen 1: chat loop, no memory
  chat_with_simple_memory.go                         gen 2: full history in a slice
  chat_with_sliding_context_window_memory.go         gen 3: fixed-size sliding window
  chat_w_diff_context_window_mgmt_strategies.go      gen 4: pluggable ContextWindow interface
  chat_w_memory_compaction.go                        gen 5: summarize + compact (last-known-good, superseded by refactor below)
  chat_w_memory_compaction_refactor.go               gen 6, CURRENT: history/context split (see "Current state" above)
```

Each file is a standalone `package main` snapshot of one learning stage — see "How to work in
this session" above for why `go build ./...` in this directory is expected to fail, and why
that's not a real signal.

### History: `chat_w_memory_compaction.go` (gen 5, superseded reference)

This predecessor file is untouched and still builds/vets clean on its own — kept as a
last-known-good reference, not part of current work. Its `ContextWindow` interface bundled
`Snapshot`/`Compact`/`Clear`/`gen` directly into each of the four window types (8 methods, ~100
lines of near-duplicated `Compact` logic across the four implementations) — this is exactly
what gen 6 (the refactor) split apart. If gen 6 ever needs to be cross-checked against
known-correct prior behavior, this file is the reference.

**Durable concepts covered while building gens 1–5** (Go fundamentals, still true, don't
re-teach unless Jerry asks):
- `sync/atomic`: why check-then-set on a plain `bool` races; `atomic.Bool` (Go 1.19+, zero
  value usable) with `Load`/`Store`/`Swap`/`CompareAndSwap`; CAS = one indivisible "flip
  false→true, tell me if I won"; `Load` is for display, never for deciding to start work;
  `defer` inside a goroutine's func literal fires when the goroutine ends.
- Go hash set idiom: `map[K]struct{}` (zero-byte value) vs `map[K]bool`; two-value lookup
  `_, ok := m[k]`.
- "Missing field" in Go = zero value (`m.ID == ""`).
- `strings.Cut` (first separator; not found → `(s, "", false)`), `strings.Fields`, `strings.TrimSpace`.
- `continue` inside a `switch` targets the enclosing `for`; a function with a non-void return
  type requires an explicit `return` on every path, unlike a `for` loop falling through.
- Interfaces are reference-like already — a struct field of an interface type should be
  declared as that interface directly (e.g. `ContextWindow`), never as a pointer to it
  (`*ContextWindow`). This mistake recurred twice in this session (`ChatContext`'s window
  field and `ChatSession.MsgHistory`) — watch for it whenever a new interface-typed field
  gets added.
- `atomic.Bool`/`sync.Mutex` fields make a struct unsafe to copy — always return `*T` from
  constructors for any struct holding one, and never pass such a struct by value.
- `append(nil, x...)` vs `make`+`copy`: equivalent except on zero elements, where `append`
  stays `nil` and `make` gives a non-nil empty slice — matters for `== nil`/`DeepEqual` in
  tests, not for anything checking `len()`.
- Defaults pattern: build `Config` pre-filled, then `json.Unmarshal` onto it — absent JSON
  keys are left untouched.

Full narrative history of the gen 1–5 work (design deliberations, bugs found and fixed session
by session) is preserved in `mvp2/learn/prompts/` transcripts if ever needed — not repeated
here since gen 6 (the current refactor) supersedes the architecture those notes describe.

## Track 1 — `mvp2/` root (agent build, last touched 2026-09-12, dormant)

```
mvp2/
  main.go          flag parsing, Config struct, defaults, fatal(), signal handling, stub run()
  config.json      anthropic + fs MCP server + requires_approval
  tool/builtin.go  Grep tool — DOES NOT COMPILE YET (see below)
  go.mod           module github.com/jerryschen31/minagent
```

`go build ./...` currently fails. Nothing else is written: no `agent/` package, no
`tool/tool.go`, no `llm/`, `memory/`, `harness/`, `hooks/`. Not touched since 2026-09-12 — all
work since then (including this whole session) has been in Track 2 (`mvp2/learn/`).

### Concepts covered (don't re-teach unless asked)
- **`fatal()` vs `defer`**: `os.Exit` skips defers. Hence the `main` → `run() error` split —
  `fatal()` only ever called from `main`, all cleanup on `defer` inside `run`.
- **`signal.NotifyContext`**: turns Ctrl-C/SIGTERM into context cancellation. `stop()` cancels
  the ctx *and* restores the signals' prior disposition. Second signal is swallowed unless
  `stop()` is called early. `os.Kill`/SIGKILL can't be caught.
- **Cancellation is cooperative**: `time.Sleep` ignores ctx; anything blocking must take a ctx
  or be raced against `ctx.Done()` in a `select`.

### Decisions made
- **`requires_approval`** (renamed from `mvp1`'s `approve`) = tools that need a human `[y/N]`.
  A *restriction* list, not an allowlist.
- **MCP tool names are prefixed** `<server>_<tool>` — approval entries must be
  `fs_write_file`, `fs_edit_file`, `fs_move_file`, `fs_create_directory`.
- **Grep is a Go-native `tool.Func`, not an MCP server** — no official MCP server does content
  search; pure Go = zero deps, cross-platform.
- **Simplified Grep over the hardened version** — the NUL-byte binary check is the guard that
  actually matters (measured 24KB vs 2.99MB output on this repo, 124×); skip-list/streaming
  reads deferred until they bite.

### Open items
1. **`tool/builtin.go` won't compile**: L29 struct-tag comma syntax, L82 missing trailing
   comma, missing import of the `agent` package.
2. **Grep schema bug**: `"required"` is nested *inside* `"properties"` — move it out.
3. Missing prerequisites for Grep: `agent` package (`agent.Tool`), `tool/tool.go` (`Func`,
   `Args`, `Truncate`), `confine` + `resolveExisting`, `const maxOutput = 16 << 10`. Port from
   `mvp1 tool/tool.go` and `tool/builtin.go:152`.
4. **`api_key_name` vs `api_key_env`** — struct tag and `config.json` disagree; pick one.
5. **`config.json` is currently mandatory** — confirm that's intended (mvp1 treats it as
   optional).
6. **Stub `run()` ignores `ctx`** — wrap the sleep in a `select` against `ctx.Done()`.
7. `gofmt -w` needed — mixed tabs/spaces in `tool/builtin.go`.

### Next phase (Jerry's stated plan, when this track resumes)
Tests and simple evals before building out the loop. `run(ctx, cfg) error` is the seam:
testable with `context.WithCancel`, no model needed. `mvp1` drives its loop with a scripted
fake LLM (`go test ./agent -run TestReActLoop`) — same approach here.
