[![Last commit](https://img.shields.io/github/last-commit/jerryschen31/minimal-agent)](https://github.com/jerryschen31/minimal-agent/commits)
[![Go](https://img.shields.io/badge/Go-%E2%89%A5%201.26.3-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)

# minimal-agent

Building a minimal AI agent harness that is easily extensible with third-party tools and harness components. Written in Go.

```sh
git clone https://github.com/jerryschen31/minimal-agent.git
cd minimal-agent
export OPENAI_API_KEY=...   # default config uses OpenAI; see config.local.json for an example using a local model
go run .                    # starts the chat
```

`go run . -h` lists all of the flags.

[Running thoughts](THOUGHTS.md)<br>
[Running decisions](DECISIONS.md)<br>
[Running session notes](SESSION.md)

[MVP 1](mvp1/): I planned, coding agent wrote code (Fable 5.1)<br>
[MVP 2](mvp2/learn/): I designed and coded several iterations, with a coding agent as thinking partner and pair programmer (Sonnet 5.5)<br>
[dea1a4f](https://github.com/jerryschen31/minimal-agent/tree/dea1a4f00121598ac380dc81eddb79a270b8620c): MVP 2 code refactored into main codebase<br>
[e5ba43a](https://github.com/jerryschen31/minimal-agent/tree/e5ba43a6b77e8571d24dcd16df0ab085cbc87ab2): Added an interactive line editor for a better chat prompt UX and added a debug mode

Currently working on building more support for tools (built-in and MCP server). Also looking into tracing harness component as a hook, with an optional built-in option.
