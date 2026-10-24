# DECISIONS.md — mvp2 design decision log (mvp2/learn/ and the repo-root migration)

This file is the durable record of *what was decided, why, and when* for the `mvp2/learn/`
build-out. It is not a status report — for "what's in progress right now / what to do next,"
see `SESSION.md`. A decision belongs here once it's settled (even if later reversed); a TODO
or an in-flight question belongs in `SESSION.md` until it resolves into a decision, at which
point it should move here.

Each entry: **what was decided**, **why**, **files affected**, **date**, and **status**
(`active` / `superseded by <entry>` / `deferred`).

**Ordering rule (Jerry, 2026-10-19): the Decision log is chronological, oldest first, newest last.**
Entries are ordered by the date the decision was first made. To record a new decision, **append a new
dated `###` entry at the end of the log**; never insert in the middle. If a later decision changes an
earlier one, append a new entry that says what it supersedes and mark the old entry's status
`superseded by <new entry>` (keep both the original and the revised reasoning). A decision that is still
being worked out on the same day can be extended in place if it is the last entry. The two living lists
(`Deferred / open decisions`, `Deferred for Later`) sit above the log because they are edited in place,
not appended. Entries without an exact recorded date are marked as approximate in their heading.

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
- **Resolved 2026-10-07 → see `§ read_file is confined to workDir and has a deny list`.** Original note:
  `read_file` can read any path; restricting paths belongs to hooks or approval, later.
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

### Deferred for Later

Copilot review comments on PR #6 (https://github.com/jerryschen31/minimal-agent/pull/6) and PR #7
(https://github.com/jerryschen31/minimal-agent/pull/7) that Jerry replied to with "deferred" or "will do
later" instead of fixing in that PR. Recorded 2026-10-07. Each
entry has: what Copilot said, what the code does today, the fix, and when to pick it back up. The first
three were answered literally "deferred"; the last two were answered "for debugging purposes, will
switch/remove later". Comments that were fixed in the PR are not listed here (see the entries on
`/config` printing, `ChatMessage` JSON tags and `read_file` confinement above). Item 6 is different: it
is a limitation introduced on purpose by the PR #7 redirect fix and deferred by Jerry's own decision.
Item 7 came out of the PR #7 `env` fix, item 10 is a topic Jerry deferred outside any review, and item 11
is a Copilot comment on PR #9 (the line editor / per-turn Ctrl+C work).

1. **`headless` and `oneshot` modes are advertised but not implemented** (`agent/agent.go`, `RunAgent`).
   - *Copilot:* the parser and usage text accept both modes, but every such run reaches the
     "unsupported agent mode" branch. Reject/unadvertise them until the drivers exist, or implement the
     dispatch and carry the parsed query / mission / JSON options through.
   - *Today:* `ParseFlags` validates `-mode oneshot|headless`, `-q`, `-mission`, `--json` and the usage
     text lists them; `RunAgent` only handles `chat` and returns `unsupported agent mode: <mode>` for the
     rest. `Config` has no fields for the query or JSON output, and `FlagsOverlay` does not carry them.
   - *Fix:* implement the two drivers (the design is the headless brainstorming in `THOUGHTS.md`) and add
     the missing `Config` fields; the stopgap alternative is to drop the two modes from the usage text and
     the validator.
   - *Revisit:* when the headless/oneshot drivers are built. Until then the failure is loud (an error and
     a non-zero exit once item 3 is done), not silent.

2. **Auto-compaction prints "complete" even when it failed** (`agent/chat.go`, the auto-compaction
   goroutine in `handleUserInput`).
   - *Copilot:* after reporting `[error] Auto-compaction error: ...` the goroutine still prints
     `[system] Auto-compaction complete.`, which contradicts the error. Return after the failure.
   - *Today:* the error branch does not `return`, so a failed compaction prints both lines.
   - *Fix:* `return` inside the `if err != nil` block (the deferred `CompactWG.Done()` still runs).
     A test needs the fake provider to fail an auto-compaction and check the output has no "complete".
   - *Revisit:* the next time the compaction messages are touched; cosmetic, no state is affected (the
     context is left unchanged on failure).

3. **A failed start or run exits with status 0** (`main.go`).
   - *Copilot:* on a setup or run error `main` prints the error and returns, so scripts and supervisors
     see success. Move the lifecycle, including the deferred shutdown, into a `run() error` helper and have
     `main` exit non-zero after it returns.
   - *Today:* both error paths in `main` print to stderr and `return`. (Parse and config-load errors
     already `os.Exit(2)`, because nothing needs shutting down yet.)
   - *Constraint to keep:* `os.Exit` skips deferred calls, so `ShutdownAgent` (which waits for compaction
     and closes MCP child processes) must run before the exit, which is why a `run() error` wrapper is the
     shape suggested. An earlier request removed a `run()` wrapper from `main` for the flag-parsing part;
     this one is about the agent lifecycle and is a different case.
   - *Revisit:* before headless/oneshot (item 1), where callers rely on the exit code.

