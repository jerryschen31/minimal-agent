package config

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

type Config struct {
	// User configuration
	UserID string `json:"userId"`

	// LLM service provider configuration
	Provider   string `json:"provider"` // "openai" (any OpenAI-compatible server) | "anthropic"
	Model      string `json:"model"`
	BaseURL    string `json:"baseUrl"`
	ApiKeyName string `json:"apiKeyName"`

	// run configuration
	AgentName     string `json:"agentName"`
	AgentVersion  string `json:"agentVersion"`
	AgentMode     string `json:"agentMode"`     // "chat" | "autonomous" | "orchestrator" | "oneshot" ("worker" only as a flag)
	ObjectiveFile string `json:"objectiveFile"` // path to objective.md, autonomous mode only
	Timeout       string `json:"timeout"`       // autonomous mode: Go duration for the whole run ("30m"); "" = none
	WorkDir       string `json:"workDir"`       // the only directory (and subdirectories) the read_file tool may read from; default "."
	MaxSteps      int    `json:"maxSteps"`      // max ReAct steps per turn; must be positive (default 10)
	MaxTokens     int    `json:"maxTokens"`     // 0 = unspecified
	Debug         bool   `json:"debug"`         // print the context window each turn and log MCP protocol traffic to stderr

	// I/O configuration for the chat session
	InBuffer  io.Reader `json:"-"` // runtime wiring, never read from a config file
	OutBuffer io.Writer `json:"-"`

	// ConfigFile is the config file this Config was read from ("" = defaults only); orchestrator mode
	// passes it on to the worker process.
	ConfigFile string `json:"-"`
	// ProtocolOut is -mode worker's MCP channel: the real stdout. main points os.Stdout at stderr in that
	// mode, so a stray print can't corrupt the protocol stream.
	ProtocolOut io.WriteCloser `json:"-"`

	// system prompt
	SystemPrompt string `json:"systemPrompt"`

	// memory configuration
	ChatStoreType string `json:"chatStoreType"` // "in-memory" | "persistent"

	// tools and MCP servers configuration
	BuiltinTools []string                   `json:"builtinTools"` // names of built-in tools to enable
	McpServers   map[string]McpServerConfig `json:"mcpServers"`   // configuration for MCP servers

	// autonomous and orchestrator mode: the director LLM that writes the worker's prompts. The top-level
	// fields above configure the worker (in orchestrator mode, the worker child process reads them).
	// Fields missing from the file keep the defaults from GetDefaultConfig.
	Director DirectorConfig `json:"director"`
}

// DirectorConfig configures the director in autonomous mode.
// [agent] It is a nested struct, not a pointer: encoding/json then only overwrites the keys present in
// the file and keeps the defaults for the rest, which is exactly "empty fields come from the default
// director config" (PLAN-autonomous-mode.md § 5).
type DirectorConfig struct {
	Provider               string `json:"provider"`
	Model                  string `json:"model"`
	BaseURL                string `json:"baseUrl"`
	ApiKeyName             string `json:"apiKeyName"`
	MaxSteps               int    `json:"maxSteps"`               // the director's own ReAct step limit per director turn
	MaxTurnsPerTask        int    `json:"maxTurnsPerTask"`        // worker turns allowed per task; must be positive
	MaxTasks               int    `json:"maxTasks"`               // the most tasks a plan may have (given in ## Tasks or from set_tasks); must be positive
	MaxIterations          int    `json:"maxIterations"`          // plan → work → review rounds; a round's gaps become the next round's tasks; must be positive
	AdditionalInstructions string `json:"additionalInstructions"` // appended to the built-in director instructions
}

// TimeoutDuration parses cfg.Timeout; "" means no timeout (0).
func (cfg Config) TimeoutDuration() (time.Duration, error) {
	if cfg.Timeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		return 0, fmt.Errorf("timeout %q: %w", cfg.Timeout, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("timeout %q must not be negative", cfg.Timeout)
	}
	return d, nil
}

// Validate checks the merged config (defaults + file + flags). Unlike ParseFlags' cross-checks, it also
// catches values that came from the config file, e.g. "agentMode": "autonomous" with no objectiveFile.
func Validate(cfg Config) error {
	if !slices.Contains(modes, cfg.AgentMode) {
		return fmt.Errorf("agentMode must be one of %s, got %q", strings.Join(modes, "|"), cfg.AgentMode)
	}
	if cfg.MaxSteps <= 0 {
		return fmt.Errorf("maxSteps must be a positive integer, got %d", cfg.MaxSteps)
	}
	if !UsesDirector(cfg.AgentMode) {
		return nil
	}
	if cfg.ObjectiveFile == "" {
		return fmt.Errorf("-mode %s requires an objective file (-objective or \"objectiveFile\")", cfg.AgentMode)
	}
	if cfg.Director.MaxTurnsPerTask <= 0 {
		return fmt.Errorf("director.maxTurnsPerTask must be a positive integer, got %d", cfg.Director.MaxTurnsPerTask)
	}
	if cfg.Director.MaxTasks <= 0 {
		return fmt.Errorf("director.maxTasks must be a positive integer, got %d", cfg.Director.MaxTasks)
	}
	if cfg.Director.MaxIterations <= 0 {
		return fmt.Errorf("director.maxIterations must be a positive integer, got %d", cfg.Director.MaxIterations)
	}
	if cfg.Director.MaxSteps <= 0 {
		return fmt.Errorf("director.maxSteps must be a positive integer, got %d", cfg.Director.MaxSteps)
	}
	if _, err := cfg.TimeoutDuration(); err != nil {
		return err
	}
	return nil
}

