# Ideas & landscape — 2026-11-20

## TL;DR

- Autonomous mode finally shipped: PR #44 is merged into `origin/build` (journal, per-plan lock, resume after a crash, director/worker run, plan-then-run). This is the first real thesis work in a week of reports, and it is the "durable, auditable" half of the thesis.
- `feat/token-usage-tracking` (3 commits) is the first "measurable" step: `Provider.Chat` now returns a `model.Response` (message, token usage, stop reason), `maxTokens` is sent, and usage shows up in the trace and digest. The auto-mode journal does not record it yet (that is step 2d in SESSION.md).
- Biggest new idea: **replay a journal as an eval.** The journal already records every tool call before and after it runs. Re-run a finished run against a fake provider fed from the journal, and you get deterministic regression tests and a cost number without paying for a model call.
- The field is converging: Pydantic AI now treats durability as a plug-in (nine engines, plus its own per-step snapshots), and its evals grade tool-call trajectories, not just answers. That is the same shape as this project's journal plus the planned eval harness.
- Recommendation: finish usage in the journal (2d), then build `-mode oneshot` and a replay-based eval runner. Do not start orchestrator mode yet.

## Part 1 — Direction

**What shipped.** Since the 11-19 report, `build` gained PR #44: `state/` (append-only journals, `Fold`, OS file lock), a fresh worker per task, a director LLM that judges each task, `tool_before`/`tool_after` journal events, crash resume (cut-off calls are settled first, and it asks before re-sending if tools changed), atomic file writes, and exit codes 0/1/2/3/130. The review round also made a `tool_after` that cannot be written stop the run. That is good: "not recorded = not run" is a real audit property. On the branch, token usage and stop reasons are now parsed from the server's reply and shown in the digest.

**Did it move toward the thesis?** Yes. Durable and auditable are now mostly true for auto mode. Measurable is half done: you can see tokens per turn, but nothing compares two runs, and nothing says whether a director prompt change helped. Earlier reports spent five days on sandboxing; the code did not move on that, and that is fine, because the real work was here. The risk is now the opposite: plans and DECISIONS.md keep growing faster than evidence from real model runs.

Earlier reports already cover sandboxing, DLP, canaries, policy as data and the patch as the boundary. I do not repeat those. Fresh ideas:

### 1. Journal replay as a deterministic eval

- **Idea:** a `-mode replay -run NNNN` that rebuilds the conversation from a finished run's journal and traces. A fake provider answers with the recorded model messages. Tools either return recorded results or run for real against a fixture copy. You then check that the same journal produces the same verdicts and the same stop reason.
- **Who and why:** anyone changing a prompt, a window strategy or a retry policy on a long-running agent. Today they change it and hope. Replay gives a free regression suite built from real runs.
- **Differentiation:** Claude Code and Codex CLI keep transcripts but do not offer a "re-run this against the new harness code" mode. LangGraph can replay from a checkpoint ("time travel"), but it replays state, not a fixed-model regression check.
- **Learn:** what a journal must contain to be replayable (are model messages stored whole? are tool results stored whole or cut?), and where nondeterminism leaks in (time, ordering of parallel tools, file state).
- **First slice:** one test that reads a finished run's journal, feeds the recorded assistant messages to `modeltest`'s fake provider, and asserts the final text. About 100 lines.
- **Downside:** the journal cuts or hashes large bodies, so some runs cannot be replayed fully. Replay checks the harness, not the model.

### 2. Cost and step budgets as a first-class stop reason

- **Idea:** finish 2d, then add `maxRunTokens` and `maxRunSeconds` as stop reasons in the journal, with exit code 3 or a new one. The usage event is the fold input.
- **Who:** people who leave an agent running overnight. A runaway director loop is the most common horror story and the cheapest to prevent.
- **Differentiation:** most CLIs show cost after the fact. A journal-backed budget survives a crash and resume (the totals carry over because `Fold` rebuilds them).
- **Learn:** accounting across process restarts, and what happens to a half-finished call when a budget trips.
- **First slice:** one `usage` event per model call, folded into a per-run total, and a hard stop when it passes a number.
- **Downside:** local Ollama servers often report no usage, so the budget must also count steps or seconds.

### 3. Failure taxonomy from the journal

