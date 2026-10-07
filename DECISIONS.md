# DECISIONS.md — mvp2 design decision log (mvp2/learn/ and the repo-root migration)

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
- **Test scaffolding built (2026-09-24)**: `windowDataStructureTypes` (factory map, one entry
  per window type), `forEachWindow(t, maxSize, fn)` (subtest-per-type runner, matches the
  originally-planned C1 item), and `msg`/`msgs` (deterministic-ID message builders, matches the
  originally-planned C3 item) now exist at the top of the file, superseding those two plan items.
- **Renamed for the same "history vs. context" reason as above (2026-09-24)**:
  `Test_Unit_OpOnGetMessagesDoesNotModifyChatHistory` → `...DoesNotModifyContext`, and
  `Test_Unit_OpOnSnapshotDoesNotModifyChatHistory` → `...DoesNotModifyContext` — both test
  `ChatContext.GetMessages()`/`Snapshot()`, not anything on `ChatHistory`.
- **`Test_ContextCompaction_DoesNotRemoveMessagesAddedDuringCompaction` tests `ChatContext.Compact`
  directly, not through `handleUserInput`** — deliberate: the fake provider is synchronous, so
  there's no way to make a "real" concurrent message arrive mid-summarization through the normal
  call path. The test hand-reproduces the scenario instead: snapshot, then simulate a concurrent
  `AddMessages`, then `Compact` against the now-stale snapshot.
- **`Test_ContextCompaction_AutoCompactionTriggeredWhenThresholdExceeded` polls with a 2s deadline**
  rather than asserting immediately, because auto-compaction runs in an un-awaitable background
  goroutine (the `A4` "make auto-compaction awaitable via `sync.WaitGroup`/a done channel" item
  from the original test plan is still unbuilt). This is a known, accepted flakiness risk until
  `A4` lands — revisit if this test ever proves flaky in practice.
- **Resolved 2026-09-24** (superseding the "not yet renamed" note this entry used to carry):
  the test is now named `Test_ContextCompaction_CompactingEmptyContextReturnsError` (already
  correct), and `Test_ContextCompaction_EmptyOrWhitespaceSummaryTreatedAsFailure`'s one
  remaining stale doc-comment word ("history" → "context") was fixed. No code behavior changed,
  just naming.
- **`Test_ChatRequest_MalformedHeaderErrorsGracefully` and `Test_ContextWindow_
  MalformedChatMessageErrorsGracefully` removed (2026-09-24)**, rather than renamed or
  implemented. Both were flagged earlier as testing a premise that doesn't map to any real code
  path: `Chat()` never inspects any response header, so nothing could make "a malformed header"
  cause an error; a "malformed" `ChatMessage` is mostly ruled out by Go's own type system, and
  the one realistic case (an unrecognized `Role` value) was never elaborated on. Resolved by
  deleting rather than redefining — no invariant was lost, since neither test description ever
  corresponded to an actual behavior of the code.
- Status: active (renames applied, two ambiguous stubs deleted). Files:
  `chat_w_history_context_session_structs_test.go`.

---

## Slash commands — text after the command (2026-09-25)

- **Original design (reversed 2026-09-25):** text after a slash command was sent as a normal
  chat prompt once the command had run. `/clear what is 3 + 2?` meant "clear, then ask". Why:
  one line could do two things.
- **Why it was reversed:**
  - **Ordering bug.** `handleUserInput` read `msgContext` before the command ran, so
    `/clear <prompt>` and `/compact <prompt>` still sent the pre-command context with the prompt.
  - **Tests missed it.** The first tests checked `fx.Context` *after* the call. That passes
    either way, because the prompt and reply are appended after the provider call returns. The
    bug only showed once a test checked the request actually sent (`fx.Provider.Calls`).
  - **Not the production convention.** In CLIs such as Claude Code, text after a command is an
    *argument to that command* (e.g. `/compact [instructions]`), never a follow-up chat turn.
- **Current design:**
  - Every slash command returns right after it runs; nothing after it is sent as a chat turn.
    This also removes the ordering bug, because the request path now only runs for plain prompts,
    where `msgContext` is always fresh.
  - `/summary <text>` and `/compact <text>` pass `<text>` to `summarizeChatContext` as extra
    summarizer instructions (the new `addlInstructions` parameter). Auto-compaction passes `""`.
  - An empty instruction adds **no** message to the summarization request. Found in review: an
    empty `system` message was being sent on every plain `/compact` and auto-compaction, and
    some servers reject empty content.
  - `/clear <text>` still clears, and the text is **ignored**. `/config` and `/exit` ignore it too.
- **Honest downside:** ignoring `/clear <text>` silently means a user who types a question there
  gets no answer and no hint why. Rejecting with a message ("`/clear` takes no arguments") was
  considered and would be the more user-friendly choice; ignoring was picked for simplicity.
- **Tests:** `Test_ContextWindow_ClearWithTrailingText_TextIgnored`,
  `Test_ContextCompaction_CompactWithTrailingText_PassedAsSummarizerInstructions`,
  `Test_ChatRequest_SummaryWithTrailingText_PassedAsSummarizerInstructions`,
  `Test_ContextCompaction_NoInstructions_NoExtraMessageInSummarizationRequest`. They replace the
  short-lived `...PromptSameLineAs{Clear,Compact}...` tests. The instructions checks match on
  content, not position, so moving the instructions (see the open item below) won't break them.
- Status: active. Files: `chat_w_history_context_session_structs.go`,
  `chat_w_history_context_session_structs_test.go`.

---

## Tools & MCP (2026-09-25)

- **Decided: an internal `Tool` interface, with MCP as one adapter behind it (option B),
  not "every tool is an MCP server" (option A).** A built-in is one Go closure → one `Tool`;
  one MCP server expands into N `Tool`s (one per `tools/list` entry). The loop only sees `Tool`.
- **Why:** the model can't tell the difference: every tool is name + description + JSON
  schema → text result. MCP is a way of packaging and delivering tools, not a different kind
  of tool. MCP-only would mean:
  - the agent can do nothing out of the box (reading a file needs an external binary, e.g.
    `npx` for the official filesystem server);
  - tools that need agent internals (subagent, history search, todo, compact) would need a
    protocol to expose `ChatSession`;
  - every call pays a process/network round trip, with more ways to fail;
  - tests need a fake MCP server.
  Same shape as `mvp1`, and consistent with Track 1's "Grep is a Go-native `tool.Func`".
- **Dividing rule:** MCP if the tool must be added without recompiling, is in another language,
  or is someone else's. Built-in if it needs agent internals or the agent is useless without it.
  Built-ins are added at compile time and switched on via `Config.Tools`. No second runtime
  plugin system; `/mcp-add` is the runtime path.
- **Planned built-ins:** `read_file`, `list_dir`/`glob`, `grep`, `write_file`/`edit_file`
  (approval-gated), `shell` (approval-gated, timeout + process-group kill). Later: subagent,
  todo, memory search. Skipped: calculator, time. Pick one source per capability, so built-in
  and MCP file tools aren't loaded side by side.
- **Honest downside:** two ways to add a tool, and adding a built-in means recompiling.
- **Rejected middle path:** built-ins compiled as an in-process MCP server over in-memory pipes.
  It keeps one protocol, but JSON-RPC-encoding a call to a function in the same binary is
  ceremony without payoff.
- **Build order:**
  1. `Tool` interface + one built-in (`read_file`) + tool fields on `ChatMessage` + a ReAct loop
     against the OpenAI tool-calling format;
  2. registry, with `cfg.Tools` choosing the built-ins;
  3. MCP stdio adapter;
  4. `/mcp-add`, `/mcp-remove`, `/mcp-list`;
  5. HTTP transport.
