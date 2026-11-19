# minimal-agent

2026-Sep-27
For a while now, I have had an idea evolving and developing in my head (yes, my own brain, not an AI's) of a simple AI agent whose usefulness is extended through capabilities. Here's what I mean:

First, we build a robust AI agent whose core is very simple, containing only the minimal components needed to run the agent. Let's call this the agent core, or agent engine, or agent kernel. I will use these terms interchangeably.

To make this agent engine useful and specific for doing tasks, we need to attach capabilities. Some of these more fundamental capabilities are what the developer community seems to refer to as harnesses, or harness components. A long-term memory harness that gives an agent a structure for remembering things across past conversations would be an example of such a fundamental capability. To avoid confusion, let's call these capabilities, and not harnesses. "Plugins" might also be a good term, but anyway, let's call these capabilities.

These capabilities are external to the agent, built by folks smarter than myself in the open source developer community. My current take is that we can attach these capabilities via an appropriate MCP interface to these capabilities, much like developing an API for web-based services. The burden would be on the developer of that capability to provide such an MCP interface (and test it of course!). With an MCP interface, these capabilities can be added to the agent with a simple 'mcp-add' command.

Note that I think we need to distinguish two different classes of capabilities. One class I will call "core-adjacent" capabilities. Examples of these would be like long-term memory, logging, general planning and thinking, audit and verification, and safety. These core-adjacent capabilities are primarily called by the agent kernel at specific points in the agentic loop. Core-adjacent capabilities are for the most part useful for all agents, and likely specific exposed methods are called within these capabilities at fixed points in the loop.

The other class I will call "peripheral" capabilities. Examples of these would be web search, domain-specific or user-specific tools or services, or specific resources like a user's email inbox or a company database. Even another agent could be an example. As inferred from these examples, these peripheral capabilities are useful for a particular instance or type of agent, and contribute to making an agent more specialized or "personal". As opposed to core-adjacent capabilities, peripheral capabilities are provided as context to the language model, and the model decides to run specific exposed methods within these capabilities, if and when they are needed.

By de-coupling core-adjacent and peripheral capabilities from the agent core, a simple AI agent becomes very flexible and extensible, and becomes as useful and specific as the attached capabilities. Also in this way, the agent gets better at doing tasks as the capabilities improve. Version control for these agents becomes a matter of locking the version of the agent engine / kernel itself, the type and version of the model(s) used, the added capabilities, and perhaps a few other components I haven't thought through yet. But the idea is that this version is like a package-lock.json but for agents.

Defining what goes into the agent core and what is deferred to a separate capability is what I'm trying to figure out in more detail right now. I'm fairly new to AI agents so I'm learning as I go along. Some of the examples above are what I've thought about so far.

So at a high-level, I think these are the things that the agent core should be responsible for:

- LLM calls: basic request-response loop to the agent's language model(s)
- Context Management: this is the context for a running agent session (a task or a conversation). This context is what gets passed on each request to a language model.
- Subagent Management and Lifecycle: when and how to call subagents, what instructions, arguments and context to pass to subagents, handling responses from subagents, etc.
- ReAct Loop and Task-State Management: reading current task state, setting up request to language model, taking action based on response from language model (which may be for example to call a tool via MCP), updating task-state based on action, and repeating until a determined goal is reached (e.g., answering the user's question).

The things that could be handled by external capabilities are numerous. Examples might be:

- Long-term Memory: a service that maintains a history of past conversations / sessions, within some data structure and some storage format (flat files, database, object storage)
- Logging: log of actions taken by the agent, tool call requests and responses, errors, etc
- Planning and Thinking: more sophisticated or specialized reasoning, planning or thinking; the planning and thinking capability itself may call a language model. An example would be if there is a very specific scientific question that a dedicated planner and thinker for scientific questions would reason through better than the default model of the agent.
- Other Agents: you could imagine other agents could expose their capabilities via an MCP interface, and thus could be called by an agent
- Verification and Audit: double-checks if a task-state has reached its goal, and audits for correctness or compliance
- Safety: checks if requests or actions are potentially dangerous or malicious BEFORE they are executed
- Web Search: whenever up-to-date information is needed to perform a task or answer a question


---

2026-Nov-12 — Checkup and direction for the next epic (written by Claude at Jerry's request)

**Where things stand.** The root build is ~15k lines of Go plus ~26k of tests. It has a solid chat loop (Ctrl+C
semantics, timeouts, four context-window strategies, compaction), a hardened file-tool suite inside a `Workspace`,
an opt-in `run_command`, MCP over stdio and HTTP (connected in parallel), a deny/ask/run approval gate, JSONL tracing
with redaction and digests, and plan mode (objective chat → `propose_plan` → `<objective>.plan.md`). Autonomous mode
is next in line (`state/` package, then a serial run), with mvp3/mvp4 as references.

Measured against the three goals:

1. *Useful to users:* not yet a daily driver. Only an OpenAI-compatible provider (`model/anthropic.tmp` is a stub), no
   streaming, no prompt caching, sessions don't survive a restart, no project instructions file (`AGENTS.md`), and
   `oneshot`/`headless` are advertised but unimplemented, so it can't be scripted or put in CI.
2. *Extensible:* the packages are well split, but the *extension points a non-author can use* are thin. Outside MCP
   servers and config rules, extending means editing `SetupAgent`. `hooks.Hook` has one method (`BeforeTool`),
   in-process only. No skills, no subagents, no after-tool / turn / compaction events.
3. *Learning:* the deepest learning so far has been OS-level safety (TOCTOU, process groups, symlink races) and loop
   mechanics. That was real, but the file tools have hit diminishing returns. The big gap is that **nothing measures
   whether the agent got better**: plan-mode prompts are tuned by reading transcripts, and "try against a real model"
   is still an open TODO.

**How the Sep-27 idea held up.** The core / capability split matches where the field went, with one correction:
core-adjacent capabilities (logging, safety, verification) ended up as *lifecycle hooks* (in-process, or an
external command taking JSON on stdin), not MCP servers, because they need to run synchronously at fixed points
and be able to veto. MCP won the *peripheral* side. Skills (a `SKILL.md` whose name and description sit in the prompt,
with the body loaded only when needed) became a third kind: cheap, prompt-only capabilities. Subagents are just a
tool that runs another loop with fresh memory. The "package-lock for agents" idea is still open and still good; the
`Environment` / `Role` hashes planned for `state/` are its first piece.

**Directions (roughly in recommended order).**

A. **Measure first: oneshot mode + a small eval harness.** Implement `-mode oneshot -q` (and `--json` output),
   then an `eval/` runner: each case is a prompt + a fixture workdir + checks (files exist, a command passes, an
   answer matches), run N times, scored from the trace (pass rate, steps, tokens, cost, vetoes). Use it straight
   away on open questions: which window strategy, whether `planModel` is worth it, whether a prompt change helped.
   *Why:* this is the skill that separates agent infrastructure people from feature builders, it makes every later
   epic cheaper to judge, and autonomous mode's per-task "Check" is the same idea. *Downside:* real-model runs cost
   money and are noisy; you need several runs per case before differences mean anything.

B. **Make it a daily driver.** An Anthropic adapter (system prompt as a field, thinking blocks replayed verbatim,
   tool results batched, `cache_control` on the stable prefix), streaming output, saved sessions with `-resume`,
   `AGENTS.md` loaded into the system prompt. *Why:* using it yourself is the fastest source of real problems, and
   the provider differences (caching, thinking, `tool_choice`) are core infra knowledge. *Downside:* mostly known
   ground; mvp1 already did an Anthropic adapter.

C. **Open the extension surface.** Lifecycle events (`BeforeTool`, `AfterTool`, `TurnEnd`, `BeforeCompact`) with
   both Go and external-command hooks; skills from a directory (`list_skills` / `load_skill`, or names in the
   prompt); subagents as a tool with their own memory and the *shared* policy hooks. *Why:* this is goal 2 directly,
   and designing a stable hook contract (what's in the payload, ordering, timeouts, can it change arguments?) is a
   real API-design lesson. *Downside:* every public contract becomes something you must keep compatible.

D. **Autonomous mode (the planned epic).** `state/`, a serial director/worker run, resume, idempotency keys.
   *Why:* long-running work is where agents are headed, and checkpoint/resume is a genuinely hard infra problem.
   *Downside:* the biggest epic of the four, mvp3/mvp4 already explored it, and without A you can't tell whether
   a director prompt or a retry policy actually works.

E. **(Later) Real sandboxing for `run_command`.** Seatbelt on macOS, Landlock/namespaces or a container on Linux.
   The arg guard is documented as "not a boundary"; this would make one. Good learning, low priority until others
   use the tool.

**Recommendation.** Make the next epic **A, with the small parts of B it needs** (oneshot, the Anthropic adapter so
evals can compare providers, prompt caching). Then do D on top of it, using the harness to judge the director and
the plans. Do C after that, when you have your own hooks/skills to write against it rather than guessing at a
contract. Smaller habits: keep each epic to a page of plan and measure its result; DECISIONS.md is at ~4.8k lines
and the test-to-code ratio is ~1.7:1, so lean toward fewer, more meaningful tests and shorter decision entries.
