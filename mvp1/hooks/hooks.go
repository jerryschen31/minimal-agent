// Package hooks holds agent.Hook implementations: observability, authorization,
// and human-in-the-loop. All are optional and composable via agent.Hooks.
package hooks

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"mvp1/agent"
)

// [agent] Base is a no-op Hook to embed so a custom hook implements only what it needs.
type Base struct{}

func (Base) BeforeLLM(context.Context, int, []agent.Message)          {}
func (Base) AfterLLM(context.Context, int, agent.Response, error)     {}
func (Base) BeforeTool(context.Context, agent.ToolCall) error         { return nil }
func (Base) AfterTool(context.Context, agent.ToolCall, agent.Message) {}

// [agent] Logger is the seed of observability: one line per LLM call and tool
// call, with token usage and latency. An OpenTelemetry/Langfuse/Braintrust
// tracer or a cost meter is the same four callbacks emitting spans instead.
type Logger struct {
	Base
	W      io.Writer
	Prefix string
	starts sync.Map // tool call id → time.Time
}

func (l *Logger) BeforeLLM(_ context.Context, step int, msgs []agent.Message) {
	fmt.Fprintf(l.W, "%s[step %d] → llm (%d msgs)\n", l.Prefix, step, len(msgs))
}

func (l *Logger) AfterLLM(_ context.Context, step int, r agent.Response, err error) {
	if err != nil {
		fmt.Fprintf(l.W, "%s[step %d] ← llm error: %v\n", l.Prefix, step, err)
		return
	}
	var names []string
	for _, c := range r.Message.ToolCalls {
		names = append(names, c.Name)
	}
	fmt.Fprintf(l.W, "%s[step %d] ← llm in=%d out=%d tools=%v text=%q\n", l.Prefix, step,
		r.Usage.Input, r.Usage.Output, names, preview(r.Message.Content, 80))
}

func (l *Logger) BeforeTool(_ context.Context, c agent.ToolCall) error {
	l.starts.Store(c.ID, time.Now())
	fmt.Fprintf(l.W, "%s  ⚙ %s %s\n", l.Prefix, c.Name, preview(string(c.Args), 120))
	return nil
}

func (l *Logger) AfterTool(_ context.Context, c agent.ToolCall, r agent.Message) {
	var dur time.Duration
	if t, ok := l.starts.LoadAndDelete(c.ID); ok {
		dur = time.Since(t.(time.Time)).Round(time.Millisecond)
	}
	status := "ok"
	if r.IsError {
		status = "ERR"
	}
	fmt.Fprintf(l.W, "%s  ⚙ %s %s %s %q\n", l.Prefix, c.Name, status, dur, preview(r.Content, 120))
}

// [agent] Allowlist is a permission policy: only listed tools may run. A denial
// returns to the model as an observation so it can choose another approach.
type Allowlist struct {
	Base
	Allowed map[string]bool
}

func (a Allowlist) BeforeTool(_ context.Context, c agent.ToolCall) error {
	if !a.Allowed[c.Name] {
		return fmt.Errorf("tool %q is not permitted by policy", c.Name)
	}
	return nil
}

// [agent] Approval is human-in-the-loop: asks on the terminal before running any
// tool in Tools (or every tool if Tools is empty). Parallel tool calls are
// serialised by the mutex so prompts never interleave.
type Approval struct {
	Base
	Tools map[string]bool
	In    *bufio.Reader
	Out   io.Writer
	mu    sync.Mutex
}

func (a *Approval) BeforeTool(_ context.Context, c agent.ToolCall) error {
	if len(a.Tools) > 0 && !a.Tools[c.Name] {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintf(a.Out, "\n⚠ allow %s %s ? [y/N] ", c.Name, c.Args)
	line, _ := a.In.ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
		return fmt.Errorf("user denied %s", c.Name)
	}
	return nil
}

func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
