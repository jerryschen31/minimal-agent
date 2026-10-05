 ▐▛███▛█   Claude Code v2.1.289
▝▜██████▀  Opus 5.5 · Claude Pro
 ▝▝   ▝▝   ~/gh/public/minimal-agent


❯ Right now the latest minimal agent (mvp1/ version coded by coding agent,      
  @mvp2/learn/chat_w_history_context_session_mcp_tools.go version coded by      
  myself with agent as pair-programmer) is a typical chat agent that does a     
  single request/response cycle (or sometimes multiple responses to a request), 
  and goes back to a user prompt. This is the old "chat" way of interacting     
  with LLMs, and modern workflows are more and more automated and led my        
  agents. Brainstorm how I would have a usage mode that, instead of a user      
  prompt at each turn, uses a minimal agent that is AUTOMATED (given a set of   
  instructions and guidelines it can follow), sends requests and responds to    
  other agents, and itself uses a language model to reason. In other words, I'm 
  envisioning a headless workflow that doesn't require a human user to prompt   
  on each turn, but rather once kicked off programatically (go run myagent.go), 
  only needs a set of instructions and guidelines (provided in markdown files)  
  and works until the goal(s) within the instructions are done. What kinds of   
  features do I need to implement so that my minimal agent can work in this     
  way? Note that this is more than just an agent that delegates tasks to        
  subagents - the agent-to-subagent workflow is still dependent on a human user 
  prompt each turn.                                                             

  Listed 1 directory, ran 3 shell commands (ctrl+o to expand)