type McpServerConfig struct {
	Type string `json:"type,omitempty"` // types: "stdio" | "http" | "sse" - note that sse is now deprecated in the MCP spec (2026-07-28+)

	// local MCP server (communication via stdio - server started as a child process)
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// remote MCP server (communication via HTTP - stateless in the latest MCP spec (2026-07-28+))
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// transportType works out which transport an MCP server uses, and rejects entries that are ambiguous or contradictory.
//
//		command             -> stdio
//		url                 -> http
//		url + "type": "sse" -> sse (caller warns and skips)
//	    weird mix of fields (e.g., command and url specified) or unknown fields -> error
func (m McpServerConfig) TransportType() (string, error) {

	hasCmd, hasURL := m.Command != "", m.URL != ""
	switch {
	case hasCmd && hasURL:
		return "", fmt.Errorf(`both "command" and "url" are set; a server is either local (command) or remote (url)`)
	case !hasCmd && !hasURL:
		return "", fmt.Errorf(`set either "command" (local server) or "url" (remote server)`)
	}

	// infer from the fields, then let an explicit type confirm or refine it
	kind := TransportHTTP
	if hasCmd {
		kind = TransportStdio
	}
	switch m.Type {
	case "": // nothing written, keep the inferred kind
	case TransportStdio:
		if !hasCmd {
			return "", fmt.Errorf(`type %q needs "command"`, m.Type)
		}
	case TransportHTTP, TransportSSE:
		if !hasURL {
			return "", fmt.Errorf(`type %q needs "url"`, m.Type)
		}
		kind = m.Type
	default:
		return "", fmt.Errorf("unknown type %q (want %q, %q or %q)", m.Type, TransportStdio, TransportHTTP, TransportSSE)
	}

	// a known field that doesn't apply is an error, not a warning: ignoring e.g. an "env" token on a
	// remote server would silently drop the credential and surface later as a confusing 401
	switch kind {
	case TransportStdio:
		if len(m.Headers) > 0 {
			return "", fmt.Errorf(`"headers" only applies to remote servers`)
		}
	case TransportHTTP:
		if len(m.Env) > 0 {
			return "", fmt.Errorf(`"env" only applies to local servers (use "headers" for remote auth)`)
		}
		if len(m.Args) > 0 {
			return "", fmt.Errorf(`"args" only applies to local servers`)
		}
	}
	return kind, nil
}

func GetDefaultConfig() Config {
	return Config{
		UserID:        "default_user",
		Provider:      "openai",
		Model:         "gpt-4o-mini",
		BaseURL:       "https://api.openai.com/v1",
		ApiKeyName:    "OPENAI_API_KEY",
		AgentName:     "minagent",
		AgentVersion:  "0.1.0",
		AgentMode:     ModeChat,
		WorkDir:       ".",
		SystemPrompt:  "You are a helpful assistant. Keep your responses concise and relevant.",
		ChatStoreType: "in-memory",
		InBuffer:      os.Stdin,
		OutBuffer:     os.Stdout,
		BuiltinTools:  []string{"ReadFile"},
		MaxSteps:      10,
		Director:      GetDefaultDirectorConfig(),
	}
}

// GetDefaultDirectorConfig is the "default director config" that fills any director field the config
// file leaves out. It deliberately does not copy the worker's provider settings.
func GetDefaultDirectorConfig() DirectorConfig {
	return DirectorConfig{
		Provider:        "openai",
		Model:           "gpt-4o-mini",
		BaseURL:         "https://api.openai.com/v1",
		ApiKeyName:      "OPENAI_API_KEY",
		MaxSteps:        10,
		MaxTurnsPerTask: 5,
		MaxTasks:        20,
		MaxIterations:   1,
	}
}

// func GetDefaultConfig() Config {
// 	return Config{
// 		UserID:        "default_user",
// 		Provider:     "openai", // Ollama exposes an OpenAI-compatible endpoint
// 		Model:        "qwen2.5:0.5b",
// 		BaseURL:      "http://127.0.0.1:11434/v1",
// 		ApiKeyName:   "", // no auth needed for a local Ollama server
// 		SystemPrompt: "You are a helpful assistant. Keep your responses concise and relevant.",
// 		ChatStoreType: "in-memory",
// 		InBuffer:      os.Stdin,
// 		OutBuffer:     os.Stdout,
// 	}
// }

// setDefaultConfig builds a Config from a config JSON file. An empty filename parses the default config file (config.default.json)
func SetDefaultConfig(flags Flags) (Config, error) {
	// first get the default config, so missing keys in the file keep their defaults
	cfg := GetDefaultConfig()

	// if no config file is specified in the flags, just return the default config
	if flags.Config == nil || *flags.Config == "" {
		return cfg, nil
	}

	// otherwise for the config file, read its contents and parse the JSON
	filename := *flags.Config
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}

	// parse config JSON - unrecognized fields display a warning and get ignored ; error if file not found
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", filename, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", filename, err)
	}

	known := jsonKeys(reflect.TypeOf(Config{}))
	var unknown []string
	for key := range raw {
		if !known[strings.ToLower(key)] { // encoding/json matches keys case-insensitively, so do the same
			unknown = append(unknown, key)
		}
	}
	// [agent] one level down for the "director" block, so a typo like "maxTurnPerTask" is reported too
	for key, val := range raw {
		if strings.ToLower(key) != "director" {
			continue
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(val, &inner) != nil {
			continue // not an object: the typed Unmarshal above already reported it
		}
		knownInner := jsonKeys(reflect.TypeOf(DirectorConfig{}))
		for k := range inner {
			if !knownInner[strings.ToLower(k)] {
				unknown = append(unknown, key+"."+k)
			}
		}
	}
	sort.Strings(unknown) // map order is random; sorted keeps the warnings stable
	for _, key := range unknown {
		fmt.Fprintf(cfg.OutBuffer, "[warning] config file %s: ignoring unrecognized field %q\n", filename, key)
	}
	cfg.ConfigFile = filename
	return cfg, nil
}

