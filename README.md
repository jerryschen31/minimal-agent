[![Last commit](https://img.shields.io/github/last-commit/jerryschen31/minimal-agent/master)](https://github.com/jerryschen31/minimal-agent/commits)
[![Go](https://img.shields.io/badge/Go-%E2%89%A5%201.26.3-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)

# minimal-agent

Building a minimal AI agent harness that is easily extensible with third-party tools and harness components. Written in Go.

To clone this repo and start the agent:

```sh
git clone https://github.com/jerryschen31/minimal-agent.git
cd minimal-agent
go run .
```
`go run . -h` lists all of the flags.

If you want to use a remote-hosted model, make sure you configure API keys for your LLM of choice. If using a local model with Ollama, see `config.local.json` for an example configuration.

[Running thoughts](THOUGHTS.md)<br>
[Running decisions](DECISIONS.md)<br>
[Running session notes](SESSION.md)<br>
[Objective file format](docs/objective-format.md) and [plan file format](docs/plan-format.md): you write the objective, the planner turns it into a plan, autonomous mode runs the plan ([example objective](examples/objective-example.md), [its plan](examples/objective-example.plan.md)).<br>
[Auto mode](docs/auto-mode.md): running a plan unattended, approvals, crashes and resume, exit codes.

[MVP 1](mvps/mvp1/): I planned, coding agent wrote code (Fable 5.1)<br>
[MVP 2](mvps/mvp2/learn/): I designed and coded several iterations, with a coding agent as thinking partner and pair programmer (Sonnet 5.5)<br>
[dea1a4f](https://github.com/jerryschen31/minimal-agent/tree/dea1a4f00121598ac380dc81eddb79a270b8620c): MVP 2 code refactored into main codebase<br>
[e5ba43a](https://github.com/jerryschen31/minimal-agent/tree/e5ba43a6b77e8571d24dcd16df0ab085cbc87ab2): Added an interactive line editor for a better chat prompt UX and added a debug mode<br>
[b6d73eb](https://github.com/jerryschen31/minimal-agent/tree/b6d73ebd7d312245717a2fcd13987dfe87ee275b): I planned and guided an agent to build an autonomous mode MVP - works well. I'll use this MVP code as a guide for the full build out.<br>
[993b075](https://github.com/jerryschen31/minimal-agent/tree/993b075794ea82d27fd2a97a28dfd05ae32f447d): Agent traces and session summary (digest)<br>
[a2259f4](https://github.com/jerryschen31/minimal-agent/tree/a2259f4edfd830483313ce842f22c71e0fb7c3a9): Deny / ask approval gates<br>
[f6db324](https://github.com/jerryschen31/minimal-agent/tree/f6db324228e6b6fe0021970b438fbd41fa6136c7): Built-in filesystem and run-command tools<br>
[f18ab2a](https://github.com/jerryschen31/minimal-agent/tree/f18ab2a82613f8ad6bbfaddd2e391a5c395fe1b6): MCP server setup and error hardening<br>
[173e50e](https://github.com/jerryschen31/minimal-agent/tree/173e50ea54b3ee02d7a6fdf96f7e860667f91a68): Plan mode<br>
[da68363](https://github.com/jerryschen31/minimal-agent/tree/da683636e254627f46c2616043416a874b3c7844): Autonomous mode v1<br>
[f65d227](https://github.com/jerryschen31/minimal-agent/tree/f65d22740e92b18b728dbc86d98acbd91ec8612a): Token usage tracking

Chat, plan and auto modes are built, with traces, digests, approval gates, built-in tools, and token and time budgets. Next: orchestrator mode.