⏺ The good news is that the inner loop is already autonomous. Agent.Run in
  mvp1/agent/agent.go:141 and reActLoop in your mvp2 file already reason → act →
  observe with no human involved. What isn't autonomous is the loop around it:
  runLoop (chat_w_history_context_session_mcp_tools.go:1746) waits on stdin. So
  the work is to replace the jobs the human does in that outer loop, not to
  change the ReAct kernel.

  What the human does now, and what replaces each job

  ┌───────────────────────────────────┬─────────────────────────────────────┐
  │       The human currently…        │        Headless replacement         │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ States the goal                   │ A mission file (instructions.md)    │
  │                                   │ loaded at startup                   │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ Decides whether to keep going     │ A driver loop that picks the next   │
  │ after each answer                 │ input on its own                    │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ Decides when the work is done     │ Explicit completion plus a verifier │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ Approves risky actions            │ A policy that never blocks, plus an │
  │ (hooks.Approval reads stdin)      │  escalation channel                 │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ Notices when it's stuck or        │ Budgets and no-progress detection   │
  │ burning money                     │                                     │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ Is the only one sending it        │ An inbox that other agents write to │
  │ messages                          │                                     │
  ├───────────────────────────────────┼─────────────────────────────────────┤
  │ Remembers what happened yesterday │ Durable state and resume            │
  └───────────────────────────────────┴─────────────────────────────────────┘

  Each row below is a feature, roughly in the order I'd build them.

  The features

  1. Mission file (the smallest step)

  go run myagent.go -mission mission.md. Split the markdown into:
  - Goal and success criteria, which become the first user message ("Done means:
    tests pass, report.md exists, …").
  - Guidelines and constraints, which go in the system prompt.
  - Optional YAML frontmatter for machine settings: max_steps, max_cost_usd,
    allowed_tools, workdir.

  Trade-off: markdown is easy for people to write but loose for the program to
  read. Frontmatter keeps the parts the program enforces strict and leaves the
  rest as prose. Never rely on the model to enforce a budget that's written only
  in prose.

  2. Explicit completion (the most important design choice)

  In chat mode, "the reply has no tool calls" means done (agent.go:166).
  Headless, that's ambiguous. A text-only reply could mean "finished", "I have a
  question for nobody", or "here's my plan, shall I continue?". Weaker models
  do the last one a lot.

  Add a finish(status, summary) tool with status ∈ {done, blocked, failed}. That
  turns a text-only reply into a "you're not finished" case, and the driver
  sends a nudge: "No human is present. Continue toward the goal, or call 
  finish."

  Optionally add a verifier before accepting done. It can be a deterministic
  check (run the tests, check a file exists) or an LLM judge comparing the
  result against the success criteria. A rejected done comes back to the model
  as an observation, the same way your other errors do.

  3. Driver loop (the new layer above the kernel)

  for !finished && withinBudget {
      input := nextInput()   // first: the mission. Then: a nudge, an inbox 
  message, or the next todo
      agent.Run(ctx, input)
  }
  This is a new layer, not a sixth interface inside agent/. The kernel stays as
  it is, which matches the rule in CLAUDE.md. The interesting part is
  nextInput(). Making it an interface (an InputSource) gives you every kickoff
  mode with one abstraction: a one-shot mission, a cron tick, a file watcher, or
  messages from other agents.

  4. Budgets and stuck detection

  With nobody watching, a loop that never ends is the most likely way this
  fails. Add limits on total steps (not per Run), tokens or cost, and wall-clock
  time. Also detect "no progress": the same tool call with the same arguments N
  times, or N turns without a todo item changing. All of this fits as Hooks. A
  hook that fails with "budget exceeded" ends the run with status: failed and a
  reason.

  5. A plan the agent writes down

  Long runs outlive the context window. Even with your compaction, the plan gets
  summarized away. Give the agent a todo tool (add/complete/list) backed by a
  file, and show the current list each turn. Claude Code does exactly this. It
  also gives you a progress signal for #4, and #7 can resume from it.

  6. A safety policy that never blocks

  hooks.Approval waits on a.In.ReadString('\n'). Headless, that hangs forever.
  Replace it with:
  - an allowlist or denylist (already exists),
  - workdir confinement: file tools can't escape a directory, and shell runs in
    a sandbox or container,
  - escalate instead of asking: a denied call becomes an observation ("needs
    human approval; logged to escalations.md; continue with other work"). The
    run keeps going and a human reviews later.

  Honest downside: autonomy makes mistakes bigger. A human used to catch the
  agent drifting off course at every turn. Now nobody does until the end. That's
  why #2's verifier and #9's trace aren't optional.

  7. Durable state and resume

  Save memory, todos, budget counters and run status to a run directory
  (runs/<id>/). Then -resume <id> picks up after a crash at step 40 instead of
  starting over. The subtle part is side effects: if the process dies between
  running a tool and saving its result, a resumed run will repeat that call.
  Writing files is fine to repeat. Sending a message to another agent may not
  be, so messages need IDs and receivers should drop duplicates.

  8. Agent-to-agent messaging (the part that goes beyond subagents)

  Your instinct is right that subagents aren't this. agent.Subagent is a
  synchronous function call: the parent blocks, and the child has no identity,
  no inbox and no life after it returns. Peer agents need:

  - Identity: a name, a role, and their own mission file.
  - A mailbox: an Inbox interface with Send(msg) and Receive(ctx). The envelope
    has {id, from, to, thread_id, kind: request|reply|event, body}.
  - Tools for the model: send_message(to, body), and optionally
    wait_for_reply(thread_id, timeout). The driver also feeds inbox messages in
    as the next input (#3), so the agent wakes when mail arrives.
  - Transport, from simplest up:
    a. Go channels: N agents as goroutines in one process. Good for learning.
    b. Filesystem mailbox: mail/<agent>/inbox/*.json. Works across processes,
       can be read with cat, and survives crashes. This is what I'd recommend
       first.
    c. HTTP and the A2A protocol (Google's Agent2Agent: agent cards, tasks), or
       expose the agent as an MCP server so other agents call it as a tool. You
       already have the MCP client side.

    Each step up adds reach (other machines, other vendors' agents) and also
    adds failure modes.
  - Topology: orchestrator/workers, a pipeline (writer → reviewer → editor), or
    a shared "blackboard" file everyone reads. Start with two agents in a
    pipeline.

  Failure modes specific to multi-agent setups, worth designing for from day
  one:
  - Deadlock: A waits on B while B waits on A. Every wait needs a timeout.
  - Endless politeness: "Thanks!" / "You're welcome!" back and forth. Cap
    messages per thread.
  - Prompt injection between agents: a message from another agent is data, not 
    instructions. Wrap it as such, and don't let a peer widen your tool
    permissions.

  9. Observability and a final report

  Nobody is watching the terminal, so the output is: a JSONL trace per run (your
  logger hook writing to a file), a final report.md with the finish summary,
  cost and the escalations, and exit codes (0 done, 1 failed, 2 blocked). Exit
  codes let a script, cron job or CI pipeline chain agent runs together.

  Suggested build order for mvp2

  Each step works on its own:
  1. Mission file, finish tool, exit code. One Run, no REPL. This is the minimal
     headless agent.
  2. Driver loop, nudges, budgets, stuck detection.
  3. todo tool, run directory, -resume.
  4. Non-blocking policy and escalation file.
  5. Two agents as goroutines over a channel inbox (writer and reviewer, each
     with its own mission), using send_message and wake-on-mail.
  6. Swap the transport to a filesystem mailbox and run them as separate 
     processes. Later: an MCP-server or A2A endpoint.

  Steps 1–2 are where you'll learn the most about why "done" is hard. Step 5 is
  where it stops being chat.

  I haven't recorded any of this in SESSION.md or DECISIONS.md, since nothing is
  decided yet. If you pick a direction (say, finish tool versus "no tool calls
  means done", or which mailbox transport), I can log it there and start the
  step-1 skeleton in the usual concepts → examples → TODO format.

✻ Cooked for 1m 36s · done Sunday 10:04 PM

※ recap: You're turning the minimal agent into a headless agent that runs from a mission file, and I've brainstormed the features it needs, ordered as build steps. Next, choose a direction (for example, the `finish` tool and mission file) so I can write the step-1 skeleton. (disable recaps in /config)