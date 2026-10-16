package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jerryschen31/minimal-agent/mvp3/agent"
	"github.com/jerryschen31/minimal-agent/mvp3/config"
)

const LogoLines = `
  <[o_o]>  minimal-agent v0.1.13
   /| |\   Ask me anything!
`
const WelcomeMsg = LogoLines + "\nType /clear to clear the conversation\n     /exit to exit\n     /compact to compact the conversation\n     /config to see the current agent configuration\n"

// Main does a few things:
// 1. Parses command-line flags and configuration -> Config instance
// 2. Calls function to setup agent -> Agent instance
// 3. Runs the agent main loop
// Exit codes: 0 ok; 1 run failed or objective not met; 2 usage or config error (incl. a bad objective
// file); 130 an autonomous run interrupted by Ctrl+C; 143 terminated by SIGTERM.
func main() {
	// Parse command-line flags -> flags variable
	flags, err := config.ParseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) { // usage was already printed
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	// load config file - configs already set by flags are not overridden
	cfg, err := config.SetDefaultConfig(flags)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	// flag values override any corresponding values from the config file
	cfg = config.FlagsOverlay(cfg, flags)

	// check the merged config: a mode set in the config file is checked here too, not only a -mode flag
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	// [agent] os.Exit skips deferred calls, so everything that needs cleanup (MCP servers, compaction)
	// lives in run(), whose defers have finished by the time it returns the exit code
	os.Exit(run(cfg))
}

// run sets up and runs the agent, shuts it down, and returns the process exit code.
func run(cfg config.Config) int {
	// setup context - catch OS SIGTERM signals (e.g., process termination signal (kill)) and terminate gracefully
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)

	// handle OS interrupt signals (e.g., Ctrl+C) through a separate channel
	intsigCh := make(chan os.Signal, 1)
	signal.Notify(intsigCh, os.Interrupt)

	// create the agent instance
	myAgent := agent.Agent{}
	defer func() {
		// context is passed to compaction, LLM requests, and other operations, so these should be canceled first, then we shutdown the agent
		stop()
		myAgent.ShutdownAgent(ctx)
	}()

	// setup the agent - make sure errors are handled properly
	if err := myAgent.SetupAgent(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error setting up agent:", err)
		if errors.Is(err, agent.ErrBadObjective) {
			return 2
		}
		return 1
	}
	if cfg.AgentMode == config.ModeChat {
		// print a welcome message (debug purposes)
		fmt.Fprint(cfg.OutBuffer, WelcomeMsg)
		fmt.Fprintf(cfg.OutBuffer, "Using model %s\n", cfg.Model)
	}

	// run the agent main loop (blocking call)
	err := myAgent.RunAgent(ctx, intsigCh)
	switch {
	case ctx.Err() != nil: // SIGTERM
		return 143
	case err == nil:
		return 0
	case errors.Is(err, agent.ErrRunInterrupted):
		return 130
	case errors.Is(err, agent.ErrObjectiveNotMet):
		return 1 // the report has already been printed
	case errors.Is(err, context.Canceled):
		return 130
	}
	fmt.Fprintln(os.Stderr, "error running agent:", err)
	return 1
}
