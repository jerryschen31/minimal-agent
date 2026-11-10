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

// main parses flags and the config file, then sets up and runs the agent.
//
// Notes:
//   - Steps: (1) parse command-line flags into a flags value, (2) load the config file and overlay the flags on it to
//     get a Config, (3) call SetupAgent to get an Agent, (4) run the agent main loop.
//   - In info mode (config.ModeInfo) it prints what the agent would have and exits through os.Exit; it never calls
//     SetupAgent, so there is no model call and no trace files.
//   - Plan mode (config.ModePlan) goes through SetupAgent and RunAgent like chat; only chat prints the welcome message
//     with the slash commands.
//   - Ctrl+C is delivered on a separate channel (intsigCh) so it can stop foreground work without killing the
//     program; SIGTERM cancels the root context.
//   - The deferred function cancels the context first (compaction and LLM requests use it), then calls ShutdownAgent;
//     os.Exit skips deferred calls, so a path that exits early must not have anything to shut down yet.
//   - A usage request (flag.ErrHelp) returns quietly; a flag, config or mode error prints "error:" to stderr and
//     exits with code 2.
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
	// the mode may have come from the config file, so check it against the flags again now
	if err := config.ValidateMode(cfg, flags); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	// setup context - catch OS SIGTERM signals (e.g., process termination signal (kill)) and terminate gracefully
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)

	// handle OS interrupt signals (e.g., Ctrl+C) through a separate channel
	intsigCh := make(chan os.Signal, 1)
	signal.Notify(intsigCh, os.Interrupt)

	// info mode prints what the agent would have and exits; it never calls SetupAgent, so no model call and no trace files
	if cfg.AgentMode == config.ModeInfo {
		configFile := ""
		if flags.Config != nil {
			configFile = *flags.Config
		}
		code := agent.PrintInfo(ctx, cfg, configFile, intsigCh)
		stop()
		// [agent] os.Exit skips deferred calls; safe here because none are registered yet and PrintInfo has
		// already closed every MCP server it started
		os.Exit(code)
	}

	// create the agent instance
	myAgent := agent.Agent{}
	defer func() {
		// context is passed to compaction, LLM requests, and other operations, so these should be canceled first, then we shutdown the agent
		stop()
		myAgent.ShutdownAgent(ctx)
	}()

	// setup the agent - Ctrl+C (intsigCh) or SIGTERM during setup aborts it; the deferred ShutdownAgent then closes
	// any MCP servers that had already connected
	if err := myAgent.SetupAgent(ctx, cfg, intsigCh); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "setup interrupted, exiting")
		} else {
			fmt.Fprintln(os.Stderr, "error setting up agent:", err)
		}
		return
	}
	// print a welcome message (debug purposes); plan mode explains its own commands when it starts
	if cfg.AgentMode == config.ModeChat {
		fmt.Fprintf(cfg.OutBuffer, WelcomeMsg, cfg.AgentVersion)
	}
	fmt.Fprintf(cfg.OutBuffer, "Using model %s\n", cfg.Model)

	// run the agent main loop (blocking call)
	if err := myAgent.RunAgent(ctx, intsigCh); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error running agent:", err)
		return
	}

}

// helper function for handling a fatal error and exiting the program immediately
// func fatal(err error) {
// 	fmt.Fprintln(os.Stderr, "fatal:", err)
// 	os.Exit(1)
// }