- Status: active. Files: `chat_w_history_context_session_mcp_tools.go` (gen 8).

### MCP protocol version: modern-only → dual-era (2026-09-27)

- **Current decision (revised later the same day): a dual-era client.** It supports modern
  (2026-07-28) and legacy (`initialize`-based, sending `2025-11-25`) servers. Modern is built
  first, then the legacy fallback. Both live inside `connectMCP`. The rest of the agent only sees
  `Tool`s and never learns which era a server is.
  - **Probe rule (stdio):** send `server/discover` with our preferred modern version in `_meta`.
    - A `DiscoverResult` means modern.
    - A recognized modern error (e.g. `-32022`) means modern: retry with a listed version, and
      do **not** fall back.
    - Any other error, or no reply before a timeout, means legacy: send `initialize`, then
      `notifications/initialized`. The spec says the fallback **must not** depend on one
      specific error code.
  - Record the era for each server and keep it for the life of that process. Probe again after
    a restart.
  - **Why the revision:** a modern-only client can't use legacy servers, and those are probably
    most servers today. Since the probe goes first either way, dual-era only adds code to one
    function.
  - **Honest downside:**
    - two ways of building requests (with `_meta` vs relying on the session);
    - per-server era state;
    - a probe timeout that slows `/mcp-add` for legacy servers that never answer;
    - the fake test server has to be able to act as either kind.
  - Build order, step 3 becomes: **3a** modern over stdio; **3b** the legacy fallback.
- **Original decision (first half of 2026-09-27, superseded above):** the client speaks only
  the stateless 2026-07-28 revision. Connecting sends
  `server/discover`; after that, every request carries `_meta` with
  `io.modelcontextprotocol/protocolVersion` and `.../clientCapabilities` (both required) and
  `.../clientInfo` (should). No `initialize` and no `notifications/initialized`.
  - Replaces an earlier recommendation (2026-09-25, never built) to start legacy-only like
    `mvp1/tool/mcp.go:66` and add the probe later.
- **Why:** it's the current spec, and it's simpler to reason about. There's no session, and the
  stdio process isn't a conversation, so a crashed server can just be restarted and the request
  retried. It's also the version to learn.
- **Honest downside:** a modern-only client **can't talk to legacy servers**, and at
  2026-09-27 most servers in the wild are probably still legacy, since the revision is two
  months old. We still send `server/discover` first, as the spec recommends. A legacy server
  then fails straight away, and `/mcp-add` can print a clear "legacy server, not supported"
  error, instead of an ambiguous `tools/call` being run under legacy rules. Dual-era support
  (fall back to `initialize` on any non-modern error or a timeout) is deferred and would stay
  inside the connect function.
- **Handling responses:**
  - pick a version from `supportedVersions`;
  - on `-32022` (UnsupportedProtocolVersion), retry with a version from `data.supported`;
  - treat a missing `resultType` as `"complete"`, and treat `"input_required"` as an error for
    now, because we declare no client capabilities;
  - keep `instructions` (a candidate to add to the system prompt).
- **Auth:** stdio gets credentials from environment variables. HTTP uses the spec's OAuth-based
  Authorization framework plus the `MCP-Protocol-Version` header. Auth is deferred to step 5.

### Tool registry, tool calls and the ReAct loop (2026-09-28/29)

Files: `chat_w_history_context_session_mcp_tools.go` and its `_test.go` (gen 8).

**Tool registry (`ToolRegistry`, `toolEntry`)**
- **Decided:** the registry holds `map[string]toolEntry{tool, def}` plus a cached `[]ToolDef` sorted
  by name, behind a `sync.RWMutex`. The sorted slice is rebuilt only in `Register` and `Remove`
  and is never modified in place (copy-on-write), so `GetToolDefs()` returns it with no copy and
  no per-request sort.
- **Why:** providers cache on an exact prompt prefix, and tool definitions sit at the front of the
  prompt. Go map iteration order is random, so an unsorted list could change the prefix and
  invalidate the cache for the whole conversation. Order never affects correctness; it only
  affects cost and latency. Sorting on every request was rejected: it is cheap (n is about 5 to
  50) but recomputes something that only changes on register or remove.
- **`toolEntry` captures `GetToolDefinition()` once, at `Register`.** The rebuild never calls into
  a `Tool`, so its cost doesn't depend on how a tool is written (an MCP wrapper can't make
  rebuilds slow), and the map key and the stored def can't disagree. A first version called
  `GetToolDefinition()` twice in `Register`; that was fixed.
- **Rejected: an ordered slice plus a lookup map, with no sort (option A).** Simplest, but the
  order would depend on registration order, which becomes nondeterministic once MCP servers
  connect concurrently. Revisit only if the sort ever shows up as a cost.
- **Duplicate names are an error, not an overwrite,** so a second MCP server can't shadow a
  built-in such as `read_file`. Empty names and a nil `Tool` are errors too. Changing a tool is
  `Remove` then `Register`.
- **A typed nil pointer is not handled.** An `isNilTool` helper (using `reflect`) was written and
  then removed: too complex for an edge case no code path creates. The plain `t == nil` check
  catches only a nil interface. If a typed nil ever arrives, `Register` panics in
  `GetToolDefinition()`.
- **`Lookup` returns `Tool`, not `toolEntry`.** `toolEntry` is an unexported detail, and callers
  only need to call the tool. Trigger to revisit: if callers need a source label (built-in vs
  `mcp:<server>`) for `/mcp-list` or approval policy, return a small exported type; don't export
  `toolEntry`.
- **`NewToolRegistry` takes `[]Tool`, not variadic.** A wash: the production caller builds a slice,
  and the tests' `newRegistry` helper stays variadic.
- **Honest downsides:** the stored def is a snapshot, so a tool whose definition changes after
  registration (e.g. an MCP `tools/list_changed`) isn't noticed until it is re-registered.
  "Don't modify the slice `GetToolDefs` returns" is a convention Go can't enforce. The `RWMutex`
  isn't needed while only one goroutine touches the registry; it was chosen for the read-heavy,
  write-rare shape, and swapping to a `Mutex` is a two-line change.

