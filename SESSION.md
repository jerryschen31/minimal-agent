# SESSION.md — mvp2 progress (last updated 2026-09-29)

Scratch handoff notes for `mvp2/` (branch `mvp2-do-myself-phase-2`). Jerry writes the code;
for the current teaching style see the first bullet below. This file is written so a **fresh session with no memory of prior
conversations** can pick up exactly where things left off — read top to bottom before doing
anything. Full transcripts of most prior sessions are in `mvp2/learn/prompts/` (latest saved:
`20260921-session-refactoring.md` — no transcript has been saved for the 2026-09-24 session that
produced the current test suite; this file is the record of it instead).

## How to work in this session (interaction style — read this first)

Jerry asked explicitly that this be captured, not just the code state. This is how the last
several sessions have worked well; keep doing it this way.

- **Current style (2026-09-28): concepts, then small examples, then a skeleton.** For each slice:
  1. the high-level concepts;
  2. a few *small* examples;
  3. a skeleton with signatures and ordered `// TODO` hints.

  Jerry fills in the bodies and then shares them for review. Do **not** hand over full
  implementations: that was tried the same day and he rejected it as "huge chunks". Pure
  figure-it-out exercises were too slow. The rest of this bullet is the older style, kept for
  context.
- **(Previous) Teach-then-let-him-type was the default.** Explain the concept, give the smallest next
  slice (a struct shape, a method signature, an old→new translation table), then stop and
  wait for Jerry to write it and share it back for review. Don't write whole functions/files
  unless he explicitly says "fix this for me" / "do this for me" / "write these" — that's the
  exception, not the norm. It happens more often once a pattern is established (e.g. once the
  test fixture and `forEachWindow` helper existed, Jerry asked for whole test sections to be
  written directly, since the pattern to follow was already agreed) — reading whether a request
  is "teach me this" vs. "apply the established pattern" matters more than a fixed rule.
- **Verify against the actual file before reviewing anything — don't trust memory of what a
  struct/function looked like a few turns ago.** The code changes fast, often in ways not
  narrated. Use Read/Grep/Bash to check current signatures. Before claiming something compiles
  or is broken, actually run it and quote the real output:
  ```sh
  cd mvp2/learn
  go vet chat_w_history_context_session_structs.go chat_w_history_context_session_structs_test.go
  gofmt -l chat_w_history_context_session_structs.go chat_w_history_context_session_structs_test.go
  go test -race chat_w_history_context_session_structs.go chat_w_history_context_session_structs_test.go -v
  ```
  **Never `go build ./...`, `go test ./...`, or trust the IDE's whole-directory diagnostics** —
  every file in `mvp2/learn/` is a standalone `package main` snapshot that intentionally
  redeclares the same top-level names (see "Two tracks" below), so a directory-wide build/lint
  always fails with a wall of `redeclared`/phantom `unknown field`/wrong-signature errors. That's
  expected noise, not a signal — it happens on *every* edit to the test file now, including pure
  comment changes, because the IDE resolves symbols against whichever generation file it feels
  like. Only the explicit two-file (`.go` + `_test.go`) invocation above is meaningful.
- **This exact noise pattern caught a real bug once (2026-09-24) — don't let familiarity with
  it cause you to wave off every red squiggle.** A block of hand-written unit tests called
  methods that don't exist anywhere in the codebase (`ClampToMax`, `cw.Add`, `cw.Messages`,
  single-argument `NewContextWindow`) — invented names that happened to *look* like plausible
  refactors of the real API. The IDE diagnostics for that block looked identical to the routine
  cross-file noise. The only way to tell the difference was running the actual scoped `go vet` —
  which is exactly why "verify, don't assume" above is the top-billed rule, not a footnote.
