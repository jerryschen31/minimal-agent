package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

type Config struct {
	// LLM service provider configuration
	Provider   string `json:"provider"` // "openai" (any OpenAI-compatible server) | "anthropic"
	Model      string `json:"model"`
	BaseURL    string `json:"base_url"`
	ApiKeyName string `json:"api_key_name"`

	// agent execution configuration
	MaxSteps     int    `json:"max_steps"`
	MaxTokens    int    `json:"max_tokens"`
	WorkDir      string `json:"work_dir"`
	SystemPrompt string `json:"system_prompt"`
	Subagents    bool   `json:"subagents"`

	// memory and context configuration
	Memory        string `json:"memory"`      // "inmemory" | "file"
	MemoryPath    string `json:"memory_path"` // path to memory file if "file"
	Context       string `json:"context"`     // "full" | "window"
	ContextWindow int    `json:"context_window"`

	// tools and MCP servers configuration
	Tools      []string `json:"tools"`
	McpServers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcp_servers"`
	RequiresApproval []string `json:"requires_approval"` // list of tools that require a human [y/N] prompt before each call
}

// helper function for handling a fatal error and exiting the program immediately
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}

func getDefaultConfig() Config {
	return Config{
		Provider:     "anthropic",
		Model:        "claude-sonnet-5",
		BaseURL:      "https://api.anthropic.com",
		ApiKeyName:   "ANTHROPIC_API_KEY",
		MaxSteps:     10,
		MaxTokens:    10000,
		WorkDir:      "./",
		SystemPrompt: "You are a helpful assistant. Reason through multiple steps until you reach a conclusion. Use tools when needed; when finished, answer in plain text.",
		Subagents:    true,
		Memory:       "inmemory",
		Context:      "full",
	}
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to model/agent/harness config file")
	flag.Parse()

	// setup hard-coded default configuration
	cfg := getDefaultConfig()

	// read the configuration file and load (unmarshal) it into the cfg struct, replacing any defaults
	if byteStream, err := os.ReadFile(*cfgPath); err != nil {
		fatal(err)
	} else {
		if err := json.Unmarshal(byteStream, &cfg); err != nil {
			fatal(err)
		}
	}

	// catch OS signals (e.g., Ctrl+C or process termination signal (kill)) and terminate gracefully
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// setup the agent object and run the main agent logic with the provided context and configuration
	if err := runAgent(ctx, cfg); err != nil {
		fatal(err)
	}
}

func setupProvider(cfg Config) interface{} {
	// placeholder for setting up the LLM provider based on the configuration
	// return the provider object
	return nil
}

func runAgent(ctx context.Context, cfg Config) error {
	// placeholder for the main agent logic
	// this function should implement the core functionality of the agent
	// using the provided context and configuration

	// create a buffered reader for standard input (user prompts, y/n approvals, etc)
	stdio := bufio.NewReader(os.Stdin)

	//// setup the agent object ////
	// 1. setup LLM provider
	provider := setupProvider(cfg)

	// 2. setup memory and context (if applicable)
	// 3. setup tools and MCP servers (if applicable)

	// print the configuration for debugging purposes
	fmt.Printf("Running agent with configuration: %+v\n", cfg)
	return nil
}
