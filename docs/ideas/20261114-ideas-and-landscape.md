# Ideas & landscape — 2026-11-14

## TL;DR

- Nothing shipped since the 11-13 report. The newest commit anywhere is `b0d8573` ("small fixes to improve plan mode UX", 2026-11-10) on `origin/feat/autonomous-mode-2`; `origin/build` still ends at PR #42. Plan mode exists, and no run consumes a plan yet. The drift flagged on 11-12 and 11-13 now has four quiet days behind it.
- Fresh ideas this time are small and concrete: a write-ahead intent record, a budget with a recorded stop reason (`MaxTokens` is still unused), a model-reliability probe, harness changes scored before they are accepted (from Reef), and approvals through a push channel.
- Landscape is rotated toward the last three months: CopilotKit OpenBot (policy gateway that writes an audit row *before* acting), Reef (self-improving harness stored as a text tree), the DeepSeek Harness (star counts are huge and disputed), TrueForge (re-checked), plus OpenCode, Codex CLI and Goose traction updates.
- Key finding: the new "governed agent" projects (OpenBot, TrueForge) put the audit/policy layer in a *server*. A small, file-based, audit-first Go kernel is still an empty niche.
- Recommendation unchanged and now more urgent: build step 3 (one serial run with `state/`), and make "budget, journal, resume" its definition of done. No new formats and no new plan-mode surface until a run exists.

## Part 1 — Direction

### What shipped, did it help?