- **Coverage-gap triage: sort by real-logic-vs-thin-wrapper before recommending tests for
  everything mechanically.** When asked to close out `<50%`-coverage functions, group them:
  functions with real branching logic (a `switch`, an `if/else` with distinct outcomes) are
  worth a dedicated test; thin pass-through wrappers are worth folding one assertion into an
  existing test or skipping outright if nothing else exercises them anyway. Saying "skip this
  one, it's just a wrapper" out loud, with the reason, was better received than testing
  everything uniformly.
- **A shared test fixture/helper needs periodic audits, not just a one-time introduction.**
  Code written across many turns can quietly drift from established helpers (a raw
  `&ChatContext{window: cw}` struct literal instead of `NewChatContext`, an ad-hoc per-strategy
  loop instead of `forEachWindow`) without it being obvious turn-to-turn. When Jerry asked for a
  fixture/helper-consistency pass, it turned out to double as the compile-error catch above —
  worth treating "make sure everything still uses X" as its own periodic review, not just
  something checked at the moment X is introduced.
- **For multi-call-site renames/refactors, give an old→new mapping table, not prose.** Scaled
  well for translating whole functions and for batches of test renames alike.
- **Every design recommendation comes with the honest trade-off, and where relevant, the
  concrete trigger that would flip the recommendation.** Never a bare "do X."
- **When something sounds appealing but has a subtle correctness problem, trace it through
  concretely** — don't hand-wave "that could be risky." This caught: the `RingBufferWindow`
  wraparound indexing bug (gen 5), the decomposed-`Compact` race (gen 6 design), and — this
  session — why an unconditional `WaitForCompaction()` call on `/exit` would make the user wait
  up to `ResponseTimeout` (5 min) instead of exiting promptly; the fix is cancel-then-wait, not
  wait alone (see `DECISIONS.md`).
- **`AskUserQuestion` was tried once for a real architecture fork; Jerry declined it in favor of
  free-form discussion.** Default to laying out options with trade-offs in prose and asking an
  open question at the end.
- **Update this file (and `DECISIONS.md`) continuously, right after each resolved decision or
  completed step** — not saved up for the end of a session.
- **When reviewing code Jerry wrote:** read the whole relevant function, not just a pasted
  excerpt. Check the build — actually run it, not "should compile." List correctness/
  compile-breaking bugs before style/consistency notes. Compliment genuine improvements Jerry
  makes beyond what was suggested and say so specifically.
- **Tone/format:** concise, headers and small tables over long prose, avoid unnecessary jargon
  (per `CLAUDE.md`). The `[agent]` comment prefix convention is for comments *inside code Claude
  writes*, not for chat responses or this file.

## Current state

> **Active work moved to gen 8 on 2026-09-25:**
> `mvp2/learn/chat_w_history_context_session_mcp_tools.go` + `..._mcp_tools_test.go` (tool
> calling + MCP). Its status is under "Next up" → "In progress". The gen 7 description below is
> still accurate for everything gen 8 inherited (windows, compaction, tests).

Working file: **`mvp2/learn/chat_w_history_context_session_structs.go`** (gen 7 — a fresh copy
of gen 6, `chat_w_memory_compaction_refactor.go`, made 2026-09-24 to continue work; gen 6 is
kept untouched as a last-known-good reference, same pattern as gen 5 before it). Test file:
**`mvp2/learn/chat_w_history_context_session_structs_test.go`**.

**Milestone reached 2026-09-24: the test suite is built out and passing.** Every section of the
original test-case outline now has real assertions (not `t.Skip` stubs) — chat request
mechanics, chat history mechanics, context window mechanics (across all four window types via
`forEachWindow`), context compaction (including concurrency edge cases), request/response wire
shape (`httptest`-based, real HTTP round trips), window-type unit tests, and 6 benchmarks. All
verified passing under `go test -race`. Statement coverage moved from 66.4% to **83.5%** over the
course of the session (verified via `go test -cover`, snapshots in `mvp2/learn/target/`).

**Production code changes made alongside the tests** (not just test-only work):
- Fixed 5 `fmt.Fprintln` calls with a redundant trailing `\n` (vet-caught, was blocking
  `go test` entirely without `-vet=off`).