**Running a tool call (`runToolCall`, `callToolSafely`)**
- **Decided: `runToolCall` returns a `ChatMessage`, never a Go `error`.** Every failure (unknown
  tool, arguments that aren't a JSON object, a tool error, a tool panic) becomes a `role:"tool"`
  message with `error: ...` in `Content` and `ToolCallID = call.ID`.
- **Why:** the failure is something the model can react to (a misspelled tool name, bad
  arguments), and providers reject a request where any `ToolCall` lacks exactly one matching tool
  message. An `error` return would force every call site to convert it, and one forgotten branch
  would produce an orphaned call. Loop-level failures (a cancelled context) are the loop's job,
  handled by checking `ctx.Err()` at the top of each step. Same principle as mvp1: `Run` returns
  an error only for LLM or memory failures and the step limit.
- **Arguments:** must be a JSON object; `null`, arrays and strings are rejected; an empty string
  is normalized to `{}` so no-arg tools work. There is no `IsError` field on `ChatMessage`, so the
  model tells failures apart only by the `error:` prefix; keep that prefix consistent.
- **`callToolSafely` wraps `CallTool` with `recover()`,** turning a panic into an ordinary error
  (`tool panicked: <value>`) that flows through the same path. Named returns are what let the
  deferred function set `err`.
  - **Limits:** it covers only the calling goroutine (a goroutine the tool spawns can still crash
    the process); fatal runtime errors (concurrent map writes, out of memory) are unrecoverable;
    a recovered panic looks like an ordinary tool error to the model, so a real bug can hide; and
    the tool's own state may be left inconsistent.
  - The stack trace is dropped. If wanted, `runtime/debug.Stack()` written to `cs.OutBuffer`.
- **Tool calls run sequentially** in the first draft. mvp1 fans out on goroutines. Trigger to
  change: a slow tool (MCP, shell). Each goroutine would then need its own `recover`.

**ReAct loop shape (`reActLoop`, `handleUserInput`)**
- **Decided: one local `turn` slice, batch persistence after success.** The loop accumulates the
  turn (user msg, assistant reply, tool results, ..., final assistant) in a local slice, and each
  step sends `system + prior context + turn`. It never touches history or context. On success
  `handleUserInput` appends the whole turn to `MsgHistory` and `MsgContext` in one batch. On any
  error nothing is persisted.
- **Why:** a failed or aborted turn leaves no half-turn (this matches the existing behavior pinned
  by `Test_ChatHistory_FailedResponse_*`); auto-compaction snapshots the context from a
  goroutine, and incremental writes could let it summarize an assistant `tool_calls` message
  whose results haven't been written; and the window can't trim mid-turn because nothing enters
  it until the turn ends.
- **Honest downside:** a crash or cancellation mid-turn loses the turn's work, so the model won't
  remember what a tool already did. Trigger to revisit: persistent history, or tools with side
  effects (write, shell). Then append each step to `MsgHistory` (the durable log) while still
  batching the context write.
- **Step limit:** `MaxReActSteps = 10` (mvp2/learn; **superseded 2026-10-07**: the repo-root `agent/` has no constant,
  the limit is `cfg.MaxSteps`, default 10 in `GetDefaultConfig`, overridable by `-maxsteps`; see `§ CLI flags`). Exceeding it returns `ErrMaxSteps`, a sentinel error
  wrapped with `%w` so callers and tests use `errors.Is` rather than comparing text. Nothing is
  persisted. Alternative not taken: persist the truncated turn with a synthetic closing assistant
  message, so tool side effects aren't lost.
- **The assistant reply is rebuilt with `createChatMessage`,** which copies `ToolCalls` (dropping
  them was a real bug: the next request would carry tool messages with no preceding tool call),
  hardcodes `Role`/`Type` to `"assistant"` (the provider is the untrusted party and may return
  an empty role), and passes an empty `ToolCallID` (assistant messages never have one; the IDs
  the model chose live in `ToolCalls[i].ID`). Alternative: stamp the provider's reply in place,
  which would keep any field added to `ChatMessage` later; `createChatMessage` copies only the
  five fields it knows.
- **Printing:** `handleUserInput` is the output boundary; the loop should return errors, not
  print them. (Cleanup pending, see SESSION.md.)
- **Ctrl+C:** `signal.NotifyContext` in `main` cancels a ctx that is passed down through
  `runLoop`, `handleUserInput`, `reActLoop`, `Chat` and `CallTool`, so the loop needs no
  `WithCancel` of its own (a child ctx nothing cancels adds nothing). What it needs is the
  `ctx.Err()` check at the top of each step and tools that honor the ctx. A `WithCancel` /
  `WithTimeout` inside the loop only makes sense for a per-turn timeout or for cancelling sibling
  tool goroutines. Open question about what Ctrl+C should mean is under § "Deferred / open
  decisions".

**Test double**
- **`fakeProvider` gained a scripted mode.** `Script []ChatMessage` plays back one reply per `Chat`
  call; once `Script` is set, running past its end is an error, so a loop that over-calls fails
  loudly instead of quietly reusing `Reply`. With `Script` unset it behaves as before (the
  other 37 uses are untouched). `ToolDefs` records each call's tools, parallel to `Calls`.

### MCP client: official Go SDK, not hand-rolled; restart with idempotency rules (2026-09-30)

- **Decided: use `github.com/modelcontextprotocol/go-sdk` (checked: v1.8.0).** Supersedes the
  2026-09-27 plan for a hand-rolled stdio client and the "leaning hand-rolled first" note.
  - **Why:** `Client.Connect` already does the dual-era probe (`server/discover` first, then
    `initialize` fallback) and injects the 2026-07-28 per-request `_meta` itself, so old slices
    A, B and E disappear. What stays ours is the adapter (`mcpTool` implementing `Tool`),
    `Config` wiring and lifecycle. HTTP + OAuth (step 5) becomes configuration, not a rewrite.
    It's also the long-term path the protocol authors provide.
  - **Honest downside:** we no longer see the wire protocol; a new dependency; 2026-07-28
    support is recent, so pin the version and expect churn.
  - **Era handling:** the SDK's `*mcp.ClientSession` holds the negotiated version, capabilities
    and the connection. No extra state structs; the agent keeps one small `mcpServer{name,
    session}` per server and never learns the era.
  - The "dual-era" decision above still stands as behaviour; only *who implements it* changed.
- **Decided: restart a crashed MCP server, with a retry rule based on idempotency.**
  - Server found dead *before* a call is sent: restart, reconnect, send. Safe, nothing in flight.
  - Dies *during* a call, tool read-only/idempotent: restart and retry once.
  - Dies *during* a call, tool not known safe: restart, **don't retry**; return an error
    observation saying the call may or may not have completed, so the model can check state.
  - Safety comes from the tool annotations (`readOnlyHint`, `idempotentHint`,
    `destructiveHint`). They are **untrusted hints**: fine for retry decisions, never to skip
    approval. A missing hint means "not safe".
  - Lives inside `mcpServer` (mutex-guarded session + `reconnect()`), not in the loop.
  - A restart of a legacy server is a new session, so server-side session state is lost.
  - **Why:** can't tell whether a mutating call finished before the crash; blind retry could
    double-apply it. **Honest downside:** more code in the adapter, and the hints are only as
    honest as the server.
  - To check when building: which error the SDK returns for a closed session. Calls on a
    session closed by the server return an error wrapping `mcp.ErrConnectionClosed` (from the SDK
    docs, `errors.Is` should work), so detection needn't use string matching. Verify in a test.
- **Findings from the first real run (2026-09-30):**
  - The official filesystem server is legacy-era (`server/discover` → `-32601`); the SDK fell
    back to `initialize` on its own. The retry rule is workable with real data: its tools are
    annotated (reads `readOnlyHint`; `write_file`/`create_directory` idempotent; `edit_file` and
    `move_file` non-idempotent, so they get the no-retry path).
  - The SDK declares the `roots` capability by default, so servers can call back `roots/list`.
    Declaring no client capabilities would avoid that; not needed yet.
  - `move_file` shows why blind retry is wrong: after a crash that happened *after* the move, a
    retry fails with "source not found" even though the first call succeeded.
- Status: scratch driver works (`mvp2/learn/mcp_scratch.go`); the adapter, config wiring and
  restart are not built yet. Files: `chat_w_history_context_session_mcp_tools.go`.

### Config file loading: `setDefaultConfig(filename) (Config, error)` (2026-10-01)

- **Decided: it returns `(Config, error)`, not just `Config`.** The spec said "error when the file
  is not found", and Go has no other way to return one. Not-found wraps `os.ErrNotExist` (`%w`).
  Invalid JSON is also an error (Jerry will add richer validation later).
- **Decided: start from `getDefaultConfig()` and overlay the file.** Keys missing from the file
  keep their defaults, and `InBuffer`/`OutBuffer` stay wired. Honest downside: you can't tell
  "file said the default" from "file omitted it".
- **Decided: unknown keys warn on stdout and are ignored.** Two passes: `json.Unmarshal` into
  `Config` (drops unknowns silently), then into `map[string]json.RawMessage`, compared against
  `Config`'s json tags via reflect (case-insensitive, like `encoding/json`). Rejected
  `DisallowUnknownFields`: it errors on the first unknown key instead of warning on all.
  Downside: only top-level keys are checked, not unknown keys inside nested structs.
- **Decided: warnings go to `cfg.OutBuffer`** (stdout by default), per the "all output through
  OutBuffer" rule. Tests capture it by swapping `os.Stdout`, because the buffer is set inside the
  function.
- **`InBuffer`/`OutBuffer` tagged `json:"-"`**: they are runtime wiring, and without the tag a
  stray `"InBuffer"` key would make the decode fail instead of warn.
- **Empty filename means `DefaultConfigFile` (`config.default.json`)**: Go has no default
  arguments.
- **Known problem:** the existing `mvp2/learn/config.default.json` is not loadable yet. It has a
  trailing comma (invalid JSON), and some keys have no `Config` field (`maxSteps`, `workDir`,
  `memory`, `contextWindow`, `mcpServers`, `subagents`). (2026-10-01, later: its key names were
  aligned with the camelCase tags, see below.)
- **Decided (2026-10-01): all config file keys are camelCase** (`userId`, `baseUrl`,
  `apiKeyName`, `systemPrompt`, `chatStoreType`, `mcpServers`), superseding the snake_case tags.
  **Why:** one convention across the file, and `mcpServers` (and `command`/`args`/`env`) is
  camelCase in every other MCP client, so copied configs load unchanged. Go field names stay
  PascalCase (`UserID`): exported fields can't be camelCase. **Honest downside:** an old
  snake_case config no longer applies; `user_id` is now an unrecognized key (warns, default
  kept), because `encoding/json` ignores case but not underscores. Also renamed the default
  file's `api_key_env` to `apiKeyName` to match the field (its value is an env var *name*).
- Files: `chat_w_history_context_session_mcp_tools.go` and its `_test.go` (gen 8).

### MCP server transport: inferred per server, optional `type` for SSE (2026-10-01)

- **Decided: the transport is per server entry** (`mcpServers` map key = server label), inferred
  from its fields: `command` → stdio; `url` → Streamable HTTP; `url` + `"type": "sse"` → legacy
  SSE. Both or neither of `command`/`url` is a config error naming the server. `type`, if given,
  must agree with the fields.
- **Why SSE is in scope:** the agent exists to run third-party MCP servers, and older ones are
  SSE-only. SSE itself is deprecated in the spec (replaced by Streamable HTTP, 2025-03-26), so a
  bare `url` defaults to HTTP and SSE is opt-in.
- **Honest downside:** a legacy SSE `url` pasted without `type` fails at connect; the error text
  must hint "set type: sse". Rejected for now: requiring `type` on every `url` server (extra
  line for the common case) and auto-fallback probe HTTP → SSE (more code, murkier failures).
  Revisit the probe if the missing-`type` error keeps biting.
- `MCPServerConfig` has no `Name`: the label is the map key (`map[string]MCPServerConfig`); sort
  keys before connecting since map order is random.
- Open: how custom `headers` attach in the Go SDK; exact SDK transport type names (verify with
  `go doc`). Nested unknown-key warnings in `setDefaultConfig` (top-level only today).

**Reversed later the same day (2026-10-01): legacy HTTP+SSE is NOT supported; warn and skip.**
The "SSE is in scope" bullet above is superseded. Source: spec 2026-07-28, transports page §
"HTTP+SSE Transport (2024-11-05)" and the deprecated-features registry: deprecated since
`2025-03-26` (SEP-2596), "eligible for removal in a future revision", replaced by Streamable HTTP.

- **Decided: support stdio and Streamable HTTP only.** The spec defines exactly those two
  transports. Within Streamable HTTP there are three response modes, all handled per request:
  (1) one JSON object, (2) a request-scoped SSE stream (progress notifications then the final
  response), (3) the long-lived SSE response of `subscriptions/listen`. They are modes of one
  transport, not three transports. Clients MUST support (1) and (2); the SDK is expected to do it.
- **Decided: a server that uses HTTP+SSE is skipped with a stdout warning, never an error**, and
  the other servers still load. Recognised by `"type": "sse"` (checked before any connection).
  Suggested text: `[warning] MCP server "<label>": skipped, it uses the deprecated HTTP+SSE
  transport (protocol 2024-11-05); its tools were not added`.
- **Decided: a bare `url` always means Streamable HTTP; no legacy detection.** If the server is
  really HTTP+SSE, the connect fails like any other connection failure (below). Rejected: the
  spec's detection probe (POST fails with 400/404/405 and a non-modern body, then GET returns an
  `endpoint` event): more code for a deprecated path. Honest downside: the warning for such a
  server says "connect failed", not "legacy SSE"; the user must add `"type": "sse"` themselves
  to get the precise message. Revisit if that confuses people.
