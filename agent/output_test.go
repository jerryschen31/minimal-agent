package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/memory"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// eventLog is a mutex-guarded list of events, shared by recordingOutput and loggingTool so a test sees
// what the loop reported and what it ran in one ordered list.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// recordingOutput is an Output that records every call instead of printing.
type recordingOutput struct{ log *eventLog }

func (o recordingOutput) Assistant(text string)  { o.log.add("assistant:" + text) }
func (o recordingOutput) ToolCalled(name string) { o.log.add("tool_called:" + name) }
func (o recordingOutput) System(text string)     { o.log.add("system:" + text) }
func (o recordingOutput) Error(what string, err error) {
	o.log.add("error:" + what + ": " + err.Error())
}
func (o recordingOutput) Interrupted() { o.log.add("interrupted") }
func (o recordingOutput) Thinking() (stop func()) {
	o.log.add("thinking")
	return func() { o.log.add("stop") }
}

// loggingTool is a Tool that records "ran:<name>" when it is called, so a test can check the loop reports
// ToolCalled before the tool runs.
type loggingTool struct {
	name string
	log  *eventLog
}

func (t loggingTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(t.name, "records that it ran", nil)
}

func (t loggingTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	t.log.add("ran:" + t.name)
	return "ok", nil
}

// ---- chatOutput ----

// - Verify chatOutput writes the exact chat-mode lines for a reply (including an empty one) and a tool call
func Test_Unit_Output_ChatOutput_AssistantAndToolCalledText(t *testing.T) {
	cases := []struct {
		name string
		call func(o chatOutput)
		want string
	}{
		{"assistant text", func(o chatOutput) { o.Assistant("hello there") }, "[assistant] hello there\n"},
		{"assistant empty (tool-only step)", func(o chatOutput) { o.Assistant("") }, "[assistant] \n"},
		{"tool called", func(o chatOutput) { o.ToolCalled("fake_read") }, "[tool] Tool called: fake_read\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			c.call(chatOutput{w: &out})
			if got := out.String(); got != c.want {
				t.Errorf("expected %q, got %q", c.want, got)
			}
		})
	}
}

// - Verify chatOutput.System and Interrupted print their exact chat-mode lines
func Test_Unit_Output_ChatOutput_SystemAndInterruptedText(t *testing.T) {
	cases := []struct {
		name string
		call func(o chatOutput)
		want string
	}{
		{"system", func(o chatOutput) { o.System("Chat context cleared") }, "[system] Chat context cleared\n"},
		{"interrupted", func(o chatOutput) { o.Interrupted() }, "[interrupted]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			c.call(chatOutput{w: &out})
			if got := out.String(); got != c.want {
				t.Errorf("expected %q, got %q", c.want, got)
			}
		})
	}
}

// - Verify chatOutput.Error prints "[error] <what>: <err>" for a real failure (plain, wrapped, deadline) and reports a
// cancel (plain or wrapped) as "[interrupted]" instead, with no "[error]" line
func Test_Unit_Output_ChatOutput_ErrorRoutesCanceledToInterrupted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"plain error", errors.New("fake model down"), "[error] Fake step: fake model down\n"},
		{"wrapped error", fmt.Errorf("chat: %w", errors.New("fake 500")), "[error] Fake step: chat: fake 500\n"},
		{"deadline exceeded is not a cancel", context.DeadlineExceeded, "[error] Fake step: context deadline exceeded\n"},
		{"canceled", context.Canceled, "[interrupted]\n"},
		{"wrapped canceled", fmt.Errorf("chat request: %w", context.Canceled), "[interrupted]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			chatOutput{w: &out}.Error("Fake step", c.err)
			if got := out.String(); got != c.want {
				t.Errorf("expected %q, got %q", c.want, got)
			}
		})
	}
}

// - Verify Thinking with showProgress off returns a non-nil stop that can be called (even twice) and prints nothing,
// even after several dot intervals
func Test_Unit_Output_ChatOutput_ThinkingWithoutProgressIsNoOp(t *testing.T) {
	shortDotInterval(t)
	out := &syncBuffer{}

	stop := chatOutput{w: out, showProgress: false}.Thinking()
	if stop == nil {
		t.Fatal("expected a non-nil stop func")
	}
	time.Sleep(5 * progressDotInterval)
	stop()
	stop()

	if got := out.String(); got != "" {
		t.Errorf("expected no output with showProgress off, got %q", got)
	}
}

