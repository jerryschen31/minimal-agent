package agent

import (
	"bufio"
	"context"
	"fmt"
	"strings"

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
		case "/exit":
			return true
		case "/clear":
			cs.MsgContext.Clear()
			fmt.Fprintln(cs.OutBuffer, "[system] Chat history cleared")
		case "/summary", "/summarize":
			summaryString, err := contextwindow.SummarizeChatContext(ctx, cs.Provider, msgContext, strings.TrimSpace(lineRest))
			if err != nil {
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
		// user may have entered a prompt following the slash command (e.g., /clear <A brand new prompt>)
		// line = strings.TrimSpace(lineRest)
		// if line == "" {
		// 	return false
		// }
	}

	// [debug] print the current chat context before sending the prompt to the chat provider
	contextwindow.DebugChatContext(msgContext)

	// initial user message
	userMsg := model.CreateChatMessage("user", "user", line, "", nil)

	// maybe consider passing a pointer to msgContext in future - to save on a full-copy of the context to reActLoop()
	turnMsgs, err := reActLoop(ctx, cs, msgContext, userMsg)
	if err != nil {
		fmt.Fprintln(cs.OutBuffer, "[error] ReAct loop error:", err)
		return false
	}

	// add new messages to history and context
	cs.MsgHistory.Append(turnMsgs)
	cs.MsgContext.AddMessages(turnMsgs)

	// check if the chat history has reached the auto-compaction threshold and a compaction is not already in progress - if so, trigger auto-compaction in a separate goroutine
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
	return false
}

// ***********************************************************//
// Run loop for handling chat session
// ***********************************************************//

func runChatLoop(ctx context.Context, cs *ChatSession) {
	lines := make(chan string)

	go func() {
		defer close(lines)
		r := bufio.NewReader(cs.InBuffer)
		for {
			lineRead, err := r.ReadString('\n') // read a line of user input from the input buffer (blocking)
			if lineRead != "" {
				select {
				case lines <- lineRead: // send read line to the lines channel
				case <-ctx.Done():
					return // context cancel signal closes ctx.Done() channel, which exits this goroutine immediately
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		fmt.Fprintf(cs.OutBuffer, "\n> ")
		select {
		// waits for context to be canceled (which closes the ctx.Done() channel), or for a new line of user input from the lines channel
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			quitSignal := handleUserInput(ctx, cs, line)
			if quitSignal {
				return
			}
		}
	}
}
