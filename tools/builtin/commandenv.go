package builtin

// DefaultCommandEnv is the list of environment variable names run_command passes to a program. Every other variable of the
// agent's own environment is dropped, so ANTHROPIC_API_KEY, OPENAI_API_KEY and the like never reach a command (or its
// output, the trace and the model).
//
// The config key commandEnv adds names to this list; it never removes any. This is the one place to change the built-in
// set. A name here is passed only if the agent's environment has it.
var DefaultCommandEnv = []string{
	"PATH", "HOME", "USER", "TMPDIR", "LANG", "LC_ALL", "TERM",
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOFLAGS", "GOTOOLCHAIN",
	"VIRTUAL_ENV", "PYTHONPATH",
}
