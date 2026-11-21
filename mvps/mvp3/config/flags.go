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
	ModeChat       = "chat"
	ModeAutonomous = "autonomous"
	ModeOneshot    = "oneshot"
)

type Flags struct {
	Config    *string
	Mode      *string
	Query     *string
	JSON      *bool
	Model     *string
	WorkDir   *string
	Objective *string
	MaxSteps  *int
	MaxTokens *int
	Debug     *bool

	// autonomous mode
	MaxTurnsPerTask *int
	Timeout         *string
	DirectorModel   *string
}

// ParseFlags parses args (os.Args[1:]) into Flags and validates each value that was given.
// It returns flag.ErrHelp when -h/--help was requested; usage has already been written to out.
//
// Cross-checks against -mode only run when -mode itself was passed. If it wasn't, the effective
// mode may still come from the config file, so that check belongs after the config merge.
func ParseFlags(args []string, out io.Writer) (Flags, error) {
	// register flags with the flag set
	fs := flag.NewFlagSet("minagent", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { printUsage(out) }

	var (
		configPath, mode, query, queryShort, model, objective, workDir string
		timeout, directorModel                                         string
		asJSON, debug                                                  bool
		maxSteps, maxTokens, maxTurnsPerTask                           int
	)
	fs.StringVar(&configPath, "config", "", "")
	fs.StringVar(&mode, "mode", "", "")
	fs.StringVar(&query, "query", "", "")
	fs.StringVar(&queryShort, "q", "", "")
	fs.BoolVar(&asJSON, "json", false, "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&objective, "objective", "", "")
	fs.StringVar(&workDir, "workdir", "", "")
	fs.IntVar(&maxSteps, "maxsteps", 0, "")
	fs.IntVar(&maxTokens, "maxtokens", 0, "")
	fs.BoolVar(&debug, "debug", false, "")
	fs.IntVar(&maxTurnsPerTask, "maxturnspertask", 0, "")
	fs.StringVar(&timeout, "timeout", "", "")
	fs.StringVar(&directorModel, "directormodel", "", "")

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
		{"objective", objective, &f.Objective},
		{"workdir", workDir, &f.WorkDir},
		{"timeout", timeout, &f.Timeout},
		{"directormodel", directorModel, &f.DirectorModel},
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

	// -maxturnspertask
	if set["maxturnspertask"] {
		if maxTurnsPerTask <= 0 {
			return Flags{}, fmt.Errorf("-maxturnspertask must be a positive integer, got %d", maxTurnsPerTask)
		}
		f.MaxTurnsPerTask = new(maxTurnsPerTask)
	}

	// -timeout: a Go duration such as 30m or 1h30m
	if f.Timeout != nil {
		if _, err := (Config{Timeout: *f.Timeout}).TimeoutDuration(); err != nil {
			return Flags{}, fmt.Errorf("-%w", err)
		}
	}

	// check that files exist
	if f.Config != nil {
		if err := requireFile("config", *f.Config); err != nil {
			return Flags{}, err
		}
	}
	if f.Objective != nil {
		if err := requireFile("objective", *f.Objective); err != nil {
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
		if err := f.validateMode(); err != nil {
			return Flags{}, err
		}
	}
	return f, nil
}

// validateMode checks the mode value (when provided) and that the other flags make sense with it.
func (f Flags) validateMode() error {
	switch *f.Mode {
	case ModeChat, ModeAutonomous, ModeOneshot:
	default:
		return fmt.Errorf("-mode must be one of %s|%s|%s, got %q", ModeChat, ModeAutonomous, ModeOneshot, *f.Mode)
	}
	mode := *f.Mode

	if f.Query != nil && mode != ModeOneshot {
		return fmt.Errorf("-query only applies to -mode %s, not %s", ModeOneshot, mode)
	}
	if mode == ModeOneshot && f.Query == nil {
		return fmt.Errorf("-mode %s requires -query", ModeOneshot)
	}
	// [agent] "-mode autonomous needs an objective" is checked by Validate after the config merge,
	// because the objective may come from the config file's "objectiveFile"
	autonomousOnly := map[string]bool{
		"objective":       f.Objective != nil,
		"maxturnspertask": f.MaxTurnsPerTask != nil,
		"timeout":         f.Timeout != nil,
		"directormodel":   f.DirectorModel != nil,
	}
	for _, name := range []string{"objective", "maxturnspertask", "timeout", "directormodel"} {
		if autonomousOnly[name] && mode != ModeAutonomous {
			return fmt.Errorf("-%s only applies to -mode %s, not %s", name, ModeAutonomous, mode)
		}
	}
	if f.JSON != nil && mode != ModeOneshot {
		return fmt.Errorf("--json only applies to -mode %s, not %s (autonomous JSON output is not built yet)", ModeOneshot, mode)
	}
	return nil
}

// checks that a required directory exists
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

// checks that a required file exists
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

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: minagent [flags]

  -h, --help             Show this help
  -config <file>         Path to configuration file (flags override its values)
  
  -mode <mode>           chat | autonomous | oneshot (chat is default)
  -q, -query <text>      Query text; required for, and only used in, oneshot mode
  --json                 Output result as JSON (oneshot mode)
  -model <name>          Language model to use (the worker's, in autonomous mode)
  -workdir <dir>         Directory the file tools may use, with its subdirectories (default ".")
  -maxsteps <n>          Maximum agent steps (positive integer)
  -maxtokens <n>         Maximum tokens the agent may use (positive integer)
  -debug                 Print the context window each turn and log MCP protocol messages to stderr

  Autonomous mode (a director LLM works through objective.md's tasks with the worker agent):
  -objective <file>      Objective file (markdown with a "# Objective" section)
  -maxturnspertask <n>   Worker turns allowed per task (positive integer, default 5)
  -timeout <duration>    Stop the whole run after this long, e.g. 30m (default: no limit)
  -directormodel <name>  The director's language model
`)
}

// FlagsOverlay returns cfg with any flag the user explicitly passed applied on top.
// Flags left unspecified (nil) leave the config value untouched.
func FlagsOverlay(cfg Config, f Flags) Config {
	if f.Model != nil {
		cfg.Model = *f.Model
	}
	if f.Mode != nil {
		cfg.AgentMode = *f.Mode
	}
	if f.Objective != nil {
		cfg.ObjectiveFile = *f.Objective
	}
	if f.MaxTurnsPerTask != nil {
		cfg.Director.MaxTurnsPerTask = *f.MaxTurnsPerTask
	}
	if f.Timeout != nil {
		cfg.Timeout = *f.Timeout
	}
	if f.DirectorModel != nil {
		cfg.Director.Model = *f.DirectorModel
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
	return cfg
}