4. **Remote MCP traffic is wrapped in `mcp.LoggingTransport`** (`mcpconnect/conn.go`, `connectRemoteMCPServer`).
   - *Copilot:* the wrapper hides the SDK connection's `sessionUpdated` hook, so `Mcp-Protocol-Version`
     is not sent after initialize and a strict server may reject later list/call requests. Use the
     streamable transport directly or log at the `RoundTripper` layer.
   - *Jerry's reply:* the logging transport is there for debugging; will switch later.
   - *Status:* already analysed in full under "Bug found 2026-10-02: `LoggingTransport` hides the client's
     `sessionUpdated` hook" (in the Remote MCP entry above), with the options and a test plan. This item
     is the pointer; the decision is still open there.
   - *Also deferred (PR #7, `mcpconnect/conn.go`, reply "noted and deferred. LoggingTransport is currently used
     for debugging and testing in this early phase"):* the same review flags protocol logging on **both**
     transports, stdio and remote: every JSON-RPC request and result, including tool arguments and results
     that may hold file contents or credentials, goes to stderr even when session output is routed elsewhere.
     Copilot's fix: use the underlying transports directly in normal runs and require an explicit opt-in
     for protocol logging. It is the same fix as above, so one `debug` setting covers both transports.
   - *Revisit:* before using a strict remote MCP server for real work, or when adding a debug setting.
   - **Mostly resolved 2026-10-09** by the `-debug` switch (see "Debug output behind `-debug`"): normal runs
     use both transports directly, so the protocol log, the `read error: EOF` on exit and the hidden
     `sessionUpdated` hook are gone. **Still open in debug mode:** with `-debug`, the remote transport is
     wrapped again and the hook problem returns; logging at the `RoundTripper` layer would fix that too.

5. **`DebugChatContext` dumps the whole conversation to stdout every turn** (`agent/chat.go`,
   `handleUserInput`).
   - *Copilot:* the unconditional call writes the full prior conversation to process stdout instead of
     `ChatSession.OutBuffer`, leaking chat content when output is redirected and corrupting structured or
     piped output. Remove it from the normal path or gate it behind an explicit debug option that uses the
     configured writer.
   - *Jerry's reply:* debugging aid for now; will remove or refactor later.
   - *Today:* it uses `fmt.Println`, so it bypasses `OutBuffer`, and the dump includes tool results, which
     means a successful `read_file` prints the file's contents to the terminal.
   - *Fix:* delete it, or behind a debug flag write to `cs.OutBuffer` (or stderr).
   - *Revisit:* before `--json` / oneshot output or before sharing terminal logs; together with item 4's
     debug setting, one `debug` switch could cover both.
   - **Resolved 2026-10-09** (see "Debug output behind `-debug`"): printed only in debug mode, and to
     `cs.OutBuffer` (`DebugChatContext` now takes a writer) instead of process stdout.


6. **`http` -> `https` redirects from a remote MCP server are rejected** (`mcpconnect/conn.go`,
   `newRemoteHTTPClient`; added with the PR #7 redirect fix, not a Copilot comment).
   - *Today:* the allowed origin is scheme + host + port of the configured `url`, so a server configured as
     `http://host/mcp` that answers with a redirect to `https://host/mcp` fails to connect with
     "redirect to https://host:443 blocked: MCP server redirects must stay on http://host:80". The fix on
     the user's side is to configure the `https` URL.
   - *Why it is rejected now:* it is the simplest rule, and an upgrade is the one case where a "same host,
     different scheme" redirect is safe, so it can be allowed later without weakening anything else.
   - *Fix:* in `CheckRedirect`, allow exactly one exception: the previous hop was `http`, the target is
     `https`, the host name is the same, and the port is the default 80 -> 443 mapping (or the same
     explicit port). Never allow `https` -> `http`, a different host, or a different non-default port. The
     `headerTransport` guard has to accept the upgraded origin too, so it needs to learn "the origin we
     were redirected to within the allowed rule" rather than a single fixed origin.
   - *Tests to change when built:* the "http to https (upgrade, rejected for now)" row of
     `Test_Unit_RemoteClient_CheckRedirect_OriginRules` flips to allowed; add cases for the other port and
     for `https` -> `http` staying blocked.
   - *Revisit:* when a real server is hit that only offers an `http` entry point and upgrades; until then
     configure `https` URLs.

7. **Small follow-ups to applying the MCP server `env`** (`mcpconnect/conn.go`, `config/config.go`; added with the
   PR #7 `env` fix, not Copilot comments). Marked in a `TODO(deferred ...)` comment in `connectLocalMCPServer`.
   - **(a) Validate env keys.** An empty key, or one containing `=` or a NUL byte, produces a malformed
     environment entry. Reject them in `McpServerConfig.TransportType()` with an error naming the key. It is the
     user's own config, so this is low risk; it is a clearer error, not a security fix.
   - **(b) No `${VAR}` expansion.** `"GITHUB_TOKEN": "${GITHUB_TOKEN}"` is passed to the child as the literal text.
     Expanding values from our environment (`os.Expand` with a lookup) would let a config file name a token
     without containing it. The same gap exists for `headers` on remote servers (see the older note about
     `os.ExpandEnv` in `headerTransport`); do both together, and decide first whether an unset variable is an
     error or an empty string.
   - **(c) Children inherit the whole parent environment,** including the LLM provider's API key
     (`apiKeyName`, e.g. `OPENAI_API_KEY`). That was already true before the `env` fix (nil `cmd.Env` inherits
     everything) and the fix keeps it. Options: drop only that variable from the child's environment unless the
     config sets it explicitly (cheap), or follow the official TypeScript MCP SDK and pass a safe list (`HOME`,
     `LOGNAME`, `PATH`, `SHELL`, `TERM`, `USER`) plus the configured `env` (safer, but can break servers that
     need other variables).
   - *Revisit:* (a) any time `config.go` validation is next touched; (b) when a config file is shared or
     committed; (c) before running MCP servers that are not trusted with the provider key.

8. **Auto-compaction writes to `cs.OutBuffer` concurrently with the chat loop** (`agent/chat.go`; PR #7, reply
   "deferred, this is on the to-do list").
   - *Copilot:* the background compaction goroutine prints status and errors to the same writer the chat loop
     uses for the prompt and replies. `OutBuffer` is any `io.Writer`, and some (the test fixture's
     `bytes.Buffer`) are not safe for concurrent use, so overlapping writes can race and corrupt output.
     Serialize all session writes through one shared synchronized writer, including `cs.Config.OutBuffer`,
     which `/config` (`PrintConfig`) uses.
   - *Today:* with the real `os.Stdout` there is no data race (each `Write` is a single call) but lines can
     interleave, for example a status line landing in the middle of the prompt. Output sent to a
     non-thread-safe writer can race. The two fields hold separate copies of the same writer, so both must be wrapped.
   - *Fix:* wrap the configured writer once, when the session is built, in a small mutex-guarded writer and
     assign it to both `ChatSession.OutBuffer` and `ChatSession.Config.OutBuffer`; every `Fprint*` then goes
     through it. Interleaving of whole lines remains possible, which is cosmetic; the race goes away. Test: a
     compaction running while the loop prints, under `-race`, against a plain `bytes.Buffer`.
   - *Revisit:* together with the larger output restructuring that headless/oneshot (item 1) will need.

9. **A symlink swapped in after `resolve()` could still be opened** (`tools/builtin/readfile.go`; PR #7, reply
   "deferred").
   - *Copilot:* `os.Root` keeps the open inside `workDir` but still follows symlinks within it. If a permitted
     file such as `notes.txt` is replaced with a symlink to `.env` after `resolve` checked it, the open follows
     the new target and returns the secret. Enforce the deny list against the actually opened target, with
     race-resistant traversal, and test the replacement between resolution and open.
   - *Today:* `resolve` follows symlinks and checks the deny list on the real path, so a symlink that exists
     beforehand is refused. Only the time-of-check/time-of-use window is open, and it needs something else
     to write inside `workDir` at the right moment, for example the model using a write-capable MCP tool.
     The earlier named-pipe fix already checks the type on the opened descriptor, which is the same style of
     fix this needs.
   - *Fix options:* (a) open the already-resolved path with `O_NOFOLLOW`, so a symlink appearing at the final
     component makes the open fail (it still does not cover a directory component being replaced by a
     symlink); (b) after opening, ask the OS for the real path of the descriptor (`F_GETPATH` on macOS,
     `/proc/self/fd/N` on Linux) and run the root and deny-list checks on it, which closes the gap at the cost
     of platform-specific code. Test: replace the file with a symlink to `.env` between `resolve` and the open
     (a test hook between the two steps), and assert the secret is not returned.
   - *Revisit:* before running the agent with write-capable tools (a filesystem MCP server, a shell tool)
     enabled inside the same `workDir`.

10. **Dependency hardening for the line editor (and the module graph in general)** (`go.mod`, `go.sum`,
    `agent/input.go`; deferred by Jerry 2026-10-09, not a Copilot comment).
    - *Concern:* a dependency of `go-multiline-ny` (e.g. `clipperhouse/uax29/v2`) being deleted from GitHub,
      tampered with, or abandoned, and breaking the agent.
    - *What the editor actually pulls in* (measured 2026-10-09 at the pinned commit
      `v0.23.2-0.20260509051138-60a7d8e7d356`, with `go mod graph` and `go list -deps`): its `go.mod` lists 11
      modules, but only 7 are compiled into the binary: go-multiline-ny, go-readline-ny (packages `readline`,
      `keys`, `moji`, `simplehistory`), go-ttyadapter, go-runewidth, uax29/v2, x/term, x/sys. `atotto/clipboard`,
      `go-colorable`, `go-box`, `go-tty` and `go-isatty` are only reached from sub-packages the agent doesn't
      import (completion, examples, Windows code), so they are recorded in `go.sum` but never built. Go
      compiles by package, not by module: a module whose packages are not in the import chain costs nothing.
      In the repo (after `go get ...@60a7d8e7d356`, 2026-10-09) the versions are exactly what go-multiline-ny
      asks for: go-readline-ny v1.15.1, go-ttyadapter v0.6.2. (The throwaway test module had resolved
      v1.16.1 / v0.7.0, because `go mod tidy` there added the directly imported packages at their latest
      versions; the key tests pass on both.)
    - *What already protects us, with no action taken:*
      - **Deletion:** `go` downloads through `proxy.golang.org` by default, which keeps every version it has
        served; the pinned commit is already cached there (fetched through it on 2026-10-09). Plus the local
        cache in `~/go/pkg/mod`. Caveat: the proxy can drop a module for legal reasons, so it is not a
        guaranteed archive.
      - **Tampering:** `go.sum` plus the checksum database (`sum.golang.org`, an append-only log). Changed
        content behind a pinned version fails the build; it can never compile silently.
      - **Deprecation/abandonment:** the pinned version keeps building (Go 1 compatibility promise); the risk
        is only missing future fixes, e.g. for a new terminal or OS.
      - **Containment:** the editor sits behind the `promptReader` interface in `agent/input.go`, with the
        `bufio` reader as a working fallback, so replacing it means rewriting one file.
      - **No automatic upgrades:** versions change only with `go get -u` / `go get ...@newer`.
    - *Options when picked back up:*
      - (a) **`go mod vendor`**: copies the source of every dependency into `vendor/`, used by `go build`
        automatically, no network. Measured: 9.6 MB in total, of which 8.8 MB is `golang.org/x` (x/sys);
        the third-party editor code is under 1 MB. Downsides: about 10 MB more in the repo, noisy diffs on every
        upgrade, and it covers all dependencies including the MCP SDK.
      - (b) **Fork** go-multiline-ny (and possibly go-readline-ny) to Jerry's GitHub and point at it with a
        `replace`. Little extra protection given the proxy and checksums; useful only to carry local patches.
      - (c) **Private `GOPROXY`** (Athens or similar). Only worth it for an organization.
      - (d) **Trim the graph:** swap the editor for one with fewer modules, or drop to `golang.org/x/term`
        (Go-team only, but single-line; see the line-editor entry for why it was rejected).
      - Related small choice, settled for now: `term.IsTerminal` (x/term is compiled in anyway, so a direct
        import adds nothing to the binary) vs. the standard-library `os.ModeCharDevice` check (no import; a
        harmless quirk where `/dev/null` counts as a terminal).
    - *Useful commands:* `go list -deps -f '{{with .Module}}{{.Path}}{{end}}' . | sort -u` (modules actually
      compiled in); `go mod graph | grep <module>` (who requires it); `go mod why -m <module>` (the import
      path that pulls it in); `go tool nm -size -sort size <binary> | head` (largest symbols in the binary).
    - *Revisit:* before builds must work offline or without outside services (CI in a locked-down
      network, a release), if a dependency is retracted, deprecated or removed from the proxy, or when the
      module graph grows noticeably (e.g. a sandbox or tracing dependency).

11. **Prompt-reader errors other than EOF are swallowed** (`agent/chat.go`, `runChatLoop`; `agent/agent.go`,
    `RunAgent`; PR #9, deferred by Jerry 2026-10-09).
    - *Copilot:* all prompt-reader errors are silently treated as normal EOF. The TTY editor can return
      terminal initialization/runtime failures, so the process then exits successfully without explaining
      why the REPL disappeared. Propagate non-EOF, non-cancellation errors from `runChatLoop` through
      `RunAgent`; this requires updating both function return paths.
    - *Today:* `runChatLoop` returns on *any* `ReadPrompt` error (`if err != nil { return }`) and has no
      return value; `RunAgent` then returns `nil`. So a real failure (e.g. go-multiline-ny failing to put the
      terminal into raw mode or to read its size: `multiline.init: ...`, `readline.Editor.Tty.Open: ...`, or a
      read error on a pipe) looks exactly like Ctrl+D: the agent quits quietly with status 0. The same
      happens if `newTTYReaderWith`'s ignored `BindKey` error was real, since `Read` reports it again.
    - *Fix:* give `runChatLoop` an `error` return. On a `ReadPrompt` error: `io.EOF` → `return nil` (Ctrl+D /
      end of input); `ctx.Err() != nil` / `errors.Is(err, context.Canceled)` → `return nil` (SIGTERM or quit);
      anything else → `return fmt.Errorf("reading prompt: %w", err)`. `RunAgent` returns
      `runChatLoop(...)` instead of `nil`; `main` already prints a non-cancel error from `RunAgent`. Tests:
      a fake `promptReader` (or an `auto.Pilot` whose `OnGetKey` returns an error) makes `runChatLoop` return
      that error; `io.EOF` and a cancelled ctx still return `nil`.
    - *Related:* item 3. Once the error reaches `main`, the process still exits 0 until `main` exits
      non-zero on a run error, so do both together for the full fix.
    - *Revisit:* together with item 3 (before headless/oneshot), or as soon as an unexplained quit of the
      REPL is seen.

12. **Autonomous mode (step 1) extras, left out of the first version** (decided 2026-10-13; see
    `§ Autonomous, orchestrator and worker modes`).
    - `-json` final report (report as JSON on stdout, logs on stderr).
    - Saving a run transcript to a file. `ChatMessage` drops `ID`/`Timestamp` from JSON, so this needs its
      own storage type. Until then: `| tee run.log`.
    - Multiple agent instances, MCP server ("worker") mode, and orchestration: these are step 2.
    - A tasks state file (`update_tasks` tool) that persists across runs; resuming a crashed run.
    - A separate verifier LLM that double-checks the director's "done" verdict.
    - Human takeover: Ctrl+C pauses the run and hands the user the REPL, instead of ending the run.
    - A token or cost budget (`MaxTokens` is still unread).
    - *Revisit:* after step 1 works end to end.

13. **Shared helper for creating subscriber files** (`trace/subscriber.go`, `trace/digest.go`; decided 2026-10-22).
    - *Today:* `NewJSONLSubscriber` and `NewDigestSubscriber` repeat the same run-ID check, `MkdirAll(0700)` and
      `O_CREATE|O_EXCL` open at `0600`.
    - *Fix:* one `createTraceFile(dir, runID, suffix)` helper. Deferred by Jerry until a third file-writing subscriber
      exists, so the shape of the helper is known; two copies of about ten lines is cheaper than a guessed abstraction.
    - *Revisit:* when a third subscriber that writes a file is added. Until then, a change to the file-creation
      security rules (mode, `O_EXCL`, ID check) must be made in both places.

14. **Ctrl+C kills the stdio MCP servers** (`mcpconnect/conn.go`, `connectLocalMCPServer`; verified 2026-10-23).
    - **Resolved 2026-10-23 for Unix and for Windows Ctrl+C** (`§ 2026-10-23 — Each stdio MCP server runs in its own process
      group`). Still open from this item: the Windows tree kill (Job Object), and reusing the helper for `run_command`
      and orchestrator workers when they are ported.
    - *Today:* stdio servers are started with plain `exec.Command`, so they share the agent's process group (Unix) or
      console (Windows). Outside the line editor's raw mode (during setup and during every running turn), the terminal
      sends Ctrl+C's SIGINT to the whole foreground process group. The agent survives (Go catches SIGINT with
      `signal.Notify`), but `npx`/`node` exit. Every tool of that server then fails for the rest of the session, and
      the only sign is `error closing MCP server: signal: interrupt` at shutdown.
    - *Verified:* scratch run with the filesystem server; the agent started in its own process group, `kill -INT` to
      the group (what the terminal does): `npm exec` and `node` gone, agent still running.
    - *Fix, Unix* (`procgroup_unix.go`): `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`, so the terminal's
      Ctrl+C never reaches the server; on close, kill the whole group (`syscall.Kill(-pgid, …)`), so a grandchild
      (`node` under `npx`) cannot outlive the agent. The SDK's own close signals only the direct child.
    - *Fix, Windows* (`procgroup_windows.go`): the console sends Ctrl+C to every process attached to it, so the same
      problem exists. `CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP` makes the server ignore console Ctrl+C
      (Ctrl+Break still reaches it). Windows has no group kill: `cmd.Process.Kill()` kills only `npx`, not its `node`.
      Killing the tree needs a Job Object (`golang.org/x/sys/windows`, ~40 lines).
    - *Why two files:* `syscall.SysProcAttr` has different fields per OS (`Setpgid` vs `CreationFlags`), so each
      setting only compiles on its own OS; build-tagged files (`_unix.go` / `_windows.go`) select one.
    - *Plan:* Ctrl+C isolation on both OSes (one line each); group kill on Unix; Windows tree kill (Job Object)
      deferred further, since nothing here is run or tested on Windows. Test: the helper server started through
      `connectLocalMCPServer` survives a SIGINT sent to the test's process group.
    - *Alternative considered:* keep the terminal from generating SIGINT at all (clear `ISIG` for the whole session
      and read Ctrl+C as a key during turns, as full-screen TUIs do). Rejected for now: the line editor owns stdin at
      the prompt, a second reader is needed during turns, and child processes stay exposed to anything else that
      signals the group.
    - *Also applies to orchestrator workers:* mvp4's `worker/mcp.go` starts each `-mode worker` child with plain
      `exec.Command`, so workers share the group too. A worker survives Ctrl+C only because it is the same Go binary
      and catches SIGINT; its own stdio MCP servers (grandchildren) die. Harmless while Ctrl+C ends the whole run,
      but not once Ctrl+C pauses it (item 12, human takeover). mvp4's `run_command` already uses `Setpgid`
      (`tools/builtin/runcommand_unix.go`), which is the pattern to copy. When porting, the same helper should start
      worker processes.
    - *Revisit:* before `run_command` (it starts children too, and needs the same group handling) or as soon as
      Ctrl+C mid-turn is used with MCP servers.

15. **Ctrl+C cannot interrupt setup** (`main.go`, `agent/agent.go`, `SetupAgent`; noted 2026-10-23).
    - **Resolved 2026-10-23** (`§ 2026-10-23 — Ctrl+C during setup aborts gracefully`).
    - *Today:* `main` catches Ctrl+C on `intsigCh` before `SetupAgent`, but nothing reads the channel until the first
      turn, and `discardStrayInterrupts` then drops the press. Setup (e.g. a slow `npx` download) can only be stopped
      by SIGTERM (`kill <pid>`) or Ctrl+\ (SIGQUIT, no cleanup). A user who forgot a flag has to wait.
    - *Fix:* pass `intsigCh` to `SetupAgent`, wrap setup in the turn watcher (rename `createTurnWatcher` to
      `createInterruptWatcher`), return `context.Canceled` from `SetupAgent` when `SetupMCPConns` fails with it
      (today any MCP error is "print and continue"), and let `main` exit quietly; the deferred `ShutdownAgent` closes
      servers that already connected. Depends on item 14: until then, that same Ctrl+C also kills the servers.
    - *Invariant to record with the fix:* the watcher's `stop()` cancels the setup context even after a successful
      setup, so nothing long-lived may keep it (same rule as auto-compaction). Checked 2026-10-23: MCP sessions detach
      from the connect context (`jsonrpc2.NewConnection` wraps it in `notDone`; the HTTP transport uses
      `xcontext.Detach`), and `SetupRecorder` ignores its `ctx`.
    - *Downside:* a Ctrl+C right as setup finishes either aborts or is discarded, depending on timing.

16. **Connecting MCP servers in parallel** (`mcpconnect/conn.go`, `SetupMCPConns`; noted 2026-10-23).
    - *Today:* sequential, so a slow server delays the next, and a hung one blocks startup.
    - *Rules for the parallel loop:* one goroutine per `ConnectMCPServer`; results in a slice indexed by the server's
      sorted position (repeatable order); **wait for every goroutine before returning, even when cancelled**, or a
      connection finishing late has no owner and its process leaks; then `errors.Join`. The `ctx.Err()` check before
      each server stops helping (all start at once); a cancel then fails the in-flight handshakes, and the SDK closes
      each one itself (stdin closed, up to 5 s, then SIGTERM, then SIGKILL), in parallel.
    - *Add with it:* a per-server connect timeout (`context.WithTimeout`, configurable) so one hung server cannot hold
      up startup.


---

## Decision log (chronological — oldest first; APPEND new entries at the end)

### Track 1 — `mvp2/` root (agent build, last touched 2026-09-12)

Decisions made before this track went dormant (see `SESSION.md` for current status):

- **`requires_approval`** (renamed from `mvp1`'s `approve`) = tools that need a human `[y/N]`.
  A *restriction* list, not an allowlist. Empty list ⇒ hook never installed ⇒ nothing gated.
  `["*"]` ⇒ everything gated.
  **Superseded by `§ 2026-10-23 — Approval gate v1: open questions settled`:** never built; replaced by
  `DenyRules` (globs that deny, plus a built-in delete/remove list) with an "allow once? [y/N]" ask only for a
  denied call. Nothing asks by default.
- **MCP tool names are prefixed** `<server>_<tool>` — approval entries must be
  `fs_write_file`, `fs_edit_file`, `fs_move_file`, `fs_create_directory`.
- **Grep is a Go-native `tool.Func`, not an MCP server** — no official MCP server does content
  search; pure Go = zero deps, zero build tags, cross-platform.
- **Simplified Grep over a hardened version** — the NUL-byte binary check is the guard that
  actually matters (measured 24KB vs 2.99MB output on this repo, 124×); skip-list/streaming
  reads deferred until they bite.


### `ContextWindow` interface — scope narrowed twice (2026-09-17 → 09-21)

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


### `ShouldStartCompaction()`, not `StartCompaction()` (2026-09-19)

Chosen specifically because a name like `StartCompaction` invites calling it and ignoring the
return value — which is exactly a bug that happened once (2026-09-19/21 window): an
auto-compact goroutine ran unconditionally, ignoring a `false` result, breaking the
single-flight guarantee. `Should` signals it's a question worth checking, closer to Go's own
`sync.Mutex.TryLock()` idiom. Status: active. Files: `chat_w_history_context_session_structs.go`.


### Compaction design (2026-09-19 → 09-22)

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


### `window` field: exported → unexported → accessor added back (2026-09-21)

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


### `ChatSession` struct — supersedes an earlier flat design (2026-09-21)

- Rejected design (2026-09-21, never built): a flat `chatSession` struct holding `provider`,
  `window`, `systemMsg`, `compactInProgress atomic.Bool`, `autoCompactThreshold` all as direct
  fields.
- **Built instead**: `ChatSession{ Provider, MsgHistory ChatHistory, MsgContext *ChatContext,
  SystemMsg ChatMessage, UserID string, InBuffer io.Reader, OutBuffer io.Writer }`. `MsgContext`
  must be a pointer — `ChatContext` holds an `atomic.Bool` and a `sync.Mutex`, so a value field
  would copy the lock on every copy of `ChatSession` (same hazard class as any struct holding a
  `sync.Mutex`/`atomic.*`: always return `*T` from its constructor, never pass by value).
- Status: active. Files: `chat_w_history_context_session_structs.go`.


### Constructors return `(*T, error)` even where nothing can fail yet (2026-09-21)

`NewInMemoryChatHistory`, `NewChatContext`, `NewChatSession` all return an error today even
though nothing in-memory can currently fail. Deliberate future-proofing so a later
persistent/fallible implementation doesn't require a breaking signature change. Status: active.


### Concurrency & locking (gen 6, ~2026-09-21; exact date not recorded)

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


### Wire format / JSON tags (2026-09-22 — discussion only, no code changed)

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
- **Reversed 2026-10-07 (PR #6 review).** `ID`, `Timestamp` and `Type` are now tagged `json:"-"`, so they
  are never sent to the provider (and never read from a response). The 2026-09-22 claim that servers
  silently drop unknown message keys is not something we verified, and it is not safe to assume:
  Copilot flagged it, and strict servers (OpenAI proper, Azure) can reject a request over an unexpected
  message field, while Ollama's compat layer tolerates it. Not sending them is cheap and removes the
  question. Chosen: the plain tag, not a separate wire struct, because `ChatMessage` is marshalled in
  exactly one place (`model/openai.go`) and nothing persists it. Cost: if a persistent chat store is
  added (`chatStoreType` has only `in-memory` today), `encoding/json` on `ChatMessage` would silently drop
  the ID that compaction depends on, so that store needs its own storage type. Tests: the request body's
  message keys are limited to `role`, `content`, `tool_calls`, `tool_call_id`; a response with extra keys
  still decodes and does not fill our `ID`.


### Test suite decisions (2026-09-22, `chat_w_history_context_session_structs_test.go`)

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


### Slash commands — text after the command (2026-09-25)

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


### Tools & MCP (2026-09-25)

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
  **Fixed 2026-10-07 → see `§ Local MCP servers receive the configured env`.**
- There is no `${VAR}` expansion in header values yet, so a token in `config.json` is literal.
  `os.ExpandEnv(v)` in `headerTransport.RoundTrip` is a one-line change when wanted.
- The checked-in `config.default.json` has `deepwiki` configured with no auth header; don't put a
  real token there.
- The `DisableStandaloneSSE` decision itself was made in the 2026-10-01 session but never
  recorded; the SESSION.md notes didn't carry it. Record decisions as they're made.
- macOS has no `timeout` command; use `go test -timeout 30s` when running experiments.


### Package dependency direction: `agent/` imports the capability packages (2026-10-05)

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


### CLI flags: precedence, "unspecified" semantics, and parsing (2026-10-05 → 10-07)

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


### Where "session" state lives (`ChatSession` vs mvp1's `Agent`) (2026-10-05 → 10-07)

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
- **`SetupMCPConns` closes what it connected when a later server fails (fixed 2026-10-07; superseded by `§ 2026-10-23 — One failing MCP server no longer drops the others`).** It used to
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

- **Status: superseded by option B (built 2026-10-09, see "Revised" below); option A kept for the record.** Resolves the deferred item "What
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
- **Revised 2026-10-08: option B chosen. Status: active, built 2026-10-09.**
  The "3x the code" estimate above included press-twice-to-exit and Ctrl+D handling in the reader; the
  line editor (next entry) now handles Ctrl+C/Ctrl+D at the prompt and quitting is Ctrl+D / `/exit` /
  `/quit`, so what remains is a per-turn cancel, roughly 30-50 lines plus tests.
  - **Rule: Ctrl+C stops foreground work; only quitting stops background work.** Foreground = a
    model turn, `/compact`, `/summary` (cancelling `/compact` part-way is safe: the `gen`/`Snapshot`
    design leaves the context unchanged). Background = auto-compaction, which Ctrl+C never touches;
    it is cancelled only by the root context (Ctrl+D, `/exit`, `/quit`, SIGTERM), which `main`'s
    deferred `stop()` already does.
  - **Root context listens for SIGTERM only.** `main` registers `os.Interrupt` once for the whole run
    on a buffered channel (`signal.Notify`) and passes it to `runChatLoop`. The registration never
    ends, so Go's default "Ctrl+C kills the process" never comes back in the gaps between turns
    (which would skip `ShutdownAgent` and leave MCP servers running). Tests pass their own channel.
  - **Per turn:** drain any stale interrupt left in the buffer, `context.WithCancel(root)` plus a
    watcher that cancels on an interrupt, run the turn, cancel. An interrupted turn prints
    `[interrupted]` (not `[error]`) and is not added to memory, like any failed turn.
  - **Auto-compaction moves from `handleUserInput` into `runChatLoop`** so it is started with the root
    context; passing two contexts into `handleUserInput` was rejected as easy to mix up.
  - **As built (2026-10-09):** `runChatLoop` = `ReadPrompt` → `discardStrayInterrupts` → `createTurnWatcher`
    → `handleUserInput(turnCtx)` → `stop()` → quit check → `maybeStartAutoCompaction(ctx)`.
    - **The discard runs before the watcher starts**, not after: otherwise the new watcher and the
      discard would race for the stale press, and the watcher could win and cancel the new turn.
    - **`stop()` waits for the watcher goroutine to exit** (an `exited` channel), added during testing.
      Without it, a watcher that had not reached its `select` yet could wake after `stop()`, find both
      `done` and a new Ctrl+C ready, and (select picks at random) swallow that press.
    - **`reActLoop` no longer prints `error: context canceled`** on a cancel, so an interrupted turn shows
      only `[interrupted]`. `[interrupted]` is checked with `errors.Is(err, context.Canceled)` after
      `reActLoop`, `/summary` and `/compact`; every layer underneath returns or wraps with `%w`.
    - **`maybeStartAutoCompaction` now runs after every input**, including slash commands and failed or
      interrupted turns (it used to run only after a successful turn). Harmless: it only starts when the
      context is over the threshold.
    - **Tests:** `FakeProvider` gained `Gate` (block until closed or ctx cancelled) and `Waiting` (signal
      that a call is blocked); the mutex is released while blocked so a concurrent compaction call isn't
      stuck. Loop tests in `agent/chat_test.go`: interrupt mid-turn, stale interrupt discarded, interrupt
      does not cancel auto-compaction, `/quit` and `/exit`, EOF; plus `createTurnWatcher` unit tests. A
      mutex-guarded `syncBuffer` is the output writer because the loop and compaction write concurrently
      (deferred item 8). Checked by breaking the code: compaction given `turnCtx`, the discard removed, and
      the `[interrupted]` check removed each make a test fail (the discard test 20/20 after it was made to
      hold the turn at the model briefly). 100 runs under `-race` pass.


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


### `/config` prints an allowlist, never the whole struct (2026-10-07)

- **Status: active.** From Copilot's review of PR #6: `PrintConfig` used `%+v` on `Config`, which dumped
  every MCP `Headers` value (e.g. `Authorization: Bearer ...`), `Env`, `Args` and URLs to the terminal and
  any captured log.
- **Decision:** print an explicit list of safe fields. MCP servers show as `name (transport)` only.
  `ApiKeyName` is printed because it is the environment variable's *name*, not the key. An allowlist (not
  a redacting copy of the struct) means a field added to `Config` later stays hidden until someone adds
  it to `PrintConfig` deliberately. Rejected: redacting only `Headers` (misses `Env`, `Args` and tokens
  in URLs). Downside: a new setting won't show up in `/config` until it is added by hand.
- **Tests:** secrets in headers, env, args and URL never appear; the API key value never appears.


### `read_file` is confined to `workDir` and has a deny list (2026-10-07)

- **Status: active.** From Copilot's review of PR #6: the default-enabled tool opened any absolute or
  relative path, and the sample config's `workDir` was not a real `Config` field, so nothing confined it.
  This supersedes the old note "`read_file` can read any path; restricting paths belongs to hooks or
  approval, later". Approval is still the long-term answer for going *outside* the boundary, but the
  agent has no approval layer yet, so a hard boundary is the stand-in.
- **Why it matters even though Claude Code can read any file:** Claude Code asks permission outside its
  working directory and has deny rules for secrets. And the leak path is not stdout: tool results go to
  the LLM provider, and a prompt-injected model (e.g. via text from a remote MCP tool) can ask for
  `~/.ssh/id_rsa` and pass it on through another tool.
- **Boundary:** new `Config.WorkDir` (JSON `workDir`, default `"."`, flag `-workdir <dir>`, flag wins
  over the file; the flag must name an existing directory). `ReadFileTool` is built with
  `NewReadFileTool(workDir)` (`SetupBuiltinTools` now takes the workDir), which resolves the directory to
  an absolute, symlink-free root once and fails at startup if it is missing or not a directory. Every call
  maps the requested path (relative or absolute) to a real path with `filepath.EvalSymlinks`, requires it to
  be inside the root, and only then opens it through `os.Root`. Subdirectories are readable.
- **Deny list (checked on the resolved path, every path component, case-insensitively):** `.env`, `.env.*`
  (except `.env.example`), `*.pem`, `*.key`, `id_rsa*`, and any `.ssh` directory. Checking the *resolved*
  path means a symlink named `notes.txt` that points at `.env` is refused. Case-insensitivity matters on
  macOS and Windows, where `.ENV` is the same file as `.env`.
- **Errors are for the model:** "`<path>` is outside the allowed directory" / "`<path>` is blocked: it
  matches the sensitive-file deny list", with no file content in them, so the model can recover.
- **Why `os.Root` as well as the resolve step:** `resolve` gives clear errors and the deny-list check;
  `os.Root` refuses `..` and symlink escapes at open time, so a symlink swapped in between the check and
  the open still cannot leave the root. Honest gap: the tests cannot exercise that race deterministically,
  so the `os.Root` layer is defence in depth with no test that fails without it.
- **Honest downsides.** Launching the agent in a subdirectory and asking about a sibling now fails until
  `-workdir` is set. The deny list is by name, so it catches common secret files, not every secret (a
  token in `config.json` is readable). Hard links are not detected (`os.Root` does not stop a hard link to
  an outside file placed inside the root). Not covered: MCP filesystem servers have their own access
  rules, and `workDir` does not restrict them.
- **Special files (added 2026-10-07, PR #7 review).** A named pipe inside `workDir` passed every check above,
  and opening it with no writer blocks forever; the call is synchronous and cannot be cancelled from inside
  `open`, so Ctrl+C could not unwind the turn, and the 64 KB cap does not help because nothing is ever read.
  Reproduced before fixing. Fix: open with `O_NONBLOCK` (a FIFO then opens immediately), then `Stat` the
  **opened descriptor** and accept only regular files; directories keep a "is a directory" message,
  other types say "not a regular file (it is a named pipe / socket / device)". The type is checked on the
  descriptor, not the path, so a regular file swapped for a pipe between the check and the open cannot
  slip through. `O_NONBLOCK` does not change how regular files are read. A cancelled `ctx` is also checked
  before reading starts. Rejected: a `Stat`/`Lstat` on the path before opening (races with a swap) and
  running the read in a goroutine with a timeout (leaves a stuck goroutine and file descriptor behind).
  Downsides: `syscall.O_NONBLOCK` ties the file to platforms that define it (fine on macOS and Linux), and
  opening a device node can still have side effects before it is rejected, which only matters if someone
  places one inside `workDir`.
- **Tests:** a sandbox with secrets planted inside the root (every deny-list name, upper-case variants,
  nested `.ssh`), outside it (`../`, absolute, a sibling directory sharing the root's name prefix,
  `/etc/hosts`) and behind symlinks (to a file, a directory, the parent, an absolute target, a denied file,
  a denied directory, and a root that is itself a symlink); allowed look-alikes (`.env.example`,
  `environment.txt`, `monkey.txt`) must still read. Every refusal is checked to leak no secret text.
  Disabling the deny list, the outside check, case-folding, or symlink resolution each makes tests fail.
  Special files (unix only): a pipe with no writer, a pipe with data waiting (which must stay unread), a
  symlink to a pipe, a Unix socket, and a cancelled context; removing the `O_NONBLOCK` flag, the type check,
  or the context check each makes tests fail.


### Remote MCP requests never leave the configured origin (2026-10-07)

- **Status: active.** From Copilot's review of PR #7 on `mcpconnect/conn.go`: the HTTP client followed
  redirects and `headerTransport` re-added the configured headers on every hop.
- **The comment is right; measured, not assumed.** With a 307 (method and body kept) to another host, a
  client using our `headerTransport` delivered `Authorization`, `X-Api-Key`, `Mcp-Session-Id` and the
  request body (tool arguments) to the other server. Plain `net/http` would have stripped `Authorization`
  on a cross-host redirect, but our transport re-adds it on each hop, and `net/http` compares host *names*
  only, so even without our transport a redirect to another port, or from `https` to `http` on the same
  host, keeps the token. Stripping headers alone would not have been enough: the body and session id go
  along too.
- **What stays the same:** `headerTransport` is still how Bearer tokens and API keys reach the MCP server.
  Redirects within the configured origin (a trailing-slash or path change) are followed and keep the
  headers and the body.
- **Decision, two layers.**
  1. `newRemoteHTTPClient` sets `CheckRedirect`: a redirect whose origin (scheme, lower-cased host, port,
     default ports filled in) differs from the origin of the **configured** `url` is refused with
     "redirect to X blocked: MCP server redirects must stay on Y". It compares against the configured
     origin on every hop, not the previous hop, so A -> A -> B is caught. A custom `CheckRedirect`
     replaces net/http's built-in 10-hop limit, so the limit is restated (`maxRedirects`).
  2. `headerTransport` refuses to send any request whose origin is not the one it was built for, instead
     of sending it without headers (which would still leak the body). It is the second line of defence
     if the redirect policy changes or the transport is reused.
- **Limitation, deliberate for now:** an `http` -> `https` redirect on the same host is rejected too,
  because scheme is part of the origin. It is safe but rejected, so a server must be configured with its
  final `https` URL. Recorded under "Deferred for Later" item 6.
- **Tests:** cross-origin redirect refused and the target receives nothing (other port, other host
  name); same-origin 307 keeps the headers and replays the body; a chain A -> A -> B is refused; an endless
  same-origin loop stops at 10; an origin-rule table (path change, explicit default ports, host-name case,
  IPv6, https -> http, http -> https, other port, other host, subdomain, look-alike prefix); the transport
  guard; `connectRemoteMCPServer` end to end against a redirecting server and a real fake MCP server; and
  invalid endpoints. Each layer was disabled in turn and the tests failed; with only one layer
  disabled the other still blocks the leak, which is the point of having two.


### Local MCP servers receive the configured `env` (2026-10-07)

- **Status: active.** From Copilot's review of PR #7 on `mcpconnect/conn.go`: `McpServerConfig.Env` was
  validated (`TransportType` rejects it on remote servers) but never applied, so a token configured as
  `"env": {"GITHUB_TOKEN": "..."}` never reached the stdio server's process.
- **Decision:** when `env` is non-empty, `connectLocalMCPServer` sets `cmd.Env` to our own environment
  (`os.Environ()`; `npx` and `node` need `PATH`, `HOME`, ...) followed by the configured `KEY=value`
  entries in sorted key order. `os/exec` uses the **last** value of a duplicate key, so configured values
  beat the parent's. With no `env` block `cmd.Env` stays nil, which already means "inherit everything", so
  behavior for existing configs is unchanged.
- **Tests:** the test binary re-run as the child reports its `MCP_TEST_*` variables to a file; checked that
  a configured entry arrives, that it overrides a same-named parent variable, that the parent's other
  variables are still inherited, and that a server with no `env` block still inherits. Disabling the
  overlay, putting the configured entries before `os.Environ()`, and not inheriting each make them fail.
- **Small follow-ups, not done:** see "Deferred for Later" item 7.


### Line editor: `go-multiline-ny` (2026-10-08)

- **Status: active, wired in 2026-10-09** (`agent/input.go`, `ttyReader`). Chosen after a throwaway test program
  passed the key checklist below. Pinned to the tested commit: `v0.23.2-0.20260509051138-60a7d8e7d356`.
  Unit-tested without a real terminal through the library's own fake terminal (`ttyadapter/auto.Pilot`),
  injected by `newTTYReaderWith(tty, out)`; the terminal must be set before the first `BindKey`, which sets
  the editor up. The prompt is the named function `writePrompt`, shared with `plainReader` (pipes, files,
  tests), which is used whenever stdin is not a terminal (`term.IsTerminal`).
- **Want:** ←/→, Ctrl+A/E, ↑/↓ moving between lines of a multi-line prompt (history at the edges),
  a new-line key, Option+←/→ by word, history across prompts. The `bufio` reader does none of it: the
  terminal in its default "cooked" mode has no history and echoes arrows as `^[[A`.
- **Decision:** `github.com/hymkor/go-multiline-ny` (MIT, built on `nyaosorg/go-readline-ny`). Bindings
  swapped to chat style: Enter submits, Ctrl+J and Alt/Option+Enter insert a new line (the library's
  default is the reverse). Ctrl+D on an empty prompt returns `io.EOF` and quits.
- **Ctrl+C at the prompt clears the prompt, it does not quit** (Jerry, 2026-10-08). The editor returns
  `readline.CtrlC`; the terminal reader discards the input and reads again, so the chat loop never sees
  it. Ctrl+C *during a turn* cancels only the current turn and returns to a fresh prompt, so Ctrl+C
  consistently means "abandon what's happening now".
  Quit is Ctrl+D, `/exit` or `/quit` (`/quit` added as an alias, 2026-10-08); Ctrl+C never quits at the
  prompt, even when the prompt is empty.
- **Rejected:**
  - `golang.org/x/term`: Go-team package with history and single-line editing built in, but no
    multi-line editing. Ctrl+J is treated as Enter and a lone Esc is swallowed as the start of an
    escape sequence. Getting Ctrl+J/Esc/Option+arrows meant a byte-rewriting reader in front of it;
    too much rework.
  - `rlwrap`: no code, but every user has to install it and run the agent through it.
  - `reeflective/readline`: the most complete (multi-line, `.inputrc`, vi mode) but larger than needed.
  - `ergochat/readline`: maintained fork of `chzyer/readline`, but mainly single-line.
  - `bubbline` / Bubble Tea `textarea`: Bubble Tea runs its own event loop and its author documents
    input loss when it is used one line at a time, which is how the chat loop reads.
- **Accepted downsides:** the library's `go.mod` pulls in six direct modules (go-readline-ny,
  go-ttyadapter, go-box, go-runewidth, go-colorable, clipboard) and five indirect ones (go-tty, go-isatty,
  uax29, x/term, x/sys); a small single-maintainer project.
  Shift+Enter can't be a new-line key because most terminals send the same byte as Enter. Esc can't
  clear the prompt because the library uses Esc as a prefix key (Esc+p/n history, Esc+arrows by word);
  Ctrl+U is the substitute.
- **Known gap, accepted: SIGTERM at the prompt.** No context-cancellation handling was found in go-readline-ny,
  so `ed.Read(ctx)` probably doesn't return when the root context is cancelled; a `kill` while sitting at
  `>` likely takes effect only after the next keypress. To verify by hand. Accepted for chat mode:
  headless mode won't use the editor, and Ctrl+D / `/quit` still work.
- **Constraints when wiring it in:** keep the `bufio` path when stdin is not a terminal (tests feed
  `cs.InBuffer`, piped input); map `io.EOF` onto the existing quit path; the
  terminal must be back in normal mode between reads so Ctrl+C during a turn still cancels it
  (see "Chat loop and Ctrl+C" above); the reader goroutine currently reads one line ahead
  while a turn runs, which a raw-mode editor must not do.


### Debug output behind `-debug` / `"debug": true` (2026-10-09)

- **Status: active.** Jerry: the per-turn context dump and the MCP startup/protocol log were printed on every
  run; put them behind a debug switch.
- **Decision:** `Config.Debug` (`"debug": true` in a config file; a JSON boolean, so `"debug": "true"` is a
  parse error, which a test checks) and a `-debug` flag. Precedence as for every other setting: flag >
  config file > default (`false`); `-debug=false` switches off `"debug": true` from the file. Shown by
  `/config`.
- **Gated by it:**
  - the `[debug] --- context window ---` dump in `handleUserInput`. `DebugChatContext` now takes an
    `io.Writer` and is given `cs.OutBuffer`, instead of printing to process stdout (Deferred item 5).
  - `mcp.LoggingTransport` on both MCP transports, through `withDebugLogging(transport, debug)` in
    `mcpconnect/conn.go`. Normal runs use the transports directly (Deferred item 4).
- **Not gated (conversation output, always shown):** `[assistant] ...`, `[tool] Tool called: ...`,
  `[system]`/`[error]`/`[interrupted]` lines, the welcome text and "Using model ...". MCP warnings
  (skipped SSE server, a tool that failed to load) still go to stderr.
- **Trade-off:** one switch for two different kinds of output (a chat-context dump on stdout and a protocol
  log on stderr). Finer switches (e.g. `-debug=context,mcp`) were not needed yet; the tracing hook planned
  next is where structured, selective output belongs.
- **Tests:** flag parsing (`-debug`, `--debug`, `-debug=false`, not passed), `FlagsOverlay`, the config key
  (applied without warning; quoted string rejected), `/config` output, the dump only in debug mode, and
  `withDebugLogging` wrapping only in debug mode.


### "Thinking" dots while waiting for the model (2026-10-09)

- **Status: active.** In chat mode, a `.` is printed every 2 seconds while a turn waits for the model; the
  line is ended before `[assistant] ...` (or `[tool] ...`, `[interrupted]`).
- **Where:** `agent/progress.go`. `reActLoop`'s model call is `chatWithProgress(ctx, cs, ...)`, which starts
  `startProgressDots` only when `cs.ShowProgress` is set; the loops themselves contain no dot code.
  `ShowProgress` is set in `SetupAgent` by `shouldShowProgress(cfg)`: chat mode **and** output is a real
  terminal (`isTerminal(cfg.OutBuffer)`, the same helper `newPromptReader` uses for input).
- **Only around the turn's model calls, not every provider call.** A wrapper around the whole `Provider`
  would also show dots for background auto-compaction, printing into the prompt while the user types.
  `/compact` and `/summary` (foreground, also slow) don't show dots yet; adding `chatWithProgress`-style
  calls there is the same two lines.
- **Never in headless/oneshot (JSON or machine-read output), pipes, files or tests** (`ShowProgress` is
  off unless set by `SetupAgent`; the test fixture never sets it).
- **`stop()` waits for the dot goroutine to exit** (same pattern as `createTurnWatcher`), so a tick
  firing as the reply arrives can't print a dot after it. Stopping before the first tick prints nothing.
- **Dots on their own line rather than a spinner overwritten in place (`\r` + `|/-\`):** a spinner breaks as
  soon as anything else prints mid-turn (`[tool]` lines, background compaction messages).
- **Tests (`agent/progress_test.go`):** dots then a newline; nothing if stopped before the first tick;
  nothing after stop; `chatWithProgress` with a gated `FakeProvider` (dots while waiting; line ended on
  Ctrl+C; nothing when `ShowProgress` is off); `shouldShowProgress` false for buffers, files and
  non-chat modes. `progressDotInterval` is a variable so tests can use 10ms. Removing the newline, the wait
  in `stop`, or the `ShowProgress` check each makes a test fail (10/10 runs).


### Staged: autonomous (one agent) first, orchestrator/worker later (2026-10-13)

- **Status: decided (the split); step 1 details being worked out.** Brainstorm transcript:
  `prompts/20261012-auto-mode-brainstorming.md`.
- **Step 1, `-mode autonomous`:** a second LLM (the *director*), guided by `objective.md`, replaces
  the human who types prompts in the REPL. Still a **single agent instance**, a single process. The
  objective file must say clearly what "done" means (`# Objective` + `## Done when` required).
- **Step 2, later and still to be designed:** `-mode orchestrator` = autonomous mode where the main
  agent delegates tasks to worker instances; `-mode worker` = an agent that only listens for MCP
  requests (JSON-RPC), does the work and replies. No worker-to-worker messaging.
- **Renames:** `headless` → `autonomous`, `mission` → `objective`. "Headless" only means "no UI",
  which `oneshot` also is.
- **Why staged (reversal of the 2026-10-12 direction):** the 2026-10-12 brainstorm went straight to
  multi-process workers (MCP over stdio/HTTP, worker pools, launchers, Kubernetes). Jerry judged that
  too much at once. Step 1 holds the core idea, "an LLM replaces the human prompter", with no
  protocol or process management. The step 2 ideas are parked, not rejected: stateless tasks, the
  LLM deciding *what* to delegate while a pool decides *how many* instances, a `workers` config
  section reusing `McpServerConfig`, and Kubernetes "pattern B" (Deployments + Services, the
  orchestrator connects by URL) first. Step 1 should keep the director behind a small interface, so
  step 2 can reuse it unchanged.


### Step 1 (`-mode autonomous`): answers decided 2026-10-13

Questions Q1–Q17 were put to Jerry on 2026-10-13 (see the brainstorm transcript). Decided:

- **Flow:** the director LLM gets a system prompt with `objective.md` and its instructions. Its reply is
  the worker's next prompt. The worker runs one normal ReAct turn, and its outcome goes back to the
  director. This repeats until the director judges the objective met; the user then gets a final report.
- **What the director sees of a worker turn (Q2):** the reply text, a short line naming the tools that
  were called, any error, and other important facts from the turn, capped so one reply can't flood the
  director's context.
- **The worker never sees the objective (Q3).** It sees only the director's prompts, so a task is the
  unit of text the worker gets.
- **The director has its own built-in tools (Q4)**, at least `read_file`, but not the worker's tools.
  Which other built-ins it gets is still open.
- **Stop conditions (Q5/Q7):** the director decides the objective is met, `maxTurns` (default 15), or
  cancellation (Ctrl+C, SIGTERM, timeout).
- **The director judges "done" (Q6).**
- **`objective.md` (Q8/Q9):** `# Objective` and a "done" section are required; `## Constraints`,
  `## Context` and `## Tasks` are optional. A `## Tasks` section is what the user intends to be done,
  so it is treated as a checklist, not as hints.
- **Director memory (Q10):** its own `ChatContext`, **with** compaction when its window fills.
- **Ctrl+C (Q11):** ends the whole run, prints the report for the current state, then shuts down
  cleanly.
- **Tools and safety (Q12):** the worker keeps its MCP tools; autonomous mode prints a warning that
  tool calls run unattended, with no refusal or opt-in flag.
- **No slash commands from the director (Q13).** Its text always goes to the worker as an ordinary prompt.
- **Output (Q14):** `[director]` lines during the run; the final report goes to stdout. `-json` is deferred.
- **Code layout (Q17):** the director is a separate capability package.
- ~~Still open: config shape (Q15), how the director signals "next prompt" vs "finished" (Q1).~~
  Resolved in the next subsection.


### Step 1: director mechanics, checklist, config (decided 2026-10-13, second round)

*Partly superseded the same day by `§ Step 1: task-driven flow` (`mark_item`, the combined checklist,
rejecting `finish`). Still current: the director as a second `ChatSession`, text reply = worker prompt,
ending the turn on a terminal tool without an extra call, the system prompt rebuilt each director turn,
and the config shape, except that `maxTurns` became `maxTurnsPerTask` with no run-wide limit.*

- **The director is a small ReAct agent: a second `ChatSession`** with its own provider, `ChatContext`
  (auto-compaction included) and tool registry. A director turn and a worker turn both go through the
  same `runTurn`. The director's tools: `read_file` (confined to the same `workDir` as the worker),
  `mark_item` and `finish`.
- **Its final text reply is the worker's next prompt.** It is passed as a value, not through
  `InBuffer`/`OutBuffer`. Those stay the terminal you watch the run on; stdin is unused in autonomous mode.
- **`finish(success, summary)` ends the run, and an accepted `finish` is the last director LLM call.**
  In a ReAct loop a tool call normally means "run it and send me the result", which would cost one more
  call. An accepted `finish` instead ends the director's turn immediately (the mechanism, such as a
  sentinel error from the tool, is decided when coding). A **rejected** `finish(success=true)` (the
  checklist has unfinished items) comes back to the director as an ordinary tool error, and its turn
  continues. `finish(success=false)` ("can't be done, here's why") is always accepted.
- **Progress lives in a checklist that is shown in the director's system prompt** (Jerry's choice), so
  compaction can't lose it. The checklist = every `## Tasks` item plus every done-condition, each
  `pending`/`done` with a short note. The director changes an item with `mark_item(id, status, note)`.
  The loop rebuilds `SystemMsg` from the objective plus the checklist before each director turn.
  - *Trade-off:* changing the system prompt breaks provider-side prompt caching for that call, so a turn
    after a change pays for the full prompt again. That's acceptable at ≤ 15 turns, and local models don't
    cache anyway. The cache-friendly alternative, putting the checklist into each new message to the
    director instead, was not chosen.
- **Config:** a `"director"` block inside the existing config. The top level stays the worker's, so
  chat configs work unchanged. Empty director fields come from a separate **default director config**,
  not from the worker's values. Precedence: flag > director block > director defaults. The director's
  instruction text is built into the code; `director.additionalInstructions` adds to it, never
  replaces it. `maxTurns` (default 15, must be > 0) lives in the director block, and `-maxturns`
  overrides it.


### Step 1: defaults (proposed by Claude 2026-10-13; Jerry asked for defaults on these)

*Partly superseded by `§ Step 1: task-driven flow`: "done = every checklist item", the required `## Done`
section and the report reasons changed. The timeout flag, the bullet-parsing rules (now for `## Tasks`),
the size limit and the exit codes still apply.*

- **Done = every checklist item `done`** (all tasks and all done-conditions). The task order is a
  suggestion, not enforced. In step 1 the director cannot add items (the checklist is the user's
  intent); it can still split work freely in its prompts.
- **`-timeout <duration>`** (Go duration such as `30m`; default 0 = none) → run reason `timeout`.
- **`objective.md` parsing:**
  - Exactly one `# Objective` H1 is required, plus one `##` heading that starts with "Done"
    (case-insensitive: `## Done`, `## Done when`, `## Done criteria`, …) with ≥ 1 bullet.
  - `## Tasks`, `## Constraints` and `## Context` are optional; other sections are allowed and passed
    through as text.
  - Checklist items are top-level bullets (`-`, `*`, `1.`). A `[ ]`/`[x]` box is stripped and ignored,
    so every item starts `pending`; nested bullets belong to their parent item.
  - The file must exist and be non-empty, and at most 20 KB. A bad file is a startup error (exit 2).
- **Confirmed by Jerry (2026-10-13):**
  - Report contents: reason `finished|max_turns|interrupted|timeout|error`, success, summary, checklist
    with notes, turns used.
  - Exit codes: 0 met / 1 not met or error / 130 interrupted.
  - A failed director turn ends the run with `error`, because there are no retries yet.
- **Worker history: per task, not kept across tasks** (Jerry). The worker needs only the current task's
  context. If a later task needs something from an earlier one, the director's instructions tell it to
  put that into the worker prompt.


### Step 1: task-driven flow (decided 2026-10-13) — the current design

The feature plan (flow, config, types, build slices, and a D1–D29 decision summary) is in
`PLAN-autonomous-mode.md`. This log stays authoritative where the two disagree.

This supersedes parts of the two subsections above: the `mark_item` tool, the checklist of tasks *plus*
done-conditions, "done = every checklist item", rejecting `finish(success=true)`, the required `## Done`
section and the overall-only `maxTurns`. Their reasoning is kept above as the earlier version.

- **Why (Jerry):** a run is a task list worked through **one task at a time**. The code owns the plan
  (the list plus the current-task pointer), and the director LLM answers one narrow question at a time:
  "does this reply complete the current task?" That is easier for an LLM to get right and easier to
  test than the LLM keeping track of the whole checklist itself.
- **`objective.md`:** only `# Objective` is required. `## Tasks` (used exactly as written: no rewriting
  or splitting by the director), `## Done` (any `##` heading starting with "Done"), `## Constraints`
  and `## Context` are optional. The objective stays in the director's system prompt for the whole run,
  because judging a task, honouring constraints and writing good worker prompts all need the "why", not
  just the task list.
- **Three phases, driven by code.** The director is offered only the tools for the current phase
  (all phases also have `read_file`):
  - **PLAN** (only if there is no `## Tasks`): `set_tasks(tasks)`, 1–20 non-empty tasks. The plan is
    printed and is fixed for the run (re-planning is deferred). Work discovered later becomes follow-ups
    within a task.
  - **WORK**, for each task in order: the director writes the worker prompt; the worker runs a turn;
    the code wraps the reply ("Task i of n: <task>. Worker reply: …, tools called: …, error: … Is the
    task complete?"). The director then either
    - calls `task_done(note)` → next task. The tool result names the next task, and the director's text
      reply becomes that task's worker prompt, so one director turn both judges a task and starts the next;
    - replies with text → a follow-up prompt to the worker for the same task;
    - calls `task_failed(reason)` → the run stops (see below).
  - **REVIEW**, after the last task: the director checks the completed tasks and their notes against
    `# Objective` (and `## Done` if present) and calls `finish(success, summary, gaps)`. `gaps` is a
    list of strings, so it is easy to read out in a later `-json` mode and to turn into new tasks in a
    follow-up run.
- **Ending a director turn early:** `task_done` on the last task, `task_failed` and `finish` end the
  director's turn at once, with no extra LLM call to acknowledge the tool result.
- **Memory:** the worker's *and* the director's context are cleared at the start of each task (the
  worker's full `MsgHistory` is still appended to). The director carries information forward through
  its `task_done` notes, which appear with each task's status in its system prompt, rebuilt before every
  director turn. The director's instructions tell it to write notes later tasks will need, and to pass
  earlier results to the worker in the prompt. Compaction stays on as a safety net for a long task.
  *Downside:* anything not written into a note is gone; the director can still `read_file` what the
  worker produced.
- **Limits and stopping:**
  - `maxTurnsPerTask` (director block, default 5) caps worker turns per task. Hitting it counts as the
    task failing.
  - **No run-wide turn limit** (Jerry, 2026-10-13). The run is already bounded by `maxTurnsPerTask` ×
    number of tasks (≤ 20 tasks). This replaces the earlier `maxTurns` (default 15). Flag:
    `-maxturnspertask`, in the same style as `-maxsteps`. *Downside:* the worst case is large: 20 × 5 = 100
    worker turns, each up to `MaxSteps` LLM calls. `-timeout` is the wall-clock safety net.
  - A failed task (`task_failed`, or the per-task limit) **stops the run** and reports progress. It still
    makes a best-effort REVIEW call for the summary and gaps; if that call fails, the report is built
    from the code's own state. Interrupt, timeout and error skip the REVIEW call and use the code's state.
- **Report:**
  - reason: `finished | task_failed | interrupted | timeout | error` (hitting the per-task limit is
    reported as `task_failed`);
  - success, summary;
  - every task with its status (`done | failed | pending`) and note;
  - gaps;
  - turns used.
- **Exit codes:** 0 = finished and success; 1 = anything else except an interrupt; 130 = interrupted.


### mvp3 reference build: tweaks and extra decisions (2026-10-13)

Claude built autonomous mode in full in `mvp3/` (a copy of the root agent, as its own Go module) at
Jerry's request, as a run-through and template for his own build at the root, like `mvp1/` before
mvp2. These are the places where the build differs from, or adds to, the decisions above. Items marked
*(real run)* came from live runs with gpt-4o-mini and claude-haiku-5-5. Each is a candidate for the root
build, not yet adopted there.

**Flow and director**
1. **Every accepted control tool ends the director's turn**, including `set_tasks` and `task_done` on a
   non-last task. This drops D12 ("task_done's result names the next task and the reply is its
   prompt"). *Why:* contexts are cleared per task, so the next task's first prompt should be written in
   that task's fresh context, not at the end of the previous task's. *Cost:* one extra director call
   per task.
2. **The turn that writes a task's first prompt offers the director no tools at all** *(real run)*,
   so its only possible answer is text. Offered file tools, gpt-4o-mini listed an empty directory until
   it ran out of steps. Offered only `task_done`/`task_failed`, it called `task_failed` before the worker
   had tried anything. Judging "is it done?" only makes sense after a worker turn.
3. **The director's read-only tools are `read_file` and `list_dir`**, offered when planning, when judging
   a worker turn, and in review.
4. **The director's system prompt also lists the worker's tool names and the per-task turn limit**, so it
   writes prompts the worker can carry out.
5. **More built-in instruction text** *(real run)*: "say what to achieve and how it will be checked;
   leave the content (code, prose) to the worker" (gpt-4o-mini was writing the code itself inside its
   prompts), and "file tools are for checking results, not exploring".
6. **`finish(success=true)` is refused while any task is not `done`.** This keeps the consistency check
   from the superseded checklist design. The director gets an error and answers again.
7. **Per-task limit handling:** when the director replies with a follow-up after the worker has used
   all its turns, it gets one reminder (`NudgeLimit`). After that, the code fails the task itself
   ("turn limit reached").
8. **Reminders ("nudges"):** at most 2 (`maxNudges`) when the director replies with text in PLAN or
   REVIEW, or with nothing in WORK. After that the run ends with `error`.
9. **Per-task context clearing waits for any background compaction first** (`WaitForCompaction`, then
   `Clear`). `ChatContext`'s generation counter would also discard a late compaction result.
10. **Known limit:** a director *commentary* reply in WORK ("the task looks done") is sent to the worker
    as a prompt, because the code can't tell it apart from an instruction. The instructions guard
    against it; no code does.

**Startup, config, CLI**

11. **The unattended-tools warning lists every worker tool except `read_file`/`list_dir`**, not only MCP
    tools, because `write_file`/`edit_file` change things too.
12. **Autonomous mode checks at startup that the worker's and the director's API-key environment variables
    are set**, instead of failing with a 401 halfway through a run.
13. **`-directormodel` flag** (new). `-objective`, `-maxturnspertask`, `-timeout` and `-directormodel` are
    refused with an explicit non-autonomous `-mode`. `--json` is refused for autonomous mode until it
    exists. `"timeout"` can also be set in the config file.
14. **The unknown-key warning also checks keys inside `"director"`** (e.g. `director.maxTurnPerTask`).
15. **SIGTERM exits 143** (the open item in the plan). `main` became `run() int`, which also fixes
    Deferred item 3 in mvp3: chat mode now exits 1 when setup fails, instead of 0.
16. **`tools.ErrEndTurn` lives in `tools/`**, next to the `Tool` interface. `reActLoop` uses
    `runToolCallEnd`, and tool calls after an ending tool in the same reply are skipped.

**Objective parsing (details the plan left open)**

17. Text before `# Objective` is ignored. Any other `#` heading is an error. Lines inside ``` fences
    are never headings or bullets. `###` and deeper are body text. Non-bullet text under `## Tasks`
    before the first bullet is kept as `TaskNotes` and shown to the director.

**Output and model**

18. **Output labels:** `ChatSession.Name` gives `[worker]`, `[worker:tool]`, `[director]` and
    `[director:tool]` (chat keeps `[assistant]`/`[tool]`). An empty `[assistant]` line is no longer
    printed when a reply contains only tool calls.
19. **The report shows the error text only for reason `error`.** For `interrupted`/`timeout` it would
    only say "context canceled".
20. **An HTTP error from the OpenAI-compatible adapter includes the start of the response body**
    (collapsed to one line), e.g. "the model `x` does not exist".

**New built-in tools** (Jerry asked for write/edit)

21. **`write_file`, `edit_file` and `list_dir`, opt-in through `builtinTools`** (`"WriteFile"`, `"EditFile"`,
    `"ListDir"`; the default is still only `ReadFile`).
    - All of them are confined to `workDir` with the same deny list as `read_file`.
    - `write_file` can't use `resolve()` (the file may not exist yet), so it uses `resolveTarget`: it
      resolves symlinks on the deepest existing ancestor, then checks the path is inside `workDir`. The
      write itself goes through `os.Root` (`MkdirAll` + `WriteFile`), the second line of defence.
    - `edit_file` replaces an exact `old_string` that must match once (or `replace_all`), and keeps the
      file's permissions.
    - There is a 1 MB cap. `read_file`'s open-and-check code became `readRegularFile`, shared with
      `edit_file`.


### mvp3 reference build, continued: items 22–26 (2026-10-14)

Continuation of `§ mvp3 reference build: tweaks and extra decisions (2026-10-13)`; the item numbers continue from 21.

22. **`run_command`: one built-in tool that runs `go`, `python3`, `python` or `pytest`** (2026-10-14;
    opt-in as `"RunCommand"`; `tools/builtin/runcommand.go`). Jerry asked for a way to run Go and Python
    so coding tasks can be tested.
    - **One combined tool rather than `run_go` + `run_python`:** one schema and one set of rules (timeout,
      output cap, environment); adding a program is one line in `RunCommandPrograms`. The `command`
      argument is a JSON-schema enum of the programs actually found on `PATH` when the tool is built,
      each resolved to an absolute path then, so a file named `go` written into `workDir` can't be run instead.
    - **Not a shell.** The program starts directly with an argument list (no `sh -c`), so pipes, `&&`,
      `$VAR` and `$(...)` are literal text. Downside: the model can't chain commands, so `go mod init`
      then `go test` is two calls.
    - **Optional `dir`**, a subdirectory checked with `read_file`'s `resolve()` (inside `workDir`, not
      deny-listed).
    - **Limits:** 3-minute timeout per call; 32 KB of combined output, and the program is killed past
      that; stdin is empty. It runs in its own process group, killed on timeout and again after it exits,
      so a background child can't outlive the call (mvp1's `tool.Shell` design). A Ctrl+C of the turn
      returns the turn's error, kept apart from the tool's own timeout.
    - **A non-zero exit is a normal result** (output plus `[exit status N]`), not a tool error: a failing
      test is information for the model.
    - **The environment is rebuilt from an allowlist** (`RunCommandEnv`: `PATH`, `HOME`, Go and Python
      settings), so `OPENAI_API_KEY`/`ANTHROPIC_API_KEY` don't reach the program.
    - **Honest downside: this is code execution, not a sandbox.** `go run`/`python3` run code the model
      wrote, with the user's permissions: they can read outside `workDir` (the deny list doesn't apply),
      use the network, delete files. That's why it is opt-in and appears in the unattended-tools warning.
      Real isolation is a container/VM runner behind the same tool name (deferred).
23. **The director is told what a failed worker turn means** (`WrapTurn`; *real run 2026-10-14*). A
    failed turn (for example the worker hitting `maxSteps`) isn't added to the worker's memory and its
    tool list is lost. The director used to read "failed … Tools called: none" as "nothing happened" and
    called `task_failed`, although the worker's tests were in fact passing. The message now says files
    the worker changed are kept, the worker won't remember the turn, and the director should check
    the files before deciding. The "Tools called" line is left out for a failed turn rather than saying
    "none".
24. **The coding example configs set the worker's `maxSteps` to 20** (`config.openai.json`,
    `config.claude-director.json`, `config.ollama.json`; the default stays 10). A write → test → fix →
    test loop uses steps quickly. These configs also enable `RunCommand`, and `objective-fib.md` now has
    the worker create the module and run `go run .`/`go test ./...` itself.

25. **The director's control tools are guarded against "I meant to send the worker back"** (*real run
    2026-10-14*, `run-20261014-0216.log`, 19-task transpiler with a Haiku director, stopped on task 2).
    - **What happened:** Haiku wrote "I'm sending the worker back to fix that" *in the same message as a
      `list_dir` call*. Text that comes with a tool call is never sent to the worker, because only a final
      text-only reply is. Then, still in that turn, it called `task_failed` with the reason "this call was
      an error; the task has not failed". The earlier "`task_done` with a note saying 'Not yet done'"
      observation is the same confusion. (An outside diagnosis Jerry pasted caught the `task_failed`
      misuse and the director's over-strict style review, but not the text-with-tool-call cause.)
    - **Instructions now say:** your instruction is sent only when your turn ends with plain text and no
      tool call, and text next to a tool call reaches no one. `task_failed` stops the whole run and is
      never a way to ask for fixes. Judge work against what the task states and against real checks, not
      unstated design preferences. The `task_done`/`task_failed` tool descriptions say the same.
    - **Code guard:** while the worker still has turns, the **first `task_failed` of a task is refused**
      with that explanation, as an ordinary tool error, so the director can recover in the same turn. A
      second call is accepted. `Plan.WorkerTurnsLeft` is set by `runTask` before each director turn.
      The refusal is **once per task, not once per director turn**. The per-turn version was tried first:
      Haiku treated each refusal as "use up the turns first" and spent 3 extra worker turns before it
      could give up. Per task, a real give-up (missing-input example) costs one extra director call and
      0 extra worker turns.
    - Not guarded in code: `task_done` with a "not done" note. Refusing `task_done` would add a director
      call to every task; the instruction and description changes are meant to cover it.

26. **`director.maxTasks` is configurable** (Jerry, 2026-10-14; default 20, must be positive; there's no flag).
    It replaces the `director.MaxTasks` constant. The same limit applies to a given `## Tasks` list
    (`ParseObjective(data, maxTasks)`, so a longer list is a bad objective file, exit 2) and to
    `set_tasks` (`Plan.MaxTasks`, set by `NewPlan(tasks, maxTasks)`). The set_tasks description and the
    PLAN-phase prompt show the configured number. Trade-off: a bigger limit makes the run longer
    (the worst case is maxTasks × maxTurnsPerTask worker turns), and every director call costs more, because
    the system prompt lists every task and its note.


### mvp3 reference build, continued: item 27, rounds (2026-10-15)

Continuation of `§ mvp3 reference build: tweaks and extra decisions (2026-10-13)`; the item numbers continue from 21.

27. **Rounds: `director.maxIterations`** (Jerry asked, 2026-10-15; default 1, which is the old behavior;
    must be positive; no flag). A round is plan → work → review. Another round starts when the review
    succeeded, said "not met", listed at least one gap, and rounds are left. That includes a round that
    ended in `task_failed`, because a turn-limit failure is often fixable with a fresh plan.
    - **Next round's input:** a PLAN phase that always runs (the objective's `## Tasks` only seeds round
      1), prompted with the last review's summary and gaps (`GapsPlanPrompt`). Files stay in the workdir.
    - **Earlier rounds** (tasks, notes, review summary, gaps) go in the director's system prompt under
      `## Earlier rounds`, because contexts are cleared per task.
    - **Picky-reviewer guard:** while rounds are left, the review prompt says the gaps become a new
      round, so list only what the objective and its quality bar require. Further stop conditions, such as
      measuring the size of the gap, are for later (Jerry).
    - **Report:** one section per round; the run succeeds if the last round does; worker turns are counted
      across rounds. `maxTasks` applies per round.
    - **Named `maxIterations`, not Jerry's `maxSessionIterations`:** "session" already means `ChatSession`.
    - **Downsides:** cost grows with each round; a task that truly can't be done (missing input) is retried
      until the limit; the system prompt grows with every round.


### mvp3 reference build, continued: observed in real runs (2026-10-14/15)

Continuation of `§ mvp3 reference build: tweaks and extra decisions (2026-10-13)`; the item numbers continue from 21.

**Observed in real runs (not changed)**

- A review can find a real defect and still report `success=true` with the defect listed under gaps
  (glossary run: a duplicated bullet). The run can't go back and fix it. This is the deferred "turn gaps
  into new tasks" round. *(Built 2026-10-15 as item 27, but only for success=false: a success=true review
  with gaps still ends the run.)*
