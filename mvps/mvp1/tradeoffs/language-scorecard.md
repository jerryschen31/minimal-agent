# Go vs TypeScript vs Python vs Rust for an agent runtime

Seventeen design considerations, grouped by what they govern. ★ marks the language that wins the row outright; ★★ marks the runner-up. Comments describe each language as used for a long-running agent process that calls LLM APIs, spawns tools, and fans out subagents. The tally is not the argument; the weighting is.

## Runtime

| Consideration | Go | TypeScript | Python | Rust |
|---|---|---|---|---|
| **Raw CPU speed** | ★★ Compiled, GC. About 2 to 5× slower than Rust in hot loops. Irrelevant for an agent, which waits on the network for seconds at a time. | V8's JIT is remarkably good for a dynamic language, but one thread per process caps throughput. | Interpreted, 10 to 100× slower in pure Python. Fast only when the work is handed to C (numpy, torch). | ★ No GC, LLVM backend, zero-cost abstractions. Fastest of the four and the most predictable latency. |
| **Concurrency model** | ★ Goroutines, channels, `context` cancellation. Parallel tool calls and N subagents are a WaitGroup; timeouts propagate for free. No async colouring. | Single-threaded event loop. Superb for I/O fan-out, but any CPU work blocks every in-flight request. `worker_threads` exist and are awkward. | `asyncio` is fine for I/O. The GIL blocks parallel CPU work; multiprocessing is heavy. Free-threaded builds are still experimental. | ★★ tokio async is fast and the compiler prevents data races. Async is coloured, lifetimes leak into signatures, and `Send + 'static` errors slow teams down. |
| **Memory footprint and GC** | ★★ Concurrent GC with sub-millisecond pauses. A goroutine starts at about 2 KB. Small, steady heap for a service. | V8 heap baseline is tens of MB per process; one process per core for parallelism multiplies it. | Every value is a heap object. Highest footprint of the four. Refcounting plus a cyclic collector. | ★ Ownership frees memory deterministically; no GC at all. Smallest RSS and no pause behaviour to reason about. |
| **Startup, binary, deployment** | ★ One static binary, millisecond startup. Cross-compile with two env vars; ships in a `FROM scratch` container. | Needs the Node runtime plus `node_modules`. Bundling helps; cold start is still 100 ms and up. | Interpreter, virtualenv, native wheels. Packaging is the ecosystem's best-known pain point. | ★★ Also a static binary, often smaller than Go's. Compile times are the cost, and they compound in CI. |
| **Sandboxing and process control** | ★ `os/exec` with context timeouts; Docker, runc, gVisor, Kubernetes are written in Go. Container and cgroup integration is first-party. | `child_process` is adequate. Real isolation is delegated to containers or a service. | `subprocess` is adequate. Process spawn is slow; isolation is delegated the same way. | ★★ Excellent primitives, and Firecracker and Wasmtime are Rust. Less glue code exists around them. |

## Correctness and safety

| Consideration | Go | TypeScript | Python | Rust |
|---|---|---|---|---|
| **Memory safety** | ★★ Memory safe via GC. Data races remain possible; the race detector catches them in tests, not at compile time. | Memory safe at runtime. Risk lives at the native addon boundary. | Memory safe at runtime. Risk lives inside C extensions, which is where the speed comes from. | ★ Compile-time ownership: no null, no data races, no undefined behaviour in safe code. The strongest guarantee available. |
| **Error handling** | ★★ Errors are explicit return values, always visible at the call site. Verbose, and nothing forces you to check them; linters fill the gap. | Exceptions with untyped `throw`. Unhandled promise rejections are easy to lose silently. | Exceptions with excellent tracebacks. A bare `except:` swallows failures with one line. | ★ `Result<T, E>` with `?` and exhaustive matching. An ignored error is a compiler warning; a missed variant is a compile error. |
| **Type system and refactoring** | Simple static types, generics since 1.18, no sum types. Refactors are safe but you write more code to express variants. | ★★ Strong structural typing, but `any` is an escape hatch and types vanish at runtime. | Optional hints, unenforced at runtime. mypy and pyright are bolted on and often incomplete in libraries. | ★ Algebraic data types, traits, generics. Large refactors are compiler-guided; if it compiles it usually works. |
| **Supply chain and dependency hygiene** | ★ The stdlib covers HTTP, JSON, crypto, exec, so dependency trees stay tiny (this repo: one). `go.sum`, module proxy, `govulncheck`, and no install-time scripts. | npm has the worst record: postinstall scripts, typosquatting, thousands of transitive packages per project. | PyPI sees frequent typosquatting; `setup.py` runs arbitrary code at install time. | ★★ `cargo audit` is good, but `build.rs` runs arbitrary code and crate trees run deep. |

## Ecosystem and people

