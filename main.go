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
  <[o_o]>  minimal-agent v0.1.13
   /| |\   Ask me anything!
`
const WelcomeMsg = LogoLines + "\nType /clear to clear the conversation\n     /exit to exit\n     /compact to compact the conversation\n     /config to see the current agent configuration\n"

// Main does a few things:
// 1. Parses command-line flags and configuration -> Config instance
// 2. Calls function to setup agent -> Agent instance
// 3. Runs the agent main loop
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
		return
	}
	// print a welcome message (debug purposes)
	fmt.Fprintf(cfg.OutBuffer, WelcomeMsg)
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