- Added `gracefulShutdown(cancel context.CancelFunc, chatSession *ChatSession)` and a
  `compactWG sync.WaitGroup` + `WaitForCompaction()` on `ChatContext`, so `/exit` (or stdin EOF)
  now cancels any in-flight background compaction and waits for it to actually unwind, instead
  of either abandoning it silently or blocking for up to `ResponseTimeout`. See `DECISIONS.md`
  § "Background-goroutine output..." — related but distinct from this — and the new entry on
  `gracefulShutdown` for the cancel-then-wait reasoning.
- `ChatMessage` gained a `Type string` field (e.g. `"summary"`, `"user"`) — not yet fully
  designed (see `DECISIONS.md` § "Wire format / JSON tags"), but already in active use by
  compaction to mark summary messages, and by several tests to identify them.

**Coverage gaps remaining, deliberately not closed this session** (Jerry's call, explicit):
`runLoop`, `runAgent`, `gracefulShutdown`, `fatal`, `main` are all still 0% — Jerry flagged
these as "high-level/systems functions I may modify or refactor a bit in the next iteration,"
so testing them now would likely be wasted work. Also still 0%, lower priority (Tier 3 from the
coverage review, optional): `getDefaultConfig` (pure data, no branching), `setupChatSession`
(near-duplicate pass-through of the already-100%-covered `NewChatSession`), and `GetWindow` on
`ChatContext` (trivial getter, never called by anything).

## Architecture reference (current, verified shape — read before touching the code)

Four types, each answering a different question:

| Type | Kind | Answers | Key methods |
|---|---|---|---|
| `ContextWindow` | interface | how are messages stored/evicted? (pure data structure) | `AddMessages`, `GetMessages`, `RemoveLast`, `Clear`, `GetSize`, `GetMaxSize` — 6 methods, nothing compaction-related |
| `OffsetWindow`/`InPlaceWindow`/`RingBufferWindow`/`LLWindow` | structs | four different storage strategies implementing `ContextWindow` | each has its own `mu sync.Mutex`, own `maxSize` |
| `ChatHistory` | interface | what was ever said? (durable, append-only, single source of truth) | `GetMessages() []ChatMessage`, `Append(msgs []ChatMessage)` — no `Clear()`, deliberately |
| `InMemoryChatHistory` | struct | in-memory `ChatHistory` impl | `mu sync.Mutex`, pointer receivers throughout |
| `ChatContext` | struct | what does the model see *right now*? (compactable cache over history) | see below |
| `ChatSession` | struct | everything needed to run one turn | `Provider`, `MsgHistory ChatHistory`, `MsgContext *ChatContext`, `SystemMsg ChatMessage`, `UserID string`, `InBuffer io.Reader`, `OutBuffer io.Writer` |

**`ChatContext`** (all load-bearing; `WaitForCompaction`/`compactWG` added 2026-09-24, everything
else unchanged since the gen-6 refactor):
```go
type ChatContext struct {
	mu                   sync.Mutex     // guards window + gen together
	window               ContextWindow  // unexported — nothing outside ChatContext may touch it directly
	gen                  int            // generation counter
	autoCompactThreshold int
	compactInProgress    atomic.Bool
	compactWG            sync.WaitGroup // tracks in-flight background (auto-triggered) compactions
}
```
Methods: pass-throughs to `window` under `c.mu` (`AddMessages`, `GetMessages`, `RemoveLast`,
`GetSize`; `GetMaxSize`/`GetWindow` deliberately skip the lock — immutable after construction);
`IsAutoCompactionNeeded() bool`; `ShouldStartCompaction() bool` / `EndCompaction()` (the
single-flight guard); `WaitForCompaction()` (blocks until `compactWG` hits zero — safe to call
even when nothing is running); `Snapshot() ContextState`; `Compact(state, summaryMsg) bool`.

**Why `Snapshot`/`Compact`/`gen` live on `ChatContext` and not `ContextWindow`, and why `window`
is unexported**: full reasoning in `DECISIONS.md` § "Data structures & type organization" — in
short, `Compact` needs "read state, check `gen`, write new state" to be one atomic operation
relative to a concurrent `AddMessages`, which only stays safe if nothing can reach the window
except through `ChatContext`'s own lock.

**`compactChatContext(ctx, cs *ChatSession, compactionType string) (ChatMessage, error)`**
(function, not a method): `defer cs.MsgContext.EndCompaction()` up top, prints "triggered...",
snapshots, summarizes via `summarizeChatContext`, builds the summary message (`Role: "user"`,
`Type: "summary"`), calls `cs.MsgContext.Compact`. Callers must call
`cs.MsgContext.ShouldStartCompaction()` themselves first — it does not guard itself. The
auto-compaction call site in `handleUserInput` wraps its `go func(){...}()` launch with
`compactWG.Add(1)` (*before* the `go` statement — see `DECISIONS.md` for why that ordering is
load-bearing, not stylistic) and `defer compactWG.Done()` inside it.

**History vs. Context — the resolved design question:** `ChatHistory` is the single source of
truth; `ChatContext` is a rebuildable cache over it (event-sourcing/CQRS pattern; parallels
`mvp1`'s `Memory`/`ContextBuilder` split). Every turn writes `cs.MsgHistory.Append(...)` first
(durable), then `cs.MsgContext.AddMessages(...)` (cache). Compaction only ever touches
`ChatContext` — the history never contains a generated summary, only what was actually said.

## Next up

The test-suite build-out (previously the whole content of this section, tracked as sections C/D)
is **done** — see "Current state" above. What's actually next, in Jerry's stated priority order:

**▶ RESUME HERE (updated 2026-09-29). In progress: tool calling + MCP, in
`mvp2/learn/chat_w_history_context_session_mcp_tools.go` (gen 8, a copy of gen 7) and
`chat_w_history_context_session_mcp_tools_test.go`.**

**Where things stand:** step 1 is built end to end. `Tool`, `read_file`, the `ToolRegistry`,
`runToolCall` (with panic recovery) and a first-draft ReAct loop (`reActLoop`) all exist, and
Jerry tried the loop by hand against a real model ("works decently well"). **Next session:**
Jerry does a small cleanup pass, then focuses on the ReAct-loop tests (list under 1d-iii below).
Slice 1d-iv (window integrity) follows the tests. Design reasoning for everything below is in
DECISIONS.md § "Tool registry, tool calls and the ReAct loop".

- **Design and build order:** DECISIONS.md § "Tools & MCP" and § "MCP protocol version:
  modern-only → dual-era".
  1. `Tool` + ReAct loop + `read_file`
  2. registry + `cfg.Tools`
  3. MCP over stdio: 3a modern (2026-07-28, `server/discover` + `_meta`), then 3b legacy
     `initialize` fallback
  4. `/mcp-add`, `/mcp-remove`, `/mcp-list`
  5. HTTP transport + OAuth
- **Verified state (2026-09-29):** `go vet` clean, `gofmt -l` clean, full suite passes under
  `go test -race` (the two-file invocation). The existing `handleUserInput` tests now run through
  `reActLoop` on the no-tool-call path (unscripted `fakeProvider`); **no test yet drives a
  scripted tool-call sequence.**
- **Uncommitted:** gen 8 files and this SESSION.md are modified but not committed. Ask Jerry
  before committing.

**Step 1 is split into slices 1a–1d:**

- **1a: wire types. DONE.**
  - `ToolCall{ID, Type, Function ToolCallFunc{Name, Arguments string}}`: `Arguments` is a JSON
    string holding JSON, as on the wire.
  - `ToolDef{Type, Function ToolDefFunc{Name, Desc, Params json.RawMessage}}`: Jerry's name,
    instead of "ToolSpec".
  - `ChatMessage` gained `ToolCalls` (`tool_calls,omitempty`) and `ToolCallID`
    (`tool_call_id,omitempty`); `ChatRequest.Tools` (`tools,omitempty`).
  - Side effect: `ChatMessage` can no longer be compared with `==`, because it now has a slice
    field. Six test comparisons were switched to `.ID`.
- **1b: `Tool` interface + `read_file`. DONE.**
  - `Tool{ GetToolDefinition() ToolDef; CallTool(ctx, args json.RawMessage) (string, error) }`
    (Jerry's names).
  - `NewToolDef` fills `Type: "function"` and turns empty params into
    `{"type":"object","properties":{}}`.
  - `ReadFileTool` is an empty struct with value receivers and `var _ Tool = ReadFileTool{}`.
    - `CallTool`: `os.Open` + `io.LimitReader(max+1)`, a NUL-byte binary check, and a
      truncation note over `ReadFileMaxBytes` (64 KB). Empty path and bad JSON are errors.
    - Claude rewrote `CallTool` at Jerry's request ("file-stream details aren't core").
  - Tests: section "Built-in tools: read_file", 4 tests: small, exactly 64 KB, over 64 KB, and
    a table of 5 failure cases.
  - **Known gap:** it can read any path. Restricting paths is for hooks/approval later.
- **1c: `Provider.Chat(ctx, msgs, tools []ToolDef) (ChatMessage, error)`. DONE.**
  - The request carries `Tools`.
  - `summarizeChatContext` and `handleUserInput` pass `nil`: no behavior change, which the
    unchanged suite proves.
  - `handleUserInput` builds `responseMsg` from the returned message, so `ToolCalls` are kept.
  - `fakeProvider.Chat` wraps `Reply` into an assistant `ChatMessage`, so the 33
    `fx.Provider.Reply = ...` lines are untouched.
- **1d: the ReAct loop. IN PROGRESS: code drafted and tried by hand; tests not yet written.**
  Split into sub-slices:
  - **1d-i: tool registry + `runToolCall`. DONE** (tested except `runToolCall`'s non-panic paths).
    - `ToolRegistry{mu RWMutex, tools map[string]toolEntry, tooldefs []ToolDef}` with
      `toolEntry{tool, def}`. `NewToolRegistry([]Tool)`, `Register` (errors on nil, empty name,
      duplicate), `Remove`, `Lookup` (returns `Tool`), `GetToolDefs` (cached, name-sorted,
      copy-on-write). 13 tests in section "Tool registry", incl. a `-race` test and a
      copy-on-write test.
    - `ChatSession` gained `Tools *ToolRegistry`; `NewChatSession(provider, history, context,
      tools, cfg)` and `setupChatSession` take it; `setupToolRegistry(cfg)` builds it in
      `runAgent` step 4. **It hardcodes `ReadFileTool{}`; `cfg.Tools` is not consulted yet**
      (the remainder of build-order step 2). The test fixture has a `Tools` field, starting empty.
    - `runToolCall(ctx, cs, call) ChatMessage` never returns an error: unknown tool, non-object
      args (`""` becomes `{}`), tool error and tool panic are all `role:"tool"` messages with an
      `error: ...` `Content` and `ToolCallID = call.ID`. `callToolSafely` wraps `CallTool` with
      `recover()`. 1 test (`Test_RunToolCall_ToolPanics_RecoveredAsErrorMessage`, two subtests).
    - `createChatMessage(role, msgType, content, toolCallID, toolCalls)` builds every message the
      loop makes. Pitfall: four positional `string` params can be swapped silently; a narrower
      `newToolMessage(toolCallID, content)` was suggested and not taken.
  - **1d-ii: scripted `fakeProvider`. DONE.** `Script []ChatMessage` plays back one reply per
    `Chat` call; running past the end is an error (a loop that over-calls fails loudly); unscripted
    still returns `Reply`. `ToolDefs [][]ToolDef` records each call's tools. Builders
    `assistantText(...)` and `assistantToolCall(id, name, args)`. 2 self-tests.
  - **1d-iii: `reActLoop`. DRAFT, WORKS BY HAND, UNTESTED.**
    - `reActLoop(ctx, cs, priorMsgs, initialMsg) ([]ChatMessage, error)` keeps one local `turn`
      slice (user msg, assistant, tool results, ..., final assistant). Each step sends
      `system + prior context + turn` with `cs.Tools.GetToolDefs()`. It does not touch history or
      context.
    - `handleUserInput` builds `userMsg`, calls the loop, and on success appends the whole turn
      to `MsgHistory` and `MsgContext` in one batch, then runs the auto-compaction check. On
      error it prints and persists nothing. Limit is `MaxReActSteps = 10`; exceeding it returns
      `ErrMaxSteps` (sentinel, wrapped with `%w`; check with `errors.Is`).
    - **Tests to write (Jerry's focus next):** one tool round (request 2 must contain the
      assistant `tool_calls` message then the matching `tool` message); unknown tool continues
      with an error observation; two tool calls in one reply, results in order; a `Chat`
      failure persists nothing; the step limit (script longer than `MaxReActSteps`, expect
      `errors.Is(err, ErrMaxSteps)`, nothing persisted); a cancelled `ctx`; `Tools` def list
      reaches the provider (`Provider.ToolDefs`).
    - **Cleanup Jerry planned:** the empty `[PLAN]` comment block at the top of `reActLoop`; the
      `Chat` error is printed in both `reActLoop` (`error:`) and `handleUserInput`
      (`[error] ReAct loop error:`), so pick one (the caller is the output boundary); print the
      final answer from `turnMsgs[len(turnMsgs)-1].Content` (currently the loop prints the whole
      `response` struct with `%s`, which is a debug view; `json.MarshalIndent` was suggested).
  - **1d-iv: window / compaction integrity. NOT STARTED.** The context window trims by message
    count, so it can cut between an assistant tool call and its results; providers reject that.
    Worse, `MaxContextWindow = 10` gives a window of 8, and a turn with two tool rounds is
    already 6 messages, so a longer turn overflows the window and trimming can drop the user
    message and leave orphaned tool results. Compaction has the same exposure. See the CLAUDE.md
    invariant and mvp1's `harness.Window` (widens backwards to a user turn). Until then, keep
    loop tests short or use a larger window in the fixture.
  - **Optional extra, offered and not yet done:** an `httptest` test where the server returns
    the SF-weather `tool_calls` JSON; assert that the request body had `"tools"` and that the
    reply decoded one `ToolCall`.
- **Open questions for later steps:**
  - Does `/mcp-add` persist to config, or last only this session?
  - Hand-rolled stdio JSON-RPC, or `github.com/modelcontextprotocol/go-sdk`? (Leaning
    hand-rolled first, for learning.)
  - Still need a real modern-protocol server to test against. Until then, use a fake stdio
    server in Go tests.
- **Things Jerry asked about this session (already taught; don't re-teach unless asked):**
  - why tool calls use dedicated fields, not `Content`;
  - why `ToolCalls` is a slice and `ToolCallID` is a single ID;
  - what `ToolDef` is for (you → model menu) vs `ToolCall` (model → you);
  - `var _ Iface = T{}` compile-time checks (when they're worth it);
  - value vs pointer receivers (empty `ReadFileTool` → value; the MCP wrapper will need a
    pointer);
  - passing `nil` tools (idiomatic; `omitempty` drops the key);
  - `server/discover` vs legacy `initialize`, and the dual-era matrix.
  - (2026-09-28/29) why the tool list is sorted (providers cache an exact prompt prefix; Go map
    order is random) and why the sort happens on register, not per request;
  - `sync.RWMutex` vs `sync.Mutex`, and not taking a write lock while holding a read lock;
  - copy-on-write: why returning the cached slice uncopied is safe;
  - variadic vs slice parameter; struct vs pointer for types with a mutex (`copylocks`); why
    `ChatHistory` is an interface holding a pointer;
  - typed nil in an interface (taught, then deliberately not handled in `Register`);
  - `recover()`: only in a deferred function, same goroutine; named returns let it set `err`;
  - sentinel errors: `errors.New`, `errors.Is`, wrapping with `%w`;
  - `toolCallID` on a `tool` message vs `ToolCalls[i].ID` on the assistant message;
  - how Ctrl+C reaches the loop (`signal.NotifyContext` in `main`, ctx passed down), and why
    `reActLoop` needs no `WithCancel` of its own.

**Start here next session (added 2026-09-25) — remind Jerry at the top of the session:**

- **Make the chat writer lockable.** `cs.OutBuffer` is written from both the main loop and the
  auto-compaction goroutine, which is a data race on a `bytes.Buffer`. Jerry to write it: a small
  type wrapping an `io.Writer` + `sync.Mutex`, wrapped once in `NewChatSession`. Downsides: only
  single `Write`s are atomic (multi-line output can still interleave), `debugChatContext` uses
  `fmt.Println` and bypasses it, and `printConfig` writes to `cfg.OutBuffer` rather than the
  session writer. Add a `-race` test where a slow fake provider makes auto-compaction overlap a
  second `handleUserInput`. Ties into the goroutine-output item under § "Deferred / open decisions".
- ~~Package doesn't compile~~ and ~~stale-context bug after slash commands~~: both resolved
  2026-09-25. Full suite passes under `-race`. Slash commands no longer send trailing text as a
  prompt; see DECISIONS.md § "Slash commands — text after the command".

1. **Refactor `runLoop`/`runAgent`/`gracefulShutdown`/`main`** — Jerry's own next-iteration plan,
   not yet started. These are the last 0%-coverage functions, left untested on purpose because
   they're expected to change shape. No design decided yet for what the refactor looks like;
   revisit this file's "Current state" note above once that starts.
2. **Decide the still-open `DECISIONS.md` items relevant to the code as it stands** — most
   pressing is probably the background-goroutine-output-racing-the-prompt fix (§ "Deferred /
   open decisions": buffered channel, drained by `runLoop`), since it's the kind of thing that'd
   naturally get touched by the runLoop refactor above anyway — worth deciding together rather
   than sequentially.
3. **Optional coverage cleanup** (Tier 3, low priority): `getDefaultConfig`, `setupChatSession`,
   `GetWindow` — only worth it if Jerry wants a "no untested branch, period" bar rather than a
   pragmatic one. Not blocking anything.
4. Anything from `DECISIONS.md` § "Deferred / open decisions" not already covered above
   (`RemoveLast`-on-`ChatHistory` undo semantics, summary-of-summaries handling, extracting the
   window types into a package) — none have a concrete trigger that's fired yet.

## Key design decisions

In `DECISIONS.md` (repo root) — see § "Data structures & type organization", § "Naming
decisions", § "Concurrency & locking", § "Compaction design", and (new 2026-09-24) the
`gracefulShutdown`/`WaitForCompaction` cancel-then-wait entry under § "Deferred / open
decisions". That file holds the full why, including decisions that changed mid-session.

Two decisions worth restating here because they're easy to forget mid-edit:
- **`fmt.Errorf` vs `fmt.Fprintln`**: only two legitimate output boundaries print directly —
  `fatal()` in `main`, and `handleUserInput` (returns `bool`, not `error`, by design). Every
  other function propagates errors by return.
- **All user-facing output goes through `cs.OutBuffer`**, never bare `fmt.Println`/`os.Stderr`
  — that's what makes it swappable for a test (and now, per the fixture, `fx.Out`).

## Open decisions / known issues

Resolved decisions live in `DECISIONS.md`. Still-open items live in `DECISIONS.md` §
"Deferred / open decisions" — that's the single list; don't duplicate items here, add them
there and link back.

One correction from the 2026-09-22 version of this file: the "not yet renamed" `History`→
`Context` naming items are now resolved — `Test_ContextCompaction_CompactingEmptyContextReturnsError`
already has the correct name, and `Test_ContextCompaction_EmptyOrWhitespaceSummaryTreatedAsFailure`'s
one stale doc-comment word ("history" → "context") was fixed 2026-09-24. `DECISIONS.md` §
"Test suite decisions" still says "not yet renamed" for these — that entry is now stale and
should be corrected next time that section is touched.

Currently open, most relevant to what's being worked on next: see "Next up" above — it now
supersedes what used to be listed here.

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
  chat_w_memory_compaction.go                        gen 5: summarize + compact (superseded reference)
  chat_w_memory_compaction_refactor.go               gen 6: history/context split (superseded reference)
  chat_w_history_context_session_structs.go          gen 7, CURRENT: gen 6 continued, now with a full test suite (see "Current state" above)
  chat_w_history_context_session_structs_test.go     the test suite itself
```

Each file is a standalone `package main` snapshot of one learning stage — see "How to work in
this session" above for why `go build ./...`/`go test ./...` in this directory is expected to
fail, and why that's not a real signal.

### History: gens 5 and 6 (superseded references)

Both untouched, both still build/vet clean standalone. `chat_w_memory_compaction.go` (gen 5) is
the pre-refactor design where `ContextWindow` bundled `Snapshot`/`Compact`/`Clear`/`gen` directly
into each of the four window types. `chat_w_memory_compaction_refactor.go` (gen 6) is the
history/context split with those moved onto `ChatContext` — gen 7 is a direct continuation of
gen 6, not a further architectural change. If gen 7 ever needs cross-checking against
known-correct prior behavior, either is the reference.

**Durable concepts covered while building gens 1–6** (Go fundamentals, still true, don't
re-teach unless Jerry asks): `sync/atomic` and CAS; the `map[K]struct{}` hash-set idiom; "missing
field = zero value" in Go; `strings.Cut`/`Fields`/`TrimSpace`; `continue` inside a `switch`
targets the enclosing `for`; interface-typed struct fields declared as the interface directly,
never `*Interface`; `sync.Mutex`/`atomic.*` fields make a struct unsafe to copy — always
constructors returning `*T`; `append(nil, x...)` vs `make`+`copy` (`nil` vs non-nil empty slice);
the `Config`-pre-filled-then-`json.Unmarshal` defaults pattern.

**New this session (gen 7, Go testing-specific — Jerry is newer to this than to core Go)**:
`t.Fatalf` (stop immediately) vs. `t.Errorf` (record and continue); `t.Helper()` so a failure
inside a fixture/assertion helper blames the caller's line, not the helper's; `t.Run` for
subtests (`TestX/ring-buffer` naming); table-driven tests for per-type-different expected
behavior (see `Test_Unit_RemoveLast_MoreThanAvailable_PerType` in the test file — a real example
of "don't force a shared assertion when the documented behavior actually differs by type");
`httptest.NewServer` for testing an HTTP client without a real network (and the specific trick of
`srv.Close()` *before* use to deterministically force a connection-refused error, rather than
guessing at an unused port); `b.N`/`b.ResetTimer()` for benchmarks, and why several of this
session's benchmarks pre-fill the window to capacity before resetting the timer (measuring
realistic steady-state cost, not empty-window growth); `sync.WaitGroup.Add`/`Done`/`Wait`, and
specifically why `Add(1)` must happen in the launching goroutine *before* the `go` statement,
never inside the spawned goroutine (a real race otherwise — see `DECISIONS.md`).

Full narrative history of the gen 1–6 work is preserved in `mvp2/learn/prompts/` transcripts.

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
