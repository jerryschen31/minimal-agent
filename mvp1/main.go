// Command mvp1 wires config → plug-ins → agent.Agent and runs a one-shot task
// or an interactive REPL. This file is the only place that knows about every
// concrete implementation; swapping any of them is a change here, not in agent/.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"mvp1/agent"
	"mvp1/harness"
	"mvp1/hooks"
	"mvp1/llm"
	"mvp1/memory"
	"mvp1/tool"
)

// [agent] Config is deliberately flat JSON: provider, model, harness choices,
// tool list, MCP servers, and safety policy. Secrets come from env vars only.
type Config struct {
	Provider  string `json:"provider"` // "openai" (any OpenAI-compatible server) | "anthropic"
	Model     string `json:"model"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	MaxTokens int64  `json:"max_tokens"`

	System   string `json:"system_prompt"`
	MaxSteps int    `json:"max_steps"`
	Workdir  string `json:"workdir"`

	Memory     string `json:"memory"`      // "inmemory" | "file"
	MemoryPath string `json:"memory_path"` // for "file"
	Context    string `json:"context"`     // "full" | "window"
	Window     int    `json:"context_window"`

	Tools      []string `json:"tools"` // builtins: shell, read_file, write_file
	MCPServers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcp_servers"`
	Subagents bool     `json:"subagents"`
	Approve   []string `json:"approve"` // tools requiring human approval; ["*"] = all
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config JSON")
	q := flag.String("q", "", "one-shot task (omit for interactive REPL)")
	flag.Parse()

	cfg := Config{Provider: "openai", Model: "llama3.1", MaxTokens: 16000,
		System:   "You are a helpful agent. Use tools when they help; answer plainly when done.",
		MaxSteps: 25, Workdir: ".", Memory: "inmemory", Context: "full", Window: 40}
	if b, err := os.ReadFile(*cfgPath); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			fatal(err)
		}
	} else if *cfgPath != "config.json" {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// [agent] errors return through run so deferred cleanup (MCP servers) executes;
	// os.Exit would skip it and orphan every child process.
	if err := run(ctx, cfg, *q); err != nil {
		fatal(err)
	}
}

func run(ctx context.Context, cfg Config, q string) error {
	stdin := bufio.NewReader(os.Stdin) // [agent] shared by REPL and approval prompts

	// [agent] 1. LLM provider ------------------------------------------------
	var provider agent.Provider
	switch cfg.Provider {
	case "anthropic":
		provider = llm.NewAnthropic(cfg.Model, os.Getenv(cfg.APIKeyEnv), cfg.BaseURL, cfg.MaxTokens)
	default:
		if cfg.BaseURL == "" { // [agent] default to a local Ollama server
			cfg.BaseURL = "http://localhost:11434/v1"
		}
		provider = &llm.OpenAI{BaseURL: cfg.BaseURL, APIKey: os.Getenv(cfg.APIKeyEnv), Model: cfg.Model, MaxTokens: cfg.MaxTokens}
	}

	// [agent] 2. Tools: builtins by name, then every tool from every MCP server -
	tools := map[string]agent.Tool{}
	builtins := map[string]agent.Tool{
		"shell": tool.Shell(cfg.Workdir, 60*time.Second), "read_file": tool.ReadFile(cfg.Workdir), "write_file": tool.WriteFile(cfg.Workdir)}
	for _, name := range cfg.Tools {
		t, ok := builtins[name]
		if !ok {
			return fmt.Errorf("unknown builtin tool %q", name)
		}
		tools[name] = t
	}
	for _, s := range cfg.MCPServers {
		c, err := tool.ConnectMCP(ctx, s.Name, s.Command, s.Args...)
		if err != nil {
			return err
		}
		defer c.Close()
		ts, err := c.Tools(ctx)
		if err != nil {
			return err
		}
		for _, t := range ts {
			tools[t.Spec().Name] = t
		}
	}

	// [agent] 3. Hooks: logging always; approval when configured ---------------
	// Policy hooks are built once and shared with subagents (below) so delegation
	// is never a way around approval; only the logger differs per agent.
	var policy agent.Hooks
	if len(cfg.Approve) > 0 {
		ap := &hooks.Approval{In: stdin, Out: os.Stderr, Tools: map[string]bool{}}
		for _, n := range cfg.Approve {
			if n != "*" {
				ap.Tools[n] = true
			}
		}
		policy = append(policy, ap)
	}
	hs := append(agent.Hooks{&hooks.Logger{W: os.Stderr}}, policy...)

	// [agent] 4. Memory + context harness --------------------------------------
	var mem agent.Memory = &memory.InMemory{}
	if cfg.Memory == "file" {
		f, err := memory.OpenFile(cfg.MemoryPath)
		if err != nil {
			return err
		}
		mem = f
	}
	var cb agent.ContextBuilder = harness.Full{}
	if cfg.Context == "window" {
		cb = harness.Window{MaxMessages: cfg.Window}
	}

	// [agent] 5. Subagents: same model and tools, fresh memory, indented logs -
	if cfg.Subagents {
		subTools := map[string]agent.Tool{}
		for k, v := range tools {
			subTools[k] = v
		}
		tools["delegate"] = agent.Subagent{Name: "delegate",
			Description: "Delegate a self-contained sub-task to a fresh agent with the same tools; returns its final answer.",
			Spawn: func() *agent.Agent {
				return &agent.Agent{LLM: provider, Tools: subTools, Memory: &memory.InMemory{}, Context: harness.Full{},
					Hooks:  append(agent.Hooks{&hooks.Logger{W: os.Stderr, Prefix: "    [sub] "}}, policy...),
					System: cfg.System, MaxSteps: cfg.MaxSteps}
			}}
	}

	a := &agent.Agent{LLM: provider, Tools: tools, Memory: mem, Context: cb, Hooks: hs, System: cfg.System, MaxSteps: cfg.MaxSteps}

	// [agent] 6. Run: one-shot or REPL (memory persists across REPL turns) -----
	if q != "" {
		out, err := a.Run(ctx, q)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	for {
		fmt.Fprint(os.Stderr, "\n> ")
		line, err := stdin.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "exit" {
			return nil
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out, err := a.Run(ctx, strings.TrimSpace(line))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		fmt.Println(out)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
