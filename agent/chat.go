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

// handleUserInput handles one line the user entered: a slash command, or a prompt that runs one ReAct turn. It reports
// whether the user asked to quit.
//
// Notes:
//   - Slash commands are /exit and /quit (return true), /clear, /summary (or /summarize), /compact and /config. An
//     unrecognized command prints a notice. Empty or all-space lines are skipped.
//   - msgContext is read once at the top, before /clear runs, and the context-cleared event records its size.
//   - /compact first checks ShouldStartCompaction, so a second compaction is never triggered while one is in progress.
//   - A prompt is traced as a task: task_start now, task_end (with status and duration) when this function returns. A
//     failed turn is traced with taskTrace.Fail ("canceled" for Ctrl+C, otherwise "error"; the message is recorded
//     either way) and reported through cs.Output.Error (a Ctrl+C cancel shows as [interrupted]); nothing from it is
//     added to history or context.
//   - Only after the ReAct loop succeeds are the turn's messages appended to history and context.
//   - [agent] With cs.Config.Debug the current chat context is dumped before the prompt is sent.
//   - [agent] Maybe consider passing a pointer to msgContext in future, to save a full copy of the context into
//     reActLoop().
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
			taskTrace.Record(trace.ContextCleared, trace.GetContextClearedData(len(msgContext))) // msgContext was read at the top of this function, before Clear
			cs.MsgContext.Clear()
			cs.Output.System("Chat context cleared")
		case "/summary", "/summarize":
			taskTrace := trace.StartTask(cs.Recorder, line)
			defer taskTrace.End()
			summaryString, err := contextwindow.SummarizeChatContext(ctx, cs.getSessionProvider(taskTrace.ID()), msgContext, cs.Config.SummarizerPrompt, strings.TrimSpace(lineRest))
			if err != nil {
				taskTrace.Fail(err)
				cs.Output.Error("Summarization error", err) // a Ctrl+C cancel shows as [interrupted]
				return false
			}
			cs.Output.System("Chat summary: " + summaryString)
		case "/compact":
			// we need to check if a compaction is already happening so we don't trigger a second compaction concurrently
			if !cs.MsgContext.ShouldStartCompaction() {
				cs.Output.System("Compaction already in progress. Skipping this compaction request.")
				return false
			}
			taskTrace := trace.StartTask(cs.Recorder, line)
			defer taskTrace.End()
			cs.Output.System("Compaction triggered...")
			summaryMsg, err := compactAndTrace(ctx, cs, taskTrace.ID(), "manual", strings.TrimSpace(lineRest))
			if err != nil {
				taskTrace.Fail(err)
				cs.Output.Error("Compaction error", err) // a Ctrl+C cancel shows as [interrupted]
				return false
			}
			cs.Output.System("Compaction complete. Chat summary: " + summaryMsg.Content)
		case "/config":
			config.PrintConfig(cs.Config)
		default:
			// if the command is not recognized, print an error message
			cs.Output.System("Unrecognized command: " + lineFirst)
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
		taskTrace.Fail(err)                      // "canceled" for Ctrl+C, otherwise "error"; the message is recorded either way
		cs.Output.Error("ReAct loop error", err) // a Ctrl+C cancel shows as [interrupted]; nothing is added to memory
		return false
	}

	// add new messages to history and context
	cs.MsgHistory.Append(turnMsgs)
	cs.MsgContext.AddMessages(turnMsgs)

	return false
}

