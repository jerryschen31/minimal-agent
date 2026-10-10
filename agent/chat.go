package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/model"
)

//***********************************************************//
// Parse and handle user input
//***********************************************************//

func handleUserInput(ctx context.Context, cs *ChatSession, line string) bool {
	// Implementation for handling user input goes here
	msgContext := cs.MsgContext.GetMessages()
	line = strings.TrimSpace(line)
	lineFirst, lineRest, _ := strings.Cut(line, " ")
	// skip empty lines (user just pushes enter or just has spaces)
	if lineFirst == "" {
		return false
	}
	// parse slash commands
	if strings.HasPrefix(lineFirst, "/") {
		switch lineFirst {
		case "/exit", "/quit":
			return true
		case "/clear":
			cs.MsgContext.Clear()
			fmt.Fprintln(cs.OutBuffer, "[system] Chat context cleared")
		case "/summary", "/summarize":
			summaryString, err := contextwindow.SummarizeChatContext(ctx, cs.Provider, msgContext, strings.TrimSpace(lineRest))
			if err != nil {
				if errors.Is(err, context.Canceled) {
					fmt.Fprintln(cs.OutBuffer, "[interrupted]") // Ctrl+C cancelled the turn's context
					return false
				}
				fmt.Fprintln(cs.OutBuffer, "[error] Summarization error:", err)
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Chat summary:", summaryString)
		case "/compact":
			// we need to check if a compaction is already happening so we don't trigger a second compaction concurrently
			if !cs.MsgContext.ShouldStartCompaction() {
				fmt.Fprintln(cs.OutBuffer, "[system] Compaction already in progress. Skipping this compaction request.")
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Compaction triggered...")
			summaryMsg, err := contextwindow.CompactChatContext(ctx, cs.MsgContext, cs.Provider, strings.TrimSpace(lineRest))
			if err != nil {
				if errors.Is(err, context.Canceled) {
					fmt.Fprintln(cs.OutBuffer, "[interrupted]") // Ctrl+C cancelled the turn's context
					return false
				}
				fmt.Fprintln(cs.OutBuffer, "[error] Compaction error:", err)
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Compaction complete. Chat summary:", summaryMsg.Content)
		case "/config":
			config.PrintConfig(cs.Config)
		default:
			// if the command is not recognized, print an error message
			fmt.Fprintln(cs.OutBuffer, "[system] Unrecognized command:", lineFirst)
		}
		return false
	}

	// [debug] print the current chat context before sending the prompt to the chat provider
	if cs.Config.Debug {
		contextwindow.DebugChatContext(cs.OutBuffer, msgContext)
	}

	// initial user message
	userMsg := model.CreateChatMessage("user", "user", line, "", nil)

	// maybe consider passing a pointer to msgContext in future - to save on a full-copy of the context to reActLoop()
	turnMsgs, err := reActLoop(ctx, cs, msgContext, userMsg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(cs.OutBuffer, "[interrupted]") // Ctrl+C cancelled the turn; nothing is added to memory
			return false
		}
		fmt.Fprintln(cs.OutBuffer, "[error] ReAct loop error:", err)
		return false
	}

	// add new messages to history and context
	cs.MsgHistory.Append(turnMsgs)
	cs.MsgContext.AddMessages(turnMsgs)

	return false
}

// check if the context has reached the auto-compaction threshold and a compaction is not already in progress - if so, trigger auto-compaction in a separate goroutine
func maybeStartAutoCompaction(ctx context.Context, cs *ChatSession) {
	if cs.MsgContext.IsAutoCompactionNeeded() && cs.MsgContext.ShouldStartCompaction() {
		cs.MsgContext.CompactWG.Add(1) // must be here synchronously BEFORE we enter the goroutine
		go func() {
			defer cs.MsgContext.CompactWG.Done() // ensure the WaitGroup counter is decremented when the goroutine finishes
			fmt.Fprintln(cs.OutBuffer, "[system] Auto-compaction triggered...")
			_, err := contextwindow.CompactChatContext(ctx, cs.MsgContext, cs.Provider, "")
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "[error] Auto-compaction error:", err)
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Auto-compaction complete.")
		}()
	}
}

// ***********************************************************//
// Run loop for handling chat session
// ***********************************************************//

func runChatLoop(ctx context.Context, intsigChRecv <-chan os.Signal, cs *ChatSession) {
	reader := newPromptReader(ctx, cs.InBuffer, cs.OutBuffer)

	for {
		line, err := reader.ReadPrompt(ctx)
		if err != nil {
			return
		}

		// this is here to drain any stray Ctrl+C presses from the previous turn (e.g., the user gets impatient when canceling a turn and presses Ctrl+C a bunch of times; the 1-buffer channel absorbs that second press and ignores the rest)
		discardStrayInterrupts(intsigChRecv)

		// turnCtx is a cancellable context for the current turn (i.e. turn can be cancelled using Ctrl+C)
		turnCtx, stop := createTurnWatcher(ctx, intsigChRecv)
		quit := handleUserInput(turnCtx, cs, line)
		// stop is called when a turn is done; it releases the watcher so it can't take a later interrupt.
		stop()
		if quit || ctx.Err() != nil {
			return
		}

		// use non-interruptible root context; this way, interrupting a turn with Ctrl+C doesn't affect background compaction
		maybeStartAutoCompaction(ctx, cs)
	}
}

// drops stray Ctrl+C key presses left over from before this turn (e.g. a second press
// after the previous turn was already cancelled), so it can't cancel the turn about to start
func discardStrayInterrupts(interrupts <-chan os.Signal) {
	select {
	case <-interrupts:
	default:
	}
}

// createTurnWatcher starts a watcher goroutine and returns a child of the root agent context  (cancelled by SIGTERM or on quit) that can be cancelled when an interrupt (Ctrl+C) arrives.
func createTurnWatcher(ctx context.Context, interrupts <-chan os.Signal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}) // this channel is closed when the turn is done (i.e., stop() is called)

	var watcher sync.WaitGroup
	// watcher goroutine checks for either an interrupt (Ctrl+C) or the turn being done (done channel closed)
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-interrupts:
			cancel()
		case <-done:
		}
	}()
	stop := func() {
		close(done)
		cancel()
		watcher.Wait() // this waits until the watcher goroutine has exited
	}
	return ctx, stop
}
