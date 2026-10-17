[![Last commit](https://img.shields.io/github/last-commit/jerryschen31/minimal-agent)](https://github.com/jerryschen31/minimal-agent/commits)
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
[Running session notes](SESSION.md)

[MVP 1](mvp1/): I planned, coding agent wrote code (Fable 5.1)<br>
[MVP 2](mvp2/learn/): I designed and coded several iterations, with a coding agent as thinking partner and pair programmer (Sonnet 5.5)<br>
[dea1a4f](https://github.com/jerryschen31/minimal-agent/tree/dea1a4f00121598ac380dc81eddb79a270b8620c): MVP 2 code refactored into main codebase<br>
[e5ba43a](https://github.com/jerryschen31/minimal-agent/tree/e5ba43a6b77e8571d24dcd16df0ab085cbc87ab2): Added an interactive line editor for a better chat prompt UX and added a debug mode<br>
[b6d73eb](https://github.com/jerryschen31/minimal-agent/tree/b6d73ebd7d312245717a2fcd13987dfe87ee275b): Agent-driven autonomous mode MVP - works well. I'll use this MVP code as a guide for the full build out.

Chat mode is built. Currently working on a full build of autonomous mode with support for more tools (built-in and MCP server). Also looking into implementing a built-in tracing harness component, that you could still swap out with a third-party tracing tool if desired.
