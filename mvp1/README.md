# mvp1 — a minimal, extensible ReAct agent in Go

~1300 lines, one third-party dependency (the Anthropic SDK), zero framework.
The kernel (`agent/`) is ~200 lines; everything else is a plug-in behind an interface.

```
mvp1/
├── main.go            wiring: config → concrete plug-ins → agent.Agent; one-shot or REPL
├── agent/             THE KERNEL (imports nothing from mvp1)
│   ├── agent.go       neutral message model, 5 interfaces, the ReAct loop
│   └── subagent.go    an Agent exposed as a Tool → delegation / subagents
├── llm/               agent.Provider adapters
│   ├── openai.go      OpenAI Chat Completions wire format: Ollama, vLLM, llama.cpp, LM Studio,
│   │                  OpenAI, Groq, OpenRouter, Together, Mistral, Gemini-compat…
│   └── anthropic.go   Anthropic Messages API via the official Go SDK
├── tool/              agent.Tool implementations
│   ├── tool.go        Func: wrap any Go closure / library call as a tool
│   ├── builtin.go     shell (timeout), read_file / write_file (confined to workdir)
│   └── mcp.go         stdio MCP client: every tool on any MCP server becomes an agent.Tool
├── memory/            agent.Memory: InMemory, File (JSON, survives restarts)
├── harness/           agent.ContextBuilder: Full, Window (cuts at user turns)
├── hooks/             agent.Hook: Logger (observability), Allowlist (policy), Approval (HITL)
├── config.json        local model via Ollama, file memory, windowed context, approvals on
└── config.anthropic.json  Claude + a filesystem MCP server
```

## The loop (agent/agent.go)

```
Run(input):
  memory.Append(user input)
  for step in 1..MaxSteps:
    history := memory.Messages()                      # what happened
    msgs    := context.Build(system, history)         # what the model should see   ← context harness
    hooks.BeforeLLM
    resp    := llm.Chat(msgs, toolSpecs)              # REASON / PLAN                ← provider
    hooks.AfterLLM
    memory.Append(resp)
    if no tool calls: return resp.Content             # final answer
    results := run all tool calls concurrently        # ACT (hooks.BeforeTool = authz gate)
    memory.Append(results)                            # OBSERVE
  return ErrMaxSteps
```

Tool errors, unknown tools, and policy denials are all fed back as `role: tool, is_error: true`
observations so the model can recover instead of the run crashing.

## Five interfaces = five extension axes

| Interface | Question it answers | Shipped | Extend by |
|---|---|---|---|
| `Provider` | which model? | OpenAI-compat, Anthropic | implement `Chat`; ~80 lines per wire format |
| `Tool` | what can it do? | shell, files, MCP, subagent | `tool.Func{...}` closure, or add an MCP server to config |
| `Memory` | what happened? | InMemory, File | SQLite / Postgres / vector store |
| `ContextBuilder` | what does the model see this step? | Full, Window | summarising, retrieval, cache-aware layout |
| `Hook` | observe / veto | Logger, Allowlist, Approval | OpenTelemetry spans, eval recorder, cost meter, policy engine |

Cross-cutting features map onto these seams:

- **Observability / tracing / eval**: a `Hook` (four callbacks). `hooks.Logger` is the seed.
- **Authorization**: `Hook.BeforeTool` returning an error denies the call. `Allowlist`, `Approval`.
- **Sandboxing**: a `Tool` decision. Replace `tool.Shell` with a container/microVM-backed tool.
- **Subagents**: `agent.Subagent{Spawn: func() *Agent}` — a Tool that runs a fresh agent
  with its own memory. Parallel tool calls already run on goroutines, so N subagents run concurrently.
- **Any LLM**: `provider: "openai"` + `base_url` reaches most local and hosted models.

## Run

```sh
cd mvp1 && go build -o minagent .

# local model (Ollama): ollama pull qwen2.5:7b
./minagent -q "List the Go files here and summarise what each does"     # uses config.json
./minagent                                                              # REPL, memory persists across turns

# Claude + a filesystem MCP server (save ANTHROPIC_API_KEY in .env file then):
./minagent -config config.anthropic.json -q "What's in README.md?"

go test ./...                         # unit tests (fake LLM drives the loop)
MCP_E2E=1 go test ./tool -run MCP -v  # integration test against a real MCP server (needs npx)
```

## Adding things

**A tool from a Go library**
```go
tools["weather"] = tool.Func{Name: "weather", Description: "Current temp for a city",
    Schema: `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`,
    Fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
        in, err := tool.Args[struct{ City string }](raw); if err != nil { return "", err }
        return myWeatherClient.Get(ctx, in.City)
    }}
```

**An MCP server** — one config line; every tool it exposes appears as `<name>_<tool>`:
```json
"mcp_servers": [{"name": "gh", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"]}]
```

**A tracing hook**
```go
type OTel struct{ hooks.Base; tracer trace.Tracer }
func (o OTel) AfterLLM(ctx context.Context, step int, r agent.Response, err error) { /* end span, record usage */ }
```

**A new provider** — implement `Chat(ctx, []agent.Message, []agent.ToolSpec) (agent.Response, error)`.
`Message.Native` lets an adapter stash a provider-specific copy of an assistant turn (e.g. Anthropic
thinking blocks) and replay it verbatim; other providers ignore it.

## Why Go

- **Concurrency is the shape of an agent**: parallel tool calls, N subagents, MCP servers as child
  processes, timeouts and cancellation everywhere. Goroutines + `context.Context` make that
  ~10 lines with no async colouring and no GIL.
- **Deployable and sandbox-friendly**: one static binary, cross-compiles, fits in a scratch container.
- **Security posture**: memory-safe, tiny dependency surface (stdlib HTTP/JSON/exec), race detector,
  `go vet`, reproducible builds. Supply chain is one `go.sum`.
- **Interfaces are the extension mechanism**: implicit satisfaction means plug-ins need no import
  of a framework; a `Tool` is any type with two methods.
- **Ecosystem for *this* job**: MCP has an official Go SDK (co-maintained with Google); Docker, Kubernetes,
  Terraform, Ollama, and most cloud tooling are Go, so infra-side integrations are native.
- **Honest trade-off**: Python/TS have far more prebuilt "agent tool" packages. MCP neutralises most of
  that (servers are language-agnostic processes), and `tool.Func` wraps anything else, but if you want to
  `pip install` your tools, Go is the wrong pick. Rust would win on raw performance and memory footprint
  but costs iteration speed for what is mostly I/O-bound glue code.
