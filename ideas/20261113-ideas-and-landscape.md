# Ideas & landscape — 2026-11-13

## TL;DR

- Since yesterday's report, nothing material shipped: `origin/feat/autonomous-mode-2` has one new commit ("small fixes to improve plan mode UX") on top of PR #42. The "plan mode is polished, no run exists yet" drift flagged on 11-12 still stands.
- Yesterday's recommendation (a `state/` run journal, then replay) is unchanged. This report adds new ideas aimed at the *next* step after it: durable approvals, a context hand-off at task boundaries, and a "dry run".
- Landscape is rotated: Hermes Agent (self-improving skills), TrueForge (sandbox-as-a-tool server), HKUDS OpenHarness, Charm Crush and CloudWeGo Eino (Go), plus a framework-by-framework look at how LangGraph, Mastra, Pydantic AI and the OpenAI Agents SDK handle crash recovery.
- Key finding: every mainstream framework either checkpoints per step in a database (LangGraph, Mastra) or hands durability to an external engine (Temporal, DBOS). Their documented failure modes are all about re-running the interrupted step. That is the exact problem a file journal with an idempotency record can own.
- Recommendation: stop adding plan-mode surface. Build step 3 (one serial run) with `state/` as an append-only journal, and make "the interrupted step re-runs safely" the first property you test.

## Part 1 — Direction

### What shipped, and did it help?