- **Idea:** classify every failed task attempt automatically: tool error, veto, hit `ErrMaxSteps`, cut off by `maxTokens`, director said failed, check failed, timeout. Print a table per run and per plan.
- **Who:** the author first; later anyone comparing models. The stop reason now in `model.Response` is the missing input.
- **Differentiation:** most harnesses give you a log. A table of "why did my runs fail" turns logs into a to-do list.
- **Learn:** designing a small closed set of reasons that stay stable across versions.
- **First slice:** a `-mode report` that folds one run and counts reasons.
- **Downside:** the categories will be wrong at first. Keep an "other" bucket and read it.

### 4. A "resume safety" test matrix

- **Idea:** crash the process at every journal-write point (kill after N bytes, after each event type) and check that resume ends in a valid state. This is fault injection for the thing PR #44 built.
- **Who:** anyone who needs to trust "resume after a crash". It is rare to have it tested systematically.
- **Differentiation:** the durable engines below get this from their runtimes; a hand-rolled journal has to prove it.
- **Learn:** the exact guarantees (at-least-once vs at-most-once tool calls) and where idempotency keys are needed.
- **First slice:** a test helper that truncates the journal at each line boundary and mid-line, then runs resume against a fake provider.
- **Downside:** time spent on a corner that real users may never hit.

### Recommendation

Do 2 (finish usage in the journal), then 1 as the first eval-harness slice, with `-mode oneshot` alongside it. Reasons: the stop reason and usage fields just landed, so the data exists; replay is the cheapest eval (no model cost, no noise); and it tests the journal that PR #44 made the centre of the project. Hold orchestrator mode: mvp4 is a working reference, and running N workers at once multiplies the questions about shared budgets that 2 will answer. Candid note: the project is not drifting. The one habit to watch is writing plans for things (orchestrator, `/reload`, more sandbox ideas) before the last thing has been measured against a real model.

## Part 2 — Landscape (fetched 2026-11-20)

Rotation: the 11-15 to 11-19 reports covered sandboxes, egress proxies and security tooling. This one covers durable execution, evals, Go frameworks and big harnesses. Star counts are from the GitHub pages read today. Release and last-commit dates did not render on those pages, so they are "unverified" below.