- **Decided: any per-server connection failure is also warn-and-skip, not fatal** (bad `npx`
  package, unreachable `url`, handshake error, legacy SSE behind a bare `url`). The warning goes
  to stdout and names the server and the error; the other servers still load, and the agent
  starts even if none do. So `setupToolRegistry` must continue past a failing server, and the
  failed server's session must be closed so a half-started process doesn't leak. Honest
  downside: a typo in the config shows up as a missing tool, not a startup failure, so the
  warning text matters. Config *shape* errors (both `command` and `url`) stay errors.
- **Not the same thing:** Streamable HTTP of protocol 2025-03-26..2025-11-25 (sessions via
  `Mcp-Session-Id`, GET stream, DELETE) is an older *era* of the supported transport, not
  deprecated; it falls under the existing dual-era fallback decision, handled by the SDK.
- **Why:** third-party breadth was the argument for SSE, but it is a deprecated path with
  removal pending, costs a second transport implementation, and the SDK doesn't give it for free
  as far as we know. **Honest downside:** old servers that only speak HTTP+SSE can't be used; a
  user needs a Streamable HTTP front-end or a stdio bridge. Revisit if a server we need has no
  alternative.
- **`subscriptions/listen` (mode 3) is a separate later slice**, not part of the adapter: it is
  a client-initiated, long-lived request (the spec's subscriptions pattern is transport-neutral,
  so it also applies to stdio). It needs a goroutine per server, cancel-then-close on shutdown,
  and re-listening after a reconnect. It only delivers best-effort `list_changed` notifications,
  so polling is still the fallback. Progress notifications are NOT delivered on it.

### `mcpTool`: the adapter from one MCP tool to `Tool` (2026-10-01)

- **Decided: one `*mcpTool` per tool listed by a server**, built by `newMCPTool(server, *mcp.Tool)`.
  The SDK's `*mcp.Tool` is a plain data struct (Name, Description, InputSchema, Annotations) and
  does not satisfy our `Tool` interface; `mcpTool` is the wrapper that does (pointer receivers,
  `var _ Tool = (*mcpTool)(nil)`).
- **Two names, deliberately:** `toolName` is the name the server knows (what `tools/call` sends);
  `toolDef.Function.Name` is `<server label>_<toolName>`, which is what the model sees and what
  keeps two servers' tools from colliding. The label is the `mcpServers` map key, not the agent
  name. Mixing the two up is the classic bug.
- **`server` is a `*mcpServer`, not a copy:** all tools of a server share one session, and the
  server will hold a mutex and a session that `reconnect()` swaps.
- **`safeToRetry`** = annotations non-nil and (`ReadOnlyHint` or `IdempotentHint`). Nil means "not
  known safe". Annotations are untrusted hints: used for retry only, never to skip approval.
  Checked against the real filesystem server: reads, `write_file`, `create_directory` are safe;
  `edit_file` and `move_file` are not. Nothing reads the field until slice 5.
- **A nil `InputSchema` is left empty** so `NewToolDef` supplies its "no parameters" default
  (marshalling nil would send the JSON text `null` to the model).
