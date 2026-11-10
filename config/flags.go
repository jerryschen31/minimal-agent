package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	ModeChat     = "chat"
	ModeHeadless = "headless"
	ModeOneshot  = "oneshot"
	ModeInfo     = "info" // print what the agent would have (config, tools, MCP servers), it starts each MCP server to list its tools, then exits
	ModePlan     = "plan" // write an objective with the user (or read one) and plan it (see PLAN-plan-apply.md)
)

type Flags struct {
	Config    *string
	Mode      *string
	Query     *string
	JSON      *bool
	Model     *string
	WorkDir   *string
	Mission   *string
	MaxSteps  *int
	MaxTokens *int
	Debug     *bool
	Approve   *string
	Objective *string // -objective or -obj: an objective file, or the objective's text
	Yes       *bool   // -yes or -y: plan mode accepts the plan without asking
}

// ParseFlags parses args (os.Args[1:]) into Flags and validates each value that was given.
//
// Notes:
//   - It returns flag.ErrHelp when -h/--help was requested; usage has already been written to out.
//   - Cross-checks against -mode only run when -mode itself was passed. If it wasn't, the effective mode may still
//     come from the config file, so that check belongs after the config merge (see ValidateMode).
//   - A flag that was not passed stays nil, never a zero value.
//   - -query and -q are the same flag, and so are -objective and -obj, and -yes and -y; a positional argument is an
//     error.
//   - -objective is only checked for being non-empty: it may name a file or be the objective's text, which plan mode
//     tells apart when it starts.
func ParseFlags(args []string, out io.Writer) (Flags, error) {
	// register flags with the flag set
	fs := flag.NewFlagSet("minagent", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { printUsage(out) }

	var (
		configPath, mode, query, queryShort, model, mission, workDir, approve string
		objective, objectiveShort                                             string
		asJSON, debug, yes, yesShort                                          bool
		maxSteps, maxTokens                                                   int
	)
	fs.StringVar(&configPath, "config", "", "")
	fs.StringVar(&mode, "mode", "", "")
	fs.StringVar(&query, "query", "", "")
	fs.StringVar(&queryShort, "q", "", "")
	fs.BoolVar(&asJSON, "json", false, "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&mission, "mission", "", "")
	fs.StringVar(&workDir, "workdir", "", "")
	fs.IntVar(&maxSteps, "maxsteps", 0, "")
	fs.IntVar(&maxTokens, "maxtokens", 0, "")
	fs.BoolVar(&debug, "debug", false, "")
	fs.StringVar(&approve, "approve", "", "")
	fs.StringVar(&objective, "objective", "", "")
	fs.StringVar(&objectiveShort, "obj", "", "")
	fs.BoolVar(&yes, "yes", false, "")
	fs.BoolVar(&yesShort, "y", false, "")

	if err := fs.Parse(args); err != nil {
		return Flags{}, err
	}
	if fs.NArg() > 0 {
		return Flags{}, fmt.Errorf("unexpected argument %q (flags only; use -q for the query)", fs.Arg(0))
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var f Flags

	// string flags: if given, must not be blank
	for _, s := range []struct {
		name string
		val  string
		dst  **string
	}{
		{"config", configPath, &f.Config},
		{"mode", mode, &f.Mode},
		{"model", model, &f.Model},
		{"mission", mission, &f.Mission},
		{"workdir", workDir, &f.WorkDir},
		{"approve", approve, &f.Approve},
	} {
		if !set[s.name] {
			continue
		}
		if strings.TrimSpace(s.val) == "" {
			return Flags{}, fmt.Errorf("-%s must not be empty", s.name)
		}
		*s.dst = new(s.val)
	}

	// -query and -q are the same flag
	switch {
	case set["query"] && set["q"] && query != queryShort:
		return Flags{}, errors.New("-query and -q were both given with different values")
	case set["query"] || set["q"]:
		v := query
		if set["q"] {
			v = queryShort
		}
		if strings.TrimSpace(v) == "" {
			return Flags{}, errors.New("-query must not be empty")
		}
		f.Query = new(v)
	}

	// -objective and -obj are the same flag
	switch {
	case set["objective"] && set["obj"] && objective != objectiveShort:
		return Flags{}, errors.New("-objective and -obj were both given with different values")
	case set["objective"] || set["obj"]:
		v := objective
		if set["obj"] {
			v = objectiveShort
		}
		if strings.TrimSpace(v) == "" {
			return Flags{}, errors.New("-objective must not be empty")
		}
		f.Objective = new(v)
	}

	// -yes and -y are the same flag (both also accept =false)
	switch {
	case set["yes"] && set["y"] && yes != yesShort:
		return Flags{}, errors.New("-yes and -y were both given with different values")
	case set["yes"]:
		f.Yes = new(yes)
	case set["y"]:
		f.Yes = new(yesShort)
	}

	// --json
	if set["json"] {
		f.JSON = new(asJSON)
	}

	// -debug (also accepts -debug=false, so a flag can switch off "debug": true from the config file)
	if set["debug"] {
		f.Debug = new(debug)
	}

	// -maxsteps
	if set["maxsteps"] {
		if maxSteps <= 0 {
			return Flags{}, fmt.Errorf("-maxsteps must be a positive integer, got %d", maxSteps)
		}
		f.MaxSteps = new(maxSteps)
	}

	// -maxtokens
	if set["maxtokens"] {
		if maxTokens <= 0 {
			return Flags{}, fmt.Errorf("-maxtokens must be a positive integer, got %d", maxTokens)
		}
		f.MaxTokens = new(maxTokens)
	}

	// -approve
	if f.Approve != nil && *f.Approve != ApprovalAsk && *f.Approve != ApprovalAllowAll {
		return Flags{}, fmt.Errorf("-approve must be %s or %s, got %q", ApprovalAsk, ApprovalAllowAll, *f.Approve)
	}

	// check that files exist
	if f.Config != nil {
		if err := requireFile("config", *f.Config); err != nil {
			return Flags{}, err
		}
	}
	if f.Mission != nil {
		if err := requireFile("mission", *f.Mission); err != nil {
			return Flags{}, err
		}
	}
	if f.WorkDir != nil {
		if err := requireDir("workdir", *f.WorkDir); err != nil {
			return Flags{}, err
		}
	}

	// validate each mode contains appropriate flags
	if f.Mode != nil {
		if err := f.validateMode(*f.Mode); err != nil {
			return Flags{}, err
		}
	}
	return f, nil
}

// ValidateMode checks the effective mode, after the config file and flags are merged, against the flags given.
//
// Notes:
//   - ParseFlags can only check an explicit -mode; this also covers a mode chosen by the config file's "agentMode",
//     so `"agentMode": "info"` with --json is refused just like `-mode info --json`.
func ValidateMode(cfg Config, f Flags) error {
	return f.validateMode(cfg.AgentMode)
}

// validateMode checks the mode value and that the other flags make sense with it.
//
// Notes:
//   - -query needs oneshot, and oneshot needs -query; -mission needs headless, and headless needs -mission; --json is
//     refused in chat, info and plan.
//   - -objective and -yes need plan mode (autonomous mode will take them too). Plan mode without -objective asks for a
//     new objective at the prompt. That plan mode needs a terminal is checked when it starts, not here: this function
//     only looks at flags.
func (f Flags) validateMode(mode string) error {
	switch mode {
	case ModeChat, ModeHeadless, ModeOneshot, ModeInfo, ModePlan:
	default:
		return fmt.Errorf("-mode must be one of %s|%s|%s|%s|%s, got %q", ModeChat, ModeHeadless, ModeOneshot, ModeInfo, ModePlan, mode)
	}

	if f.Query != nil && mode != ModeOneshot {
		return fmt.Errorf("-query only applies to -mode %s, not %s", ModeOneshot, mode)
	}
	if mode == ModeOneshot && f.Query == nil {
		return fmt.Errorf("-mode %s requires -query", ModeOneshot)
	}
	if f.Mission != nil && mode != ModeHeadless {
		return fmt.Errorf("-mission only applies to -mode %s, not %s", ModeHeadless, mode)
	}
	if mode == ModeHeadless && f.Mission == nil {
		return fmt.Errorf("-mode %s requires -mission", ModeHeadless)
	}
	if f.JSON != nil && (mode == ModeChat || mode == ModeInfo || mode == ModePlan) {
		return fmt.Errorf("--json only applies to -mode %s or %s, not %s", ModeOneshot, ModeHeadless, mode)
	}
	if f.Objective != nil && mode != ModePlan {
		return fmt.Errorf("-objective only applies to -mode %s, not %s", ModePlan, mode)
	}
	if f.Yes != nil && mode != ModePlan {
		return fmt.Errorf("-yes only applies to -mode %s, not %s", ModePlan, mode)
	}
	return nil
}

// requireDir checks that a required directory exists.
//
// Notes:
//   - flagName starts the error message ("-workdir: ...").
func requireDir(flagName, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("-%s: %w", flagName, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("-%s: %s is not a directory", flagName, path)
	}
	return nil
}

// requireFile checks that a required file exists.
//
// Notes:
//   - flagName starts the error message ("-config: ..."); a directory is an error.
func requireFile(flagName, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("-%s: %w", flagName, err)
	}
	if info.IsDir() {
		return fmt.Errorf("-%s: %s is a directory, want a file", flagName, path)
	}
	return nil
}

// printUsage writes the help text for the command line flags to w.
//
// Notes:
//   - Installed as the flag set's Usage, so it runs on -h/--help and on a flag parse error.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: minagent [flags]

  -h, --help             Show this help
  -config <file>         Path to configuration file (flags override its values)
  
  -mode <mode>           chat | headless | oneshot | info | plan (chat is default)
  -q, -query <text>      Query text; required for, and only used in, oneshot mode
  --json                 Output result as JSON (oneshot and headless modes)
  -model <name>          Language model to use
  -mission <file>        Mission file; with headless mode
  -obj, -objective <file|text>  Plan mode: the objective file to plan, or the objective's text (saved to a new
                         file); none = write a new objective in a chat
  -y, -yes               Plan mode: accept the plan without asking
  -workdir <dir>         Directory the read_file tool may read from, with its subdirectories (default ".")
  -maxsteps <n>          Maximum agent steps (positive integer)
  -maxtokens <n>         Maximum tokens the agent may use (positive integer)
  -debug                 Print the context window each turn and log MCP protocol messages to stderr
  -approve <value>       ask | allow-all: allow-all runs tool calls that match ask rules without asking
                         (chat asks you to confirm at startup); deny rules always apply
`)
}

// FlagsOverlay returns cfg with any flag the user explicitly passed applied on top.
//
// Notes:
//   - Flags left unspecified (nil) leave the config value untouched.
func FlagsOverlay(cfg Config, f Flags) Config {
	if f.Model != nil {
		cfg.Model = *f.Model
	}
	if f.Mode != nil {
		cfg.AgentMode = *f.Mode
	}
	if f.Mission != nil {
		cfg.MissionFile = *f.Mission
	}
	if f.WorkDir != nil {
		cfg.WorkDir = *f.WorkDir
	}
	if f.MaxSteps != nil {
		cfg.MaxSteps = *f.MaxSteps
	}
	if f.MaxTokens != nil {
		cfg.MaxTokens = *f.MaxTokens
	}
	if f.Debug != nil {
		cfg.Debug = *f.Debug
	}
	if f.Approve != nil {
		cfg.Approval = *f.Approve
	}
	if f.Objective != nil {
		cfg.ObjectiveFile = *f.Objective
	}
	if f.Yes != nil {
		cfg.AssumeYes = *f.Yes
	}
	return cfg
}
