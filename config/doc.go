// Package config holds the agent's settings: the Config struct, loading a JSON config file, and command-line flags.
//
// # Where a setting comes from
//
// flags > config file > defaults. GetDefaultConfig holds every default. A config file (-config <file>) is decoded on
// top of the defaults, so:
//
//   - a key missing from the file keeps its default;
//   - a key in the file replaces the default, a list included (it is not merged): "denyRules": [] means no rules;
//   - an unknown key prints a warning and is ignored; keys match case-insensitively, as encoding/json does.
//
// A flag the user didn't pass leaves the setting alone (FlagsOverlay). Values that would fail later are rejected at
// startup, in chat mode and in info mode alike (-mode info prints the effective settings and any error).
//
// # Settings
//
// Defaults in brackets; "-flag" means a flag overrides the key.
//
// Model provider:
//
//	provider        "openai" (any OpenAI-compatible server, e.g. Ollama) is the only one built so far     ["openai"]
//	model           model name sent to the provider; -model                                               ["gpt-4o-mini"]
//	baseUrl         the provider's API URL; shown with any credential part hidden                         ["https://api.openai.com/v1"]
//	apiKeyName      NAME of the environment variable holding the API key, never the key itself;
//	                "" = no key (e.g. local Ollama)                                                        ["OPENAI_API_KEY"]
//
// Run:
//
//	userId          who the chat session belongs to                                                       ["default_user"]
//	agentName       shown in info mode and the trace                                                       ["minagent"]
//	agentVersion    shown in info mode and the trace                                                       ["0.1.0"]
//	agentMode       "chat" | "info" | "headless" | "oneshot"; -mode. Only chat and info run so far       ["chat"]
//	missionFile     mission file for headless mode; -mission                                               [""]
//	workDir         the only directory (and below) read_file may read; -workdir                            ["."]
//	maxSteps        most ReAct steps (model call + tool calls) per turn; must be positive; -maxsteps       [10]
//	maxTokens       parsed but not yet used; -maxtokens                                                    [0]
//	debug           print the context window each turn, log MCP protocol traffic to stderr; -debug         [false]
//	systemPrompt    the model's system message; "" sends no system message                                 ["You are a helpful assistant. …"]
//	chatStoreType   where chat history is kept: "in-memory" (the only one built so far)                    ["in-memory"]
//
// Context window and compaction (checked by contextwindow.CheckConfig):
//
//	contextWindowMessages  most messages the model sees; the oldest are dropped first, and the window always
//	                       starts at a user message; at least 4                                            [50]
//	contextStrategy        how the window stores messages: "offset" | "in-place" | "ring-buffer" |
//	                       "linked-list"; all behave the same, they differ only in speed                   ["offset"]
//	autoCompact            summarize the context automatically when it gets full; false = only /compact
//	                       does, and the window just drops its oldest messages                             [true]
//	autoCompactThreshold   how full the window is when auto-compaction starts: 0 < x <= 1, as messages
//	                       rounded down (50 × 0.9 = 45). Checked even when autoCompact is false; with it
//	                       on, it must start above 2 messages                                              [0.9]
//	summarizerPrompt       system prompt for /summary, /compact and auto-compaction (what a summary keeps);
//	                       must not be empty. /compact <text> adds <text> to it for that one call         ["You are a helpful assistant that summarizes …"]
//
// Approval gate:
//
//	denyRules       glob rules on model-facing tool names (<server>_<tool>, case-insensitive); a matching
//	                tool call is refused (in chat, the user is asked). [] = no rules                       [["*delete*", "*remove*"]]
//
// Tracing (see package trace):
//
//	traceEnabled        write an event trace, one <run_id>.jsonl per run; false = no trace                [true]
//	digestEnabled       also write <run_id>.digest.md, a readable summary; needs traceEnabled             [true]
//	redactionLevel      how secrets are masked in the trace: "standard" (the only level so far)            ["standard"]
//	traceDir            directory for trace files, relative to where the agent starts; read_file never
//	                    reads inside it; must not be empty                                                [".agent-trace"]
//	traceMaxEventBytes  an event bigger than this (as JSON) is replaced by a marker; positive              [100 MB]
//
// Tool output caps (bytes, positive; they bound how much context one tool call can use):
//
//	readFileMaxBytes    most one read_file call returns; bigger files are read in pieces                  [1 MB]
//	mcpResultMaxBytes   most of an MCP tool result passed to the model; the rest is cut with a note        [256 KB]
//
// Tools:
//
//	builtinTools    built-in tools to enable: "ReadFile" (read_file)                                       [["ReadFile"]]
//	mcpServers      MCP servers by name: {"<name>": {...}}; a tool is named <name>_<tool> for the model    [none]
//
// Each mcpServers entry is either local or remote:
//
//	command   local: program to start (stdio transport)
//	args      local: its arguments
//	env       local: extra environment variables (often tokens)
//	url       remote: the server's URL (HTTP transport)
//	headers   remote: HTTP headers sent with every request (often an Authorization token)
//	type      optional: "stdio" | "http" | "sse" (deprecated, skipped with a warning); inferred if left out
//
// Setting both command and url, or a field the transport doesn't use (env on a remote server), is an error.
// args, env, url and headers can hold credentials, so /config, info mode and the trace show only a server's name
// and transport.
//
// # Flags without a config key
//
//	-config <file>   the config file to load (none = defaults only)
//	-q, -query       the query; required for, and only used in, oneshot mode
//	--json           JSON output; oneshot and headless modes only
package config