- **Result flattening (`flattenMCPResult`, a pure function):** text blocks joined with `\n`; an
  image becomes `[image omitted: <mime>]`; any other content type becomes `[non-text content
  omitted]`; with no content blocks it falls back to `StructuredContent` as JSON; `IsError`
  becomes a Go error carrying the text (so `runToolCall` prefixes `error:`), as does a nil
  result. Output is capped at `MCPResultMaxBytes` (64 KB) with a truncation note, and
  `strings.ToValidUTF8` drops a multi-byte character the byte cut split. **Honest downside:**
  images, audio and resources are not passed to the model at all yet.
- **Two failure kinds in `CallTool`:** a transport/protocol error (`err != nil` from the SDK)
  versus a tool-level failure (`IsError`). Both become error observations.
- Verified end to end (2026-10-01, throwaway test run from the scratchpad, not saved in the
  repo) against the real `@modelcontextprotocol/server-filesystem`: 14 tools listed, calls with
  `json.RawMessage` arguments work, a missing file and a missing argument come back as errors.
- **Not decided:** whether `CallTool` goes through a `server.call(...)` seam now or when slice 5
  needs it (recommended: now, one line). Name sanitizing and the length cap for model-facing
  names (see SESSION.md).

### Shutdown: one `Agent` object, one deferred `gracefulShutdown` in `runAgent` (2026-10-01)

