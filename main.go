package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jerryschen31/minimal-agent/agent"
	"github.com/jerryschen31/minimal-agent/config"
)

const LogoLines = `
  <[o_o]>  minimal-agent v%s
   /| |\   Ask me anything!
`
const WelcomeMsg = LogoLines + "\nType /clear to clear the conversation\n     /exit to exit\n     /compact to compact the conversation\n     /config to see the current agent configuration\n"

// Exit codes (exitCode). [agent] So a script running auto mode unattended can tell "broken" from "needs a person".
const (
	exitOK          = 0   // finished, or the user answered no (Run?, Accept?): nothing failed
	exitFailed      = 1   // setup failed, the run failed, a task was judged not done on every attempt
	exitUsage       = 2   // bad flags or config
	exitNeedsPerson = 3   // auto mode stopped for a person: a gate or resume question nobody could answer, or a declined gate
	exitInterrupted = 130 // Ctrl+C or SIGTERM (128 + SIGINT, the shell convention)
)

// main runs the program and exits with run's code.
//
// Notes:
//   - [agent] os.Exit skips deferred calls, so everything that needs a deferred cleanup (shutting the agent down, which
//     closes MCP servers and the trace) lives in run, which returns before os.Exit.
func main() {
	os.Exit(run())
}

// run parses flags and the config file, then sets up and runs the agent, and returns the exit code.
//
// Notes:
//   - Steps: (1) parse command-line flags, (2) load the config file and overlay the flags on it (loadConfig), (3)
//     SetupAgent, (4) RunAgent.
//   - In info mode (config.ModeInfo) it prints what the agent would have and returns PrintInfo's code; it never calls
//     SetupAgent, so there is no model call and no trace files.
//   - Plan mode (config.ModePlan) goes through SetupAgent and RunAgent like chat; only chat prints the welcome message
//     with the slash commands. When plan mode ends with *agent.ErrRunPlan ("Run now?" answered yes, or -yes), the
//     plan-mode agent is shut down and a new agent is set up for auto mode from the same flags (config.FlagsForAuto)
//     and run: the same as typing `-mode auto -plan <file> -yes`.
//   - Ctrl+C is delivered on a separate channel (intsigCh) so it can stop foreground work without killing the
//     program; SIGTERM cancels the root context.
//   - The deferred function cancels the context first (compaction and LLM requests use it), then shuts down the
//     current agent.
//   - A usage request (flag.ErrHelp) returns 0 quietly; a flag, config or mode error prints "error:" to stderr and
//     returns exitUsage. Other codes: exitCode.
func run() int {
	// Parse command-line flags -> flags variable
	flags, err := config.ParseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) { // usage was already printed
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}
	cfg, err := loadConfig(flags)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	// setup context - catch OS SIGTERM signals (e.g., process termination signal (kill)) and terminate gracefully
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	// handle OS interrupt signals (e.g., Ctrl+C) through a separate channel
	intsigCh := make(chan os.Signal, 1)
	signal.Notify(intsigCh, os.Interrupt)

	// info mode prints what the agent would have and exits; it never calls SetupAgent, so no model call and no trace files
	if cfg.AgentMode == config.ModeInfo {
		configFile := ""
		if flags.Config != nil {
			configFile = *flags.Config
		}
		return agent.PrintInfo(ctx, cfg, configFile, intsigCh)
	}

	// the agent instance; plan mode's "Run now?" replaces it with one set up for auto mode
	current := &agent.Agent{}
	defer func() {
		// context is passed to compaction, LLM requests, and other operations, so these should be canceled first, then we shutdown the agent
		stop()
		current.ShutdownAgent(ctx)
	}()

	for {
		// setup the agent - Ctrl+C (intsigCh) or SIGTERM during setup aborts it; the deferred ShutdownAgent then closes
		// any MCP servers that had already connected
		if err := current.SetupAgent(ctx, cfg, intsigCh); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "setup interrupted, exiting")
				return exitInterrupted
			}
			fmt.Fprintln(os.Stderr, "error setting up agent:", err)
			return exitFailed
		}
		// print a welcome message (debug purposes); plan mode explains its own commands when it starts
		if cfg.AgentMode == config.ModeChat {
			fmt.Fprintf(cfg.OutBuffer, WelcomeMsg, cfg.AgentVersion)
		}
		fmt.Fprintf(cfg.OutBuffer, "Using model %s\n", cfg.Model)

		// run the agent main loop (blocking call)
		err := current.RunAgent(ctx, intsigCh)
		next, runPlan := errors.AsType[*agent.ErrRunPlan](err)
		if !runPlan {
			return exitCode(err)
		}
		// plan mode's "Run now?": shut the plan-mode agent down, then set up a new one for auto mode on the plan
		current.ShutdownAgent(ctx)
		if cfg, err = loadConfig(config.FlagsForAuto(flags, next.Path)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitUsage
		}
		current = &agent.Agent{}
	}
}

// loadConfig builds the Config for flags: the config file (or defaults) with the flags overlaid, checked against the
// flags given.
//
// Notes:
//   - Configs already set by flags are not overridden by the file; the mode may come from the file, so it is checked
//     against the flags after the merge.
func loadConfig(flags config.Flags) (config.Config, error) {
	cfg, err := config.SetDefaultConfig(flags)
	if err != nil {
		return config.Config{}, err
	}
	cfg = config.FlagsOverlay(cfg, flags)
	if err := config.ValidateMode(cfg, flags); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// exitCode returns the exit code for RunAgent's error, and prints the error when nothing else has.
//
// Notes:
//   - nil → exitOK. Ctrl+C or SIGTERM (context.Canceled) → exitInterrupted. agent.ErrNeedsPerson (auto mode waiting
//     or declined; its report was printed) → exitNeedsPerson. Anything else is printed ("error running agent:") →
//     exitFailed.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.Canceled):
		return exitInterrupted
	case errors.Is(err, agent.ErrNeedsPerson):
		return exitNeedsPerson
	}
	fmt.Fprintln(os.Stderr, "error running agent:", err)
	return exitFailed
}
