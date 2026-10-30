package config

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
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
	Provider   string `json:"provider"` // "openai" (any OpenAI-compatible server) | "anthropic" (TODO)
	Model      string `json:"model"`
	BaseURL    string `json:"baseUrl"`
	ApiKeyName string `json:"apiKeyName"`

	// run configuration
	AgentName    string `json:"agentName"`
	AgentVersion string `json:"agentVersion"`
	AgentMode    string `json:"agentMode"`   // "chat" | "autonomous" (TODO) | "oneshot" (TODO) | "info"
	MissionFile  string `json:"missionFile"` // path to mission file, autonomous mode only (TODO)
	WorkDir      string `json:"workDir"`     // the only directory (and subdirectories) the read_file tool may read from; default "."
	MaxSteps     int    `json:"maxSteps"`    // max ReAct steps per turn; must be positive (default 10)
	MaxTokens    int    `json:"maxTokens"`   // 0 = unspecified (TODO)

	ResponseTimeoutSeconds int  `json:"responseTimeoutSeconds"` // most seconds one model call may take; must be positive (default 300)
	Debug                  bool `json:"debug"`                  // print the context window each turn and log MCP protocol traffic to stderr

	// I/O configuration for the chat session
	InBuffer  io.Reader `json:"-"` // runtime wiring, never read from a config file
	OutBuffer io.Writer `json:"-"`
	// McpServerStderr is where stdio MCP servers' stderr goes; nil = os.Stderr. Info mode discards it: a server can
	// print its environment (tokens) there, and info's output is meant to be safe to paste.
	McpServerStderr io.Writer `json:"-"`

	// system prompt
	SystemPrompt string `json:"systemPrompt"`

	// memory configuration
	ChatStoreType string `json:"chatStoreType"` // "in-memory" | "persistent" (TODO)

	// context window and compaction configuration (validated by contextwindow.CheckConfig)
	ContextWindowMessages int     `json:"contextWindowMessages"` // most messages the context window holds; the oldest are dropped first; at least 4 (default 50)
	ContextStrategy       string  `json:"contextStrategy"`       // how the window stores messages: "offset" | "in-place" | "ring-buffer" | "linked-list" (default "offset")
	AutoCompact           bool    `json:"autoCompact"`           // summarize the context automatically when it gets full; false = only /compact does (default true)
	AutoCompactThreshold  float64 `json:"autoCompactThreshold"`  // how full the window is (0 < x <= 1) when auto-compaction starts (default 0.9)
	SummarizerPrompt      string  `json:"summarizerPrompt"`      // the system prompt for /summary, /compact and auto-compaction; must not be empty

	// deny rules configuration
	DenyRules []string `json:"denyRules"` // glob rules on model-facing tool names; a config file's list replaces the default, [] means no rules

	// tracing configuration
	TraceEnabled       bool   `json:"traceEnabled"`       // write an event trace log for this run; false = tracing is off (trace.Nop)
	RedactionLevel     string `json:"redactionLevel"`     // how strictly secrets are masked in the trace: "standard"
	DigestEnabled      bool   `json:"digestEnabled"`      // also write an L1 digest, <run_id>.digest.md, next to the trace; needs traceEnabled
	TraceDir           string `json:"traceDir"`           // directory for trace files, one <run_id>.jsonl per run (default ".agent-trace")
	TraceMaxEventBytes int    `json:"traceMaxEventBytes"` // an event payload larger than this (as JSON) is replaced by a marker holding its size; must be positive (default 100 MB)

	// tool output caps (bytes, all must be positive). These bound how much of the model's context one tool call can use.
	ReadFileMaxBytes  int `json:"readFileMaxBytes"`  // most one read_file call returns; a bigger file is read in pieces with offset/limit (default 1 MB, about 250k tokens)
	MCPResultMaxBytes int `json:"mcpResultMaxBytes"` // most of an MCP tool result that is passed to the model; the rest is cut with a note (default 256 KB)

	// tools and MCP servers configuration
	BuiltinTools  []string                   `json:"builtinTools"`  // names of built-in tools to enable
	DenyFileNames []string                   `json:"denyFileNames"` // name patterns the file tools never read, added to the built-in list (which can't be removed)
	McpServers    map[string]McpServerConfig `json:"mcpServers"`    // configuration for MCP servers
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

// TransportType works out which transport an MCP server uses, and rejects entries that are ambiguous or contradictory.
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
		AgentVersion:  "0.1.13",
		AgentMode:     ModeChat,
		WorkDir:       ".",
		SystemPrompt:  "You are a helpful assistant. Keep your responses concise and relevant.",
		ChatStoreType: "in-memory",
		InBuffer:      os.Stdin,
		OutBuffer:     os.Stdout,
		BuiltinTools:  []string{"read_file"},
		MaxSteps:      10,

		ResponseTimeoutSeconds: 300, // 5 minutes; slow local models may need more
		TraceEnabled:           true,
		DigestEnabled:          true,
		RedactionLevel:         "standard",
		DenyRules:              []string{"*delete*", "*remove*"},

		ContextWindowMessages: 50,
		ContextStrategy:       "offset",
		AutoCompact:           true,
		AutoCompactThreshold:  0.9,
		SummarizerPrompt:      "You are a helpful assistant that summarizes chat history. Summarize the key conversational points and important details concisely.",

		TraceDir:           ".agent-trace",
		TraceMaxEventBytes: 100 << 20, // 100 MB
		ReadFileMaxBytes:   1 << 20,   // 1 MB
		MCPResultMaxBytes:  256 << 10, // 256 KB
	}
}