- **Decided (Jerry's design):** `runAgent` creates the cancellable context at the top, creates an
  empty `Agent{session, toolRegistry, mcpServers}`, and does `defer gracefulShutdown(cancel,
  &agent)` before any setup. Each setup step fills the `Agent`; shutdown cleans up whatever is in
  it at that moment. `runLoop` no longer defers or creates its own cancel.
- **Why it works:** the defer captures the *pointer*, so it sees the struct's final contents
  (deferring with `chatSession` as an argument would capture `nil`, since defer evaluates its
  arguments immediately). It also covers an early return from setup and a Ctrl+C during setup,
  which the old `defer` inside `runLoop` could not.
- **Shutdown order:** cancel, wait for background compaction, close MCP servers. `gracefulShutdown`
  must tolerate a nil `agent.session` (setup failed before step 8); a missing guard was a real
  nil-dereference bug, fixed 2026-10-01.
- **`ctx.Err()` is checked before `runLoop`** so a Ctrl+C during setup doesn't start a loop on a
  dead context. **`main` treats `context.Canceled` as a clean exit** (`errors.Is`), not `fatal`.
  Honest downside: a real failure that surfaces wrapped as a cancel would also exit quietly;
  tighten to `ctx.Err() != nil` if that ever matters.
- **Rejected:** a `defer s.Close()` inside `connectLocalMCP` (a defer runs when its own function
  returns, so it would close the server immediately). The owner of a resource closes it.

### Remote MCP: `DisableStandaloneSSE: true`, headers via `RoundTripper` (decided 2026-10-01, recorded 2026-10-02)

- **Decided (Jerry, in the 2026-10-01 session; not written down at the time):** the remote
  (Streamable HTTP) transport is built with `DisableStandaloneSSE: true`. The client sends only
  POSTs and never opens the long-lived GET stream. Replies to a POST (plain JSON or a
  request-scoped SSE stream) still work, so tool listing and tool calls are unaffected.
- **Why it is safe here:** the agent lists tools once at startup and implements no
  server-initiated features, so it has nothing to receive on that stream.
- **Honest downside:** server-pushed messages are lost: `tools/list_changed` notifications and
  server-to-client requests (sampling, elicitation). **Revisit trigger:** if the agent wants live
  tool-list refresh or any of those features, set it back to `false`.
- **Also avoids:** servers that answer the GET with a 405 or handle it badly.
- **Custom headers (e.g. `Authorization`):** `StreamableClientTransport` has no `Headers` field.
  Checked 2026-10-02 against v1.8.0 (the newest tag, and Jerry's version) and the SDK `main`
  branch (v1.8.1-pre): fields are `Endpoint`, `HTTPClient`, `MaxRetries`, `DisableStandaloneSSE`,
  `OAuthHandler`, `MaxEventSize`. So config `headers` are applied by an `http.Client` whose
  `Transport` is a small `RoundTripper` that clones each request and sets the headers.
- **Files:** `chat_w_history_context_session_mcp_tools.go` (`connectRemoteMCP`, `headerTransport`);
  tests in `..._mcp_tools_test.go`.
- **Status:** active for the decision; **but the flag currently has no effect, see the bug below.**
  Code written and smoke-tested against `https://mcp.deepwiki.com/mcp` on 2026-10-02.

#### Bug found 2026-10-02: `LoggingTransport` hides the client's `sessionUpdated` hook (OPEN, needs Jerry's decision)

- **What happens.** After the initialize handshake the SDK client calls `sessionUpdated()` on the
  connection, but only if it implements the `clientConnection` interface (`mcp/client.go:401`, and
  `:332` on the discover path). `mcp.LoggingTransport` wraps the connection in `loggingConn`,
  which has no such method, so the call is silently skipped. `connectRemoteMCP` wraps the
  transport in `LoggingTransport` for debugging, so for remote servers:
  1. **`DisableStandaloneSSE` is a no-op.** The standalone GET stream is started from
     `sessionUpdated` (`mcp/streamable.go:2138-2165`); it never starts, whatever the flag says.
  2. **`Mcp-Protocol-Version` is missing** on every request after initialize. The spec requires
     it. DeepWiki tolerated that in the smoke test; a stricter server could reject the requests.
- **Evidence (a scratch copy against the in-process fake server, recorded request headers):**

  | | with `LoggingTransport` (current code) | without it |
  |---|---|---|
  | GET stream, flag `false` | not opened | opened |
  | GET stream, flag `true` | not opened | not opened |
  | `Mcp-Protocol-Version` after initialize | empty | `2025-11-25` |
  | `Mcp-Session-Id` | present | present |

- **Why the first version of the test missed it.** Two separate reasons, both worth remembering:
  the SDK skips the GET stream entirely on protocol `2026-07-28` and later (so a fake server that
  speaks the new spec never triggers it), and the wrapper hid it on the old path. The fake server
  in the tests now rejects `server/discover` like DeepWiki does, so it exercises the old path.
- **Local (stdio) is not affected:** `sessionUpdated` on the I/O connection is server-side only.

**Decisions Jerry needs to make:**

1. **What to do about `LoggingTransport` on the remote path.**
   - **(a) Remove it from `connectRemoteMCP` (recommended).** Restores the header and makes the
     flag meaningful. Downside: no JSON-RPC debug log for remote servers. HTTP-level logging can
     come back as a logging `RoundTripper` around `headerTransport.base`, which doesn't touch the
     SDK's connection type.
   - **(b) Gate it behind a debug setting, off by default.** Keeps the log. Downside: debug mode
     changes behavior, which is exactly what bit us; the bug returns whenever it is on.
   - **(c) Leave it and document it.** Cheapest, but keeps the missing header.
   - **Revisit trigger for (a):** if a future SDK version makes `loggingConn` forward
     `sessionUpdated`, (b) becomes safe.
2. **Whether to keep `LoggingTransport` on the local (stdio) path.** Harmless today because of the
   point above; decide together with 1 so both paths log the same way.
3. **After 1 is decided:** add the assertions that the current tests cannot make meaningfully
   (no GET with the flag on; `Mcp-Protocol-Version` set on non-initial requests) and a mutation
   check that flipping the flag to `false` fails the test.

**Claude's notes (2026-10-02):**

- Tests added today: remote connect with and without headers, `headerTransport` (3 tests), local
  connect via a re-exec of the test binary (`TestHelperMCPServer`, skipped in normal runs), and
  two error-path tests. Mutation checks: dropping `Clone` is caught; not wiring `headerTransport`
  is caught; flipping the SSE flag is **not** caught (that is the bug above).
- `connectLocalMCP` never applies `config.Env`, so a configured `env` map doesn't reach the child
  process (the local test uses `t.Setenv`). Still open, SESSION.md "To do next" item 2.
- There is no `${VAR}` expansion in header values yet, so a token in `config.json` is literal.
  `os.ExpandEnv(v)` in `headerTransport.RoundTrip` is a one-line change when wanted.
- The checked-in `config.default.json` has `deepwiki` configured with no auth header; don't put a
  real token there.
- The `DisableStandaloneSSE` decision itself was made in the 2026-10-01 session but never
  recorded; the SESSION.md notes didn't carry it. Record decisions as they're made.
- macOS has no `timeout` command; use `go test -timeout 30s` when running experiments.

---

## Package structure & CLI flags — `mvp2/` root migration (2026-10-05 → 10-07)

### Package dependency direction: `agent/` imports the capability packages

- **Status: active** (decided 2026-10-06). The arc is kept because the first two positions were
  each argued for and then dropped.
- **Position 1 — mvp1's design (reference, not adopted for mvp2).** `agent/` is a pure kernel that
  imports nothing from the module; it owns the message model and the five interfaces, and every
  plug-in (`llm/`, `memory/`, `harness/`, `hooks/`, `tool/`) imports `agent/`. Only `main.go`
  knows concrete implementations. Upside: kernel is embeddable with zero extra deps. Downside:
  capabilities are coupled to the agent package just to name `Message`/`Tool`.
- **Position 2 — layered (proposed 2026-10-05, not adopted).** Keep the pure kernel, add
  `config/` and `app/` (`Setup(cfg, opts...)`, mode drivers) and `cmd/<binary>/main.go`, so a
  future daemon, eval runner or MCP-wrapper binary reuses `app/`. Rejected as more packages than
  the design needs now; it also kept capabilities dependent on `agent/`.
- **Position 3 — adopted.** Capabilities (`llm/`, `memory/`, `harness/`, `safety/`, `mcp/`,
  `builtin/`, ...) are independent packages that provide functionality; they do **not** import
  `agent/`. `agent/` holds the ReAct loop and the wiring and imports them. `main.go` →
  `agent/` + `config/`.

  ```
  llm/       leaf: owns Message, ToolCall, ToolSpec, Response + provider adapters
  memory/ harness/ safety/ mcp/ builtin/   import llm (shared types) only, never agent/
  agent/     imports all of the above: loop, setup, wiring; defines Tool/Memory/Hook/... interfaces
  config/    leaf: Config, file loading, flag parsing + overlay
  main.go    imports agent, config
  ```

- **Why**: Jerry's view is that a capability (memory, safety, MCP) is functionality *offered to*
  an agent, so it should be independent of any agent instance; the agent depends on the
  capabilities, not the reverse. It also makes each capability testable and usable without
  constructing an agent.
- **How it stays acyclic.** Go interfaces are satisfied structurally, so `agent/` can define
  `Tool`, `Memory`, `Hook`, `ContextBuilder` where they are consumed, and `mcp.Tool` etc. satisfy
  them without importing `agent/`. The catch: any type in those method signatures must live
  *below* the capabilities — hence `Message`/`ToolCall`/`ToolSpec` live in `llm/`, not `agent/`.
  Rule: **no capability may need a type defined in `agent/`**; the compiler flags a violation as
  an import cycle.
- **Honest downsides.** (1) `llm/` becomes the foundation package, so `memory/` imports it only
  to get `Message` — if that grates, extract a tiny `msg/` leaf later (mechanical move).
  (2) `agent/` is no longer a pure kernel: it pulls in every concrete implementation including the
  MCP SDK, so outside embedders carry that weight. Accepted: there is no planned external embedder.
  (3) Test fakes need constructor overrides (e.g. `WithProvider`) since the agent builds its own
  parts by default.
- **Subagents** stay simple: `agent.Subagent` is a `Tool` living in `agent/`.
- **Deferred, not decided:** extra binaries (`cmd/agentd`, eval runner) and an `app/` layer. If a
  second binary appears, the wiring in `agent/` is what it would reuse; revisit then.
- **Config / agent construction rule kept from Position 2:** setup takes a `Config` value, never
  `os.Args` / `flag.*`, and never calls `os.Exit`, so a non-CLI caller could build an agent too.
- Files affected: repo root (`main.go`, `config/`); `CLAUDE.md` gets a separate section for this
  layout (the mvp1 section stays as the reference design).

### As built: package names, where `Tool` lives, and a drift from the plan (2026-10-07)

- **Status: active.** The adopted dependency direction above was implemented with different names
  than the sketch used, and one interface landed somewhere other than planned.

  | Sketch | As built | Holds |
  |---|---|---|
  | `llm/` | `model/` | `ChatMessage`, `ToolCall`, `ToolDef`, the `Provider` interface, `OpenAICompat`, `PrepareChatRequest`, `NewToolDef`, `CreateID`; `model/modeltest/` has the shared test doubles |
  | `harness/` | `contextwindow/` | `ChatContext`, the four window strategies, summarize/compact |
  | `memory/` | `memory/` | `ChatHistory` + in-memory implementation |
  | `builtin/` + `mcp/` | `tools/builtin/` + `mcpconnect/` | built-in tools; MCP client (see its own entry) |
  | (`agent/` defines `Tool`) | `tools/` defines `Tool` | `Tool` interface, `ToolRegistry`, `SetupBuiltinTools` |

- **`Tool` lives in `tools/`, not `agent/`.** Three packages need to name it (the registry, the
  built-ins, `mcpconnect`), all on the tools side; `agent/` only consumes it. Putting it in
  `agent/` would have forced `mcpconnect` to import `agent/`, the exact cycle the design forbids.
  Honest downside: "the consumer defines the interface" is weakened for `Tool`, and `mcpconnect`
  now depends on `tools`.
- **Capabilities also import `config/`.** `SetupProvider`, `SetupMemoryStore`, `SetupChatContext`
  and `SetupMCPConns` take a `config.Config`, so the rule "capabilities import only the shared
  types" is not literally true; `config/` is a leaf, so no cycle. Downside: each capability knows
  `Config`'s shape. Narrower parameters are the cleanup if that grates.
- **Names settled along the way.** A package called `context` was rejected (it shadows the stdlib
  `context` and invites aliasing), so `contextwindow/`; `harness` was rejected as far too broad for
  "the messages the model sees"; `msgcontext.go` is the file holding `ChatContext`. Import paths
  are the module path plus the folder (`github.com/jerryschen31/minimal-agent/model`); Go has no
  relative imports.
- **Proposed but not applied:** rename `Provider.Chat` to `Complete` (it is one request/response
  cycle used by every mode, not just chat) and drop the `Chat` prefix from `ChatMessage`,
  `ChatRequest`, `ChatResponse`. The code still uses the `Chat*` names; the rename is mechanical, so
  do it when nothing else is in flight.

### Where "session" state lives (`ChatSession` vs mvp1's `Agent`)

- **Status: leaning, not yet implemented.** mvp2/learn's `ChatSession` bundled capabilities
  (`Provider`, `Tools`, `SystemMsg`), conversation state (`MsgHistory`, `MsgContext`) and terminal
  I/O (`InBuffer`, `OutBuffer`, `Config`, `UserID`). mvp1 has no session type: capabilities are
  `Agent` fields and the state is the injected `Memory` + `ContextBuilder`.
- Direction: do not carry the grab-bag into `agent/`. Terminal I/O, slash commands and the REPL
  belong to the mode drivers; compaction state belongs to the context-builder capability; MCP
  connection lifetime belongs to setup/shutdown, not the agent struct.
- Open trade-off to record when decided: mvp2/learn's `reActLoop` returns the turn's messages and
  the caller commits them (a failed turn leaves history untouched); mvp1 appends as it goes
  (better for resuming a crashed headless run, worse for a clean REPL after `ErrMaxSteps`).

### CLI flags: precedence, "unspecified" semantics, and parsing

- **Precedence: flags > config file > built-in defaults** (decided 2026-10-05). Flags are the most
  specific, per-run signal; a config-wins rule would make a flag silently do nothing whenever the
  file sets that field. The original comment in `main.go` said the opposite ("-config … overrides
  flag values"); it was corrected to "flag values override config".
- **Unspecified stays unspecified.** `Flags` fields are all pointers; `nil` = not passed, so
  `-json=false` or `-maxsteps 5` are distinguishable from absence. Go's `flag` package cannot
  report "was this set", so values are registered into throwaway variables and `fs.Visit` is used
  to copy only the flags actually passed. Downside: callers nil-check before reading.
- **`FlagsOverlay(cfg, flags)`** applies only `-model`, `-mode`, `-mission`, `-maxsteps`,
  `-maxtokens`. Matching fields were added to `Config` (`mode`, `mission`, `maxSteps`,
  `maxTokens`); default `Mode` is `chat`. `MaxTokens` defaults to 0 = unspecified and is not read yet.
  `MaxSteps` defaults to 10 and **is** read (2026-10-07): `reActLoop` loops `cs.Config.MaxSteps` times and the
  old `MaxReActSteps` constant is gone. A non-positive value (possible from a config file; the flag rejects it)
  makes every turn fail immediately with `ErrMaxSteps (limit 0)` rather than being silently replaced by 10.
- **Mode cross-checks** (`oneshot` needs `-query`, `headless` needs `-mission`, `-query`/`-mission`/
  `--json` rejected where they don't apply) only run when `-mode` was passed explicitly, because
  the effective mode may still come from the config file. **TODO:** re-validate the merged mode
  after `FlagsOverlay`.
- **No flags → chat mode, not usage.** Printing usage on empty args was tried and reverted
  (2026-10-05): bare `minagent` is the chat shortcut; `-h`/`--help` print usage.
- **`-x` and `--x` are both accepted** for every flag (Go `flag` default). Rejecting single-dash
  `-json` was offered and declined.
- **Flag parsing lives in `config/`** (`config.ParseFlags`, `config.FlagsOverlay`), a leaf package,
  rather than `package main`. Note for context: in Go, all files of one package share a namespace,
  so calls across files need no qualifier; only a real package needs `pkg.Name` with a capitalised
  name.
- **Context setup in `main`:** `signal.NotifyContext` alone is enough (`stop()` also cancels);
  the extra `context.WithCancel` was redundant and only becomes useful if something needs to
  cancel the run itself (e.g. a headless timeout).

### MCP: `mcpconnect/` — one package for connections and tools (2026-10-07)

- **Status: active.** Arc kept.
- **First cut: two packages.** `mcpservers/` (connect, `MCPServer`, `Close`) and `tools/mcptools/`
  (wrap an MCP tool as a `Tool`). Jerry's reason: an MCP server exposes more than tools —
  resources and prompts today, plausibly other callable categories as the spec evolves — so the
  connection should be a layer that several capability packages share.
- **Why it felt clunky.** `mcptools` imported `mcpservers`, so any test that needs "connect, then
  use" spans both packages: `checkEchoRoundTrip` (connect to a fake server, list tools, call one)
  could live in neither without an import cycle. Every new primitive would also have added a
  sibling package (`mcpresources`, `mcpprompts`) with the same problem.
- **Decision: one package, split by file** — `mcpconnect/conn.go` (connect local/remote, headers,
  `Close`) and `mcpconnect/tools.go` (tool adapter), with `resources.go` / `prompts.go` added
  as files when needed. This keeps Jerry's goal (one shared connection layer for every primitive)
  without the cross-package seams. Tests mirror the files: `conn_test.go`, `tools_test.go`.
- **Dependencies:** imports `config`, `model` and `tools` (for the `Tool` interface, and
  `GetMCPTools`/`SetupMCPTools` return `[]tools.Tool`); only `agent/` imports `mcpconnect`.
  A variant was offered where `mcpconnect` does not import `tools` (export the concrete tool type,
  have `agent/` convert the slice); not taken, because the extra conversion loop buys little.
- **Names:** `McpConnection` (type), `SetupMCPConns`, `connectLocalMCPServer`,
  `connectRemoteMCPServer`, variables `mcpConn` / `mcpConns`. (`MCPServer` → `McpConn` →
  `McpConnection` along the way; the type is "our live session with one configured server", not the
  server itself.)
- **Honest downsides.** The namespace is shared across primitives, so unexported names can collide;
  if the package grows past a thousand lines, split by primitive then (the connection type is
  already the shared base, so that is cheap).
- **`SetupMCPConns` closes what it connected when a later server fails (fixed 2026-10-07).** It used to
  return `nil, err` at the first failing server, dropping the connections already made without
  `Close()`, so their child processes outlived the failed setup. Now every error path (bad transport
  config, connect failure, unsupported transport) closes the connections made so far and returns
  `nil, err`; the error is returned, not swallowed, and `agent/` still prints it and carries on
  without MCP tools. Servers are also connected **in sorted name order** (map order is random), so
  which server fails first is repeatable. Rejected: returning the partial list alongside the error
  (callers would have to remember to close it). Test: a good local server (the test binary
  re-run as a child) plus a failing one; the good child's pid must be gone after the call, checked
  for both a server that cannot start and a bad config entry, and it fails with `closeAll` disabled.
  Remaining downside: a server that is slow to start delays the next one, because connecting is
  sequential.

### Context window never hands out an orphaned tool result (2026-10-07)

- **Status: active.** Resolves the deferred item "Window and compaction can split a tool call from
  its results". Fix commit `f7c9c88`; tests added afterwards.
- **The bug.** The windows trim by message count (window of 8 with `MaxContextWindow = 10`) and a
  turn is several messages (user, assistant tool call, tool result, ..., final). A trim can land
  between a tool call and its result, so the context starts with a `role: tool` message. Providers
  reject that (OpenAI: a tool message must follow an assistant message with a matching
  `tool_calls` entry). It sticks: a failed turn is never added back, so the identical context is
  re-sent every turn until `/clear` or `/compact`. Compaction's `clampToMax` had the same exposure.
- **Options.** (1) Trim forward to the next `user` message, like mvp1's `harness.Window` widening
  backwards to a user turn: simple invariant, but may drop a whole older turn. (2) Store and evict
  whole turns: cleaner model, but changes the `ContextWindow` interface and all four strategies.
  (3) Window by tokens: the right long-term answer (providers limit tokens, not messages), but a
  separate piece of work that needs the same rule. (4) Raise the limits: only hides it.
  (5) **Chosen:** after trimming from the front, drop leading `tool` messages.
- **Why (5) is enough.** Trimming from the front removes tool calls *before* their results, so a
  leading tool result is the only invalid shape; an assistant tool call at the front is fine
  because its results follow it. It keeps more context than (1).
- **Where, and why not in `ChatContext`.** Each strategy does it inside its own `AddMessages`, under
  its existing lock: offset reslices; in-place extends the drop count before its single `copy`;
  ring buffer advances its logical start (`count--`, slot zeroed); linked list pops the front.
  Cost is O(k) per add, k = orphans dropped (usually 0–3), no allocation. A wrapper in
  `ChatContext` (read everything, filter, `Clear`, refill) was rejected as O(n) per turn.
  `clampToMax` skips leading tool survivors when it clamps.
- **Downsides / limits.** The window may start with an assistant message that has no user message
  before it; OpenAI accepts that, but a strict Anthropic adapter requires a user-first context, so
  that adapter needs (1) or its own fix. A window can end up shorter than its max. A single turn
  larger than the whole window is cut by count (its user message can be lost), though what remains
  is valid. `RemoveLast(n)` can still strip results and leave a call without results (the opposite
  orphan); nothing in `agent/` calls it, so it is left alone.
- **Tests:** four tests over all four strategies (varied-length turns after every add; exact result
  when the trim lands on a tool result; oversized batch; no over-trimming on a turn boundary) and
  three for `clampToMax` / `Compact`. They fail against `f7c9c88^` and pass now.

### Chat loop and Ctrl+C (2026-10-07)

- **Status: active (option A); option B designed, deferred.** Resolves the deferred item "What
  should Ctrl+C mean?".
- **Problem.** `signal.NotifyContext` removes the default kill-on-SIGINT, and a blocked
  `ReadString` can't see a cancelled context, so Ctrl+C did nothing at the prompt; after one
  mid-turn Ctrl+C every later turn failed with `context canceled` while the prompt kept returning.
- **Decision (A): Ctrl+C quits.** `runChatLoop` reads stdin in its own goroutine and sends lines on
  a channel; the main loop `select`s on `ctx.Done()` and that channel. The reader sends any text
  that arrives with `io.EOF`, so a final line with no trailing newline (piped input) is processed,
  not dropped. `lines` is unbuffered, so the reader reads at most one line ahead while a turn runs.
- **Option B (not built): interrupt only the current turn, press twice to exit** (Claude Code style).
  It needs a per-turn child context, SIGINT owned by the chat loop (the root context would listen
  for SIGTERM only) and a watcher goroutine per turn; the double-press needs a 2-second window and
  Ctrl+D handling, and Ctrl+D is not a signal but EOF on a terminal, so the reader must report it,
  keep reading when stdin is a TTY and exit when it is a pipe. The auto-compaction goroutine must
  keep the root context or the turn's `cancel()` would kill it. About three times the code of A;
  declined for now.
- **Known cosmetic leftover:** after a mid-turn Ctrl+C the loop prints one more `>` before it exits.
  A `ctx.Err()` check at the top of the loop removes it.

### Test layout after the migration (2026-10-07)

- **Status: active.** The single 2,680-line `mvp2/learn/chat_w_history_context_session_mcp_tools_test.go`
  was copied (not moved; the original stays untouched) into per-package test files: `agent/`
  (`chat_test.go`, `compaction_test.go`, `agent_test.go`, `fixture_test.go`), `contextwindow/`
  (`window_test.go`, `window_bench_test.go`, `msgcontext_test.go`), `memory/`, `model/`
  (`openai_test.go`, `provider_test.go`), `model/modeltest/`, `tools/`, `tools/builtin/`,
  `config/` and `mcpconnect/` (`conn_test.go`, `tools_test.go`).
- **Shared doubles** (`FakeProvider`, `AssistantText`, `AssistantToolCall`, `Msg`, `Msgs`) live in
  `model/modeltest`, a regular package, because a `_test.go` file can't be imported by another
  package. The session fixture stays `agent`-only. Tests that touch unexported code
  (`handleUserInput`, `runToolCall`, `clampToMax`, `newMCPTool`, `connectLocalMCPServer`, ...) are
  internal (`package x`), not `x_test`.
- **Dropped on purpose:** the "empty filename reads `config.default.json`" test (there is no default
  config file now: no `-config` means built-in defaults) and two `ChatHistory` tests that built the
  whole session fixture just to reach a history object. **Added:** three `PrepareChatRequest` tests
  (ordering, empty inputs, no aliasing of the caller's slices). `SetDefaultConfig` tests now pass
  `Flags{Config: &path}`.

---

## Deferred / open decisions

- **Resolved 2026-10-07 → see `§ Context window never hands out an orphaned tool result`.**
  Original note: **Window and compaction can split a tool call from its results (slice 1d-iv, next
  after the loop tests).** The window trims by message count, and a turn is now several messages (user,
  assistant, tool results, ..., final). With `MaxContextWindow = 10` (window of 8) a two-round
  turn is already 6 messages, and a longer one overflows and can drop the user message, leaving
  orphaned tool results that providers reject. Compaction has the same exposure. Options: make
  eviction turn-aware (widen backwards to a user turn, as mvp1's `harness.Window` does), treat a
  turn as one atomic unit, or simply raise the limits. Not yet decided.
- **Resolved 2026-10-07 (option A) → see `§ Chat loop and Ctrl+C`.** Original note: **What should
  Ctrl+C mean?** Today it cancels the program-wide ctx: `stdin.ReadString` isn't
  ctx-aware, so at the prompt nothing happens until Enter, and `runLoop` never checks
  `ctx.Err()`, so afterward every turn fails with `context canceled` while the REPL keeps
  prompting. Option A: Ctrl+C quits (add a `ctx.Err()` check in `runLoop`). Option B: Ctrl+C
  interrupts only the current turn (a per-turn ctx in `runLoop`, with `os.Interrupt` removed from
  `main`'s registration). Leaning A first; revisit B when tools run long enough to be worth
  interrupting.
- **`read_file` can read any path.** Restricting paths belongs to hooks or approval, later.
- **`createChatMessage` takes four positional `string` params,** so a swapped argument compiles.
  A narrower `newToolMessage(toolCallID, content)` was suggested; revisit if a second caller
  pattern appears or a swap bug bites.

- **Summarizer instructions are sent as a trailing `system` message** (after the `user`
  transcript). OpenAI and Ollama accept this. It conflicts with the reasoning in § "Compaction
  design" (a `system` message mid-list breaks Anthropic and some Ollama chat templates), and
  Anthropic takes the system prompt as a separate field, not a message. Options: append the
  instructions to the first system prompt, or to the user message ("Focus on: …"). Open since
  2026-09-25; revisit when an Anthropic provider is added.

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
- **`gracefulShutdown` + `ChatContext.WaitForCompaction` — cancel-then-wait, not wait alone
  (built 2026-09-24)**: `runLoop` now derives `ctx, cancel := context.WithCancel(ctx)` and
  defers `gracefulShutdown(cancel, chatSession)`, which calls `cancel()` *then*
  `chatSession.MsgContext.WaitForCompaction()`, on every exit path (`/exit` or stdin EOF).
  `WaitForCompaction()` itself just wraps a `compactWG.Wait()`; the `compactWG.Add(1)` happens
  synchronously in `handleUserInput`, before the `go func(){...}()` that launches a background
  auto-compaction — never inside the spawned goroutine, which would be a real race (a test or
  caller could call `WaitForCompaction()` before the goroutine had even started incrementing the
  counter). **Why cancel first, not just wait**: `OpenAICompat.Chat` derives its own
  `context.WithTimeout(ctx, ResponseTimeout)` (5 minutes) from whatever `ctx` it's given: Go's
  context cancellation propagates parent→child regardless of the child's own timeout, so
  cancelling the parent aborts an in-flight HTTP request almost immediately. An earlier version
  of this idea (`WaitForCompaction()` alone, no `cancel()`) was rejected before being built: a
  user typing `/exit` during a slow compaction would have been forced to wait up to
  `ResponseTimeout` for the program to actually quit — worse than the pre-existing behavior of
  silently abandoning the goroutine. Known accepted rough edge: a cancelled compaction's error
  still prints via `fmt.Fprintln(cs.OutBuffer, ...)` from inside the goroutine, which can land
  on-screen right as (or just after) the program appears to have exited — not a crash, just a
  slightly odd trailing line. Files: `chat_w_history_context_session_structs.go`
  (`gracefulShutdown`, `runLoop`, `ChatContext.compactWG`/`WaitForCompaction`, the
  `compactWG.Add(1)` call site in `handleUserInput`'s auto-compaction branch). Status: active.
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