// - Verify Thinking with showProgress on prints dots while waiting and stop ends the dot line, writing nothing after
func Test_Unit_Output_ChatOutput_ThinkingWithProgressPrintsDots(t *testing.T) {
	shortDotInterval(t)
	out := &syncBuffer{}

	stop := chatOutput{w: out, showProgress: true}.Thinking()
	waitForOutput(t, out, ".")
	stop()
	afterStop := out.String()
	time.Sleep(5 * progressDotInterval)

	if strings.Trim(afterStop, ".") != "\n" || !strings.HasSuffix(afterStop, "\n") {
		t.Errorf("expected dots then one newline, got %q", afterStop)
	}
	if got := out.String(); got != afterStop {
		t.Errorf("expected no output after stop, got %q more", strings.TrimPrefix(got, afterStop))
	}
}

// ---- reActLoop reports through cs.Output ----

// - Verify reActLoop reports a tool-call step then a final reply through cs.Output, in order: Thinking/stop around each
// model call, Assistant per step, ToolCalled per call (before that tool runs), and writes nothing to cs.OutBuffer
func Test_Unit_Output_ReActLoop_ReportsEventsInOrder(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	log := &eventLog{}
	fx.Session.Output = recordingOutput{log: log}
	for _, name := range []string{"fake_a", "fake_b"} {
		if err := fx.Tools.Register(loggingTool{name: name, log: log}); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	twoCalls := model.ChatMessage{Role: "assistant", Content: "let me look", ToolCalls: []model.ToolCall{
		{ID: "c1", Type: "function", Function: model.ToolCallFunc{Name: "fake_a", Arguments: "{}"}},
		{ID: "c2", Type: "function", Function: model.ToolCallFunc{Name: "fake_b", Arguments: "{}"}},
	}}
	fx.Provider.Script = []model.ChatMessage{twoCalls, modeltest.AssistantText("all done")}

	_, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	want := []string{
		"thinking", "stop", "assistant:let me look",
		"tool_called:fake_a", "ran:fake_a",
		"tool_called:fake_b", "ran:fake_b",
		"thinking", "stop", "assistant:all done",
	}
	if got := log.list(); !slices.Equal(got, want) {
		t.Errorf("expected events\n  %q\ngot\n  %q", want, got)
	}
	if fx.Out.Len() != 0 {
		t.Errorf("expected reActLoop to write nothing to OutBuffer, got %q", fx.Out.String())
	}
}

// - Verify a failed model call makes reActLoop report only Thinking/stop and return the error: reporting it is the
// caller's job (handleUserInput), so the loop calls neither Error nor Assistant and writes nothing to OutBuffer
func Test_Unit_Output_ReActLoop_ModelErrorIsReturnedNotReported(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	log := &eventLog{}
	fx.Session.Output = recordingOutput{log: log}
	modelErr := errors.New("fake model unavailable")
	fx.Provider.Err = modelErr

	_, err := reActLoop(context.Background(), fx.Session, trace.StartTask(fx.Session.Recorder, "hi"), nil, modeltest.Msg("user", "u1", "hi"))

	if !errors.Is(err, modelErr) {
		t.Fatalf("expected the provider's error, got %v", err)
	}
	want := []string{"thinking", "stop"}
	if got := log.list(); !slices.Equal(got, want) {
		t.Errorf("expected events %q, got %q", want, got)
	}
	if fx.Out.Len() != 0 {
		t.Errorf("expected reActLoop to write nothing to OutBuffer, got %q", fx.Out.String())
	}
}

// ---- handleUserInput and auto-compaction report through cs.Output ----

// - Verify handleUserInput reports a model error exactly once, as "[error] ReAct loop error: …", with no bare "error:"
// line from the loop, and adds nothing to the context
func Test_Unit_Output_HandleUserInput_ModelErrorReportedOnce(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Err = errors.New("fake model unavailable")

	handleUserInput(context.Background(), fx.Session, "hi")

	if got, want := fx.Out.String(), "[error] ReAct loop error: fake model unavailable\n"; got != want {
		t.Errorf("expected exactly %q, got %q", want, got)
	}
	if n := fx.Context.GetSize(); n != 0 {
		t.Errorf("expected a failed turn to add nothing to the context, got size %d", n)
	}
}

// - Verify a Ctrl+C cancel during the model call is reported by handleUserInput as exactly one "[interrupted]" line,
// with no "[error]" or "error:" line
func Test_Unit_Output_HandleUserInput_CancelReportedAsInterruptedOnce(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	out := &syncBuffer{}
	fx.Session.OutBuffer = out
	fx.Session.Output = chatOutput{w: out}
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleUserInput(ctx, fx.Session, "hi")
	}()
	receiveOrFail(t, fx.Provider.Waiting, "the model call to start")
	cancel()
	receiveOrFail(t, done, "handleUserInput to return")

	if got, want := out.String(), "[interrupted]\n"; got != want {
		t.Errorf("expected exactly %q, got %q", want, got)
	}
}

