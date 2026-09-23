# DECISIONS.md — mvp2/learn/ design decision log

This file is the durable record of *what was decided, why, and when* for the `mvp2/learn/`
build-out. It is not a status report — for "what's in progress right now / what to do next,"
see `SESSION.md`. A decision belongs here once it's settled (even if later reversed); a TODO
or an in-flight question belongs in `SESSION.md` until it resolves into a decision, at which
point it should move here.

Each entry: **what was decided**, **why**, **files affected**, **date**, and **status**
(`active` / `superseded by <entry>` / `deferred`). Entries are grouped by topic, not strict
chronological order — where a decision changed over time, the whole arc is kept in one place
so the "changed my mind" reasoning isn't lost.

---

## Data structures & type organization

### `ContextWindow` interface — scope narrowed twice

- **gen 4** (`chat_w_diff_context_window_mgmt_strategies.go`, 2026-09-17): 3 methods —
  `AddMessages`, `GetMessages`, `RemoveLast`. Four implementations: `OffsetWindow`,
  `InPlaceWindow`, `RingBufferWindow`, `LLWindow`.
- **gen 5** (`chat_w_memory_compaction.go`, 2026-09-18 → 09-19): grew to 6 methods, adding
  `Snapshot() ContextState`, `Compact(state, summaryMsg) bool`, `Clear()`, plus a `gen` field
  on every window type. Each of the four types independently implemented `Compact` —
  ~100 lines of near-duplicated logic across them.
- **gen 6 / refactor** (`chat_w_history_context_session_structs.go`, 2026-09-21 → 09-22,
  **current — status: active**): `Snapshot`/`Compact`/`gen` moved *off* `ContextWindow` onto
  a new `ChatContext` struct; the window is now unexported inside `ChatContext`
  (`window ContextWindow`, lowercase) so nothing else can reach it directly. This let
  `Snapshot`/`Compact` be written **once**, generically, in terms of
  `GetMessages`/`Clear`/`AddMessages`/`GetMaxSize` — deleting the ~100 duplicated lines.
  `ContextWindow` is back down to 6 methods: `AddMessages`, `GetMessages`, `RemoveLast`,
  `Clear`, `GetSize`, `GetMaxSize` (`Clear` stays implemented per-type, kept for the
  window-strategy performance comparison).

  **Why moved**: `Compact` needs "read current state, check `gen` hasn't changed, write new
  state" to be one atomic operation relative to a concurrent `AddMessages` call (e.g. the main
  loop adding a message while a background auto-compaction is mid-summarization — a real,
  designed-for scenario). Decomposing `Compact` into separate `GetMessages`+`Clear`+
  `AddMessages` calls only stays safe if nothing can reach the window except through one lock.
  Root cause of the gen-5 design being unsafe: `gen` and the messages were protected by the
  *window's own* mutex, which is fine standalone, but nothing prevented a second caller from
  reaching the window directly and interleaving with an in-progress `Compact`.

  Files: `chat_w_diff_context_window_mgmt_strategies.go` → `chat_w_memory_compaction.go` →
  `chat_w_history_context_session_structs.go`.

### `window` field: exported → unexported → accessor added back

- Early gen-6 discussion (2026-09-21): `window ContextWindow` was exported on `ChatContext`,
  with a `GetWindow()` getter judged redundant and removed ("field is already exported").
- **Reversed same day**: once the atomicity problem above was traced through concretely,
  `window` was made **unexported** so `ChatContext`'s own mutex is the only path to it — this
  is what makes the generic `Snapshot`/`Compact` safe. A `GetWindow() ContextWindow` accessor
  was then added back (current code, `chat_w_history_context_session_structs.go`) to expose
  read access without exposing the field itself.
- Status: active. Logged specifically because this is a decision that flipped within the same
  session once the concurrency reasoning was worked through — not a case of "changed later,"
  but "corrected within hours." Worth remembering if `window`'s visibility ever comes up again.

### `ChatHistory` — introduced as a separate type from `ContextWindow` (2026-09-21)