Recent `origin/build` work: `run_command` with deny/ask rules and an argument guard (#37, #38), concurrent MCP connects (#40), then plan mode (#42): `objective/`, `plan/` with a `gate` field, an objective chat, a planner, `Accept?`. The only newer commit is UX polish on the feature branch. SESSION.md's last entry (2026-11-09) says `state/` is still a skeleton awaiting review and the next step is "one complete serial run".

Against the thesis (durable, auditable, measurable): **auditable** improved (deny/ask/run levels, traced vetoes, reviewable plans). **Durable** and **measurable** did not move. `MaxTokens` is still parsed but unused. Candidly: the repo is still drifting toward formats and documentation (DECISIONS.md is huge) rather than runtime behaviour. Plan mode is well made, but nothing consumes the plan yet. mvp3/mvp4 prove the run loop works; the root build has not caught up.

### Fresh ideas (new relative to 11-12)

**1. Approvals as a durable paused state.**
- *What:* when a task has a `gate`, or a tool call hits an `ask` rule with nobody at the terminal, the run writes "waiting for approval: <what>" to the journal and exits cleanly. `-mode resume` shows pending approvals, takes y/n, and continues.
- *Who:* anyone running long or overnight agents, or on a server without a TTY. Today `AskYesNo` only works while a person is watching, which is most real autonomous use cases failing.
- *Differentiation:* Claude Code and Codex CLI block on a prompt. Pydantic AI does this via deferred tool results and Agentspan keeps paused approvals in a server; no small Go harness does it with a plain file.
- *Learn:* serializing "everything needed to continue" and what is not serializable (open MCP connections, in-flight tool output).
- *First slice:* `ErrNeedsApproval` with no prompt function writes a `pending` record and returns a distinct exit code; `-mode resume -approve <id>` finishes the call.
- *Downside:* resume must rebuild context from the journal, so it forces the journal design to be right first.

**2. Context hand-off at task boundaries.**
- *What:* when a plan task completes, the worker's context is rebuilt from the objective, the plan, and a short summary of finished tasks, not the raw transcript. Tacklr (below) does this at each `complete_todo`.
- *Who:* anyone whose long runs degrade as context fills. This is the practical form of "long-horizon runs" in THOUGHTS.md.
- *Differentiation:* compaction in the big CLIs is automatic and opaque; here the hand-off is a visible, tested artifact you can diff.
- *Learn:* what a task summary must contain so the next task does not redo or contradict work.
- *First slice:* a `ContextBuilder`-style strategy in `contextwindow/` that starts each task from a summary; measure tokens per task against the plain window on the same objective.
- *Downside:* summaries lose detail; a bad one silently poisons later tasks. Needs the eval idea to know.

**3. `--dry-run` for a whole plan.**
- *What:* run a plan with the real model but with every write tool and `run_command` replaced by a recorder that says "would do X" and returns a canned success. HKUDS OpenHarness lists an unreleased `--dry-run` preview, which suggests demand.
- *Who:* people who want to trust an agent before letting it touch a repo.
- *Differentiation:* "plan mode" in other tools blocks writes; it does not run the *whole* plan to show what it would do.
- *Learn:* how far a model can proceed against faked tool results, and where it notices.
- *First slice:* a `tools.Tool` wrapper that implements `Call` as record-and-return, selected by a flag.
- *Downside:* later steps depend on earlier real effects (a file that was never written), so long dry runs go off the rails. It shows intent, not outcome.

**4. Idempotency record for side effects (carried over, sharpened).**
- Boltrope's dedup ledger and the Mastra issue below (an auto-recovery mode that "can reissue LLM calls and re-execute tools") show this is the hard bit of resume. First slice: each tool call gets an ID from (run, task, step); the journal records started/finished; on resume, a "started, not finished" write tool asks the user instead of re-running. *Downside:* a shell command cannot be made idempotent; you can only warn.

**5. A skills layer, measured (Hermes-style, but small).**
- *What:* after a run, the agent proposes a reusable note ("how I solved X") saved as a markdown skill file, which later runs may load. Score it: does a second run of a similar objective take fewer steps?
- *Learn:* long-term memory as files, plus the honest result of whether it helps. Mastra's text-memory approach (see 11-12) is the comparable.
- *Downside:* self-written notes can encode mistakes. This only makes sense after an eval harness exists.

### Recommendation

**Do step 3 now: one serial autonomous run at the root, with `state/` as an append-only journal, and make "interrupted mid-task, then resumed" the first test.** Why: ideas 1, 2 and 4 above, and replay and eval from yesterday, all read that journal. Without it each is a design on paper. Pause new plan-mode work (`/plan` in chat, `Run now?`) until a plan can run. This is the same advice as 11-12 because nothing has changed; the risk is another week of polish.

## Part 2 — Landscape

Star counts come from third-party pages and are approximate unless stated; "unverified" means I could not confirm a figure. Not repeated from 11-12 unless noted: Pi, Tacklr, Boltrope, DBOS, Agentspan, DeepAgents, Goose, OpenHands, Mastra, Inspect.

**1. Hermes Agent (NousResearch/hermes-agent, Python, MIT)** — launched 2026-02-25. Its README calls it "the only agent with a built-in learning loop": it writes reusable skills from experience, has a tiered memory, and a companion repo evolves prompts with DSPy/GEPA. Traction is the largest of any new project: about 113k stars on 2026-04-24 and roughly 212k in mid-July (Gradually.ai); sources for late July disagree (214k–220k), so treat it as "over 200k, unverified exact" ([ossinsight](https://ossinsight.io/analyze/NousResearch/hermes-agent), [gradually.ai](https://www.gradually.ai/en/hermes-agent-statistics/), [heyuan110 review](https://www.heyuan110.com/posts/ai/2026-04-24-hermes-agent-v010-deep-review/)). One review argues its novelty is economic, not technical. *Borrow:* the skill-as-markdown learning loop (idea 5), but only with a measurement.

**2. TrueForge (TrueFoundry, TypeScript, MIT)** — announced 2026-08-18 as a vendor-neutral alternative to Claude Managed Agents. A server runs the loop, streams steps, pauses for approvals, and persists sessions across reconnects. The sandbox is a *tool* that starts only when code must run (Daytona is the launch partner). Tracing is not built in; it is sold through their separate AI Gateway. The benchmark claiming parity with Claude Managed Agents at 30–75% lower cost is the vendor's own. Star count not stated on the post (1.8k per an aggregator, unverified) ([TrueFoundry](https://www.truefoundry.com/blog/engineering/trueforge-open-source-agent-harness), [AI Weekly](https://aiweekly.co/alerts/truefoundry-open-sources-trueforge-a-claude-agents-rival)). *Borrow:* sandbox as a lazily started tool, and the lesson that tracing is where vendors monetise, which is the niche this project wants to own.

**3. HKUDS OpenHarness (Python)** — an inspectable research harness: 43+ tools, permission modes (default/auto/plan), path rules, Pre/PostToolUse hooks, auto-compaction, a MEMORY.md and resumable sessions. 16.2k stars, latest release v0.1.7 on 2026-04-18 (GitHub page fetched today; an unreleased `--dry-run` is listed) ([GitHub](https://github.com/HKUDS/OpenHarness)). Its design is close to a Claude Code clone, so there is little new structurally. *Borrow:* the `--dry-run` idea (idea 3); otherwise a reminder that a hooks-plus-permissions loop is now the baseline, not a differentiator.

**4. Charm Crush (Go, Functional Source License)** — terminal coding agent in Go. Stars vary by tracker, roughly 21k–28k (27.7k on a directory snapshot with an August 2026 update); one listing shows v0.50.1 on 2026-03-17; no confirmed newer release found ([amplifying.ai](https://amplifying.ai/coding-agents/crush), [awesome.ecosyste.ms](https://awesome.ecosyste.ms/projects/github.com%2Fcharmbracelet%2Fcrush)). The license is not OSI open source (it converts to MIT later). *Borrow:* proof that a Go terminal agent can gain a real audience; note the licence choice as a trade-off if this project is ever published.

**5. CloudWeGo Eino (Go, ByteDance-origin)** — Go LLM application framework with an Agent Development Kit (`eino/adk`), graph orchestration and callbacks. About 13.2k stars and version 0.9.22 in October 2026 on one directory; another snapshot shows 12.7k undated ([Enterprise DNA](https://enterprisedna.co/directories/open-source/eino), [overview](https://www.cloudwego.io/docs/eino/overview/)). *Borrow:* its callback points (before/after each node) are a worked example of an observation axis; compare with your `hooks/`. It is also the evidence that a graph-shaped Go framework exists, so a *loop-shaped* kernel is a deliberate difference.

**6. LangGraph (Python)** — the durability reference. Per a hands-on comparison tested on `langgraph==1.2.14`: a checkpoint per super-step keyed by `thread_id`; only `durability="sync"` survives a mid-run crash; the interrupted node re-runs from its start; resuming from `interrupt()` also restarts the node, so code before the interrupt runs again; recovery is not automatic, you call `invoke(None, config)`. Version 1.2.12 (2026-09-21) added `response_schema` to `interrupt()` ([DevelopersDigest](https://www.developersdigest.tech/blog/durable-ai-agent-frameworks-compared-2026)). Stars unverified. *Borrow:* the semantics "completed steps skipped, interrupted step re-runs" as the written contract of your journal.

**7. Mastra durable agents (TypeScript)** — `@mastra/core@1.45.0`, beta. Default recovery keeps only pending/paused runs; `recovery.durableAgents: 'auto'` re-drives orphaned runs and "can reissue LLM calls and re-execute tools", with multiple replicas able to race. An issue filed 2026-07-07 reported silent loss of in-flight runs on deploy, fixed 2026-07-12 ([DevelopersDigest](https://www.developersdigest.tech/blog/durable-ai-agent-frameworks-compared-2026)). *Borrow:* a candid list of what goes wrong; copy it as your resume test cases (lost in-flight run, double execution, two resumers).

**8. Pydantic AI and the OpenAI Agents SDK (Python)** — neither has its own checkpoint store. Pydantic AI attaches Temporal or DBOS (v2.54.0 on 2026-10-02 blocks two durable engines on one agent) and does approvals with deferred tools: the run ends with `DeferredToolRequests` and a new run continues with `DeferredToolResults`. The OpenAI SDK captures `RunState` only at interruptions, serialized with `to_json()`; a crash loses the turn unless Temporal, Restate, DBOS or Dapr is used; `RunState.from_json()` does not authenticate data ([DevelopersDigest](https://www.developersdigest.tech/blog/durable-ai-agent-frameworks-compared-2026)). *Borrow:* the "run ends with a typed pending-approval value; a *new* run continues" model, which is the cleanest shape for idea 1, and the warning to authenticate saved state.

**Revisits worth a line.** DeepSeek Harness: the New Stack page returned 403 today, and a search snippet claims 33k stars within hours of an August 2026 launch (MIT, Node.js, swappable model/tool/loop plugins); still unverified ([search result](https://thenewstack.io/deepseek-harness-open-source-plugins/)). Vercel's AI SDK 7 (2026-06-25) added a `WorkflowAgent` where each tool call is a durable step and approval is a `needsApproval` property on the tool (same DevelopersDigest source).

### Takeaways for minimal-agent

- Durability is now table stakes in frameworks, but each one documents the same weak spot: the *interrupted step* re-runs. A harness that records started/finished per tool call and refuses to blindly re-run a write has a concrete, testable edge.
- Approval is moving from "blocking prompt" to "typed pending value, resumed by a new run" (Pydantic AI, Agentspan, TrueForge). Your `ErrNeedsApproval` is already shaped like this.
- Hermes shows the audience wants self-improving agents; nobody there publishes a measured result. Pair any such feature with the eval idea.
- Go has real projects (Crush, Eino), but none are small, file-based and audit-first.

## Sources

- https://ossinsight.io/analyze/NousResearch/hermes-agent
- https://www.gradually.ai/en/hermes-agent-statistics/
- https://www.heyuan110.com/posts/ai/2026-04-24-hermes-agent-v010-deep-review/
- https://www.truefoundry.com/blog/engineering/trueforge-open-source-agent-harness
- https://aiweekly.co/alerts/truefoundry-open-sources-trueforge-a-claude-agents-rival
- https://github.com/HKUDS/OpenHarness
- https://amplifying.ai/coding-agents/crush
- https://awesome.ecosyste.ms/projects/github.com%2Fcharmbracelet%2Fcrush
- https://enterprisedna.co/directories/open-source/eino
- https://www.cloudwego.io/docs/eino/overview/
- https://www.developersdigest.tech/blog/durable-ai-agent-frameworks-compared-2026
- https://thenewstack.io/deepseek-harness-open-source-plugins/
- Earlier report: `origin/ideas:ideas/20261112-ideas-and-landscape.md`
- Repo: SESSION.md, DECISIONS.md, THOUGHTS.md, `git log origin/build`, `git log origin/feat/autonomous-mode-2`