Last features on `origin/build`: `run_command` with deny/ask rules and an argument guard (#37, #38), parallel MCP connects (#40), plan mode with objective files, `plan/` and a `gate` field (#42). The branch after it only polishes plan mode. SESSION.md's last entry (2026-11-09) says: try plan mode against a real model, then step 3.

Against the thesis (durable, auditable, measurable): auditable moved (three approval levels, traced vetoes, reviewable plans). Durable and measurable did not. Candidly, this is two days of plan-mode polish on a feature that produces a file nobody reads yet. mvp3/mvp4 already show a run loop works, so the root build is behind its own reference, not blocked by a hard problem. The risk is not that the work is bad, it is that every day without a run delays learning the thing the thesis is about (what breaks when a run dies halfway).

### Fresh ideas

**1. Write-ahead intent record for tool calls.**
- *Idea:* before a tool with side effects runs, append `intent {call id, tool, args hash}` to the trace/journal; after, append `result`. On resume, an intent with no result is surfaced as "unknown outcome", never silently re-run. OpenBot's gateway does the same ordering: resolve, evaluate policy, write the audit row, *then* act.
- *Who:* anyone letting an agent edit files or run commands unattended. The crash-between-act-and-record window is where double-writes happen.
- *Differentiation:* Claude Code and Codex CLI log after the fact. Boltrope (11-12 report) has a dedup ledger but needs Postgres.
- *Learn:* the real meaning of "at least once vs at most once" for tools, and which of your tools are naturally idempotent (`write_file` with identical content) and which are not (`run_command`).
- *First slice:* two trace events around `executeToolCall`, plus a test that kills the process between them and checks resume refuses to guess.
- *Downside:* doubles trace volume for writes; "unknown outcome" needs a human or a verification tool to resolve.

**2. Budgets with a recorded stop reason.**
- *Idea:* implement `MaxTokens` (currently parsed and ignored), plus wall-time and a tool-call count, as one `Budget` checked in the loop. A run ends with a typed reason: `done`, `max_steps`, `budget_tokens`, `timeout`, `interrupted`, `error`.
- *Who:* anyone running overnight; "how much did it cost and why did it stop" is the first question after a long run.
- *Differentiation:* most harnesses show cost in a UI; few write the stop reason into the durable record so a later run or eval can read it.
- *Learn:* token accounting across providers (Ollama vs Anthropic report usage differently) and how stop reasons feed resume.
- *First slice:* sum `Usage` from `model.Provider` responses; end the turn with a sentinel error like the existing `ErrEndTurn`.
- *Downside:* local models may not report usage, so the budget needs an estimate with an honest label.

**3. A model-reliability probe.**
- *Idea:* `-mode probe` runs ~10 fixed micro-tasks (call a tool with exact args, recover from a tool error, stop when done, emit valid JSON, survive a 50 KB tool result) against the configured model and prints a pass table.
- *Who:* people choosing among local and cheap models for agent work. CLAUDE.md already notes sloppy local models emit invalid JSON; today that is anecdote.
- *Differentiation:* benchmarks like SWE-bench score coding outcomes. This scores *harness-relevant behaviour* and costs cents.
- *Learn:* which model failures the kernel should absorb and which belong to the model; the seed of the eval harness from earlier reports.
- *First slice:* three probes using the fake-provider test fixtures' shape but a real model; output as a table plus JSON.
- *Downside:* a small fixed suite gets stale and can be gamed by prompt tweaks.

**4. Score harness changes before accepting them (from Reef).**
- *Idea:* treat system prompt, tool descriptions and rules as one versioned text bundle (the "agent lockfile" from 11-12). A change is a candidate, run against the probe/eval tasks, and accepted only if it does not regress.
- *Who:* anyone who tweaks prompts by feel. Reef does this with training infrastructure; a file bundle plus a pass table is the cheap version.
- *Differentiation:* Reef serves a harness tree over HTTP and needs a control plane; here it is a directory diffed in git.
- *Learn:* how noisy agent evals are (same task, N runs), which is humbling and valuable.
- *First slice:* `config + prompts` hashed into every trace header; run idea 3 twice with two bundles and diff.
- *Downside:* depends on ideas 2 and 3 existing; without repeats a single win is noise.

**5. Approvals through a push channel.**
- *Idea:* when an `ask` rule fires and nobody is at the terminal, send the question to a webhook (ntfy, Slack) and wait, or write a pending record and exit (11-13 idea 1). A Mac/iPhone app, `coucou`, now exists only to approve actions from coding agents, so people want this.
- *Differentiation:* those apps bolt onto Claude Code hooks; here it is part of the approval gate.
- *Learn:* timeouts, replay of stale approvals, and authenticating the reply.
- *First slice:* one `PromptAllowDenyCall` implementation that posts and polls a file.
- *Downside:* an approval channel is an attack surface; replies must be bound to one call id.

### Recommendation

Do step 3 now, and let ideas 1 and 2 be inside it: one serial run over a plan, `state/` as an append-only journal with intent/result records, a `Budget`, a typed stop reason, and `-mode resume` that handles "interrupted step". Do not touch plan mode, `/reload` or docs until a run can be killed and resumed in a test. Ideas 3 and 4 follow as the measurement layer.

## Part 2 — Landscape

Projects below are new to this series or changed. Star counts are snapshots from the pages cited; GitHub pages fetched today did not show release dates.

**1. CopilotKit OpenBot (TypeScript, MIT template).** Self-hostable workspace of AI "coworkers", each with its own computer (browser, files). 12.3k stars on the repo page fetched today; ranked 6th (10,506 stars) in a weekly trending list for 2 to 8 November. Design: a gateway is the only path for browser/file actions: it resolves the target, evaluates policy, writes an audit row, then acts. Policies are CEL expressions, deny before allow, no policy means nothing permitted, and a broken rule refuses. `/admin/audit` lists permitted, refused and failed actions. Isolation via gVisor (`runsc`) and per-container tokens; durable threads and memory live in a separately licensed "Intelligence" service ([GitHub](https://github.com/CopilotKit/OpenBot), [trending list](https://www.josedacruz.com/2026/11/11/github-weekly-ai-agent-tools-trending-right-now-november-2-november-8-2026/)). *Borrow:* "audit row before action, and name the rule that refused" (idea 1). Your deny/ask levels already match its fail-closed stance.

**2. Reef (Human-Agent-Society, Python).** "Infrastructure for continual self-improving agents": serves OpenAI/Anthropic-compatible endpoints, hands back a receipt header per interaction, accepts scored feedback against receipts, and either trains weights or edits the harness (a tree of text files: prompts, rules, skills). Candidates are evaluated; a selection policy publishes the winner to a version history. About 7.8k stars (page fetched today), `reef-infra` v0.1.0 in September 2026, and a November note says it is refactoring to a control plane plus workers, so `main` may break ([GitHub](https://github.com/Human-Agent-Society/reef)). *Borrow:* receipts and "no candidate is published without an evaluation" (idea 4).

**3. DeepSeek Harness / "DSH" (TypeScript, MIT).** "Everything is a plugin" harness (built on Cordis), developer preview with a warning of compatibility-breaking changes. The repo page shows 281k stars today; the trending list says 276,783 for the same project a week earlier; an earlier report had a different, unverified figure. These are extraordinary numbers I could not corroborate from an independent source, so treat them as **unverified**. The page says nothing on tracing or sandboxing beyond a SAFETY.md link ([GitHub](https://github.com/deepseek-ai/deepseek-harness)). Its trending companion `dsh-our-free-model` (66k stars, a plugin giving free model access) is a reminder that a plugin ecosystem can swamp the core. *Borrow:* nothing in design yet; the lesson is that plugin-everything has no stated answer for audit, which is your opening.

**4. TrueForge (TrueFoundry, TypeScript, MIT) — revisited.** The 11-13 report left stars as 1.8k (aggregator, unverified). The repo page today shows about 6.5k stars and 1,043 commits, with an empty Releases section. Newly visible: local mode is one process plus SQLite, hosted mode is Postgres and Redis; skills are git-backed `SKILL.md` packs; features include deferred tool loading, large-result offloading and compaction ([GitHub](https://github.com/truefoundry/trueforge)). *Borrow:* "large-result offloading": write a big tool result to a file and give the model a path and a head. It fits your `read_file` limits and is cheap.

**5. OpenCode (now `anomalyco/opencode`, MIT).** 226.9k stars on the repo page today. A secondary comparison reported 210.6k in late September, so growth of about 16k in under two months, if both are right ([GitHub](https://github.com/anomalyco/opencode), [morphllm](https://morphllm.com/comparisons/opencode-vs-codex)). Built-in `build` agent (full access) and read-only `plan` agent that asks before bash, plus a `@general` subagent. One tracker says OpenAI Codex CLI leads in measured *use* while OpenCode leads GitHub *attention* ([keenable](https://select.keenable.ai/r/open-source-coding-agents-user-share-vs-github-star-share/ob_TmpnOoRVMQ_ct5svFjg)); I did not verify that. *Borrow:* the plan/build split as two permission profiles of one loop, which your plan mode already mirrors.

**6. OpenAI Codex CLI (Rust).** Secondary sources give about 126.9k stars in late September (up from 83k in May); unverified against GitHub today ([morphllm](https://morphllm.com/comparisons/opencode-vs-codex), [pinggy](https://pinggy.io/blog/best_open_source_cli_coding_agents/)). Open-source since April 2025. *Borrow:* nothing new; it is the baseline you are differentiating from: a good interactive tool with approval prompts and no durable run record.

**7. Goose (aaif-goose, Linux Foundation).** One comparison page lists 55,316 stars, undated, up 697 in 30 days; the 11-12 report had 54.4k on 2026-09-18, so growth is slow ([libhunt](https://www.libhunt.com/compare-opencode-vs-goose)). *Borrow:* nothing new; mainly evidence that MCP-native agents are settling into foundation governance.

**8. coucou (Louis-CFM, Swift).** Mac menu-bar and iPhone companion for approving actions from Claude Code, Codex and Cursor. 7,068 stars in the same trending list; small project, big signal ([list](https://www.josedacruz.com/2026/11/11/github-weekly-ai-agent-tools-trending-right-now-november-2-november-8-2026/)). *Borrow:* the demand for out-of-band approval (idea 5).

**9. google/elephant-goldfish-model (Python).** A spec-driven-development plugin for Claude Code, Codex and Gemini, 15,496 stars in that list; details not checked, so what it does is taken from the list's one-line summary ([list](https://www.josedacruz.com/2026/11/11/github-weekly-ai-agent-tools-trending-right-now-november-2-november-8-2026/)). *Borrow:* agreeing a spec before code is your objective-then-plan flow; popularity suggests the framing resonates.

### Takeaways

- Governed-agent projects now compete on *what happens before an action*: policy, audit row, named rule. Write-ahead records are the next step on a path you are already on.
- Self-improvement (Hermes on 11-13, Reef today) keeps appearing, and the honest version has an evaluation gate. That gate is your eval idea, not a new feature.
- Star numbers in this space are volatile and sometimes implausible. Use them as weak signals; the designs are what carries lessons.
- Go + small + file-based + audit-first remains uncontested. TrueForge and OpenBot are servers.

## Sources

- https://github.com/CopilotKit/OpenBot
- https://github.com/Human-Agent-Society/reef
- https://github.com/deepseek-ai/deepseek-harness
- https://github.com/truefoundry/trueforge
- https://github.com/anomalyco/opencode
- https://www.josedacruz.com/2026/11/11/github-weekly-ai-agent-tools-trending-right-now-november-2-november-8-2026/
- https://morphllm.com/comparisons/opencode-vs-codex
- https://pinggy.io/blog/best_open_source_cli_coding_agents/
- https://select.keenable.ai/r/open-source-coding-agents-user-share-vs-github-star-share/ob_TmpnOoRVMQ_ct5svFjg
- https://www.libhunt.com/compare-opencode-vs-goose
- Earlier reports: `origin/ideas:ideas/20261112-ideas-and-landscape.md`, `origin/ideas:ideas/20261113-ideas-and-landscape.md`
- Repo: SESSION.md, DECISIONS.md, THOUGHTS.md, `git log origin/build`, `git log origin/feat/autonomous-mode-2`