- With small models, the objective's `## Context` decides success. The DeepWiki example failed until
  Context said the repository is *not* in the working directory, because the worker tried `read_file`
  on paths DeepWiki had mentioned.
- *(2026-10-14, with `run_command`)* gpt-4o-mini as worker spent a whole task failing to fix its own
  test (it compared `[]int` with `int`). The gpt-4o-mini director then gave wrong advice (it said the
  last element of `fib(10)` is 55) and called `task_failed`. `go test` caught it honestly, which is the point.
- *(same day)* With a Haiku director, the worker put `fib_test.go` in a stray `fib/` subdirectory. The director
  noticed, but then called **`task_done` with a note that began "Not yet done"** instead of
  sending a follow-up prompt. Possible fixes, not made: make the `task_done` description say "only when
  the task is complete; to continue, reply with a prompt", or have review turn such notes into gaps.


### Step 2 (`-mode orchestrator`): direction from Jerry (2026-10-15)

- **Status: direction agreed, not designed in detail.** Transcript: `mvp3/prompts/` (2026-10-15 session).
- **Flow:** (1) the director plans tasks, with dependencies, from a catalog of available worker agent
  types; (2) an LLM (probably the director) assigns each task an agent type; (3) a **plain-Go
  orchestrator** (no LLM) starts MCP worker agents and sends the tasks whose dependencies are done, within
  resource limits; (4) replies go to the director, which decides "done" or "follow-up", and the
  orchestrator delivers the follow-up to the same worker; (5) repeat until all tasks are finished, then
  review the objective.