**1. Pydantic AI (MIT).** 21.3k stars ([GitHub](https://github.com/pydantic/pydantic-ai)). Python agent framework. The README lists nine durable-execution engines (Temporal, DBOS, Prefect, Restate, AWS Lambda, Airflow, Absurd, pgtask, Azure Durable Task), the first five co-maintained with the vendors. It also has `StepPersistence` in its Harness package, which snapshots each settled step so a run can be saved, resumed and forked; the README says an engine is only needed to recover a crash in the middle of a step. `pydantic_evals` grades final outputs and tool-call trajectories, on code datasets or on sampled live traffic. Built-in human approval for tools. *Borrow:* the split between "snapshot per settled step" (cheap, local) and "engine for mid-step crashes". That is exactly where the journal sits.

**2. Restate (server in Rust; Go SDK exists).** 4.7k stars ([GitHub](https://github.com/restatedev/restate)). A separate server persists key-value state together with execution progress, so a handler resumes after failure. License and latest release: unverified. Adopters named in a vendor post: Replit uses it for agent orchestration ([Restate](https://restate.dev/what-is-an-agent-runtime); vendor source, July 2026). *Borrow:* the idea that every side effect is a recorded step, so replaying a handler returns the old result instead of redoing the call. That is the model for idempotent resume.

**3. DBOS Transact for Go (MIT).** 876 stars ([GitHub](https://github.com/dbos-inc/dbos-transact-golang)). A library, not a server: workflows checkpoint to Postgres (SQLite is also supported by URL), steps run through `dbos.RunAsStep`, and a workflow resumes from the last completed step after a restart. *Borrow:* a Go library that makes durability a few function calls. Reading its step table is a good check on `state/`'s event design. It is a real option if the journal ever needs to be queried across many runs.

**4. Inspect AI (UK AI Security Institute, MIT).** 3.3k stars ([GitHub](https://github.com/UKGovernmentBEIS/inspect_ai)). An evaluation framework: built-in prompt, tool-use, multi-turn and model-graded components, 200+ prebuilt evals, a web viewer for logs. The README did not cover tasks, solvers, scorers or sandboxes, so I do not describe them. *Borrow:* the shape of an eval log as a file you can open in a viewer, separate from the run itself. Study its log format before designing the project's `eval/` output.

**5. Eino (CloudWeGo, Apache-2.0).** 13.7k stars ([GitHub](https://github.com/cloudwego/eino)). A Go agent framework. A `ChatModelAgent` runs the ReAct loop; `compose` builds graphs; the README says any agent or tool can pause for human input and resume from a checkpoint. It also ships a DeepAgent that coordinates sub-agents. Sandboxing: unverified. *Borrow:* it is the nearest Go neighbour. Compare its checkpoint interface to `state.Journal` to see what a more general design would look like, and what this project's append-only choice gains (simple, auditable) and loses (no graph resume).

**6. LangGraph (MIT).** 45.2k stars ([GitHub](https://github.com/langchain-ai/langgraph)). Low-level orchestration for stateful agents; README claims durable execution, short- and long-term memory, and interrupts for human review. DeepAgents sits on top (31.4k stars on 11-19, per the earlier report). The checkpoint mechanism is unverified from the README. *Borrow:* "interrupt, inspect, edit state, continue" as one user-facing verb. The project's exit code 3 ("needs a person") is the start of it; a `-mode resume --edit` would complete it.

**7. OpenClaw (MIT).** 394.9k stars, 82.9k forks ([GitHub](https://github.com/openclaw/openclaw)). A personal-assistant gateway: a local control plane for sessions, tools, events and channels; models and agent harnesses (Claude, Codex, local) are plugins; state and credentials stay on your hardware; the README says tools run on the host for the main session unless sandboxing is configured. Agent loop and checkpointing: unverified. *Borrow:* that default (host, opt-in sandbox) is the same trade-off as `run_command` here. The traction number shows demand for local, self-hosted agents; it is not evidence about the design.

**8. DeepSeek Harness ("dsh"), MIT, developer preview.** Repo URL and stars: unverified (the HPCwire article returned 403). A search snippet citing TheNewStack (2026-08-13) says the model adapter, tool registry and agent loop are all swappable plugins, and that extension contracts may break during the preview ([search result summary](https://app.dealroom.co/news/feed/deepseek-releases-open-source-agent-harness-with-plugin-based-architecture)). This is secondary, so treat it lightly. *Borrow:* the same "everything is a plugin" story as `agent/` wiring capability packages; worth reading once the repo is confirmed.

Also noted, not read: Microsoft Agent Framework Harness (blog post dated 2026-07-22), Diagrid Catalyst 2.0 (2026-07-28), Temporal's reported $550M Series E (2026-09-14, from a search summary of a vendor-heavy result set). All unverified.

### Takeaways

- Durability is becoming a plug-in layer under every framework. A small local journal plus per-step snapshots is a legitimate design, and Pydantic AI's README says so in its own way.
- Evals that grade the *trajectory* (which tools, in what order) are standard now. The journal already holds trajectories.
- The Go agent-framework space is thin: Eino is the one large project found today. A small, auditable, journal-first Go harness is still an open niche.

## Sources

- Pydantic AI: https://github.com/pydantic/pydantic-ai
- Restate: https://github.com/restatedev/restate and https://restate.dev/what-is-an-agent-runtime
- DBOS Go: https://github.com/dbos-inc/dbos-transact-golang
- Inspect AI: https://github.com/UKGovernmentBEIS/inspect_ai
- Eino: https://github.com/cloudwego/eino
- LangGraph: https://github.com/langchain-ai/langgraph
- OpenClaw: https://github.com/openclaw/openclaw
- DeepSeek Harness (secondary): https://app.dealroom.co/news/feed/deepseek-releases-open-source-agent-harness-with-plugin-based-architecture
- Web searches (durable runtimes, harness releases): https://www.datacamp.com/blog/top-open-source-ai-agent-harnesses, https://infoq.com/news/2026/08/diagrid-catalyst-ai-agents
- Repo: `git log origin/build`, `git log origin/feat/token-usage-tracking`, `SESSION.md`, earlier reports on `origin/ideas`