// - Verify handleUserInput's slash commands report through cs.Output (System for /clear and for an unrecognized
// command, naming only the command word) and write nothing to OutBuffer
func Test_Unit_Output_HandleUserInput_SlashCommandsUseOutput(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{"/clear", []string{"system:Chat context cleared"}},
		{"/nope", []string{"system:Unrecognized command: /nope"}},
		{"/nope with fake args", []string{"system:Unrecognized command: /nope"}},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			fx := newChatSessionFixture(t, 10, 100)
			log := &eventLog{}
			fx.Session.Output = recordingOutput{log: log}

			if quit := handleUserInput(context.Background(), fx.Session, c.line); quit {
				t.Fatalf("expected %q not to quit", c.line)
			}

			if got := log.list(); !slices.Equal(got, c.want) {
				t.Errorf("expected events %q, got %q", c.want, got)
			}
			if fx.Out.Len() != 0 {
				t.Errorf("expected nothing written to OutBuffer, got %q", fx.Out.String())
			}
		})
	}
}

// - Verify a failed /summary reports Error("Summarization error", …) through cs.Output and nothing else
func Test_Unit_Output_HandleUserInput_SummaryErrorUsesOutput(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	fx.Context.AddMessages(modeltest.Msgs(2))
	log := &eventLog{}
	fx.Session.Output = recordingOutput{log: log}
	fx.Provider.Err = errors.New("fake summarizer down")

	handleUserInput(context.Background(), fx.Session, "/summary")

	got := log.list()
	if len(got) != 1 || !strings.HasPrefix(got[0], "error:Summarization error: ") || !strings.Contains(got[0], "fake summarizer down") {
		t.Errorf("expected one Summarization error event, got %q", got)
	}
	if fx.Out.Len() != 0 {
		t.Errorf("expected nothing written to OutBuffer, got %q", fx.Out.String())
	}
}

// - Verify a cancelled background auto-compaction prints "[interrupted]" (not "[error] Auto-compaction error: …") and
// never claims it completed
func Test_Unit_Output_AutoCompaction_CancelReportedAsInterrupted(t *testing.T) {
	fx := newChatSessionFixture(t, 4, 4)
	out := &syncBuffer{}
	fx.Session.OutBuffer = out
	fx.Session.Output = chatOutput{w: out}
	fx.Context.AddMessages(modeltest.Msgs(4)) // at the threshold
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	maybeStartAutoCompaction(ctx, fx.Session)
	receiveOrFail(t, fx.Provider.Waiting, "the compaction's model call to start")
	cancel()
	fx.Context.WaitForCompaction()

	if got, want := out.String(), "[system] Auto-compaction triggered...\n[interrupted]\n"; got != want {
		t.Errorf("expected exactly %q, got %q", want, got)
	}
}

// ---- NewChatSession wiring ----

// - Verify NewChatSession sets Output to a chatOutput on cfg.OutBuffer, with progress dots off when OutBuffer is not a
// terminal (a bytes.Buffer), even in chat mode
func Test_Unit_Output_NewChatSession_SetsChatOutputOnOutBuffer(t *testing.T) {
	history, err := memory.NewInMemoryChatHistory()
	if err != nil {
		t.Fatalf("NewInMemoryChatHistory: %v", err)
	}
	chatContext, err := contextwindow.NewChatContext(contextwindow.NewOffsetWindow(10), 100)
	if err != nil {
		t.Fatalf("NewChatContext: %v", err)
	}
	registry, err := tools.NewToolRegistry(nil)
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	out := &bytes.Buffer{}
	cfg := config.Config{
		UserID:       "test-user",
		SystemPrompt: "You are a helpful assistant.",
		MaxSteps:     10,
		AgentMode:    config.ModeChat,
		InBuffer:     strings.NewReader(""),
		OutBuffer:    out,
	}

	cs, err := NewChatSession(&modeltest.FakeProvider{}, history, chatContext, registry, trace.Nop(), cfg)
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}

	if cs.Output == nil {
		t.Fatal("expected a non-nil Output")
	}
	co, ok := cs.Output.(chatOutput)
	if !ok {
		t.Fatalf("expected Output to be a chatOutput, got %T", cs.Output)
	}
	if co.w != out {
		t.Errorf("expected Output to write to cfg.OutBuffer, got %v", co.w)
	}
	if co.showProgress {
		t.Error("expected showProgress off for a bytes.Buffer")
	}
	cs.Output.Assistant("probe")
	if got, want := out.String(), "[assistant] probe\n"; got != want {
		t.Errorf("expected %q on cfg.OutBuffer, got %q", want, got)
	}
}
