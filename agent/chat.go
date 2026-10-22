package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/trace"
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
			taskTrace := trace.StartTask(cs.Recorder, "/clear")
			defer taskTrace.End()
			cs.MsgContext.Clear()
			fmt.Fprintln(cs.OutBuffer, "[system] Chat context cleared")
		case "/summary", "/summarize":
			taskTrace := trace.StartTask(cs.Recorder, line)
			defer taskTrace.End()
			summaryString, err := contextwindow.SummarizeChatContext(ctx, cs.tracedProvider(taskTrace.ID()), msgContext, strings.TrimSpace(lineRest))
			if err != nil {
				taskTrace.Fail(err)
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
			taskTrace := trace.StartTask(cs.Recorder, line)
			defer taskTrace.End()
			fmt.Fprintln(cs.OutBuffer, "[system] Compaction triggered...")
			summaryMsg, err := compactAndTrace(ctx, cs, taskTrace.ID(), "manual", strings.TrimSpace(lineRest))
			if err != nil {
				taskTrace.Fail(err)
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

	// trace this user turn: task_start now, task_end (with status and duration) when this function returns
	taskTrace := trace.StartTask(cs.Recorder, line)
	defer taskTrace.End()

	// ReAct turn loop - maybe consider passing a pointer to msgContext in future - to save on a full-copy of the context to reActLoop()
	turnMsgs, err := reActLoop(ctx, cs, taskTrace, msgContext, userMsg)
	if err != nil {
		taskTrace.Fail(err) // "canceled" for Ctrl+C, otherwise "error"; the message is recorded either way
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(cs.OutBuffer, "[interrupted]") // Ctrl+C canceled the turn; nothing is added to memory
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
			_, err := compactAndTrace(ctx, cs, cs.Recorder.RunID(), "auto", "")
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "[error] Auto-compaction error:", err)
				return // don't also claim it completed
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Auto-compaction complete.")
		}()
	}
}

// ***********************************************************//
// Run loop for handling chat session
// ***********************************************************//

func runChatLoop(ctx context.Context, intsigChRecv <-chan os.Signal, cs *ChatSession) error {
	reader := newPromptReader(ctx, cs.InBuffer, cs.OutBuffer)

	for {
		line, err := reader.ReadPrompt(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) { // Ctrl+D or end of piped input is a normal quit, not a failure
				return nil
			}
			return err
		}

		// this is here to drain any stray Ctrl+C presses from the previous turn (e.g., the user gets impatient when canceling a turn and presses Ctrl+C a bunch of times; the 1-buffer channel absorbs that second press and ignores the rest)
		discardStrayInterrupts(intsigChRecv)

		// turnCtx is a cancellable context for the current turn (i.e. turn can be cancelled using Ctrl+C)
		turnCtx, stop := createTurnWatcher(ctx, intsigChRecv)
		quit := handleUserInput(turnCtx, cs, line)
		// stop is called when a turn is done; it releases the watcher so it can't take a later interrupt.
		stop()
		if quit {
			return nil
		}
		if err := ctx.Err(); err != nil { // the root context was cancelled (SIGTERM)
			return err
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

// ***********************************************************//
// Show progress dots when the model is thinking
// ***********************************************************//

// progressDotInterval is how often a "thinking" dot is printed while waiting for the model. A variable rather than a constant so tests can shorten it.
var progressDotInterval = 2 * time.Second

// chatWithProgress wraps a call to the model provider's Chat request/response method, and outputs thinking dots to the output buffer when cs.ShowProgress is set (e.g., chat mode on a terminal).
// Example:
//
//	> What are best practices for writing Go code?
//	....
//	[assistant] ...
func chatWithProgress(ctx context.Context, cs *ChatSession, provider model.Provider, msgs []model.ChatMessage, toolDefs []model.ToolDef) (model.ChatMessage, error) {
	if cs.ShowProgress {
		stop := startProgressDots(cs.OutBuffer, progressDotInterval)
		defer stop()
	}
	return provider.Chat(ctx, msgs, toolDefs)
}

// startProgressDots prints a "." to out every interval until stop is called.
func startProgressDots(out io.Writer, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		dots := 0
		for {
			select {
			case <-ticker.C:
				fmt.Fprint(out, ".")
				dots++
			case <-done:
				if dots > 0 {
					fmt.Fprintln(out)
				}
				return
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// shouldShowProgress reports whether thinking dots belong in this run's output: e.g., chat mode, writing to a real terminal.
// Not for headless/oneshot (their output may be JSON or read by a program), and not for when output is redirected or captured (pipes, files, tests).
func shouldShowProgress(cfg config.Config) bool {
	return cfg.AgentMode == config.ModeChat && isTerminal(cfg.OutBuffer)
}
