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
//	agentMode       "chat" | "info" | "plan" | "autonomous" | "oneshot"; -mode. Chat and info run; plan is
//	                a stub so far (prints the objective, saves nothing)                                        ["chat"]
//	missionFile     mission file for headless mode; -mission                                               [""]
//	workDir         the only directory (and below) the file tools (read_file) may use; must exist, even
//	                with no file tool enabled; -workdir                                                    ["."]
//	maxSteps        most ReAct steps (model call + tool calls) per turn; must be positive; -maxsteps       [10]
//	maxTokens       parsed but not yet used; -maxtokens                                                    [0]
//	responseTimeoutSeconds  most seconds one model call may take before it fails; positive. Raise it
//	                for slow local models                                                                  [300]
//	toolTimeoutSeconds  most seconds one tool call (built-in or MCP) may take; the call is stopped and the
//	                model sees "tool timed out". Positive                                                  [300]
//	turnTimeoutSeconds  most seconds one whole turn (model calls plus tools) may take; the turn is stopped
//	                and nothing is added to memory. Positive and not less than toolTimeoutSeconds          [600]
//	mcpConnectTimeoutSeconds  most seconds one MCP server may take to connect at startup; a server that takes
//	                longer is stopped and skipped (the others keep their tools). Positive                  [120]
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
//	                       on, it must start above 4 messages (so a window of 4 needs autoCompact false)  [0.9]
//	summarizerPrompt       system prompt for /summary, /compact and auto-compaction (what a summary keeps);
//	                       must not be empty. /compact <text> adds <text> to it for that one call         ["You are a helpful assistant that summarizes …"]
//
// Approval gate:
//
//	denyRules       glob rules on model-facing tool names (<server>_<tool>, case-insensitive); a matching
//	                tool call is always refused, in every mode, never asked. [] = no rules                 [[]]
//	askRules        same form; a matching call needs the user's approval ("Allow once? [y/N]" in chat on a
//	                terminal). With nobody to ask (piped input, tests, autonomous) it is refused. A deny
//	                rule wins over an ask rule. [] = no rules        [["*delete*", "*remove*", "write_file", "edit_file",
//	                                                                    "create_directory", "copy_file", "move_file"]]
//	approval        "ask" | "allow-all". allow-all runs ask-rule calls without asking; it applies ONLY to
//	                askRules, never to denyRules. Chat asks you to confirm it once at startup (no terminal
//	                = not confirmed, so calls stay denied). Flag: -approve                                 ["ask"]
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
//	copyFileMaxBytes    largest file one copy_file call copies (a larger one is refused; the bytes go from
//	                    file to file, not into the model's context)                                         [100 MB]
//	mcpResultMaxBytes   most of an MCP tool result passed to the model; the rest is cut with a note        [256 KB]
//	commandMaxOutputBytes  most output one run_command call returns; the program is killed past it and the
//	                    rest is lost (a plain cut-off; use read_file to read a file)                      [128 KB]
//
// Search limit (positive; bounds how long one call can take, not only what it returns):
//
//	maxSearchFiles      most files and directories one list_files or search_files call looks at, matching
//	                    or not; a call that hits it ends with a note to narrow the path                    [10000]
//
// Tools:
//
//	builtinTools    built-in tools to enable, by the name the model sees: "read_file" ("ReadFile",
//	                the old spelling, still works), "list_files", "search_files",
//	                "create_directory", "write_file", "edit_file", "copy_file", "move_file", "run_command"
//	                [every tool except "run_command", which must be listed by hand together with runCommands]
//	runCommands     programs run_command may start: bare names, looked up on PATH once at startup. The
//	                only way to enable run_command: with [] (the default) the tool is not set up, and a
//	                warning says so. A listed program runs unasked unless a deny or ask rule matches
//	                (Command rules below). NOT a sandbox, see Running commands below                      [none]
//	commandEnv      environment variable names run_command passes to a program, ADDED to the built-in
//	                list (PATH HOME USER TMPDIR LANG LC_ALL TERM GOPATH GOROOT GOCACHE GOMODCACHE GOPROXY
//	                GOFLAGS GOTOOLCHAIN VIRTUAL_ENV PYTHONPATH; tools/builtin/commandenv.go). Never shrinks
//	                it. Everything else of the agent's environment (API keys) is not passed               [none]
//	denyPaths       extra path patterns the file tools never read, list or search, e.g. "*.secret",
//	                "secrets/", "config/creds.json". Added to the built-in list (.env, .env.*, *.pem,
//	                *.key, id_rsa*, .ssh, .agent-trace), which can't be removed; .env.example is always
//	                allowed. Path pattern rules below. A bad pattern fails startup                         [none]
//	ignorePaths     path patterns list_files and search_files leave out (noise, not secrets). An ignored
//	                file isn't shown or searched; an ignored directory is shown as "dir/ (ignored)" and its
//	                contents can't be listed or searched, even by asking for it directly (read_file still
//	                reads them). Path pattern rules below. A config file's list replaces the default;
//	                [] = ignore nothing; a bad pattern fails startup
//	                                                [[".git/", "node_modules/", "__pycache__/", ".venv/", "*.pyc"]]
//	mcpServers      MCP servers by name: {"<name>": {...}}; a tool is named <name>_<tool> for the model    [none]
//
// Running commands (run_command). Not a shell: the program starts with an argument list, so pipes, && and $VAR are
// literal text; stdin is empty, so interactive programs do not work; it runs in its own process group, killed when
// the call ends, so nothing it starts outlives the call (on Windows only the program itself is killed, not what it
// started). A non-zero exit is a normal result ("[exit status N]").
// It is NOT contained: a running program can read and write anything you can, which bypasses every file-tool check
// (.git, the config file, denyPaths, the hard-link refusal). An allowed interpreter (python3, go, npm) is code
// execution. cat, head, grep and sed read .env and the other names read_file refuses: do not list them.
// Best-effort guard: before a program starts, each argument (and the value of a --flag=value) that names a file the
// file tools refuse (the deny list and denyPaths, .env but not .env.example, and the trace directory) refuses the
// whole call, never asks, whether or not the file exists, and also when a symlink or ".." leads to it. It catches
// mistakes, not a determined program: it cannot see a directory that holds such a file ("grep -r KEY ."), a name an
// interpreter or "sh -c" builds, "git show HEAD:.env", or a path the program gets another way. Not a boundary.
// Choosing runCommands is the consent; the safety nets are deny and ask rules, and later a sandbox.
//
// Command rules (denyRules, askRules): a rule starting with "run_command:" applies to run_command calls and is
// matched, ignoring case, against the command line "run_command:<program> <arguments joined by spaces>" with *
// matching anything, '/' included. A rule without that prefix matches tool names only, so "*delete*" does not
// match "go test -run TestDelete". "run_command:*" is every command; "run_command" is every call by name. Deny
// beats ask; with no match the command runs. A glob over the command line is not a detector: "go -C dir run x"
// does not match "run_command:go run*". A call whose arguments cannot be read is asked. With nobody to ask, an
// ask is refused unless approval is "allow-all". run_command is NOT in the default askRules.
//
// Example, Go and Python (go test and go build run unasked; go run, go install, python3 and pip ask first):
//
//	{ "builtinTools": ["read_file", "write_file", "run_command"],
//	  "runCommands": ["go", "python3"],
//	  "askRules": ["run_command:go run*", "run_command:go install*", "run_command:python3*",
//	               "write_file", "edit_file", "*delete*", "*remove*"] }
//
// Example, npm (npm test and npm ls run unasked; install, run, exec, npx and publish ask first or are refused):
//
//	{ "builtinTools": ["read_file", "write_file", "run_command"],
//	  "runCommands": ["npm", "node"],
//	  "commandEnv": ["NPM_CONFIG_CACHE", "NODE_PATH"],
//	  "denyRules": ["run_command:npm publish*"],
//	  "askRules": ["run_command:npm install*", "run_command:npm run*", "run_command:npm exec*",
//	               "run_command:node*", "write_file", "edit_file", "*delete*", "*remove*"] }
//
// (NODE_OPTIONS is left out of commandEnv on purpose: it can inject code.) Remember that a config file's list
// replaces the default, so repeat the default askRules you still want.
//
// Path patterns (denyPaths, ignorePaths), .gitignore-style; paths are relative to workDir and matched ignoring case:
//
//	".env", "*.pyc"     no "/": a file or directory with a matching name, at any depth (".env" is exactly that
//	                    name; "*.env" also matches prod.env)
//	"secrets"           a file or directory named secrets at any depth, and everything under that directory
//	"secrets/"          trailing "/": directories only (still at any depth), and everything under them
//	"secrets/*.env"     a "/" at the start or in the middle anchors the pattern at workDir: secrets/a.env, but not
//	                    secrets/old/a.env ("*" never crosses a "/") or app/secrets/a.env
//	"/build"            a leading "/" also anchors at workDir, so it matches only the top-level build path
//
// A matched directory covers everything under it, so the way to protect a whole tree is to deny the directory itself
// ("secrets" or "secrets/"), not "secrets/*.env". Not supported: "!" negation; "**" is an error. A pattern with spaces
// at either end, a "\", or an empty, "." or ".." part is an error.
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
//	-obj, -objective the objective file; plan mode only (its draft; none = asked at the prompt)
//	-out <file>      where plan mode saves the objective file; plan mode only; never overwritten
//
// # Codemap (auto-generated)
//
// Generated by `go run ./cmd/codemap` from the code; do not edit by hand. Each file lists its function and
// method signatures in source order.
//
//	config.go
//	  func (m McpServerConfig) TransportType() (string, error)
//	  func GetDefaultConfig() Config
//	  func SetDefaultConfig(flags Flags) (Config, error)
//	  func configJSONKeys() map[string]bool
//	  func PrintConfig(cfg Config)
//	  func DisplayURL(raw string) string
//
//	flags.go
//	  func ParseFlags(args []string, out io.Writer) (Flags, error)
//	  func ValidateMode(cfg Config, f Flags) error
//	  func (f Flags) validateMode(mode string) error
//	  func requireDir(flagName, path string) error
//	  func requireFile(flagName, path string) error
//	  func printUsage(w io.Writer)
//	  func FlagsOverlay(cfg Config, f Flags) Config
package config