// setDefaultConfig builds a Config from a config JSON file. An empty filename causes the agent to use the built-in config defaults
func SetDefaultConfig(flags Flags) (Config, error) {
	// first get the default config, so missing keys in the file keep their defaults
	cfg := GetDefaultConfig()

	// if no config file is specified in the flags, just return the built-in config defaults
	if flags.Config == nil || *flags.Config == "" {
		return cfg, nil
	}

	// otherwise for the config file, read its contents and parse the JSON
	filename := *flags.Config
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}

	// parse config JSON - unrecognized fields display a warning and get ignored ; error if JSON cannot be parsed
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", filename, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", filename, err)
	}

	known := configJSONKeys()
	var unknown []string
	for key := range raw {
		if !known[strings.ToLower(key)] { // encoding/json matches keys case-insensitively, so do the same
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown) // map order is random; sorted keeps the warnings stable
	for _, key := range unknown {
		fmt.Fprintf(cfg.OutBuffer, "[warning] config file %s: ignoring unrecognized field %q\n", filename, key)
	}
	return cfg, nil
}

// configJSONKeys returns the lowercased JSON key of every Config field that can be set from a file.
// Untagged or `json:"-"` fields (InBuffer, OutBuffer, McpServerStderr) are left out.
func configJSONKeys() map[string]bool {
	keys := make(map[string]bool)
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",") // drop options like ",omitempty"
		if name == "" || name == "-" {
			continue
		}
		keys[strings.ToLower(name)] = true
	}
	return keys
}

// PrintConfig prints an explicit allowlist of settings that are safe to show.
// MCP servers are listed by name and transport only.
func PrintConfig(cfg Config) {
	w := cfg.OutBuffer
	fmt.Fprintln(w, "Current agent configuration:")
	fmt.Fprintf(w, "  userId:        %s\n", cfg.UserID)
	fmt.Fprintf(w, "  provider:      %s\n", cfg.Provider)
	fmt.Fprintf(w, "  model:         %s\n", cfg.Model)
	fmt.Fprintf(w, "  baseUrl:       %s\n", DisplayURL(cfg.BaseURL))
	fmt.Fprintf(w, "  apiKeyName:    %s (name of the environment variable, not the key)\n", cfg.ApiKeyName)
	fmt.Fprintf(w, "  agentName:     %s\n", cfg.AgentName)
	fmt.Fprintf(w, "  agentVersion:  %s\n", cfg.AgentVersion)
	fmt.Fprintf(w, "  agentMode:     %s\n", cfg.AgentMode)
	fmt.Fprintf(w, "  missionFile:   %s\n", cfg.MissionFile)
	fmt.Fprintf(w, "  workDir:       %s\n", cfg.WorkDir)
	fmt.Fprintf(w, "  maxSteps:      %d\n", cfg.MaxSteps)
	fmt.Fprintf(w, "  maxTokens:     %d\n", cfg.MaxTokens)
	fmt.Fprintf(w, "  responseTimeoutSeconds: %d\n", cfg.ResponseTimeoutSeconds)
	fmt.Fprintf(w, "  debug:         %t\n", cfg.Debug)
	fmt.Fprintf(w, "  systemPrompt:  %q\n", cfg.SystemPrompt)
	fmt.Fprintf(w, "  chatStoreType: %s\n", cfg.ChatStoreType)
	fmt.Fprintf(w, "  contextWindowMessages: %d\n", cfg.ContextWindowMessages)
	fmt.Fprintf(w, "  contextStrategy: %s\n", cfg.ContextStrategy)
	fmt.Fprintf(w, "  autoCompact:   %t\n", cfg.AutoCompact)
	fmt.Fprintf(w, "  autoCompactThreshold: %g\n", cfg.AutoCompactThreshold)
	fmt.Fprintf(w, "  summarizerPrompt: %q\n", cfg.SummarizerPrompt)
	fmt.Fprintf(w, "  traceEnabled:  %t\n", cfg.TraceEnabled)
	fmt.Fprintf(w, "  digestEnabled: %t\n", cfg.DigestEnabled)
	fmt.Fprintf(w, "  redactionLevel: %s\n", cfg.RedactionLevel)
	fmt.Fprintf(w, "  traceDir: %s\n", cfg.TraceDir)
	fmt.Fprintf(w, "  traceMaxEventBytes: %d\n", cfg.TraceMaxEventBytes)
	fmt.Fprintf(w, "  readFileMaxBytes: %d\n", cfg.ReadFileMaxBytes)
	fmt.Fprintf(w, "  mcpResultMaxBytes: %d\n", cfg.MCPResultMaxBytes)
	fmt.Fprintf(w, "  builtinTools:  %v\n", cfg.BuiltinTools)
	fmt.Fprintf(w, "  denyFileNames: %v (added to the built-in list)\n", cfg.DenyFileNames)
	fmt.Fprintf(w, "  denyRules:     %v\n", cfg.DenyRules)
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

// DisplayURL returns raw in a form that is safe to print: scheme, host and path stay, while the parts that can
// carry a credential (user:password@, the query string, the fragment) are replaced by [hidden], whatever they
// contain. It hides by position in the URL, not by recognising tokens. A token inside the path is still shown.
func DisplayURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[hidden: not an absolute URL]" // can't tell which part is which, so show none of it
	}
	shown := u.Scheme + "://"
	if u.User != nil {
		shown += "[hidden]@"
	}
	shown += u.Host + u.EscapedPath()
	if u.RawQuery != "" || u.ForceQuery {
		shown += "?[hidden]"
	}
	if u.Fragment != "" {
		shown += "#[hidden]"
	}
	return shown
}
