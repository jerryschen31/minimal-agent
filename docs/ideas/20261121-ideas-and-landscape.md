# Ideas & landscape — 2026-11-21

## TL;DR

- Since the 11-20 report, `origin/build` got only housekeeping (PR #48: `PlanState` rename, `mvps/` and `utils/` moves, docs moved to `docs/`). No new feature. The last real features are autonomous mode (#44) and token budgets (#46).
- The field's newest theme matches this project's thesis: **durable harnesses**. Vercel (`durable-harness`) and Temporal (`Temporal Agent Harness`) both wrap existing agent loops with a step log, resume and a stream. DeepSeek's harness (Aug 2026) makes an append-only session log the source of truth. That is what `state/` already is.
- New idea: give `state/` a small **storage / queue / stream seam** (Vercel calls it a "World"), so the journal can be read live by a second process. That unlocks `status`, `tail` and attach commands, and it is the first real step toward orchestrator mode.
- New idea: **a hooks-compatibility bridge** (DeepSeek ships one) and **an Inspect-style Task/Solver/Scorer split** for the eval harness.
- Recommendation: stop cleanup, build `-mode oneshot` plus a journal-replay eval (still not done), then orchestrator. The project is not drifting yet, but one day of renames is the third "tidy-up" in a week.

## Part 1 — Direction

**What shipped.** `git log origin/build` shows, after PR #46 (token usage, `maxTokens`, `/usage`, end-of-run report), five cleanup commits: `State` → `PlanState`, `mvp*` → `mvps/`, helpers → `utils/`, plans and ideas → `docs/`. These are fine and cheap, but none moves the thesis (durable, auditable, measurable runs). SESSION.md's handoff still says "step 3, orchestrator mode" is next, and the 11-20 report's top item (replay eval, oneshot) has no commit yet. Candidly: the measurable half of the thesis is still only budgets and a usage report. There is no way yet to say "this change made the agent better".

### 1. A "World" seam for `state/`: storage, queue, stream

**Idea.** Vercel's durable-harness says durability needs three things: a log of each step, a way to resume a turn when its process dies, and a stream for whoever is watching. Today `state/` has the first (journal) and, since #44, the second (resume). It has no *stream*. Add a tiny interface with `Append`, `Read` and `Follow` (tail new lines) on the journal, with a file implementation.
**Who and why.** People who run agents for an hour or more and want to check on them from another terminal, CI or a phone. Long runs are where agent use is growing.
**Differentiation.** Claude Code and Codex keep a transcript but not a plan-level journal with a lock and settle-on-resume semantics. Vercel/Temporal need a Node/Temporal server; this one is a single Go binary and a file.
**Learn.** Log-tailing without races (partial last line, rotation), and the reader/writer split. It is also the exact thing orchestrator mode needs: the parent reading a child's progress.
**First slice.** `minagent -mode status -plan x.plan.md` prints the latest run's tasks and token usage from `state.Rebuild`; then `-follow` using `fsnotify` or a 200 ms poll. About an evening; no loop changes.
**Downside.** A second consumer of the journal format freezes it sooner. A poll loop is simpler than fsnotify and fine here.

### 2. Hooks compatibility bridge

**Idea.** DeepSeek's harness runs a user's existing `hooks.json` from either product against its own interception points. `hooks.Hook` here has one method. Add an external-command hook: send `{tool, args}` JSON on stdin, read allow/deny from the exit code, at `BeforeTool` only.
**Who.** Teams that already wrote safety scripts for another agent and do not want to rewrite them.
**Differentiation.** A hook that can only veto, and whose veto is a tool result and is journaled (`tool_vetoed`), is stricter than most harnesses' "hooks may rewrite anything".
**Learn.** An API contract: payload shape, timeout, what a crashing hook means (fail closed). This is the lesson earlier reports called "contract design".
**First slice.** One `hooks.Command` type, config `hookCommands: ["./check.sh"]`, 60 lines plus a fake-script test.
**Downside.** Every field in the payload becomes a promise. Keep it tiny and say so in the doc.

### 3. Task / Solver / Scorer split for the eval harness

**Idea.** Inspect AI separates a *task* (dataset of cases), a *solver* (the agent run) and a *scorer* (grades the transcript). Build the eval runner the same way, with one extra twist that fits this repo: a solver may be "replay this journal" (no model call), a scorer reads the journal.
**Who.** Anyone changing prompts or window strategies and wanting a pass rate rather than a feeling. That is nearly every agent builder.
**Differentiation.** Inspect is Python and model-eval oriented; here the unit is an auditable run with tool-call trajectories and token cost from the journal.
**Learn.** Eval design (variance, N runs per case, why replay solves flakiness for non-model changes).
**First slice.** `-mode oneshot -q`, then `eval/cases/*.yaml` with a fixture dir and one shell-command check; scorer prints pass/steps/tokens.
**Downside.** Real-model runs cost money and are noisy; replay only tests things that do not change the model's inputs.

### 4. A shared background run (OpenCode 2.0 pattern), later

**Idea.** OpenCode (reported Oct 2026) turned into a background service that terminal, web and desktop clients attach to. A run that outlives the terminal is the same pain auto mode has (Ctrl+C, closed laptop lid, SSH drop).
**First slice.** None yet: do idea 1 first; a detached process (`setsid`) that writes the journal is the smallest version, and `status`/`follow` is the client.
**Downside.** Daemon lifecycle, auth and port handling are a lot of surface for a learning project. Skip until 1 exists.

**Recommendation.** Do **idea 3's first slice (oneshot + one eval case + replay)**, then idea 1's `status`. Reason: orchestrator mode multiplies workers, and without a pass-rate and a way to look at a run from outside, you cannot tell if more workers help. Tidy-ups can wait; no more renames this week.

## Part 2 — Landscape

Traction numbers come from search snippets of aggregator pages, not GitHub itself, unless noted. Treat as approximate.

**1. Vercel `durable-harness` (new; Oct 2026).** Open-source adapters that let existing harnesses keep sessions in a Workflow SDK "World" (storage, queue, stream behind one interface); switch to Postgres by two env vars ([Vercel](https://vercel.com/blog/making-every-harness-durable), [MarkTechPost, 2026-10-21](https://www.marktechpost.com/2026/10/21/vercel-durable-harness-crash-proof-coding-agents/)). Stars: unverified. *Lesson:* the three-part split (log, resume, stream) is a checklist for `state/`; idea 1.

**2. Temporal Agent Harness (new; Aug 2026).** Early, pre-preview open-source outer harness that wraps OpenAI Agents SDK, Pydantic AI and Gemini with durable execution, tool-call approval policies and typed interfaces ([Temporal blog, 2026-08-20](https://temporal.io/blog/temporal-agent-harness-durable-agent-infrastructure), [summary](https://daily.dev/posts/temporal-agent-harness-an-early-look-at-durable-agent-infrastructure-bqrctxdka)). Stars: unverified. *Lesson:* durability as an *outer* layer around any loop; this repo is the inner version of the same idea. Their need for a Temporal server is the cost you avoid with a file journal.

**3. DeepSeek Harness (`dsh`, MIT, developer preview, 2026-08-13).** "Everything is a plugin" on a kernel called Cordis: model adapter, tool registry, session log and the loop itself are replaceable. Append-only session log is the source and model history is derived from it. Sandbox per OS (bwrap/Landlock, Seatbelt, restricted tokens) and it fails closed; ships bridges for existing `hooks.json` ([The New Stack](https://thenewstack.io/deepseek-harness-open-source-plugins/), [Datacamp](https://www.datacamp.com/blog/what-is-deepseek-harness), [repo](https://github.com/deepseek-ai/deepseek-harness)). Stars: sources conflict (about 95k vs 170k+), so unverified. *Lesson:* "history is a view over the log" is exactly this repo's memory/context rule; the hooks bridge is idea 2.

**4. OpenCode (now `anomalyco/opencode`).** Terminal coding agent. One report says about 214k stars in mid-Oct 2026 and a 2.0 release on 2026-10-12 that made it a background service with several clients; I found no second source for that date ([tech-insider](https://tech-insider.org/opencode-160k-github-stars-2026/), [Star History](https://star-history.com/anomalyco/opencode)). *Lesson:* client/server split early; idea 4.

**5. Goose (moved to `aaif-goose/goose`, Linux Foundation's Agentic AI Foundation).** About 56.3k stars, 13 releases in 90 days, latest 1.57.0 (readings 31 Oct–6 Nov 2026) ([Enterprise DNA](https://enterprisedna.co/directories/open-source/goose), [repo](https://github.com/aaif-goose/goose)). MCP-first extensions. *Lesson:* neutral-foundation governance is where MCP-centred projects are going; MCP-first tool design keeps this repo's tool surface small.

**6. Flue (Astro team; `withastro/flue`, 1.0 beta).** A "describe what it knows, don't script what it does" harness framework on the Pi harness, running on Cloudflare's Agents SDK for durable execution; packages for runtime, CLI and client SDK; agents defined as files with models, sandboxes, tools, skills and durable sessions ([Cloudflare blog](https://blog.cloudflare.com/agents-platform-flue-sdk), [repo](https://github.com/withastro/flue)). Stars: unverified. *Lesson:* agents as declarative files is a good shape for a plan/skill format, and "harness" is now a product category on its own.

**7. CloudWeGo Eino (Go, ByteDance).** About 12–13.6k stars in snapshots from July–Nov 2026 ([Enterprise DNA](https://enterprisedna.co/directories/open-source/eino), [repo](https://github.com/cloudwego/eino)). Go LLM framework with graph orchestration and an ADK for agents. Also Google's [`adk-go`](https://github.com/google/adk-go) (1.0 released; star count unverified). *Lesson:* these are the Go competitors to compare against; both are broad libraries. This repo's niche is small, readable and auditable; Eino's graph/interrupt-resume checkpoints are worth reading next to `state/`.

**8. Inspect AI (UK AI Security Institute).** About 3.2k stars, +467 in the 61 days to 2026-11-09 ([Trendshift](https://trendshift.io/repositories/14500)). Python eval framework: tasks, solvers, scorers, agent and sandbox support ([docs](https://inspect.aisi.org.uk)). *Lesson:* idea 3's vocabulary. Notably, its stars are low next to harnesses: evals are the underbuilt part of the field.

**9. Mastra (TypeScript, YC W25).** About 29.5k stars, 496 contributors, +1,434 in 61 days to 2026-11-16 ([Trendshift](https://trendshift.io/repositories/12285)); 1.0 in January 2026. *Lesson:* TypeScript teams adopt a batteries-included framework with built-in evals and tracing; the bundled-eval pitch is a hint that evals sell.

**10. Codex CLI (revisit).** Latest release seen: rust-v0.162.1 on 2026-10-09, 16 releases in 30 days, per a third-party tracker ([havoptic](https://www.havoptic.com/tools/openai-codex)); stars unverified (sources disagree). *Lesson:* a very fast release cadence; the cheap thing to copy is small, frequent, changelogged releases.

### Takeaways

1. "Durable harness" is now a named category with three big players in three months; the three-part checklist (log, resume, stream) is a good scorecard. This project has two and a half.
2. Evals are under-supplied relative to demand (Inspect's low stars vs harness stars), so a small, journal-native eval is a real niche.
3. Go has framework-scale options (Eino, adk-go). Do not compete on breadth; compete on being readable and auditable.

## Sources

- https://vercel.com/blog/making-every-harness-durable
- https://www.marktechpost.com/2026/10/21/vercel-durable-harness-crash-proof-coding-agents/
- https://temporal.io/blog/temporal-agent-harness-durable-agent-infrastructure
- https://daily.dev/posts/temporal-agent-harness-an-early-look-at-durable-agent-infrastructure-bqrctxdka
- https://thenewstack.io/deepseek-harness-open-source-plugins/
- https://www.datacamp.com/blog/what-is-deepseek-harness
- https://github.com/deepseek-ai/deepseek-harness
- https://tech-insider.org/opencode-160k-github-stars-2026/
- https://star-history.com/anomalyco/opencode
- https://enterprisedna.co/directories/open-source/goose
- https://github.com/aaif-goose/goose
- https://blog.cloudflare.com/agents-platform-flue-sdk
- https://github.com/withastro/flue
- https://enterprisedna.co/directories/open-source/eino
- https://github.com/cloudwego/eino
- https://github.com/google/adk-go
- https://trendshift.io/repositories/14500
- https://inspect.aisi.org.uk
- https://trendshift.io/repositories/12285
- https://www.havoptic.com/tools/openai-codex