// maybeStartAutoCompaction starts auto-compaction in a separate goroutine when the context has reached the
// auto-compaction threshold and a compaction is not already in progress.
//
// Notes:
//   - CompactWG.Add(1) must happen synchronously BEFORE the goroutine starts, so ShutdownAgent's wait cannot miss it.
//   - Must get the root context (see chatLoop): a turn's Ctrl+C must not cancel background compaction.
//   - On failure it reports "Auto-compaction error" and does not also claim it completed.
func maybeStartAutoCompaction(ctx context.Context, cs *ChatSession) {
	if cs.MsgContext.IsAutoCompactionNeeded() && cs.MsgContext.ShouldStartCompaction() {
		cs.MsgContext.CompactWG.Add(1) // must be here synchronously BEFORE we enter the goroutine
		go func() {
			defer cs.MsgContext.CompactWG.Done() // ensure the WaitGroup counter is decremented when the goroutine finishes
			cs.Output.System("Auto-compaction triggered...")
			_, err := compactAndTrace(ctx, cs, cs.Recorder.RunID(), "auto", "")
			if err != nil {
				cs.Output.Error("Auto-compaction error", err)
				return // don't also claim it completed
			}
			cs.Output.System("Auto-compaction complete.")
		}()
	}
}

// confirmAllowAll decides whether ask-rule calls run without asking this session (cs.AllowAskRules).
//
// Notes:
//   - Only with approval "allow-all", and only once the user confirms it on the terminal; anything else (No, Enter,
//     Ctrl+C, no terminal) keeps asking. Deny rules apply either way.
//   - With no terminal (tty nil) it prints that ask-rule calls will be denied.
//   - [agent] Asked at startup rather than trusted from the config alone: a forgotten "approval": "allow-all" in a
//     config file would otherwise silently drop every approval prompt, and the prompt is what catches a
//     prompt-injected call.
func confirmAllowAll(ctx context.Context, cs *ChatSession, tty *ttyReader) {
	if cs.Config.Approval != config.ApprovalAllowAll {
		return
	}
	if tty == nil {
		cs.Output.System("approval is allow-all, but there is no terminal to confirm it: tool calls that match ask rules will be denied")
		return
	}
	cs.Output.System(fmt.Sprintf("approval is allow-all: tool calls that match ask rules %v will run without asking; deny rules %v still apply", cs.Config.AskRules, cs.Config.DenyRules))
	if tty.AskYesNo(ctx, "Run ask-rule tool calls without asking for this session? [y/N] ") {
		cs.AllowAskRules = true
		cs.Output.System("allow-all on: ask-rule tool calls run without asking (deny rules still apply)")
		return
	}
	cs.Output.System("allow-all declined: you will be asked before each tool call that matches an ask rule")
}

// ***********************************************************//
// Run loop for handling chat session
// ***********************************************************//

// runChatLoop runs the chat loop with the prompt reader newPromptReader picks for cs.InBuffer.
//
// Notes:
//   - Thin wrapper over chatLoop; the reader is a ttyReader only for a real terminal.
func runChatLoop(ctx context.Context, intsigChRecv <-chan os.Signal, cs *ChatSession) error {
	return chatLoop(ctx, intsigChRecv, cs, newPromptReader(ctx, cs.InBuffer, cs.OutBuffer))
}

