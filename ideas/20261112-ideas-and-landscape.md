# Ideas & landscape — 2026-11-12

## TL;DR

- Since 2026-11-07 the repo shipped **plan mode** (objective → plan file, with gates) and parallel MCP setup. That is good groundwork for autonomous mode, but nothing yet *runs* a plan at the repo root, so the "durable, auditable, measurable" thesis has not moved on its measurable side.
- Biggest drift risk: formats and docs (`objective/`, `plan/`, `mdscan/`, a 4,700-line DECISIONS.md) are growing faster than run-time behaviour. Fix: finish one serial run with a `state/` journal, then replay and eval on top.
- Strongest fresh ideas: (1) replay a run from its trace, (2) an eval harness that scores plan gates, (3) an "agent lockfile" stamped into every trace, (4) a tamper-evident trace.
- Landscape: the fast movers are *minimal* harnesses (Pi), huge general CLIs (Codex CLI, opencode, Goose), and a new wave of **durable runtimes** (DBOS, Agentspan, and early Go projects Tacklr and Boltrope). Durability is where minimal-agent's thesis has real company.
- Recommendation: build `state/` as an append-only run journal first, because replay, resume, eval and audit all read it.

## Part 1 — Direction

### What shipped and does it help the thesis?

From `origin/build` (PR #37–#42) and `origin/feat/autonomous-mode-2`:
`run_command` with deny/ask rules and an argument guard; concurrent MCP connects with a per-server timeout; a `procutils`/`textutils` split; then plan mode: an `objective/` format, a `plan/` format with a `gate` field, an objective chat (`/save`, `/edit`), a planner that proposes a plan, and `Accept?`. The newest commit only polishes plan-mode UX.

- **Helps (auditable):** deny/ask/run gates, traced vetoes, and plan files a human reviews before anything runs.
- **Doesn't yet help (durable, measurable):** `state/` is still a skeleton awaiting review (SESSION, 2026-11-09). There is no eval, no replay, and `MaxTokens` is parsed but unused. Plan mode is a lot of design for a run that does not exist at the root yet (mvp3/mvp4 prove it can work).

### Fresh ideas

**1. Replay a run from its trace (`-mode replay <trace>`).**
- *What:* feed recorded model responses back through the real loop, with tools stubbed from recorded results, so you can re-run a failed run, change a hook or rule, and see where it would diverge.
- *Who:* anyone debugging long autonomous runs, and teams that must explain what an agent did. That is a large audience as agents get more autonomy.
- *Differentiation:* Claude Code and Codex CLI keep transcripts but do not offer deterministic replay through your own kernel. Replay is table stakes in durable runtimes like DBOS and Agentspan (stored steps return saved results), but not in coding CLIs.
- *Learn:* what must be recorded for determinism (model ids, tool results, clocks, random ids), and where side effects break replay.
- *First slice:* a provider that reads `model` responses from an existing trace file, plus a test proving a recorded turn replays identically.
- *Downside:* real runs are not deterministic once tools touch the file system, so you replay decisions, not the world.

**2. An eval harness that scores plan gates.**
- *What:* `-mode eval` runs a set of objective files N times each and reports gate pass rate, steps, tokens and cost. The `gate` field added in PR #42 is already a machine-checkable pass/fail.
- *Who:* anyone choosing between models or prompts for long runs. Few harnesses ship evals next to the loop; it is usually a separate project (UK AISI's Inspect is the reference).
- *Learn:* variance across runs, flaky gates, cost accounting, how to keep a benchmark honest.
- *First slice:* three tiny objectives with file-exists/test-passes gates, run 5 times with the fake provider, then with one real model.
- *Downside:* real-model runs cost money, and weak gates make a meaningless score.

**3. An agent lockfile (the "package-lock.json for agents" from THOUGHTS.md).**
- *What:* each run stamps its trace with a hash of: kernel commit, config, model id, tool list with MCP server versions, and the objective/plan hash.
- *Who:* teams that need "why did the same task behave differently last week?" Nothing mainstream does this end to end.
- *Learn:* what actually determines agent behaviour, and how hard it is to pin (hosted models change silently).
- *First slice:* write the fingerprint into the first trace event and print it in `-mode info`.
- *Downside:* you can pin everything except the hosted model's weights, so it records drift rather than preventing it.

**4. Tamper-evident trace.**
- *What:* chain each trace event's SHA-256 over the previous one (Boltrope does this per session).
- *Who:* regulated or shared-team use; also protects the model-facing `.agent-trace` deny rule from being the only defence.
- *Learn:* append-only logs, what "audit" requires versus plain logging.
- *First slice:* one `prev_hash` field and a `-mode verify` command. About 60 lines.
- *Downside:* a chain on one disk proves order, not honesty. Without an outside anchor the process could rewrite it all.

**5. Long-term memory as an MCP capability, measured.**
- *What:* build THOUGHTS.md's "core-adjacent" memory as a separate MCP server and score it on a small LongMemEval-style set (Mastra publishes 94.87% for its approach, per its own post).
- *Learn:* memory design, plus measuring it instead of guessing.
- *Downside:* MCP only fits *tool-shaped* hooks; memory that must run at fixed loop points (before each step) does not fit MCP without a hook, which is a real design question.

### Recommendation

**Build `state/` as the append-only run journal now, finish one serial autonomous run on it, then add replay (idea 1) and the lockfile stamp (idea 3).** Reason: replay, resume, eval and audit all read the same journal, so one structure serves the whole thesis, and it forces the run to exist. Candidly: pause new plan-mode format work. It is polished, but a plan nobody can execute, replay or score does not yet teach the hardest part of agent infrastructure.

## Part 2 — Landscape

Star counts come from third-party trackers unless noted; none could be checked live, so treat them as approximate. No earlier reports exist, so every entry is new.

**1. Pi (badlogic/pi-mono, now earendil-works/pi)** — minimal terminal coding agent; packages for model access (`pi-ai`), loop (`pi-agent-core`), TUI and CLI. Deliberately has no sub-agents, plan mode or permission popups; everything is an extension. About 95.5k stars on an undated GitHub view; 1.0 reported 2026-10-01, with 1.4.0 on 10-21 ([implicator](https://www.implicator.ai/pi-is-not-a-claude-code-rival-it-is-a-harness-rebellion/), [tech-insider](https://tech-insider.org/pi-1-0-coding-agent-tokens-cut-2026/)). *Borrow:* its "small core, everything else is an extension" stance is exactly your kernel idea; copy the rule that a feature must be justified as an extension first.

**2. Tacklr (Go SDK)** — opinionated Go harness: turns, parked human-input interrupts, plan/to-do cycle that rebuilds context at each `complete_todo`, sessions snapshotted and resumed, optional Temporal backend, OpenTelemetry spans per turn, secrets kept out of snapshots. v0.3.2 on 2026-09-03, Apache-2.0, 0 known importers, so no traction ([pkg.go.dev](https://pkg.go.dev/github.com/ryanaldo34/tacklr@v0.3.2)). *Borrow:* three separate data planes (snapshot, scheduler state, secrets) and a context hand-off at each task boundary, which fits your plan tasks.

**3. Boltrope (xd1lab/harness-ai)** — Go, PostgreSQL event-sourced session log, replay and branching, dedup ledger so mutating tools do not run twice, SHA-256 hash chain per session. 1 star, so a design to read, not a project to depend on ([GitHub](https://github.com/xd1lab/harness-ai)). *Borrow:* the dedup ledger (idempotent side effects on resume) and hash chain, directly relevant to ideas 1 and 4.

**4. DBOS Transact (Python, TS, Go, Java)** — durable execution as a library: mark functions as workflows and steps; results are checkpointed to your Postgres, and after a crash it skips recorded steps. Integrates with Pydantic AI and the OpenAI Agents SDK. $8.5M seed; no usage numbers given ([Capital & Compute, 2026-07-13](https://capitalandcompute.net/blog/dbos-durable-execution-agent-harness/)). *Borrow:* "completed steps return saved results" is the replay/resume contract; mimic it with a file journal before considering Postgres.

**5. Agentspan (Orkes)** — MIT durable runtime; the server, not your process, owns execution state; paused approvals can wait days; every LLM call and tool call is stored and replayable; OTel included. Post dated 2026-04-03, updated 2026-09-12; stars not stated ([Orkes](https://orkes.io/blog/open-sourcing-agentspan-durable-ai-agents)). *Borrow:* human approval as a *durable paused state*, not a blocking prompt, which your `AskYesNo` flow is not yet.

**6. LangChain DeepAgents** — open-source harness modelled on Claude Code: planning tool, file system, sub-agents. About 30.9k stars and last push 2026-11-03 on a directory listing; v0.5.0 on 2026-04-07 per the official changelog ([listing](https://meshkore-web.pages.dev/agent/langchain-ai-deepagents), [changelog](https://docs.langchain.com/oss/python/releases/changelog/index.html)). A third-party note about an October Schedules SDK is unverified. *Borrow:* its planning-as-a-tool pattern; yours is more explicit (plan files), so compare cost and failure modes.

**7. Goose (now aaif-goose/goose)** — MCP-native local agent; Block gave it to the Linux Foundation's Agentic AI Foundation (with MCP and AGENTS.md); governance moved 2026-04-07. About 54.4k stars on 2026-09-18, per a secondary site ([latent.space](https://latent.space/wiki/aaif/), [rywalker](https://rywalker.com/research/goose)). Diagrid announced Dapr-based durability for it ([Luca Berton](https://lucaberton.com/blog/diagrid-goose-durable-execution-2026/)). *Borrow:* durability is being bolted on from outside, which is evidence the gap is real.

**8. OpenHands (+ Software Agent SDK)** — autonomous coding agent with a separate MIT SDK and an April 2026 paper. Stars 89,167 on a 2026-10-14 listing; release notes are third-party and inconsistent, so versions are unverified ([arXiv](https://arxiv.org/html/2511.03690v2), [rywalker](https://rywalker.com/research/openhands)). Reported features include native hooks and a KVM-accelerated Docker sandbox (unverified). *Borrow:* sandbox as a swappable tool runtime, which is the real fix for your "run_command is not contained" gap.

**9. Mastra (TypeScript)** — framework with "Observational Memory": text-based memory with no vector or graph database; 94.87% on LongMemEval with gpt-5-mini (self-reported); announced 2026-02-09, still updated in May ([Mastra](https://mastra.ai/blog/observational-memory)). Stars 21.6k at an undated snapshot ([ecosyste.ms](https://packages.ecosyste.ms/registries/npmjs.org/packages/@mastra%2Fmemory)). *Borrow:* long-term memory built from plain compressed notes plus a benchmark: cheap to copy and to measure (idea 5).

**10. UK AISI Inspect** — evaluation framework with sandbox plugins (Docker, Kubernetes); the agent harness runs outside the sandbox and issues commands into it; adopted by other safety labs per AISI. Stars not found ([AISI](https://aisi.gov.uk/blog/the-inspect-sandboxing-toolkit-scalable-and-secure-ai-agent-evaluations)). *Borrow:* the layout for idea 2: dataset + solver + scorer, and the "harness outside, work inside" sandbox split.

**Big general CLIs, briefly.** Codex CLI: about 127k stars in a Q3 review, rust releases roughly weekly (0.161–0.166 in October), unverified aggregator figures ([Havoptic](https://www.havoptic.com/blog/deep-dive-openai-codex-q3-2026)). opencode (anomalyco, formerly sst): 210,052 stars on 2026-10-18 per one tracker, though another lists 222.8k with no date ([tracker](https://rywalker.com/research/opencode), [Trendshift](https://trendshift.io/repositories/17001)). These win on polish and distribution; competing on features is a losing game, which supports the durable/auditable focus.

**Watch, unverified.** "DeepSeek Harness" (MIT, August 2026) is reported at 200k+ stars by one outlet, but I could not fetch a primary source, so I make no claim beyond its existence being reported ([search result](https://thenewstack.io/deepseek-harness-open-source-plugins/)).

### Takeaways for minimal-agent

- A real, still-small niche exists: **Go + file-based journal + approval gates + eval**, between toy kernels and Postgres/Temporal platforms.
- The two Go durable entrants have essentially no adoption, so there is room, but also little proof of demand. The author's learning goal is the real payoff.
- The leaders compete on breadth; the thesis competes on *explaining and measuring a run*.

## Sources

- https://rywalker.com/research/opencode ; https://trendshift.io/repositories/17001
- https://meshkore-web.pages.dev/agent/langchain-ai-deepagents ; https://docs.langchain.com/oss/python/releases/changelog/index.html
- https://latent.space/wiki/aaif/ ; https://rywalker.com/research/goose ; https://lucaberton.com/blog/diagrid-goose-durable-execution-2026/
- https://pkg.go.dev/github.com/ryanaldo34/tacklr@v0.3.2
- https://github.com/xd1lab/harness-ai
- https://capitalandcompute.net/blog/dbos-durable-execution-agent-harness/
- https://orkes.io/blog/open-sourcing-agentspan-durable-ai-agents
- https://arxiv.org/html/2511.03690v2 ; https://rywalker.com/research/openhands
- https://www.implicator.ai/pi-is-not-a-claude-code-rival-it-is-a-harness-rebellion/ ; https://tech-insider.org/pi-1-0-coding-agent-tokens-cut-2026/
- https://mastra.ai/blog/observational-memory ; https://packages.ecosyste.ms/registries/npmjs.org/packages/@mastra%2Fmemory
- https://aisi.gov.uk/blog/the-inspect-sandboxing-toolkit-scalable-and-secure-ai-agent-evaluations
- https://www.havoptic.com/blog/deep-dive-openai-codex-q3-2026
- https://thenewstack.io/deepseek-harness-open-source-plugins/
- Repo: SESSION.md, DECISIONS.md, THOUGHTS.md, `git log origin/build`, `git log origin/feat/autonomous-mode-2`