// jsonKeys returns the lowercased JSON key of every field of struct type t that can be set from a file.
// Untagged or `json:"-"` fields (InBuffer, OutBuffer) are left out.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := make(map[string]bool)
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",") // drop options like ",omitempty"
		if name == "" || name == "-" {
			continue
		}
		keys[strings.ToLower(name)] = true
	}
	return keys
}

// PrintConfig prints an explicit allowlist of settings that are safe to show. MCP servers are listed by
// name and transport only: their args, env, url and headers are left out because they commonly hold
// credentials (e.g. an Authorization header or a token in a URL or argument). A field added to Config
// stays hidden until it is added here on purpose.
func PrintConfig(cfg Config) {
	w := cfg.OutBuffer
	fmt.Fprintln(w, "Current agent configuration:")
	fmt.Fprintf(w, "  userId:        %s\n", cfg.UserID)
	fmt.Fprintf(w, "  provider:      %s\n", cfg.Provider)
	fmt.Fprintf(w, "  model:         %s\n", cfg.Model)
	fmt.Fprintf(w, "  baseUrl:       %s\n", cfg.BaseURL)
	fmt.Fprintf(w, "  apiKeyName:    %s (name of the environment variable, not the key)\n", cfg.ApiKeyName)
	fmt.Fprintf(w, "  agentName:     %s\n", cfg.AgentName)
	fmt.Fprintf(w, "  agentVersion:  %s\n", cfg.AgentVersion)
	fmt.Fprintf(w, "  agentMode:     %s\n", cfg.AgentMode)
	fmt.Fprintf(w, "  objectiveFile: %s\n", cfg.ObjectiveFile)
	fmt.Fprintf(w, "  timeout:       %s\n", cfg.Timeout)
	fmt.Fprintf(w, "  workDir:       %s\n", cfg.WorkDir)
	fmt.Fprintf(w, "  maxSteps:      %d\n", cfg.MaxSteps)
	fmt.Fprintf(w, "  maxTokens:     %d\n", cfg.MaxTokens)
	fmt.Fprintf(w, "  debug:         %t\n", cfg.Debug)
	fmt.Fprintf(w, "  systemPrompt:  %q\n", cfg.SystemPrompt)
	fmt.Fprintf(w, "  chatStoreType: %s\n", cfg.ChatStoreType)
	fmt.Fprintf(w, "  builtinTools:  %v\n", cfg.BuiltinTools)
	d := cfg.Director
	fmt.Fprintf(w, "  director:      provider=%s model=%s baseUrl=%s apiKeyName=%s maxSteps=%d maxTurnsPerTask=%d maxTasks=%d maxIterations=%d\n",
		d.Provider, d.Model, d.BaseURL, d.ApiKeyName, d.MaxSteps, d.MaxTurnsPerTask, d.MaxTasks, d.MaxIterations)
	if len(cfg.McpServers) == 0 {
		fmt.Fprintln(w, "  mcpServers:    none")
		return
	}
	fmt.Fprintln(w, "  mcpServers:")
	for _, name := range slices.Sorted(maps.Keys(cfg.McpServers)) {
		transport, err := cfg.McpServers[name].TransportType()
		if err != nil {
			transport = "invalid config"
		}
		fmt.Fprintf(w, "    %s (%s)\n", name, transport)
	}
}