Decision: split "what was ever said" (durable, append-only, truth) from "what does the model
see right now" (compactable, evictable cache). Named pattern: event-sourcing/CQRS — durable
log + a derived, disposable read-model. Direct parallel to `mvp1`'s `Memory` (truth) vs.
`ContextBuilder` (view) split (see `mvp1/CLAUDE.md`'s architecture table).

- **Naming**: `ChatHistory`, not `ChatStore`/`History` — matches the file's own header comment
  and the `<Strategy>Window` naming pattern already used for `ContextWindow` implementations.
- **Scope**: one instance per session, no session-ID parameter on any method — matches
  `mvp1`'s `Memory` (fresh instance per agent/subagent, never ID-keyed). A future persistent
  implementation carries the session ID at *construction* time (`NewFileChatHistory(sessionID)`),
  not on every call. A "list/resume past sessions" feature, if ever built, is a separate small
  interface, not an ID param bolted onto `ChatHistory`.
- **No `Clear()` on `ChatHistory`, deliberately** — putting it there would commit every future
  backing store (file/DB) to supporting hard delete, cutting against "history is the truth that
  survives compaction." `/clear` only ever resets `ChatContext`.
- **`Append(msgs []ChatMessage)` plural**, matching `ContextWindow.AddMessages` — lets a caller
  build one slice and pass the *same* slice to both `History.Append` and `Context.AddMessages`,
  so the two calls can't drift apart in content.
- Status: active. Files: `chat_w_history_context_session_structs.go`.

### `ChatSession` struct — supersedes an earlier flat design

- Rejected design (2026-09-21, never built): a flat `chatSession` struct holding `provider`,
  `window`, `systemMsg`, `compactInProgress atomic.Bool`, `autoCompactThreshold` all as direct
  fields.
- **Built instead**: `ChatSession{ Provider, MsgHistory ChatHistory, MsgContext *ChatContext,
  SystemMsg ChatMessage, UserID string, InBuffer io.Reader, OutBuffer io.Writer }`. `MsgContext`
  must be a pointer — `ChatContext` holds an `atomic.Bool` and a `sync.Mutex`, so a value field
  would copy the lock on every copy of `ChatSession` (same hazard class as any struct holding a
  `sync.Mutex`/`atomic.*`: always return `*T` from its constructor, never pass by value).
- Status: active. Files: `chat_w_history_context_session_structs.go`.

### Constructors return `(*T, error)` even where nothing can fail yet

`NewInMemoryChatHistory`, `NewChatContext`, `NewChatSession` all return an error today even
though nothing in-memory can currently fail. Deliberate future-proofing so a later
persistent/fallible implementation doesn't require a breaking signature change. Status: active.

---

## Naming decisions

### `ShouldStartCompaction()`, not `StartCompaction()`

Chosen specifically because a name like `StartCompaction` invites calling it and ignoring the
return value — which is exactly a bug that happened once (2026-09-19/21 window): an
auto-compact goroutine ran unconditionally, ignoring a `false` result, breaking the
single-flight guarantee. `Should` signals it's a question worth checking, closer to Go's own
`sync.Mutex.TryLock()` idiom. Status: active. Files: `chat_w_history_context_session_structs.go`.

---

## Concurrency & locking

- **Per-window internal `mu` kept even after `ChatContext` gained its own mutex** — not
  redundant: needed so each window type can still be tested standalone (per the test-suite
  plan), and it's cheap defense-in-depth; nests harmlessly under `ChatContext`'s lock.
- **`compactInProgress atomic.Bool` lives on `ChatContext`**, shared by the auto-compaction
  path and the manual `/compact` command. Earlier drafts had it as a bare local in `runLoop`;
  a per-window flag was explicitly rejected (would need repeating 4×, and "a compaction is
  running" is session policy, not window mechanics). CAS via `ShouldStartCompaction()` is the
  only way to claim it (see naming decision above); `EndCompaction()` releases it, called via
  `defer` in `compactChatContext` so release happens on every path.
- **`gen` counter's purpose**: IDs alone can't distinguish "cleared mid-summary" from "every
  compacted message was legitimately evicted" — `gen` can, and it also rejects a stale second
  compaction. Moved from each window type onto `ChatContext` in the gen-6 refactor (see above),
  once `window` became unexported and a single lock could cover both `gen` and the messages
  together.
- Status: all active. Files: `chat_w_history_context_session_structs.go`.

---

## Compaction design

- **ID-set approach, not a per-message "replace" flag** (decided 2026-09-19, carried through
  every later refactor unchanged): every `ChatMessage` already has a unique `ID`, so `Compact`
  builds a `map[string]bool` from the frozen snapshot and keeps window messages whose ID isn't
  in it. Nothing is mutated on shared state, so a failed summary needs no cleanup and
  overlapping compactions can't confuse each other.
- **The ID set is built inside `Compact` from the snapshot's `state.Messages`**, never from a
  live `GetMessages()` call — using live messages would drop anything added concurrently.
- **`Compact` takes one `ContextState{Messages, Gen}`**, not separate `ids`+`gen` arguments, so
  the two can't come from different snapshots by mistake.
- **`clampToMax(msgs, maxSize)`** — shared helper, keeps the summary at index `[0]` plus the
  newest `maxSize-1` survivors. Guards the overflow case: window fills with new messages while
  a slow summarization call is in flight, so summary + all survivors exceeds `maxSize`.
- **Summary message `Role` is `"user"`, not `"system"`** — a second `system` message mid-history
  breaks Anthropic and some Ollama chat templates. Known, accepted trade-off: consecutive
  `user` turns after compaction (fine for OpenAI/Ollama, would need merging for Anthropic).
  Revisited in discussion 2026-09-22 (see "Wire format" below) but the decision didn't change —
  `Role` stays `"user"`; a separate marker field was proposed instead of changing `Role`.
- **Compaction writes the summary only into `ChatContext`, never into `ChatHistory`** — resolved
  2026-09-21, restated in the gen-6 refactor: "the history should only ever contain what was
  actually said, not a generated artifact." `compactChatContext`/`ChatContext.Compact` only
  ever mutate `cs.MsgContext`; `cs.MsgHistory` is untouched by compaction, success or failure.
  This surfaced as a concrete test-writing issue 2026-09-22 — a stub named
  `Test_ContextCompaction_CreatesSummaryMessageInChatHistory` had to be renamed to
  `...InContext`, because the behavior it described doesn't exist by design (see "Test suite
  decisions" below).
- Status: all active. Files: `chat_w_memory_compaction.go` (origin) →
  `chat_w_history_context_session_structs.go` (current).

---

## Wire format / JSON tags (2026-09-22 — discussion only, no code changed)

Context: considered adding an `IsSummary bool` marker field to `ChatMessage`, to distinguish
generated summaries from real user turns — motivated by (a) a future "derive context from
history on demand" design that would need to find the most recent summary, and (b) wanting to
filter summaries out when searching real user messages.

- **`Role` must stay within the provider's expected enum** (`"user"`/`"assistant"`/`"system"`/
  `"tool"`) — a custom value like `"summary"` is a real rejection risk, since `role` is
  typically schema-validated server-side, unlike an arbitrary extra field. This is why summary
  messages keep `Role: "user"` rather than getting their own role value (ties back to the
  compaction decision above).
- **Corrected mid-discussion**: extra/unrecognized JSON fields on a message object (`id`,
  `timestamp`, a hypothetical `is_summary`) are *not* actually risky to send to the provider —
  OpenAI-compatible servers (including Ollama's compat layer) decode into a known schema and
  silently drop unrecognized keys; nothing reaches the model or costs tokens. Initial advice to
  blanket-tag such fields `json:"-"` and split `ChatMessage` from a separate wire-only
  `RequestMessage` type was overstated on that basis and walked back.
- **Net decision**: no code changed. If `IsSummary` (or similar) is added later, a plain field
  with no special JSON tag is fine functionally. A `RequestMessage`/wire-type split remains a
  legitimate *architecture-taste* call (payload hygiene, not coupling persisted format to wire
  format) — but it's optional, not required for correctness.
- Status: deferred / not built. Candidate file: `chat_w_history_context_session_structs.go`.

---

## Test suite decisions (2026-09-22, `chat_w_history_context_session_structs_test.go`)

- **Stub convention**: `Test_<Subject>_<Scenario>_<Expectation>` naming, `t.Skip("TODO: ...")`
  body until implemented, one function per outline bullet (not table-driven except for
  eviction count and failure-kind variations).
- **Removed as untestable-as-named** (no code path exists to exercise the behavior):
  - `Test_ChatHistory_RecentChatMessage_CannotBeRemoved` — `ChatHistory` has only
    `GetMessages()`/`Append()`; there is no removal method to call and confirm blocked.
  - `Test_ChatRequest_UnrecognizedSlashCommands_ErrorOut` — `handleUserInput` returns a plain
    `bool` (the quit signal), never a Go `error`; only the sibling
    `Test_ChatRequest_UnrecognizedSlashCommands_NoRequestSent` (checking no request was sent)
    is actually verifiable for this code path.
- **Renamed for accuracy**, after tracing what compaction and `/clear` actually touch — all
  three because compaction/`/clear` only ever mutate `ChatContext`, never `ChatHistory`:
  - `Test_ContextCompaction_CreatesSummaryMessageInChatHistory` → `...InContext`
  - `Test_ContextCompaction_UnsuccessfulDoesNotAlterChatHistory` → `...Context`
  - `Test_ContextCompaction_FailedCompactionAllowsNewMessages` — description wording
    "chat history" → "context"
- **De-duplicated**: `Test_ContextWindow_ClearContext_SlashCommand` had been accidentally
  written twice — once filed under "chat history mechanics," once under "context window
  mechanics" with a `_Basic` suffix — both with identical bodies. Kept once, filed under
  context-window mechanics, since `/clear` only ever calls `cs.MsgContext.Clear()` and never
  touches `cs.MsgHistory`.
- **Known remaining inconsistency, not yet fixed**: `Test_ContextCompaction_
  CompactingEmptyHistoryReturnsError` and `Test_ContextCompaction_
  EmptyOrWhitespaceSummaryTreatedAsFailure` still say "history" in name/comment/skip text, but
  the emptiness/whitespace check in `summarizeChatContext` operates on the `ChatContext`
  snapshot, not `ChatHistory`. Flagged 2026-09-22, not yet renamed.
- Status: active (renames applied), one item deferred (see above). Files:
  `chat_w_history_context_session_structs_test.go`.

---

## Deferred / open decisions

- **`RemoveLast` (undo) — window only, or also `ChatHistory`?** Undecided; no `/undo` command
  exists yet. Leaning window-only, matching `/clear`, per the "history is truth" model.
- **`RemoveLast(n)` with `n > len(messages)`**: `OffsetWindow`/`InPlaceWindow`/`RingBufferWindow`
  silently no-op; `LLWindow` removes what it can. Inconsistent across the four types, not yet
  resolved. Expected to surface once the window-type test suite runs the same test across all
  four via a shared `forEachWindow` helper.
- **Empty `GetMessages()` on `OffsetWindow` can return `nil`** rather than a non-nil empty slice
  (`append(nil, x...)` on zero elements stays `nil`) — tests must compare via `len()`, not
  `reflect.DeepEqual`/`== nil` against a literal `[]ChatMessage{}`.
- **`RingBufferWindow` keeps stale messages in its backing array** after `Clear`/`Compact` until
  overwritten — memory retention only, never observable through the public API. Not a bug.
- **"Pure derive-context-from-history" design** — no stored `ChatContext` at all; context
  computed on demand from `ChatHistory` each time it's needed. Compaction would become an
  *append* (a summary entry self-describing its own coverage, e.g. "covers through message ID
  X"), not a mutation — eliminating `Compact`'s `gen`/`Snapshot` concurrency protocol entirely.
  Seriously considered and explicitly **deferred 2026-09-22**. Why deferred: would discard the
  already-built, correct `Compact`/`Snapshot`/`gen` machinery; weakens the four-window-strategy
  comparison (interesting specifically because of incremental mutation over a session); was a
  bigger rewrite than finishing the ~90%-done design already in flight. **Revisit trigger**:
  when persistence (`ChatHistory` backed by file/DB) becomes real — that's the point where a
  single-source-of-truth design pays off most.
- **Extracting `ContextWindow` + the four window types into a separate package** — discussed
  and **deferred 2026-09-22**. The composability goal is already met at the interface level
  within this program; a package boundary adds a different kind of composability (reuse across
  separate programs/modules) with no live use case yet. **Revisit trigger**: when `mvp2/`
  Track 1 (the real agent build, dormant since 2026-09-12) resumes and needs a context-window
  abstraction — porting these already-debugged implementations there is the natural move.
- **Background-goroutine output racing the `"> "` prompt** — auto-compaction's status messages
  (`compactChatContext`'s `fmt.Fprintln(cs.OutBuffer, ...)` calls) run in a goroutine launched
  from `handleUserInput` with no coordination against `runLoop`'s own `fmt.Print("\n> ")` +
  `stdin.ReadString`. Both write to the same underlying stream from different goroutines with no
  ordering guarantee, so a background message can land spliced right after an already-printed,
  empty prompt (observed 2026-09-22, e.g. `>[system] Auto-compaction triggered`). **Chosen
  direction (2026-09-22), not yet built**: background goroutines stop writing to `cs.OutBuffer`
  directly and instead send their text on a buffered channel; `runLoop` drains the channel and
  prints anything pending right before it prints the next `"\n> "`, so output only ever appears
  at the boundary between one line finishing and the next prompt starting. Known limitation,
  accepted: a message that arrives while the user is mid-line-typing still waits until they hit
  enter — this doesn't fully eliminate every race, only the specific artifact seen. **Deferred —
  not critical**, explicitly not being built right now. A full event-loop restructure (`stdin`
  read on its own goroutine feeding a channel, `runLoop` becomes a `select` between "new user
  line" and "new background notice") would close the remaining mid-typing gap too, but is more
  surgery than this cosmetic issue currently warrants. Files (when built):
  `chat_w_history_context_session_structs.go` (`compactChatContext`, `handleUserInput`,
  `runLoop`).
- **Summary-of-summaries handling, and quality degradation from repeated summarization** —
  not yet decided. Once a session runs long enough to trigger auto-compaction more than once,
  the second (and later) compaction summarizes a context whose oldest entry is already itself a
  summary (see "Compaction design" above: the summary message is inserted with `Role: "user"`
  and no marker distinguishing it from a real turn, so `summarizeChatContext` has no way to
  treat it differently even if a future design wanted to). Open questions, none resolved yet:
  whether to summarize-the-summary-plus-new-messages as one blended pass (simplest, but
  compounds lossiness — each pass is a lossy re-compression of an already-lossy artifact, same
  failure mode as repeated JPEG re-encoding); keep the original summary verbatim and only
  append a new summary covering what's happened since (avoids re-compressing old context, but
  summaries accumulate without bound over a long enough session, undermining the point of
  compaction); or cap how many summary "generations" are allowed before something else has to
  give (e.g. forced truncation, or surfacing this to the user). Flagged 2026-09-22; no design
  work done yet. **Revisit trigger**: once auto-compaction is actually exercised more than once
  in a single session (currently untested — the D3/D4 compaction test stubs only cover a single
  compaction pass), or once the `IsSummary`-style marker field discussed under "Wire format /
  JSON tags" above gets built, since that marker is the prerequisite for `summarizeChatContext`
  to even know it's being asked to summarize a summary.

---

## Track 1 — `mvp2/` root (agent build, last touched 2026-09-12)

Decisions made before this track went dormant (see `SESSION.md` for current status):

- **`requires_approval`** (renamed from `mvp1`'s `approve`) = tools that need a human `[y/N]`.
  A *restriction* list, not an allowlist. Empty list ⇒ hook never installed ⇒ nothing gated.
  `["*"]` ⇒ everything gated.
- **MCP tool names are prefixed** `<server>_<tool>` — approval entries must be
  `fs_write_file`, `fs_edit_file`, `fs_move_file`, `fs_create_directory`.
- **Grep is a Go-native `tool.Func`, not an MCP server** — no official MCP server does content
  search; pure Go = zero deps, zero build tags, cross-platform.
- **Simplified Grep over a hardened version** — the NUL-byte binary check is the guard that
  actually matters (measured 24KB vs 2.99MB output on this repo, 124×); skip-list/streaming
  reads deferred until they bite.
