package agent

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
)

// progressDotInterval is how often a "thinking" dot is printed while waiting for the model.
// A variable rather than a constant so tests can shorten it.
var progressDotInterval = 2 * time.Second

// chatWithProgress sends one model request of a turn. When cs.ShowProgress is set (chat mode on a
// terminal), it prints a dot every progressDotInterval while waiting, then ends the line, e.g.:
//
//	> What are best practices for writing Go code?
//	....
//	[assistant] ...
func chatWithProgress(ctx context.Context, cs *ChatSession, msgs []model.ChatMessage, toolDefs []model.ToolDef) (model.ChatMessage, error) {
	if cs.ShowProgress {
		stop := startProgressDots(cs.OutBuffer, progressDotInterval)
		defer stop()
	}
	return cs.Provider.Chat(ctx, msgs, toolDefs)
}

// startProgressDots prints a "." to out every interval until stop is called. stop waits for the
// printing goroutine to exit (so no dot can land after the reply), then ends the line if any dots
// were printed. If stop comes before the first tick, nothing is printed at all.
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

// shouldShowProgress reports whether thinking dots belong in this run's output: chat mode, writing to a
// real terminal. Never for headless/oneshot (their output may be JSON or read by a program), and never
// when output is redirected or captured (pipes, files, tests).
func shouldShowProgress(cfg config.Config) bool {
	return cfg.AgentMode == config.ModeChat && isTerminal(cfg.OutBuffer)
}
