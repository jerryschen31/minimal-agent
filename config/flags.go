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
)

type Flags struct {
	Config    *string
	Mode      *string
	Query     *string
	JSON      *bool
	Model     *string
	Mission   *string
	MaxSteps  *int
	MaxTokens *int
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
		configPath, mode, query, queryShort, model, mission string
		asJSON                                              bool
		maxSteps, maxTokens                                 int
	)
	fs.StringVar(&configPath, "config", "", "")
	fs.StringVar(&mode, "mode", "", "")
	fs.StringVar(&query, "query", "", "")
	fs.StringVar(&queryShort, "q", "", "")
	fs.BoolVar(&asJSON, "json", false, "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&mission, "mission", "", "")
	fs.IntVar(&maxSteps, "maxsteps", 0, "")
	fs.IntVar(&maxTokens, "maxtokens", 0, "")

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
	case ModeChat, ModeHeadless, ModeOneshot:
	default:
		return fmt.Errorf("-mode must be one of %s|%s|%s, got %q", ModeChat, ModeHeadless, ModeOneshot, *f.Mode)
	}
	mode := *f.Mode

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
	if f.JSON != nil && mode == ModeChat {
		return fmt.Errorf("--json only applies to -mode %s or %s, not %s", ModeOneshot, ModeHeadless, mode)
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
  
  -mode <mode>           chat | headless | oneshot (chat is default)
  -q, -query <text>      Query text; required for, and only used in, oneshot mode
  --json                 Output result as JSON (oneshot and headless modes)
  -model <name>          Language model to use
  -mission <file>        Mission file; with headless mode
  -maxsteps <n>          Maximum agent steps (positive integer)
  -maxtokens <n>         Maximum tokens the agent may use (positive integer)
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
	if f.Mission != nil {
		cfg.MissionFile = *f.Mission
	}
	if f.MaxSteps != nil {
		cfg.MaxSteps = *f.MaxSteps
	}
	if f.MaxTokens != nil {
		cfg.MaxTokens = *f.MaxTokens
	}
	return cfg
}