| Consideration | Go | TypeScript | Python | Rust |
|---|---|---|---|---|
| **Extensibility and plugin model** | Implicit interfaces make compile-time plugins clean: a Tool is any type with two methods. No dynamic loading; runtime plugins are subprocesses or MCP. | ★★ Dynamic `import()` plus structural typing. Nearly as flexible as Python with better types. | ★ Duck typing, dynamic import, decorators. The most flexible plugin registries; load a tool from a path at runtime in three lines. | Traits and `dyn` are powerful, but there is no stable ABI, so dynamic plugins are genuinely hard. |
| **AI and agent library ecosystem** | Official Anthropic and OpenAI SDKs, an official MCP SDK. Few frameworks, which is partly why writing one is reasonable. | ★★ Vercel AI SDK, every vendor SDK, the MCP reference implementation. A strong second. | ★ LangChain, LlamaIndex, Hugging Face; every vendor ships Python first. The centre of gravity for AI code. | rig, async-openai, and not much else. The thinnest of the four. |
| **MCP support** | Official `go-sdk` co-maintained with Google. Servers are language-neutral processes, so every MCP server plugs in regardless of what it was written in. | ★ The reference SDK and the majority of published servers. Best place to author servers. | ★★ Official SDK and the second-largest set of servers, including FastMCP. | Official `rmcp` SDK. Fewer servers and examples. |
| **Native and C interop** | `cgo` works but slows builds, breaks static linking, and adds GC and thread hand-off costs. | N-API and napi-rs. Workable, awkward, rarely done by application teams. | ★ ctypes and cffi; the whole numeric stack is wrapped C. Calling a native library in-process is the normal case. | ★★ Zero-cost FFI with bindgen. Excellent, and the reason Rust is now what Python wraps. |
| **Tooling** | ★ gofmt, go test, race detector, pprof, vet: one toolchain, zero configuration. Every Go repo looks the same. | Fragmented: tsc, eslint, prettier, vitest, plus a bundler, each with its own config file. | Fragmented and improving fast; uv, ruff, and pytest are converging on a standard. | ★★ cargo, clippy, rustfmt are every bit as good as Go's, arguably a tie. Compile time is the tax. |
| **Observability and production ops** | ★ pprof and expvar built in; Prometheus and OpenTelemetry were born here. Profiling a live process is routine. | OpenTelemetry is fine. CPU profiling is harder and async stack traces lose context. | OpenTelemetry is fine. Profiling is weak and the GIL hides contention from you. | ★★ The `tracing` crate is excellent; profiling via perf. Strong second. |
| **Developer velocity and learning curve** | Learnable in a week. Verbose, but boring in the way infrastructure should be; code reviews are quick. | ★★ Fast, and nearly every developer already knows some JavaScript. | ★ Fastest to write and read; REPL and notebooks for exploration. Prototype-to-demo in an afternoon. | Steepest curve. The borrow checker fights prototypes; velocity arrives after months, not days. |
| **Community, hiring, longevity** | Strong cloud and infrastructure community, Google-backed, stable since 1.0 in 2012. Smaller hiring pool than the two scripting languages. | ★★ Largest web community; ubiquitous. Churn in frameworks and tooling is the cost. | ★ Largest community and the default language of AI. Easiest to hire for; every AI paper ships Python. | Growing fastest and highly regarded, but the smallest pool of the four. |

## Tally

| | Go | TypeScript | Python | Rust |
|---|---|---|---|---|
| **Rows won (★)** | 6: concurrency, deployment, sandboxing, supply chain, tooling, observability | 1: MCP support | 5: plugins, AI libraries, C interop, velocity, community | 5: speed, memory, memory safety, errors, types |
| **Runner-up (★★)** | 4: CPU speed, memory, memory safety, errors | 5: types, plugins, AI libraries, velocity, community | 1: MCP support | 7: concurrency, deployment, sandboxing, supply chain, C interop, tooling, observability |

The runner-up pattern is a talking point on its own. Rust is the consistent second on every infrastructure row, so it is the credible alternative to Go, not Python. TypeScript is the consistent second on every ecosystem row, so it is the credible alternative to Python.

## How to defend the choice

> An agent runtime is an I/O-bound orchestrator: it waits on model APIs, spawns and supervises tool processes, fans out concurrent calls, and executes instructions it did not write. Go wins exactly the rows that describe that job. Python wins the rows about writing ML code, which the agent does not do. It calls it.

**Choose Go when** the agent is a long-running service or CLI, spawns many subprocesses or subagents, deploys in containers, and a small team wants boring reliability and a tiny dependency surface.

**Choose Python when** the work is research or data science, tools need torch or pandas in-process, or the team lives in notebooks and wants the largest library shelf.

**Choose TypeScript when** the agent runs in the browser or at the edge, ships inside a full-stack web app, or the team will author many MCP servers.

**Choose Rust when** the runtime embeds in another program, targets WASM or tight memory limits, or is a library others link and every millisecond of tail latency is billed.

### Anticipated pushback

**"Python has the ecosystem."** MCP made tool servers language-neutral: this Go agent already runs the TypeScript filesystem server unchanged. Anything else is an HTTP call behind a closure. In-process Python only matters for compute-heavy libraries, which an agent orchestrates rather than runs.

**"Rust is safer."** Go is already memory-safe. Agent bugs are logic and prompt bugs, not use-after-free. Rust's extra guarantee costs the team months of velocity for a class of bug we do not have.

**"Go's error handling is verbose and it has no sum types."** Conceded. Explicit errors are also an audit trail, which matters when the code decides whether to run a shell command. Sum types are the feature I miss most.

**"What about GC pauses?"** Sub-millisecond, against model calls measured in seconds. It never appears in the latency budget.

**"Why not TypeScript, since MCP is TS-first?"** Authoring servers and hosting the runtime are different jobs. The runtime needs real parallelism for subagents and a supply chain I can audit; npm's install scripts are a poor fit for a process that executes untrusted instructions.

### One judgment call to know about

Memory safety runner-up went to Go rather than TypeScript. Go has a race detector and a compiled, typed runtime, but Go data races can in theory break memory safety, while single-threaded TypeScript cannot. If an interviewer pushes there, concede it.