- **No local worker LLM in this mode:** the local process is the director plus the Go orchestrator.
- **Workers are MCP servers** (agent as an MCP tool), not A2A for now. Plain request/response, with a
  `status` field in the JSON result (completed / question / failed …) standing in for A2A's task
  lifecycle. A2A stays a later option, for long-running or remote tasks.
- **Task state lives in Go, not in the director's context** (a `TaskManager`: task ID, worker ID,
  status, history). Jerry's analogy: a team lead with hundreds of reports can't track it in their head.
- **Before step 2:** add session iterations to mvp3. A review with gaps becomes a new plan → work →
  review round, up to `maxSessionIterations` rounds; the first session counts as 1. *(Built 2026-10-15 as
  `director.maxIterations`: § mvp3 reference build, item 27.)*
- **Shared files: tasks run one at a time first** (agreed 2026-10-15), to prove the flow with one MCP worker.
  Parallel tasks come later, with declared write sets plus locks.
- **The local worker stays (proposed by Claude, 2026-10-15):** a `Worker` interface (run a task, send a
  follow-up) with two implementations: the in-process worker (today's `ChatSession` and its config) and
  an MCP worker. The local worker is then just one worker in the pool, and the orchestrator can be built
  and tested with it before any MCP code. `-mode worker` serves that same in-process worker over MCP.
- **Decentralized mode is deferred** (Jerry, 2026-10-15): the architecture needs research first.
- **A shared board for worker-to-worker messages: wanted, design proposed (Claude, 2026-10-15).**
  - The orchestrator owns the board and serves it as MCP tools (`board_post`, `board_read`). Workers
    already have an MCP client, so each worker connects to it like any other MCP server.
  - Entries are append-only and structured: id, author, task, topic, text, time. They are kept in
    memory and written to a JSONL file, for debugging and for resuming a run.
  - Not a file that agents edit directly: that only works for local workers, needs file locking, and
    one bad write by an agent corrupts the board.
- **Open:** a shared filesystem across parallel workers (code, documents, data).


### Step 2 MVP: answers decided 2026-10-15

- **Built in `mvp4/`,** a fresh copy of `mvp3/` (module `.../minimal-agent/mvp4`, copied 2026-10-15 without run
  logs, scratch dirs or prompts). mvp3 stays the frozen autonomous-mode reference.
- **The seam:** `runTask`'s `resetContext(worker)` + `runTurn(worker, prompt)` go behind a `Worker` interface,
  with an in-process worker first and an MCP worker (`-mode worker`) second. Everything else (director,
  phases, control tools, rounds, report) stays.
- **Dependencies:** `set_tasks` takes `depends_on` and a worker type per task now. Go checks that the IDs
  exist and that there are no cycles. Tasks still run one at a time, in dependency order.
- **`TaskManager` owns the task list** (ID, depends on, worker type, worker ID, status, history, notes).
  `director.Plan` keeps only the phase and the review state.
- **Task IDs are short strings chosen by the director** (`"t1"`), not list indexes, so dependencies and
  follow-ups refer to tasks reliably.
- **One shared worker config** for all workers in the MVP.
- **stdio only** for the MVP. HTTP waits: auth, and a worker with `run_command` behind a port is a remote shell.
- **Each MCP worker call has a 10-minute timeout**; the run's `-timeout` still applies on top.
- **No shared board in the MVP.** The TaskManager `notes` field, written by the director and pushed into
  task prompts, is enough until there's a clear need.
- **No token/cost field in the worker reply yet;** maybe later.
- **Worker protocol:** MCP tools `run_task {task_id, prompt}` (first call for an ID starts a fresh worker
  conversation; later calls are follow-ups) and `end_task {task_id}`. The result's `structuredContent` is
  `{status, reply, error, tools_called}`. The director sees mvp3's `WrapTurn` text plus a `Status:` line.
- **Go sets `status`, not the worker LLM** (Jerry, 2026-10-15): `completed` / `step_limit` / `error`, derived
  from how `runTurn` ended. Always present, needs no LLM cooperation, and the orchestrator can act on it
  (retry, fail after N errors). A worker's question is reply text the director reads. Rejected for now: a
  worker `report(status, message)` tool (small models forget to call it, it duplicates the director's
  judgment); revisit if Go code must route questions without the director.
- **No third config block (Jerry: "config sprawl"):** in orchestrator mode the top-level config *is* the
  worker config, as the turn LLM already was the worker in autonomous mode. The orchestrator starts
  `<same binary> -mode worker -config <same file> -workdir <same dir>`, forwarding worker flags
  (`-model`, `-maxsteps`); the child ignores `"director"`. Downside: the top-level config configures nothing in
  the parent process, so the start banner must say where the worker runs. Named worker types (overlays of
  the top-level part) are deferred until a second type is needed.


### mvp4 reference build: orchestrator mode, slices A–C (2026-10-15)

Built by Claude in `mvp4/` from `PLAN-orchestrator-mode-serial.md`, at Jerry's request. Choices made
while building, beyond the answers above:

1. **Packages:** `worker/` (interface, `Result`, `Info`, protocol types, `MCPWorker`) and `tasks/` (`tasks.Manager`
   is the TaskManager). Both are capability packages; `director` imports them, `agent` wires them.
2. **A third MCP tool, `worker_info`** (model + tool names). In orchestrator mode the parent never sets up the
   worker's tools, so the director's prompt gets them from the child.
3. **`localWorker` holds one conversation:** a new task ID clears the context, `EndTask` (deferred in `runTask`)
   clears it too. Round 2 reusing `t1` is safe because of that `EndTask`.
4. **`RunTurn`'s error becomes an observation** (`status: error`) unless the run itself was cancelled; the
   per-task turn limit bounds a dead worker.
5. **stdout in `-mode worker`:** `main` points `os.Stdout` at stderr before loading the config and keeps the real
   stdout in `cfg.ProtocolOut`. So `-mode worker` must be a flag; `"agentMode": "worker"` in a file is refused (exit 2).
6. **Child flags come from the merged config** (`-model`, `-maxsteps`, `-workdir`, `-config`, `-debug`), not by
   re-forwarding the parent's argv.
7. **`set_tasks` takes objects only** (`id` and `task` required). No plain-string fallback: simpler, at the cost
   of small directors needing to get the shape right (the error says what to fix).
8. **Task status `running`** replaces mvp3's computed `[current]` in the director's task list.
9. **Ctrl+C:** the child ignores SIGINT (`main` registers it); the parent cancels the MCP call (the SDK sends
   `notifications/cancelled`) and closing the session ends the child. Not yet checked by hand with a real Ctrl+C.
10. **Verified:** `go test -race ./...`; a raw JSON-RPC check that `-mode worker` writes only JSON-RPC to stdout
    (a config warning went to stderr); a full `-mode orchestrator` run against a scripted fake OpenAI-compatible
    server (planning with `depends_on` reversed in the list, two tasks, review, exit 0, child exited). Seen in the
    raw check: at end of input the server exits without answering requests still queued; harmless for the
    orchestrator, which waits for each reply.


### Slice D (parallel tasks): answers decided 2026-10-16

Plan: `PLAN-orchestrator-mode-parallel.md`. Jerry's answers to Claude's open questions:

1. **One director conversation per running task** (Claude's recommendation; Jerry asked how they're coordinated).
   The LLM API is stateless: every request carries its whole conversation, so N conversations are just N
   independent requests to the same model endpoint and need no coordination in our code. They coordinate only
   through shared Go state (the task list, status and notes, under a mutex), which is rebuilt into each
   director's system prompt every turn. Downside: N× director cost, and a director doesn't see a sibling
   task's live progress, only its status and notes. Rejected: one director in an event loop with a
   `send_to_worker(task_id, …)` tool (bigger rewrite; one context mixing several tasks).
2. **One shared workdir** for the MVP. The planning prompt tells the director to split tasks by file and use
   `depends_on` for anything shared. Downside: races on shared files (`go.mod`) and concurrent builds. Declared
   write sets + locks (2026-10-15 note above) or a worktree per task stay later options.
3. **A failed task drains the round:** no new tasks start, running ones finish, then review. Rejected:
   cancelling the siblings (cheaper, but throws away nearly-finished work).
4. **Worker pool = N `-mode worker` child processes, each running one task at a time.** `localWorker` is
   unchanged. Downside: N processes, each starting its own MCP tool servers.
5. **Parallel only in orchestrator mode.** Autonomous mode stays "a director plus the in-process worker";
   `maxParallel > 1` there is refused.
6. **Shared board stays deferred.**
7. **Built in `mvp4/`,** not a new copy: `maxParallel: 1` behaves exactly like the serial MVP.
8. **Rate limits (429):** observations for now; retries with backoff later.
9. **The director plans for the parallelism it has** (Jerry): with N > 1 workers, a director may shape the task
   list differently (more independent tasks, fewer chains), so the PLAN and gap-planning prompts and the
   `## Worker` section say how many tasks can run at once and that tasks without `depends_on` between them
   may run at the same time in the shared workdir.

**Second round (Jerry: "These five are fine", 2026-10-16):** Claude's recommendations from the plan's § 4:

10. **Tasks given in `## Tasks` are chained** (each depends on the one before) when `maxParallel > 1`; with 1
    they stay unchained, so the serial prompts and report are unchanged. A hand-written list can't run in
    parallel yet.
11. **One lock** for the plan and its tasks (in `director.Plan`), never held during an LLM or worker call.
12. **All N workers start with the run**, so a child that can't start fails the run before any work.
13. **Hard errors drain too** (see "As built", item c, for how this ended up).
14. **The name is `director.maxParallel`** (a maximum), with a `-maxparallel` flag.

**As built (Claude, 2026-10-16), beyond the answers above:**

a. **Plan still owns the transitions:** `StartNext` returns nil when nothing can start (none ready, a task
   failed, or the phase is over); `TaskDone` / `TaskFailed` act on a `TaskRun` (the per-task state: task,
   worker turns left, the once-per-task `task_failed` refusal) and move to REVIEW only when no task is
   running and none will start. So `Plan.Current` is gone, and the tools' schema didn't change.
b. **A director conversation per task** (`newDirector`): its own memory and context, the shared provider. PLAN
   and REVIEW keep `r.dir`.
c. **Hard errors (a failed director turn) stop new tasks, wait for the running ones, then end the round
   without a review**, as in the serial run. This differs from the plan's "drain, then review": a review after
   a failed director turn (often a 401 or an outage) would usually fail too, and it keeps `maxParallel: 1`
   identical to slices A–C. Easy to change if a real run shows otherwise.
d. **Output:** a `lineWriter` per task prefixes `[t3 w2] ` and writes whole lines under one shared mutex; each
   child's stderr goes through one with `[w2] ` (only when N > 1). Serial output is unchanged.
e. **Report:** `PeakTasks` ("up to 2 tasks at once", shown when > 1). The "TASK t3 (3/5)" counter is taken when
   the task starts.
f. **Verified:** `go test -race ./...` (all earlier tests unchanged = the serial regression check); new
   `agent/parallel_test.go` (two tasks provably overlap, a dependent task waits, drain after a failure,
   Ctrl+C ends all tasks, `lineWriter`), director drain and prompt tests, config tests; parallel tests
   repeated 20× under `-race`. A full `-mode orchestrator -maxparallel 2` run against a threaded fake server:
   t1 and t2 overlapped on w1/w2, t3 waited for both, exit 0, both children exited. `-mode autonomous
   -maxparallel 2` is refused (exit 2). Not yet: a real-provider parallel run.


### Slice E (worker types): decided and built 2026-10-16

Plan: `PLAN-orchestrator-worker-types.md`. Jerry asked for worker types such as a "software engineer" and a
"code test and review" agent, with eager start first ("Let's build the worker types with eager start first"),
and asked how a type could differ by its prompt alone.

1. **A `"workers"` list in the main config:** `{type, count, config, description}`; the first entry is the
   default type. A list, not a map, so the default is well defined (JSON maps have no order).
2. **A worker type's `config` is a partial file applied over the top-level config** (Claude's answer to "how do
   I just use different worker prompts"): `{"systemPrompt": "You are a tester..."}` is a whole type. The run's
   keys are refused there; `builtinTools` and `mcpServers` replace rather than merge (merging would make
   "fewer tools" impossible). Its path is relative to the main config file. Rejected: full config files per
   type (every shared setting copied N times) and inline overrides in the main file (Jerry pictured separate
   files; easy to add later).
3. **Children get `-config <main> -workerconfig <type file>`;** the parent applies the same file to compute
   each type's `-model` / `-maxsteps` flags, so a type's `model` wins over the parent's `-model`.
4. **Eager start:** every worker of every type starts with the run (`count` is how many, not a maximum).
   Lazy start is the planned follow-up (Claude's suggestion: one per type at setup, more on demand).
5. **`description` is shown to the director;** the worker's system prompt is not (it can be long, and the
   director only needs to know what the type is for).
6. **`"workers"` replaces `director.maxParallel`:** setting both (`maxParallel > 1`) is a config error; tasks at
   once = the sum of the counts. `"workers"` in autonomous mode is refused, like `maxParallel > 1`.
7. **Scheduling:** one idle pool per type; the first ready task whose type has a free worker starts
   (`tasks.Manager.NextFor`). Worker IDs are `<type><n>` (`engineer2`) with a `"workers"` section, `w<n>` without.
8. **Verified:** `go test -race ./...` (earlier tests unchanged), new config tests (partial files, refused and
   unknown keys, path resolution, `-workerconfig`), director tests (unknown type, a ready tester task starts
   while engineers are busy, the prompt's type list), `Test_Parallel_WorkerTypes` (two engineer tasks never
   share the one engineer, the tester runs beside them). A fake-server run with `engineer ×2` + `tester ×1`:
   the tester child's requests carried the tester system prompt from its file, the engineers' the top-level
   one; exit 0, all children exited. Example: `mvp4/examples/config.worker-types.json` with
   `worker-engineer.json` and `worker-tester.json`.
9. **First real-provider run (2026-10-16, Jerry):** `config.worker-types.json` (engineer ×4 + tester ×2,
   gpt-5-mini workers, claude-sonnet-5-5 director) on `examples/objective-build-minisearch.md`, result in
   `mvp4/scratch-test-13/`, log `mvp4/run-minisearch-20261016-1046.log`. Finished with success in one round,
   in about 11 minutes. There were 14 tasks (9 engineer, 5 tester) and 17 worker turns, with up to 5 tasks
   at once.
   - **Typing was right.** Every implementation task went to an engineer and every test task to a tester. Each
     tester task started as soon as its own implementation finished. The 2 testers were the bottleneck.
   - **What it shows and what it doesn't.** The objective itself asked for implement-then-verify pairs and
     separate verifier agents, so the run shows the director *following* such a structure with types. It
     doesn't show the director inventing one. The four packages didn't overlap, so the shared workdir never
     clashed. Nothing failed and no tester reported a bug, so drain, retries and "fix it in a later engineer
     task" were not exercised against a real model.
   - **Smaller observations:** the director sent the README task back twice because its examples didn't match
     the real CLI output, which is review working as designed. The README went to an engineer because there
     was no docs type.
   - **The `count` question (Jerry, 2026-10-16):** with `"workers"`, `maxParallel` is implied (the sum of the
     counts) and is left out. The sum is a ceiling: dependencies keep the real peak lower (5 of 6 here).

Slice D also had its first real-provider runs (Jerry, 2026-10-16): `objective-build-backup-archiver.md` with
`maxParallel` built a working `dedupbackup` CLI in `mvp4/scratch-test-12/`. The finding from that run: the
objective didn't ask for a README or usage examples, so none were written. It is worth adding a "README with
example commands" requirement to objectives (not yet done for that file).


### Porting autonomous / orchestrator mode into the root code (decided 2026-10-16)

Jerry, at the end of the slice E session: "Next up is to incorporate these into the main codebase, using mvp3/
and mvp4/ as good templates and AI as a pair programmer / thought partner / teacher."

- **mvp3/ and mvp4/ are reading templates, not code to copy.** This works the way mvp1 preceded mvp2. Jerry
  builds the root version himself, with the usual style: concepts, then small examples, then a TODO skeleton he
  fills in (`SESSION.md § How to work in this session`).
- **Suggested order (Claude's, open to change):**
  1. autonomous mode (mvp3: `tasks/`, `director/`, the round loop in `agent/autonomous.go`);
  2. the `Worker` interface with a local worker;
  3. `-mode worker` over MCP plus `-mode orchestrator`, serial (slices A–C);
  4. parallel tasks (D);
  5. worker types (E).

  Each step runs on its own before the next, so each one is a working checkpoint. The honest downside: it is
  slower than porting mvp4 in one go, and some steps get reworked (e.g. `Plan`'s per-task state moves into
  `TaskRun` in step 4). That rework is the lesson, since it shows why the mvp4 shape exists.
- **Where the mvp4 design is still open, decide it during the port, not before:** lazy worker start, retries
  on 429, isolation (a worktree per task), the shared board, reviewer types without write tools.


### Build order: tracing core and approval before the autonomous port (decided 2026-10-18)

Revises the 2026-10-08 plan in `SESSION.md` ("concurrent MCP connection setup → hook interface + tracing hook →
sandboxed shell → headless mode"). Only the **order** is decided here; the tracing design itself is still a
draft in `PLAN-tracing.md` and gets its own entries when it is settled.

1. **Tracing core, in chat mode** (`PLAN-tracing.md` §13 slices 1–4): event format + testdata (including one
   hand-written multi-agent example, to check the format can express director → worker runs before that code
   exists), recorder + JSONL file, `standard` redaction **before anything reaches a log file**, hash chain.
   A first digest (`tracecat`) is optional and kept simple: only if reading raw JSONL gets tedious.
2. **Hook interface with an approval hook.** Called in `runToolCall` after the tool lookup and argument check,
   before `callToolSafely`; a denial becomes an error observation. Produces the plan's `tool_decided` events.
3. **`run_command` tool, ported from mvp4** (`mvp4/tools/builtin/runcommand.go`), with the program list made
   configurable. **Replaces the "sandboxed shell" step**, which Jerry judged too hard for now.
4. **The autonomous / orchestrator port**, in the order of `§ Porting autonomous / orchestrator mode` above.
   Trace events for tasks are added as that code is written; multi-agent tracing (per-worker trace files, the
   director's span ID passed to workers, merging) arrives with orchestrator mode.

- **Why tracing first:** autonomous and orchestrator runs are long, unattended and parallel, and the mvp4 text
  logs (`mvp4/runs/logs/`) are already hard to follow. With the recorder in place, new code gets its recording
  calls as it is written instead of being retrofitted. Downside: autonomous mode starts later, and some
  multi-agent tracing work waits for the orchestrator anyway.
- **Why approval before the port:** autonomous workers run tools unattended (mvp4 prints a warning saying so);
  an approval or policy hook is the main control over that until real isolation exists.
- **Why `run_command` instead of a sandbox:** mvp4's tool already has no shell, an allowlist of programs, a
  filtered environment, a timeout, an output cap and a process-group kill. **Honest downside, unchanged from
  the mvp3 entry: it is code execution, not a sandbox** (`go run`/`python3` run model-written code with the
  user's permissions). The approval hook is what limits it for now. A real sandbox (container/VM runner behind
  the same tool name) stays deferred.
- **"Headless mode"** in the 2026-10-08 plan is the old name for autonomous mode (renamed 2026-10-13), so it
  is step 4, not a separate step.
- **Unchanged:** concurrent MCP connection setup and the small fixes in `SESSION.md` can go before step 1 or
  between steps; they don't depend on this order.


### Tracing: event envelope, state-change capture, redaction shape (decided 2026-10-19)

> Partly superseded 2026-10-19 by "Tracing: version moves to run_start; ts is a time.Time" (end of this log): `v` is no longer on every event.
> Also partly superseded 2026-10-21 by "Tracing: recorder pipeline, redaction interface, JSONL file, task scope" (end of this log): the envelope field names changed (`run`/`turn`/`seq`/`kind`/`data` became `run_id`/`task_id`/`counter`/`event_type`/`event_data`), and a detector is now an interface, not just data.

Settled in discussion with Jerry before slice 2 of `PLAN-tracing.md` §13. The plan stays the long-form
reference; this entry records what was agreed. Where it differs from the plan, this entry wins.

**Event envelope** (every event; `v` stays 1 while fields are only added):

- `v`, `ts` (RFC 3339 with nanoseconds), `run`, `turn` (chat: turn ID; autonomous: task ID; may be empty for
  `run_*` and background events such as auto-compaction), `seq`, `kind`, `data`. Slice 3 adds `prev`.
- **No per-step ID** (too granular). **No `span`/`parent` yet**: adding fields later doesn't bump `v`, and
  nothing needs them before subagents / mvp4.
- **No per-event PID.** The PID (plus ppid, hostname, binary version, git commit) goes in `run_start` only. It
  is constant within a run, and PIDs get reused. Multi-agent linking will use a `parent_run` ID passed to
  workers at startup, not PIDs.
- **Event hash = hash chain, not a hash inside the event.** An event can't contain a hash of its own bytes, so
  the hash of event N is stored in event N+1 as `prev` (slice 3). `seq` is the per-file line counter.
- `redactions` (detector names applied) on the envelope, so a reader can tell text was altered.
- **Tool events carry `call_id` and `verified` inside `data`**, not on the envelope. `call_id` pairs
  `tool_requested` with `tool_finished` (needed once calls run in parallel). `verified:false` means the outcome
  is only what the tool *claimed* (typical for third-party MCP tools that return just "done").
- New kind `model_error` for LLM call failures. `model_response` carries `reasoning` from day one (empty
  until the provider adapters fill it; needs `ChatMessage.Reasoning` and token usage, see the plan §5.4).

**State-change capture (effects)**:

- Remote state changes (case 1): the burden is on the tool's response (success/failure, rows, table); the
  trace records the response and marks `verified:false` when the tool gives nothing structured.
- Local state changes (case 2): before/after snapshots behind a small `Snapshotter` interface so specialised
  versions (a real diff for a coding agent) can be added later. First implementation: SHA-256.
- Tools that name their files get per-file hashes; **`run_command` and anything unspecific gets a hash of the
  working directory before and after.** Directory hash: walk, skip hidden config dirs (`.git`, …) and
  symlinks, hash each file, list `path mode filehash` lines sorted by path, hash the list.
- Hash before and after **even when the call errors**. Absent file (not yet created / deleted): the literal
  string `"absent"`, **not** the hash of empty content, which would collide with a real empty file.
- **Deny-listed files are never hashed** (a hash of a low-entropy secret file can be guessed offline); the
  directory hash excludes them too, using `read_file`'s deny list. Downside: a change to `.env` goes unseen.
- Concurrent writes to one path: tool calls currently run sequentially, so no problem today. When they go
  concurrent, built-in write tools take an in-process per-path mutex around hash → write → hash. OS file
  locks are advisory only, and don't cover shell commands or other processes.
- **Snapshots are deferred out of slice 2** (plan slices 8–9). Reviewers rarely read hashes; they matter for
  claim-vs-evidence checks (L2) and for catching shell side effects, so they wait until a digester exists.
  Slice 2's `tool_requested` / `tool_finished` already show what each call asked for and returned.

**Redaction shape** (slice 2, `standard` level first):

- A detector is data (`name`, pattern or field-name rule); a level is a named list of detectors; `extraPatterns`
  and `allow` are list operations. `stringent`/`metadata` are later entries in the same table, no refactor.
- The redactor walks the event's `data` as generic JSON (decode to `any`, redact every string, re-encode), so a
  new event kind or field is redacted by default. Honest downside: a decode/encode per event. Bonus: re-encoded
  maps have sorted keys, which keeps the bytes stable for the hash chain.
- Fail closed: an unknown level is a config error; a redactor failure drops the payload, never writes it raw.
- Placeholder `[REDACTED:name#tag]`, tag = HMAC under a random per-run key.
- The "contents of deny-listed files" detector from the plan is dropped: `read_file` already refuses them.

### Tracing: version moves to run_start; ts is a time.Time (decided 2026-10-19)

> Partly superseded 2026-10-21: `Data` is now `EventData json.RawMessage` (the recorder snapshots the payload in `Record`, so the "must not be mutated afterwards" rule is enforced, not hoped for), and the open `turn` -> `task` question is settled: `task`. See "Tracing: recorder pipeline, redaction interface, JSONL file, task scope" at the end of this log.

- **`v` is removed from the event envelope.** The format version is written once, in the `run_start`
  payload (`trace.Version`). One process writes one file with one version, so a per-line copy is redundant.
  Honest downside: a single line pasted or grepped out of a file no longer says which format it is; a reader
  must look at the file's first line. Readers stay tolerant; removing or renaming a field still bumps the version.
- **`ts` is a `time.Time`**, created with `time.Now().UTC()` at emit time (not in a writer goroutine).
  `encoding/json` writes it as RFC 3339 with nanoseconds. Downside: `RFC3339Nano` trims trailing zeros, so
  timestamps don't sort correctly as plain text; order by `seq`.
- **`Data` is `any`**: the recorder marshals it, so callers pass plain Go values. If marshalling fails (e.g.
  invalid `json.RawMessage` tool args, NaN), the recorder writes `{"marshal_error": "..."}` instead of the payload.
  Values handed to the recorder must not be mutated afterwards (a later async writer would see the change).
- Open: whether `turn` is renamed `task` (and `turn_start/end` to `task_start/end`).

### Size caps: trace events, read_file, MCP results (decided 2026-10-21)

Prompted by "what if a malicious tool returns 10 TB?". Three caps now exist, all config fields with defaults in
`GetDefaultConfig`, all required to be positive (a negative cap would panic when slicing):

| Config field | Default | What it bounds |
|---|---|---|
| `traceMaxEventBytes` | 100 MB | JSON size of one trace event payload. Over the cap, the whole payload is replaced by `{"truncated": true, "original_bytes": N}`. Never cut: a cut can split a secret in half (the redactor stops matching it and the rest is written raw) and leaves invalid JSON. |
| `readFileMaxBytes` | 1 MB (~250k tokens) | Most one `read_file` call returns. A bigger file is read in pieces: `read_file` takes `offset` and `limit` (bytes), and the truncation note says `call read_file again with offset=N to continue`. Pieces are cut on character boundaries. |
| `mcpResultMaxBytes` | 256 KB | Most of an MCP tool result passed to the model; the rest is cut with a note. |

- **The tool caps protect the context window, not only memory.** A tool result goes into chat history and is sent
  to the model on the next request. 50 MB is ~12M tokens, more than any context window. Context windows trim by
  message count, not tokens, so one oversized result would stay in history and fail every later request (the
  "same bad context is re-sent forever" failure). That is why the caps were *not* raised to 50 MB / 10 MB, and why
  big files are paged instead.
- **The trace cap is a last resort.** `json.Marshal` cannot stop early, so by the time `Record` can measure a
  payload it is already in memory. The cap bounds the later copies (the redaction tree can be 10-20x the JSON size,
  the 256-event queue, the disk). Worst case is 256 x 100 MB if nothing upstream caps; the tool caps are what keep
  real payloads far below that. 100 MB was chosen by Jerry over my suggested 1-4 MiB; lowering it is a config edit.
- Honest downsides: a legitimately huge payload loses its content in the trace (the event and its size remain);
  1 MB per `read_file` call can fill the window of a small-context model, so lower `readFileMaxBytes` for those.

**Deferred: client-side limit on raw server responses** (added to the "Deferred for Later" list, 2026-10-21).
`mcpResultMaxBytes` is applied *after* the whole response is in memory, so it does not protect memory:

```
server -> [transport read] -> SDK decodes JSON -> CallToolResult -> flattenMCPResult -> [mcpResultMaxBytes] -> model
          ^ deferred hard limit                  (all in memory)                          ^ exists today (256 KB)
```

- Two different jobs. The transport limit is generous (order of 10 MB) and exists for protection: it stops reading
  the bytes once the threshold is crossed and the call fails with an error that the loop turns into an ordinary
  tool-error observation. `mcpResultMaxBytes` is small and exists to protect the context window. A legitimate 3 MB
  response passes the transport limit and is then trimmed to 256 KB; a hostile endless stream is cut at 10 MB.
- Checked 2026-10-21: go-sdk v1.8.0 has response-size limits only on its *server* side (`MaxRequestBodyBytes`),
  none on the client. We would add our own, once per transport:
  1. **HTTP:** one more `RoundTripper` layer next to `newRemoteHTTPClient` / `headerTransport`, wrapping `resp.Body`
     in a reader that returns an error after N bytes (about 15 lines).
  2. **stdio:** wrap the child process's stdout pipe in the same limited reader where `conn.go` creates the
     connection. Not yet looked at how that code is shaped.
- Honest downside: when the limit is hit mid-message the protocol framing is broken, so the clean handling is to
  treat the session as dead and reconnect. That is a bigger change than the cap itself and needs a decision on
  reconnect behavior, which is why both are deferred. Only a hostile or buggy server can trigger it.
- Also unchecked: whether the model provider's HTTP response body (`model/`) has a size cap.

### Tracing: recorder pipeline, redaction interface, JSONL file, task scope (decided 2026-10-20 to 2026-10-21)

Built and tested (milestone 1: `go run .` then `/quit` writes `.agent-trace/<run_id>.jsonl` with `run_start`
and `run_end`). Recorded here because several choices changed or were made after the earlier tracing entries.

**Workflow: build top-down.** Jerry's request (2026-10-20): start each slice at the call site in the loop, with the
lower layers as stubs, and open each slice with a short diagram of where it sits in the whole flow. A tree that
doesn't compile for a while is acceptable. Reason: he learns the code by seeing how each struct and function ties
into the main loop. (Also saved in Claude's memory as `feedback-learning-mode`.)

**Envelope and payloads as built** (`trace/event.go`).
- Envelope: `ts` (UTC `time.Time`), `run_id`, `task_id` (omitted when empty), `counter`, `event_type`,
  `event_data`, `redactions`. The `turn` question is settled: it is `task`. Chat: one user message through the
  final answer is one task. Autonomous: one `objective.md` task is one task.
- `EventData` is a `json.RawMessage`, and `Record` takes the payload as `any` (typed payload structs). Reason: the
  redactor has to marshal the payload to search it anyway, so the event holds bytes from the start.
- Built payloads: `RunStartData`, `RunEndData`, `MessageRecord`, `ModelRequestData`, `ModelResponseData`,
  `ModelErrorData`. Not yet defined: tool, task and compaction payloads.
- **`ModelRequestData` holds only the messages added since the previous request** (plus `ContextMessages`, the
  count sent). Chosen by Jerry for audit usefulness without re-logging the whole context every step. Honest
  downside: to see the exact context of one request, a reader replays the earlier requests (and compaction events).
- **`run_start` contents:** full `SystemPrompt` (it goes through redaction like any string), `McpServers` as
  name -> transport type only (a server's command, args, env, url and headers hold tokens), `GitCommit` from
  `debug.ReadBuildInfo` (hash, with `+dirty` if the tree had uncommitted changes; empty under `go run`),
  `ParentPID` is only the launching process. Built by `trace.GetRunStartData(cfg)`; `trace.GetRunEndData` builds
  `run_end` (`status` "ok"/"error", `error`, `duration_ms`).

**Recorder pipeline** (`trace/recorder.go`).
- `Record` runs on the loop's goroutine and does only the cheap work: stamp, snapshot, size cap, queue. Then one
  **tracewriter goroutine** takes events off a queue of 256 and does redact -> number -> marshal -> subscribers.
- **Snapshot:** `Record` calls `json.Marshal` on the payload, so the queued event owns its bytes. Without it, a
  slice or map that the loop changes after `Record` would be read changed by the tracewriter (wrong audit data),
  and a map read during a write is a fatal, unrecoverable Go error. Cost: one extra marshal and copy per event.
  A payload that can't be marshalled becomes `{"marshal_error": "..."}`; the event is kept.
- **Backpressure, not dropping:** when the queue is full, `Record` blocks. Honest downside: a slow disk can stall
  the agent. Chosen because a dropped event is a hole in an audit log.
- **Close:** the closed flag and the channel send share one mutex, because sending on a closed channel panics.
  `Close` closes the channel, waits for the tracewriter to drain it (`<-done`), closes the subscribers once
  (`sync.Once`), and returns the first error. It is safe to call twice; a `Record` after `Close` is dropped.
- **Counter** is assigned by the tracewriter, and only after the line is built, so the file never has a gap (the
  hash chain in slice 3 needs that). Order in the file is queue order.
- **Fail closed:** if redaction fails, the payload is replaced by `{"dropped":"redaction failed"}` and the error
  is kept for `Close`. A panic anywhere under the tracewriter (including a subscriber) is recovered, reported with
  its stack trace, and does not crash the agent. Accepted weakness: a subscriber that panics skips the later
  subscribers for that event.
- **Disabling and setup:** `traceEnabled: false` gives the loop `trace.Nop()`, so the loop has no nil checks.
  `SetupRecorder(ctx, cfg)` turns config into pieces and `NewRecorder(runID, redactor, maxEventBytes, subs...)`
  takes the pieces. Config errors are fatal at startup: unknown `redactionLevel`, non-positive
  `traceMaxEventBytes`, empty or uncreatable `traceDir`. Reason: silently running without an audit trail is worse
  than failing to start.
- Size caps are in "Size caps: trace events, read_file, MCP results" (above).

**JSONL file subscriber** (`trace/subscriber.go`).
- Path `<traceDir>/<run_id>.jsonl`; directory `0700`, file `0600`. The file is opened with `O_EXCL`, so a run
  never reuses another run's file and never follows a planted symlink; the run ID must not contain a path.
- One unbuffered `Write` per line (line plus `\n`), so a crash loses at most the event being written. `Sync` runs
  at `Close`, not per event. Honest downside: a power cut can lose the last few events.
- The run ID is made once in `SetupRecorder` (`20261021T101500Z-9f3a41c7`) and used for both the file name and
  every event's `run_id`.
- Default `traceDir` is `.agent-trace`; it is in `.gitignore` and in `read_file`'s deny list, so the model can't
  read its own audit log. Honest gap: the deny list holds only that default name, so a different `traceDir` inside
  `workDir` is not protected. An existing directory keeps its permissions (`MkdirAll` doesn't change them); the
  file itself is still `0600`.

**Redaction as built** (supersedes "a detector is data").
- `Detector` is an interface (`Name()` for the engine, `Find(key, s) []Finding`); `Finding{Rule, Secret}` prints
  without the secret. The built-in detector applies 14 rules: value-shape patterns (provider keys, AWS, GitHub,
  JWT, Bearer, PEM, URL credentials, `key=value`) and one field-name rule. `Redactor` holds a slice of detectors so
  another engine can run alongside. The two tiers (standard at L0, stringent in the digest later) are two
  `Redactor` instances; the second skips text that already starts with `[REDACTED:`.
- Placeholder `[REDACTED:rule#8hex]`; the tag is an HMAC of the secret under a random per-run key, so the same
  secret has the same tag within a run.
- **Field-name rule:** the secret word (`pass`, `password`, `pw`, `secret`, `token`, `key`, `credential(s)`,
  `auth`, `cookie`, plus `api_key`/`api_token`/`private_key`) must be the whole name, or follow `_ . -` or a
  camelCase boundary. So `max_tokens`, `input_tokens`, `compass` and `monkey` are not masked. A bare `key` in
  prose counts only with `=` or a quoted `"key":`. Honest downside: rules are regex heuristics, with false
  positives and misses; the list needs upkeep.
- **Gitleaks (checked in a scratch module):** usable as a library, but the import path is
  `github.com/zricethezav/gitleaks/v8` (v8.30.1); a test binary was 12 MB with 204 modules; tuned for scanners
  (entropy, stopwords), so low-entropy passwords slip through; goroutine-safety not verified. **TruffleHog**
  rejected (believed AGPL; verifies secrets live). Gitleaks stays an optional future detector in its own module.
- **Swappable detectors are deferred:** Go imports are compile-time. The plan is a registry plus a blank-import
  manifest (`plugins.go` in `main`, like `database/sql` drivers), built when the second engine arrives.

**Run, task and task ID.**
- A run is one process lifetime: `run_start` .. `run_end`. A crash leaves no `run_end`, so a reader treats the end
  of the file as an unclean stop.
- **`task_id` stays on the envelope** (not inside payloads): one field for every event, so a new payload type
  can't forget it and a reader can filter without knowing payload shapes; background auto-compaction can finish
  during a later task and is still attributed to the task that triggered it; parallel tasks interleave in the
  file. Jerry's challenge was that sequence plus payload IDs could do it; that works for serial work only.
- Planned: the chat session keeps a counter (`task-1`, `task-2`, ...); autonomous mode uses the objective's own
  task identity. Run-level events have an empty `task_id` (a `RunScope` constant was the alternative).
- **`run_end` status:** `runChatLoop` returns an error; `io.EOF` (Ctrl+D, end of piped input) and `/quit` are a
  normal quit (`ok`); a cancelled root context returns `ctx.Err()` (`error`). A context cancelled before
  `RunAgent` starts records neither event.

**Still open.**
- How `Record` learns the task: keep the `taskID` argument, or `Recorder.ForTask(id)` returning a task-scoped
  recorder so the call is `Record(eventType, data)` (my recommendation; not yet decided).
- `AgentVersion` comes from the config file, so a file can claim any version; make it a constant in code.
- Not built: task/model/tool/compaction events in the loop, hash chain (`counter` is ready for it), effects and
  snapshots, `ChatMessage.Reasoning` and usage in `model/`, L1 digest, detector registry, client-side limit on
  raw MCP responses (see the size-caps entry).

### Tracing: task IDs, traced slash commands, no span ID yet (decided 2026-10-21)

- **IDs are prefixed timestamps, not counters.** Run IDs are `r20261021T101500Z-9f3a41c7`, task IDs
  `t20261021T101530Z-5be0d2a1` (one `newID(prefix)` helper in `trace/recorder.go`). The random suffix is kept
  because parallel autonomous tasks can start in the same second, and it makes an ID greppable across trace
  files. Honest downside: harder to say aloud than `task-3`, and the ID repeats what `ts` already says. In
  autonomous mode the objective's own name goes into the `task_start` payload, since the ID no longer carries it.
  This supersedes the planned `task-N` counter above.
- **`task_start` / `task_end` are paired by a `defer`** declared right after `task_start`, reading a local
  status/err variable (`handleUserInput` returns a `bool`, so there is no named return to use).
- **Which slash commands are traced:** any that makes a model call or changes agent state or what the model sees:
  `/clear`, `/compact`, `/summary`, `/summarize`. Not traced: info-only commands (`/config`, unknown) and `/quit`
  (`run_end` covers it). Reason: without `/clear` and `/compact` a later `model_request` with fewer messages is
  unexplained, and their model calls need something to belong to.
- **Traced commands are bracketed by `command_start` / `command_end`, not `task_start` / `task_end`** (Jerry's
  call: they are not tasks), but they get a `t`-prefixed ID in the same `task_id` envelope field, "essentially a
  chat turn" (Jerry chose this over renaming the field to `scope_id`). Honest downside: the field name says
  "task" for something that isn't one; accepted. Payloads: `CommandStartData{Name, Args}`; `TaskEndData` is
  shared by `task_end` and `command_end`. This replaces my earlier proposal of an empty `task_id` for `/compact`.
- **No span/trace ID for pairing start and end (yet).** Each pair already has a natural key: run -> `run_id`;
  task and command -> `task_id`; tool -> `call_id`; model request/response/error -> `(task_id, step)` (the
  response and error payloads will carry `step`). A universal span ID with parent IDs (the OpenTelemetry way)
  would give explicit nesting at the cost of a field on every event and a bigger `Record` API. Revisit if nested
  or parallel spans of the same type appear within one task (subagents). Adding fields later is backwards
  compatible.
- **Gap found: model calls from `/compact`, `/summarize` and auto-compaction bypass `chatWithProgress`**
  (`contextwindow/msgcontext.go:222` calls `Provider.Chat` directly), so tracing only in the loop would miss
  them. Plan: a tracing wrapper around `model.Provider` that records `model_request` / `model_response` /
  `model_error`, so every caller is covered. Downsides: it sees the full request, so "new messages only" means
  diffing against message IDs already seen, and it doesn't know the step number. Decide when model events are built.
- **Still open:** `ForTask(id)` vs the `taskID` argument on `Record` (unanswered).

### Tracing: `trace.Scope` wraps start/end/fail for runs and tasks (decided 2026-10-21)

- **Problem:** `task_start`, the deferred `task_end` and the status bookkeeping were cluttering the agent loop, and
  one branch had already forgotten to set the status (Ctrl+C recorded `ok`). Also, `defer rec.Record(..., GetTaskEndData("ok", "", 0))`
  evaluates its arguments at the `defer` line, so the end event would always say `ok` with duration 0; a deferred
  closure (or a method on an object that holds the state) is required.
- **Decision:** `trace/scope.go`: `StartRun(rec, cfg)` and `StartTask(rec, input)` record the start event and return a
  `*Scope`; `defer scope.End()` records the end event; `scope.Fail(err)` sets the outcome (nil: no change;
  `context.Canceled`: `canceled`; anything else: `error`; the text is kept either way); `scope.Record(type, data)` records
  an event with the scope's `task_id`. Call sites: `handleUserInput` (`taskTrace`) and `RunAgent` (`runTrace`).
- **This resolves the `ForTask` question:** a `*Scope` is the task-scoped recorder, so the `Recorder` interface keeps
  `Record(taskID, ...)` and no tests changed. `reActLoop` and `runToolCall` will take the `*Scope` and call
  `scope.Record(...)`.
- **Behaviour change:** `run_end.status` can now be `canceled` (SIGTERM cancels the root context); it was `error`.
- **Honest downside:** one more type; what `End` writes is in `trace/`, not at the call site. `End` must be called
  once (nothing guards a second call). Not built yet: `StartCommand` for `/clear`, `/compact`, `/summary` (same type,
  `command_start`/`command_end`).

### Tracing: a run's scope ID is its run ID (decided 2026-10-21)

- **Decision:** `run_start` and `run_end` carry the run ID in `task_id` (same value as `run_id`), instead of an empty
  `task_id`. Every event now belongs to a scope; the `r` / `t` prefixes keep the two ID kinds apart. Supersedes
  "run-level events have an empty `task_id`" and the `RunScope` constant alternative above.
- **Why:** an empty value is also what a forgotten ID looks like, so "run-level on purpose" and "forgot to set it"
  were indistinguishable; readers can group by `task_id` without special-casing blanks; every `Scope` has a real ID.
- **How:** `Recorder` gained `RunID() string` (`Nop` returns ""); `StartRun` uses it as the scope ID. `Record`'s
  signature is unchanged.
- **Honest downside:** the value is duplicated on run events (about 30 bytes); `task_id` now holds run and command IDs
  as well as task IDs, so "task events" is a prefix match on `t`, not `task_id != ""`. Renaming the field to
  `scope_id` was offered again and is still open (Jerry kept `task_id` for commands).

### Tracing: envelope field renamed `task_id` -> `scope_id` (decided 2026-10-21)

- **Decision:** the envelope field is now `scope_id` (Go: `Event.ScopeID`; `Record(scopeID, eventType, data)`). It holds
  the ID of the unit of work an event belongs to: the run ID (`r…`) for run events, a task ID (`t…`) for tasks and
  for traced slash commands. Values are unchanged; only the label is. Supersedes the "keep `task_id`" choice in the
  entries above (the format was unreleased, so no trace-reader migration).
- **Why:** the field already held run and command IDs, so `task_id` was misleading; the name now matches `trace.Scope`.
- **Honest downside:** "task events" are found by the `t` prefix, not by the field name; older notes and the
  mvp3/mvp4 reference code still say `task_id` for their own, unrelated worker task IDs.

### Tracing: compaction event and the `compactAndTrace` helper (decided 2026-10-21)

- **Decision:** one `compaction` event per compaction, recorded by `compactAndTrace` (`agent/compaction.go`), which both
  `/compact` and the auto-compaction goroutine call. Payload `CompactionData{Trigger, Status, Error, MessagesBefore,
  MessagesAfter, SummaryID, DurationMS}`; recorded on failure and cancellation too. The summarize model call is a
  separate `model_request` / `model_response` pair (provider wrapper, not built yet).
- **Scope:** manual: the `/compact` command's own task ID (`taskTrace.ID()`); auto: the run ID, because no user task
  owns a background compaction. This replaces the earlier plan to attribute auto-compaction to the task that
  triggered it (that scope has already ended when the compaction starts). Honest downside: the field alone doesn't say
  which turn triggered it; the counter order does (it follows that `task_end`).
- **Single event, not start/end:** auto-compaction runs in a background goroutine and can overlap the next task, so
  a pair would interleave with unrelated events. Honest downside: a compaction that hangs leaves no event until it ends.
- **Also added:** `trace.OutcomeOf(err)` (shared by `Scope.Fail` and the helper) and `Scope.ID()`.
- **Known imprecision:** `messages_after` for an auto compaction is read when it finishes, so it can include messages
  a later turn added in the meantime. A skipped request ("compaction already in progress") records nothing.

### PR #19 review fixes: quoted credentials, placeholder lookalikes, MCP cap, run_end timing, read_file paging (decided 2026-10-22)

- **Quoted credentials (`key_value`, `key_assignment`):** the value is now matched as a complete double-quoted value, a
  complete single-quoted value (spaces and backslash-escaped characters allowed; runs to the end of the text if the closing
  quote is missing, so a cut-off value is still masked), then a bare token. Each alternative is its own capture group and
  `Find` takes the one that matched. Quoted values must be 4+ characters, like bare ones. Honest downside: a
  quoted value longer than 3 characters is masked everywhere it occurs in the same string (`ReplaceAll`), and a short
  quoted value (1-3 characters) is still not masked.
- **Placeholder exemption:** only a finding that is exactly one placeholder (`^\[REDACTED:[a-z0-9_]+#[0-9a-f]{8}\]$`) is
  skipped; a value that merely starts with `[REDACTED:` is masked in full. Idempotence still holds.
- **`mcpResultMaxBytes`:** a non-positive value is a fatal `SetupAgent` error. MCP tool setup only warns and skips a tool
  that fails to build, so a bad cap used to silently drop every MCP tool.
- **`run_end` timing:** `RunAgent` starts the run scope; `ShutdownAgent` ends it, after background auto-compaction has been
  cancelled (by `main`) and drained, and before the recorder is closed. Honest downside: a run's duration now includes
  shutdown, and `run_end` is only written if `ShutdownAgent` runs (a hard crash leaves none, as before).
- **`read_file` paging:** when a limit (or the configured cap) is smaller than the next character, trimming leaves nothing
  and the old note said "continue at offset=N" with N unchanged, forever. It is now an error naming the problem and telling
  the model to use a larger limit.
- **Credentials in tests:** gitleaks over the PR commits and the tree found only synthetic values (`hunter2xyz`,
  `abcd1234efgh`, repeated-character keys). The two PEM fixtures were changed from real PKCS#8 header bytes to base64 of
  "fake-key-body". gitleaks still flags 6 test lines (generic key patterns, a PEM header); they are the redactor's inputs
  and have to look like credentials. `mvp1/.env` holds real-format keys but is gitignored and was never committed.

### 2026-10-22 — Model calls are traced by a Provider wrapper (`trace.TracedProvider`)

- `trace.WrapProvider(p, rec, scopeID, cfg)` returns a `model.Provider` that records `model_request` before and `model_response` / `model_error` after each `Chat`, under one scope ID. `ChatSession.tracedProvider(scopeID)` is the one place the agent builds it. Three call sites use it: `reActLoop` (task scope, sets `Step` = 1-based step before each call), `/summary` (command's task scope), `compactAndTrace` (the command's scope for `/compact`, the run ID for auto-compaction). `chatWithProgress` and `reActLoop` take the provider / `*trace.Scope` as parameters.
- Payload builders `GetModelRequestData`, `GetModelResponseData`, `GetModelErrorData` live in `trace/event.go`, like the run and task ones.
- **`model_request` records the full message list**, not a diff. Self-contained events; downside: a K-step turn repeats the context K times (bounded by `traceMaxEventBytes`, which swaps an oversize payload for a marker). A diff against already-seen message IDs is possible later.
- **Not available yet:** `usage`, `stop_reason`, reasoning, and `http_status` (the `Provider` interface returns only a `ChatMessage` / plain error). The response message has no ID at record time (the loop assigns it afterwards), so a response is matched to its later appearance in a request by content and order.

### 2026-10-22 — Renames: `GetTracedProvider` and `getSessionProvider`

`trace.WrapProvider` is now `trace.GetTracedProvider` (matches the `Get…Data` builders) and `ChatSession.tracedProvider` is now `ChatSession.getSessionProvider`. Flow: a call site asks `cs.getSessionProvider(scopeID)` for the provider to use, which calls `trace.GetTracedProvider(...)` and returns a `*trace.TracedProvider`. Behaviour is unchanged; the entry above still uses the old names.

### 2026-10-22 — `model_request` records only what the turn added (supersedes "full message list" above)

The earlier entry recorded the full request with every call. Jerry: that would overwhelm the trace, so record only the newest messages of the turn.

- `ModelRequestData.Messages` holds the messages added since the previous request of the same turn: the user's message at step 1; the assistant reply and tool results at later steps. `ContextMessages` counts the leading messages that were sent but are not repeated (system prompt + prior context at step 1, everything earlier in the turn afterwards).
- Mechanism: `TracedProvider.Baseline` (set by `reActLoop` to `1 + len(priorMsgs)`) and an internal `prevLen`; the skip count for a call is `max(Baseline, prevLen)`. Calls outside the loop (`/summary`, compaction) leave `Baseline` 0 and record their whole request.
- **Downside:** the trace no longer holds the exact context the model saw. It must be rebuilt from earlier events (the system prompt from `run_start`, prior turns from their own events), and a compaction in between changes what "prior context" means. It relies on the loop only appending within a turn; a step that rewrote earlier messages would not show. `/summary` and compaction still record their whole transcript, which restates earlier messages; it is the one remaining big payload.

### 2026-10-22 — `model_request` bookkeeping simplified: the loop passes its turn messages (supersedes `Baseline` / `prevLen`)

Jerry: the `Baseline` + `prevLen` arithmetic was too much bookkeeping, since `reActLoop` already holds the turn's messages as `requestMsgs`. `TracedProvider` now has `TurnMsgs`, which the loop sets next to `Step` each step: `provider.Step, provider.TurnMsgs = stepNum+1, requestMsgs`. When set, `Chat` records `TurnMsgs` and counts the rest of the request as `ContextMessages`; when nil (`/summary`, compaction) it records the whole request. `GetModelRequestData(cfg, step, contextMessages, msgs, toolDefs)` records exactly the `msgs` it is given.

**Trade-off accepted:** `requestMsgs` is the whole turn so far, so step 2 repeats step 1's user message and step 3 repeats steps 1-2. The repetition is bounded by one turn. Everything else from the previous entry still holds (no exact context in the trace; `/summary` and compaction still record their whole transcript).

### 2026-10-22 — Tool calls are traced: `tool_requested` / `tool_finished`

- `runToolCall(ctx, cs, taskTrace, call)` is now the only place a tool call is recorded: `tool_requested` (call ID, name, arguments) before, `tool_finished` (call ID, name, content, failed, duration) after. The old body moved to `executeToolCall`, which returns `(string, error)`; `runToolCall` turns an error into the same `"error: …"` text as before, so what the model sees is unchanged. All outcomes (unknown tool, invalid JSON, non-object arguments, tool error, tool panic, success) go through the same two `Record` calls.
- `tool_requested` is recorded for **every** call the model makes, including ones that then fail validation.
- **`tool_finished.content` is what the agent sees**, not what the tool could have returned. `read_file` (1 MB) and MCP results (256 KB) are capped by the tools themselves and carry their own truncation note, so there is no separate `truncated` flag. Downside: if a cap is set too low, the trace cannot show what was cut. `traceMaxEventBytes` stays a backstop for tools with no cap of their own.
- `tool_requested` repeats the arguments already in `model_response`'s tool calls. Kept so the event stands alone and marks when execution started.
- **Parallel tools later:** `Scope.Record`/`ID` only read fields set at creation and the recorder has its own lock, so they are safe from several goroutines; `Fail`/`End` stay with the goroutine that owns the scope (tool goroutines must return their errors, not call `Fail`). Events of different calls will interleave; `call_id` pairs them. Results must still be appended in the model's order.

### 2026-10-22 — `command_start` / `command_end` dropped; slash commands stay tasks

Slash commands that call the model or change agent state (`/clear`, `/compact`, `/summary`, `/summarize`) are traced as tasks (`task_start` / `task_end`, command text as the input), as already decided. The unused `CommandStart` / `CommandEnd` event types and `CommandStartData` / `CommandEndData` were deleted, and `doc.go` now says so. Supersedes the "intended event types for commands" in the task-ID entry above. **Trade-off:** a reader tells a command from a chat turn by its input starting with `/`, not by event type; a `StartCommand` can be added later without touching the rest.

### 2026-10-22 — Hash chain (slice 3) deferred; threat model first

Jerry: an unkeyed per-line hash chain can be rewritten and recomputed in full by anyone who can write the file, including a malicious agent, so it adds little on its own. Nothing was implemented. The `prev_hash` / `Verify` design sketched in the session (hash of each raw line, first `prev` = sha256(run ID), verify walks the raw bytes) is still valid, but only worth building once there is something the agent cannot rewrite:
- an external `Subscriber` sending events or hashes to a sink the agent cannot write to;
- periodic (not only `run_end`) anchoring of the chain head outside the agent, e.g. the PR comment or a remote service;
- signing with a key held by a different process or machine (a key on the agent's machine adds nothing);
- OS-level separation of the trace file from the agent (other user, sandbox), including closing write paths such as `RunCommand`.
Currently in place: `0600` file in a `0700` directory, and `.agent-trace` on the `read_file` deny list (the model cannot read its own trace). Neither stops a same-user process from writing the file.

### 2026-10-22 — L1 digest: `Digester` interface, `ActivityDigest`, `digestEnabled`

- **Shape.** `trace.Digester` has `Add(Event)` and `Render(io.Writer)`; it sees only L0 events. `NewDigestSubscriber` is the live adapter (a `Subscriber`: decodes each line, calls `Add`, renders `<run_id>.digest.md` at `Close`, `0600`, create-only). `Replay(r, d)` feeds a finished `.jsonl` file to any `Digester`, so a digest can be rebuilt after a crash or with a newer implementation. `Replay` reads with `bufio.Reader.ReadBytes`, not `Scanner`, because one event can be as large as `traceMaxEventBytes`. Chosen over a model-based or file-only digest: live and offline from one reducer, and nothing ever reads more than the already-redacted events, so a digest cannot leak more than L0.
- **`ActivityDigest`** (first implementation; purpose: a quick human "what did the agent do and change" review). Per task: the user input as a heading, then one block of tool calls (`N tool calls:` + a line each, in finish order, failures marked `(failed: …)`), or for a text-only turn a one-liner (`answered, no tools (N chars)`). Built from `tool_requested` / `tool_finished` paired by `call_id`, never from model text. Calls requested but never finished are counted and flagged. Footer counts redactions, model errors and compactions. Grouped per task, not per step, because tool events carry no step number.
- **Describers.** `describe(name, args, content, failed)` is a per-tool switch: `read_file` → `read <path>`; `shell` / `run_command` / `bash` → the command verbatim (whitespace collapsed, cut at 300 chars; L0 keeps the original); everything else → name plus truncated arguments. Downside: a new tool shows the generic line until someone adds a case. `write_file` will need its result text to say `created` / `overwrote`, since the digest cannot infer "new file" from events.
- **Untrusted text.** User input, arguments and error text are collapsed to one line and cut, so they cannot start a new markdown heading or bullet in the digest.
- **Text-only turns: no model-written summary.** A `/summarize`-style call per run would cost a model call, make the digest non-deterministic, and put model prose into an audit document. A deterministic one-liner is used instead; an opt-in `LLMDigester` can be a second implementation of the same interface later.
- **Config.** `digestEnabled` (default `true`; only takes effect when `traceEnabled` is on). If the digest file can't be created, `SetupRecorder` fails (like the trace file) and closes the JSONL file it had already opened.
- **Known limits.** (1) The digest is written at `Close`; a crash leaves none until `Replay`. Rendering after every `task_end` would fix it at the cost of rewriting the file each time. (2) Once a shell tool exists, files changed through it are invisible to a tool-call digest; a separate workdir-diff `Digester` (e.g. `git status` at task end) would cover that. (3) `/clear` and the skipped "compaction already in progress" are not L0 events yet, so they are not in the digest. (4) The first user-visible gap: no `usage` or `stop_reason` in the footer until `Provider` reports them.
- Shared file-creation helper for the two subscribers deferred: see Deferred for Later, item 13.

### 2026-10-22 — `context_cleared` event; digest shows what slash commands did (corrects the digest entry above)

- **Correction:** the digest entry above said `/clear` was "not an L0 event". That was wrong: `/clear` was already traced as a task (`task_start` / `task_end`, input `/clear`). What was missing was its *effect*, so the digest showed "no tools, no reply" under it.
- **New event `context_cleared`** (`ContextClearedData{messages_dropped}`), recorded by `/clear` inside its own task, before `Clear()`, from the message list `handleUserInput` already read at its top. Chat history (the durable record) is not touched, and the event says nothing about it.
- **Digest rendering.** `ActivityDigest` prints a command task's effect under its heading: `context cleared (N messages)`, or for `/compact` `compaction ok: B -> A messages` (a `compaction` event whose scope is a known task). An auto compaction is recorded under the run ID, which is not a task, so it is only counted in the footer. Otherwise the digest would show a bogus task.
- **Not done (Jerry: ignore):** the skipped "compaction already in progress" is still unrecorded. The "Known limits" bullet (3) in the digest entry is therefore replaced by this one.

### 2026-10-22 — Digest: missing-payload markers are reported, not decoded (Copilot, PR #20)

When a payload is over `traceMaxEventBytes` the recorder writes `{"truncated":true,"original_bytes":N}`, and a redaction failure writes `{"dropped":"…"}`. Both decode without error into any typed payload (all zero values), so a truncated `tool_finished` became a nameless "finished" call and left the real request "never finished". `ActivityDigest.Add` now checks for the two markers first (`isMissingPayload`; no real payload has either key), counts the event as incomplete (per task, or per run when its scope is the run ID) and does not read it. The digest says `N events had incomplete data (payload truncated or dropped)`, and an unmatched request in such a task reads `result missing (trace incomplete)` instead of "never finished", since the finish event may be one of the missing ones. **Downside:** with several calls in one task and one lost completion, the digest cannot say which call it was; L0 holds the `call_id`s and `original_bytes`.

### 2026-10-23 — One failing MCP server no longer drops the others (supersedes the 2026-10-07 `SetupMCPConns` bullet)

- **The problem.** Since 2026-10-07, `SetupMCPConns` closed every connection and returned `nil, err` at the first failing server, and `SetupAgent` carried on with **no MCP tools**. So a remote server with an expired token also took away a working local server. Jerry expected "skip it and connect the next one", which is the behaviour users expect.
- **Now.** `ConnectMCPServer(ctx, name, cfg)` (exported) connects one server and picks its transport; every error names the server. `SetupMCPConns` loops over it in sorted name order, skips a server that fails, and returns the servers that connected plus every failure joined with `errors.Join` (nil if all connected). The deprecated SSE transport is now one of those errors instead of a separate stderr warning. `SetupAgent` prints the failures (one per line, "continuing without their tools") and keeps the connections.
- **The 2026-10-07 concern (callers must remember to close a partial list).** There is one caller, and it stores the list in `agent.mcpConns`, which `ShutdownAgent` closes, also when a later setup step fails (`main` defers `ShutdownAgent`). The doc comment says the connections must be closed even when the error is non-nil.
- **Why a separate `ConnectMCPServer`.** Info mode (`PLAN-info-mode.md`) needs a status line per server, and connecting each server in its own goroutine later only changes the loop.
- **Test.** `Test_Unit_SetupMCPConns_ServerFails_SkipsItAndKeepsTheOthers` replaces the 2026-10-07 test: a good local server (the test binary as a child) between three bad ones (cannot start, bad config, SSE). Only the good one is returned, it answers a tool call, the error names all three bad ones, and closing it leaves no child process.
- **Honest downside.** The agent can start with fewer tools than the config lists. The failure is one printed line that is easy to miss in chat; `-mode info` will be the reliable check, and the welcome message could show the tool count later. Connecting is still sequential, so a slow server delays the next one.
- **Follow-up (Copilot, PR #22): the loop stops once the setup context is cancelled.** `connectLocalMCPServer` builds the process with `exec.Command`, which ignores the context, and the SDK's `CommandTransport.Connect` starts it without checking, so after a SIGTERM during setup the new skip-and-continue loop would have launched every remaining server only to tear it down. The old all-or-nothing loop stopped at the first error, so this was a regression of this change. `SetupMCPConns` now checks `ctx.Err()` before each server, stops, and adds one `MCP setup stopped before server <name>` error (still `errors.Is(err, context.Canceled)`). Test: `Test_Unit_SetupMCPConns_CancelledContext_StartsNoServer` (fails without the check). Info mode's own loop must do the same.

### 2026-10-23 — Each stdio MCP server runs in its own process group (resolves Deferred item 14)

- **The bug** (verified with a scratch run): stdio servers shared the agent's process group, so the terminal's
  Ctrl+C, meant to cancel a chat turn, also sent SIGINT to `npx`/`node`. The agent survived (it catches SIGINT); the
  server died, and its tools failed for the rest of the session.
- **Fix.** `connectLocalMCPServer` calls `startInOwnGroup(cmd)` before starting the server. **One group per server**,
  led by the server (Jerry asked about one shared group: rejected, since shutdown is per server anyway, killing one
  server's tree must not touch another, and joining the first server's group fails if it already exited).
  `McpConnection` keeps the `cmd` (unexported; nil for remote servers), and `Close` calls `killGroup(cmd)` after the
  SDK's close, because the SDK only stops the direct child and `node` under `npx` could outlive it. A failed handshake
  also calls `killGroup`.
- **Per OS** (build-tagged files, since `syscall.SysProcAttr` has different fields per OS):
  `procgroup_unix.go` sets `Setpgid` and kills with `syscall.Kill(-pid, SIGKILL)` (a negative PID is the group);
  `procgroup_windows.go` sets `CREATE_NEW_PROCESS_GROUP` (console Ctrl+C no longer reaches the server; Ctrl+Break
  still does) and `killGroup` is a no-op. Cross-compiles for Windows; not run there. Other OSes (plan9, wasm) have no
  file and would not compile; none is a target.
- **Tests** (`procgroup_unix_test.go`): the helper server's process group is its own pid and not the test's group;
  the helper can start a grandchild (`sleep 60`, as npx starts node), which must be gone after `Close`. Both fail with
  the fix disabled. Scratch run with the real filesystem server: both `npm exec` and `node` survive a SIGINT sent to
  the agent's group, and are gone after quitting.
- **Honest downsides.** (1) The terminal's Ctrl+C was also an accidental "kill everything"; now a server that hangs
  is only stopped by quitting the agent (or by hand). (2) If the agent itself is killed hard (SIGKILL, crash),
  servers no longer get the terminal's signals; they rely on stdin closing, which most MCP servers treat as "exit",
  but one that ignores it is left running. (3) `killGroup` runs after the server is reaped, so in theory its PID
  could be reused by a new group leader in that moment; with SIGKILL as the signal this is accepted as negligible.
- **Next:** Deferred item 15 (Ctrl+C gracefully aborts setup), which needed this first.

### 2026-10-23 — Ctrl+C during setup aborts gracefully (resolves Deferred item 15)

- **Before:** nothing read the Ctrl+C channel until the first turn, so a press during setup (e.g. a slow `npx`
  download) did nothing and was then discarded. Jerry: during setup Ctrl+C should quit gracefully; in the chat loop
  it only cancels a turn.
- **Now.** `SetupAgent(ctx, cfg, interrupts)` takes the channel and wraps the MCP steps (connect, list tools) in
  `createInterruptWatcher` (renamed from `createTurnWatcher`, now used for turns and setup). After each MCP step, a
  cancelled setup context returns its error (`context.Canceled`) instead of "print and continue without the
  remaining servers". The servers that had connected stay in `agent.mcpConns`. `main` now returns on **any** setup
  error, prints `setup interrupted, exiting` for `context.Canceled`, and its deferred `ShutdownAgent` closes the
  servers. A server still in its handshake fails it and is closed by the SDK, then `killGroup`.
- **Also fixed:** `main` used to skip only the *message* for a `context.Canceled` setup error and then went on to
  the welcome text and `RunAgent`. Harmless for SIGTERM (`RunAgent` checks the root context first), but a Ctrl+C
  abort would have started the chat loop anyway.
- **Only the MCP steps get the setup context**, because `stop()` cancels it when `SetupAgent` returns, even after a
  successful setup. The other steps are instant, and anything long-lived (the recorder, the session) must not keep
  it. MCP sessions detach from their connect context (checked in the SDK), so connected servers survive `stop()`.
  A nil channel (tests) means setup cannot be interrupted by Ctrl+C.
- **Depends on** the process-group change above: before it, the same Ctrl+C also killed the stdio servers.
- **Tests.** `Test_SetupAgent_CtrlCWhileConnecting_AbortsAndKeepsConnectedServersForShutdown`: a remote server
  (in-process, `httptest`) connects, a stdio server (`sh -c 'echo $$ > pid; exec sleep 60'`) never answers; Ctrl+C
  arrives during its handshake. Checks `context.Canceled`, the remote server kept for shutdown, no session created,
  the hanging process gone, and `ShutdownAgent` safe without a session. Fails with the abort checks disabled.
  Scratch run with the real filesystem server plus a hanging one: `setup interrupted, exiting`; `npm exec`, `node`
  and `sleep` all gone.
- **Honest downsides.** (1) **Aborting takes up to 5 s** when a server is stuck in its handshake: the SDK's close
  waits `TerminateDuration` (default 5 s) for the server to exit after closing its stdin, before SIGTERM. Setting
  `CommandTransport.TerminateDuration` lower would speed this up but also shortens the grace period of a normal
  shutdown; left at the default. (2) The program still exits with status 0 after an interrupted setup (Deferred
  item 3). (3) A Ctrl+C right as setup finishes either aborts or is discarded before the first prompt.

### 2026-10-23 — Approval gate v1: open questions settled (plan only, nothing built)

Settles the open points of `PLAN-approval-gates.md` v1 and `PLAN-info-mode.md`. Both plans are updated.

- **Order: info mode first**, then the approval gate. Deny rules match tool names exactly as the model sees them
  (`<server>_<tool>`), and info mode is how you see them; the default list is checked against real names before
  it is fixed. Info mode's deny-rules section waits until `DenyRules` exists.
- **Ctrl+C at the "allow once? [y/N]" prompt cancels the whole turn**, the call included. During a turn Ctrl+C is
  a signal (the line editor's raw mode is off), and the turn's interrupt watcher already cancels on it. Rejected:
  reading the answer through the line editor so Ctrl+C only denies the call. Downside: you can't refuse one call
  with Ctrl+C and let the model carry on; `n` or Enter does that.
- **Default deny list: anything that deletes.** `*delete*` and `*remove*`, matched case-insensitively;
  `run_command:rm` / `rmdir` once `run_command` exists. **Moving, writing and editing files stay allowed**, so the
  agent can still do edits without prompts. Downsides: a move can overwrite a file, which is a delete in effect;
  `*remove*` may catch harmless tools (e.g. "remove_label"), which then only cost one `y`; a destructive tool with
  another name (`tidy`, `purge`) gets through — add it in config.
- **Supersedes** the Track 1 `requires_approval` decision (a list of tools that always ask): never built, and asking
  on every listed call is what v1 is meant to avoid.
- **Two guardrails against a tampered config** (from the `exec.Command` discussion), added to the plans:
  1. Info mode shows each stdio server's program and argument count (`npx (4 args)`), never the arguments or env.
     Part of info mode's build.
  2. A first-run trust prompt per stdio server, keyed on a hash of `command` + `args` + `env`, stored in a local
     `0600` trust file (hash and name only). In autonomous mode an untrusted or changed server is not started unless
     trusted beforehand or `allow-all` is set. Under "Later" in the approval plan; needs a trust-file decision first.

### 2026-10-23 — Info mode (`-mode info`) built (`PLAN-info-mode.md` steps 1–4, 6)

`go run . -mode info [-config f] [-workdir d]` prints the header (agent name/version, config file or "none,
defaults only", absolute resolved working dir, whether the API-key variable is set), `config.PrintConfig`, the
built-in tools (with `read_file`'s "never reads" list), every MCP server's status and every MCP tool by its
model-facing name with its hints, then exits. Files: `agent/info.go`, `main.go`, `config/flags.go`,
`mcpconnect/tools.go` (`ToolHints`, `HintsOf`), `tools/builtin/readfile.go` (`DeniedNames`).

- **`main` dispatches before the agent is created** and never calls `SetupAgent`: no provider, memory or trace
  recorder, so no model call, no API key needed, no trace file. `main` takes the config path from `flags.Config`,
  so `config/` did not need to report which file it loaded (the plan's small `config/` change was dropped).
- **Same setup functions as chat mode** (`SetupBuiltinTools`, `ConnectMCPServer`, `GetMCPTools`), so the names it
  prints are the names the model gets. That replaces the plan's "compare both paths" drift test. What can still
  drift is the wiring around them (info skips `SetupAgent`'s checks such as a positive `maxSteps`).
- **One server at a time**: connect, list tools, close (`defer` in `inspectServer`), then the next. A failed server
  gets a FAILED row and the rest still run. Stdio rows show the program and argument count (`npx (3 args)`), never
  arguments or env. Connection errors have the configured URL replaced by `scheme://host/[rest hidden]` (the SDK's
  error quotes the URL, which can hold a token).
- **Hints** come from the server's MCP annotations, with the spec's default (`destructiveHint` true unless the tool
  is read-only). Shown as `read-only`, `destructive`, `idempotent`, `additive` or `(no hints)`; they are claims.
- **Exit codes**: 0 all good, 1 a server or the built-in tools failed, 130 Ctrl+C/SIGTERM. `main` calls `os.Exit`
  in the info branch only; that is safe because no `defer` is registered yet and `PrintInfo` closed its servers.
  Ctrl+C uses `createInterruptWatcher`, as setup does; servers not yet started are listed as "not started".
- **Found while building:** the real filesystem server (1.0.0) has **no delete tool**, so the default deny rules
  `*delete*` / `*remove*` match nothing there today; `move_file` and `write_file` are its destructive tools. The
  default list earns its keep with other servers and with `run_command`.
- **Honest downsides.** Info starts every configured server (real processes, network, auth). Serial, so a slow
  `npx` download makes it slow (Deferred item 16). The exit code says that something failed, not which. A
  server's own name/version and tool descriptions are printed as the server sent them. The `hideURL` scrub only
  covers info: chat-mode setup still prints the raw connect error, URL included (pre-existing; see `SESSION.md`).
  Ctrl+C while a **remote** server hangs in its handshake takes about 5 s: the SDK sends `notifications/cancelled`
  with a 5 s timeout and waits for it (seen in the test; same for chat-mode setup, like the 5 s stdio
  `TerminateDuration` in the setup-abort entry). Its HTTP requests to that server also stay open until exit.