// chatLoop is runChatLoop with the prompt reader passed in: read a prompt, handle it, maybe start auto-compaction, repeat.
//
// Notes:
//   - newPromptReader only builds a ttyReader for a real terminal, so tests pass a scripted one (newTTYReaderWith +
//     auto.Pilot) to reach the ask paths.
//   - tty is nil when stdin is not a terminal: nobody to ask, so ask-rule calls are denied. Otherwise it sets
//     cs.PromptAllowDenyCall; tool name and arguments are printed with %q because a name from an MCP server could hold
//     terminal escape codes.
//   - io.EOF from the reader (Ctrl+D or end of piped input) is a normal quit, not a failure.
//   - Each turn runs under createInterruptWatcher, after discardStrayInterrupts. stop is called when the turn is done;
//     it releases the watcher so it can't take a later interrupt.
//   - If the root context was cancelled (SIGTERM) the loop returns its error.
//   - Auto-compaction gets the non-interruptible root context; this way, interrupting a turn with Ctrl+C doesn't
//     affect background compaction.
func chatLoop(ctx context.Context, intsigChRecv <-chan os.Signal, cs *ChatSession, reader promptReader) error {
	tty, _ := reader.(*ttyReader) // nil when stdin is not a terminal: nobody to ask, so ask-rule calls are denied
	if tty != nil {
		cs.PromptAllowDenyCall = func(ctx context.Context, call model.ToolCall, reason error) bool {
			fmt.Fprintln(cs.OutBuffer, "[system] Tool call needs approval:")
			fmt.Fprintf(cs.OutBuffer, "Tool: %q\n", call.Function.Name) // %q: a name from an MCP server could hold terminal escape codes
			for _, line := range approvalPreview(call) {                // a short preview, not the whole arguments (a written file can be 1 MB)
				fmt.Fprintln(cs.OutBuffer, line)
			}
			fmt.Fprintln(cs.OutBuffer, "Reason:", reason)
			return tty.AskYesNo(ctx, "Allow once? [y/N] ")
		}
	}
	confirmAllowAll(ctx, cs, tty)

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
		turnCtx, stop := createInterruptWatcher(ctx, intsigChRecv)
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

// discardStrayInterrupts drops a stray Ctrl+C key press left over from before this turn.
//
// Notes:
//   - E.g. a second press after the previous turn was already cancelled (the user gets impatient and presses Ctrl+C a
//     bunch of times; the 1-buffer channel absorbs the second press and ignores the rest), so it can't cancel the turn
//     about to start.
//   - Non-blocking: drains at most one signal.
func discardStrayInterrupts(interrupts <-chan os.Signal) {
	select {
	case <-interrupts:
	default:
	}
}

// createInterruptWatcher starts a watcher goroutine and returns a child of ctx that is cancelled when an interrupt
// (Ctrl+C) arrives, plus a stop function.
//
// Notes:
//   - ctx is the root agent context (cancelled by SIGTERM or on quit). Used for one chat turn, and for setup.
//   - stop() cancels the returned context too, so nothing that outlives the turn or setup may keep it. It also waits
//     until the watcher goroutine has exited, so the watcher cannot take a later interrupt.
//   - The done channel is closed when the turn (or setup) is done (i.e., stop() is called). stop must be called once.
func createInterruptWatcher(ctx context.Context, interrupts <-chan os.Signal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}) // this channel is closed when the turn (or setup) is done (i.e., stop() is called)

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

// *******************************************************************//
// Chat that includes showing progress dots when the model is thinking
// *******************************************************************//

// progressDotInterval is how often a "thinking" dot is printed while waiting for the model. A variable rather than a constant so tests can shorten it.
var progressDotInterval = 2 * time.Second

// chatWithProgress wraps a call to the model provider's Chat request/response method, and tells cs.Output it is waiting.
//
// Notes:
//   - Chat mode on a terminal prints thinking dots while waiting (see Output.Thinking); the dots stop when the reply or
//     an error arrives.
func chatWithProgress(ctx context.Context, cs *ChatSession, provider model.Provider, msgs []model.ChatMessage, toolDefs []model.ToolDef) (model.ChatMessage, error) {
	stop := cs.Output.Thinking()
	defer stop()
	return provider.Chat(ctx, msgs, toolDefs)
}

// startProgressDots prints a "." to out every interval until stop is called.
//
// Notes:
//   - If at least one dot was printed, stop ends the line with a newline.
//   - stop waits for the printing goroutine to exit.
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

// shouldShowProgress reports whether thinking dots belong in this run's output: e.g., chat mode, writing to a real
// terminal.
//
// Notes:
//   - Not for headless/oneshot (their output may be JSON or read by a program), and not for when output is redirected
//     or captured (pipes, files, tests).
func shouldShowProgress(cfg config.Config) bool {
	return cfg.AgentMode == config.ModeChat && isTerminal(cfg.OutBuffer)
}
