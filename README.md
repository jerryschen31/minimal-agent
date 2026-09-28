# minimal-agent

2026-Sep-27
For a while now, I have had an idea evolving and developing in my head (yes, my own brain, not an AI's) of a simple AI agent whose usefulness is extended through capabilities. Here's what I mean:

The idea is that we build a robust AI agent whose core is very simple, containing only the minimal components needed to run the agent. Let's call this the agent core, or agent engine, or agent kernel. I will use these terms interchangeably.

To make this agent engine useful and specific for doing tasks, we need to attach capabilities. Some of these more core capabilities are what the developer community seems to refer to as harnesses. A long-term memory harness that gives an agent a structure for remembering things across past conversations would be an example of such a core capability.

These capabilities are external to the agent, built by folks smarter than myself in the open source developer community. My current take is that we can attach these capabilities via an appropriate MCP interface to these capabilities, much like developing an API for web-based services. The burden would be on the developer of that capability to provide such an MCP interface (and test it of course!). With an MCP interface, these capabilities can be added to the agent with a simple 'mcp-add' command.

By de-coupling capabilities from the agent core, a simple AI agent becomes very flexible and extensible, and become as useful and specific as the attached capabilities. Also in this way, the agent gets better at doing tasks as the capabilities improve. Version control for these agents becomes a matter of locking the version of the agent engine itself and the added capabilities, like a package-lock.json but for agents.

Defining what goes into the agent core and what is deferred to a separate capability is what I'm trying to figure out right now. I'm a very mediocore developer who is fairly new to AI agents, so I'm also learning as I go along.

At a high-level, I think these are the things that the agent core should be responsible for:

- LLM calls: basic request-response loop to the agent's language model(s)
- Context Harness: this is the context for a running agent session (a task or a conversation). This context is what gets passed on each request to a language model.
- Subagent Management and Lifecycle: when and how to call subagents, what instructions, arguments and context to pass to subagents, handling responses from subagents, etc.
- ReAct Loop and Task-State Management: reading current task state, setting up request to language model, taking action based on response from language model (which may be for example to call a tool via MCP), updating task-state based on action, and repeating until a determined goal is reached (e.g., answering the user's question).

The things that could be handled by external capabilities are numerous. Examples might be:

- Long-term Memory Harness: a service that maintains a history of past conversations / sessions, within some data structure and some storage format (flat files, database, object storage)
- Logging: log of actions taken by the agent, tool call requests and responses, errors, etc
- Planning and Thinking: more sophisticated or specialized reasoning, planning or thinking; the planning and thinking capability itself may call a language model. An example would be if there is a very specific scientific question that a dedicated planner and thinker for scientific questions would reason through better than the default model of the agent.
- Other Agents: you could imagine other agents could expose their capabilities via an MCP interface, and thus could be called by an agent
- Verification and Audit Harness: double-checks if a task-state has reached its goal, and audits for correctness or compliance
- Safety Harness: checks if requests or actions are potentially dangerous or malicious
- Web Search: whenever up-to-date information is needed to perform a task or answer a question
