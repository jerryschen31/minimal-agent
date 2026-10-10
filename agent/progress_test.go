package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

// - Verify dots are printed while waiting, on one line, and the line is ended when stopped
func Test_StartProgressDots_PrintsDotsThenEndsLine(t *testing.T) {
	out := &syncBuffer{}
	stop := startProgressDots(out, 10*time.Millisecond)
	waitForOutput(t, out, "..") // wait for two ticks to actually happen, not for a fixed time
	stop()

	if !regexp.MustCompile(`^\.{2,}\n$`).MatchString(out.String()) {
		t.Errorf("expected a line of 2+ dots ending in a newline, got %q", out.String())
	}
}

// - Verify stopping before the first tick prints nothing, not even the newline
func Test_StartProgressDots_StopBeforeFirstTick_PrintsNothing(t *testing.T) {
	out := &syncBuffer{}
	stop := startProgressDots(out, time.Hour)
	stop()

	if out.String() != "" {
		t.Errorf("expected no output, got %q", out.String())
	}
}

// - Verify nothing is printed after stop returns (so no dot can land after the assistant's reply)
func Test_StartProgressDots_NothingPrintedAfterStop(t *testing.T) {
	out := &syncBuffer{}
	stop := startProgressDots(out, 5*time.Millisecond)
	waitForOutput(t, out, ".") // make sure the ticker is really running before stopping it
	stop()
	afterStop := out.String()
	time.Sleep(30 * time.Millisecond) // several more intervals

	if out.String() != afterStop {
		t.Errorf("output changed after stop: %q -> %q", afterStop, out.String())
	}
}

// shortDotInterval shortens the dot interval for one test and restores it afterwards
func shortDotInterval(t *testing.T) {
	t.Helper()
	orig := progressDotInterval
	progressDotInterval = 10 * time.Millisecond
	t.Cleanup(func() { progressDotInterval = orig })
}

// - Verify chatWithProgress shows dots while the model is slow and ends the line before returning the reply
func Test_ChatWithProgress_ShowProgress_PrintsDotsWhileWaiting(t *testing.T) {
	shortDotInterval(t)
	fx := newChatSessionFixture(t, 10, 100)
	out := &syncBuffer{}
	fx.Session.OutBuffer = out
	fx.Session.ShowProgress = true
	fx.Provider.Reply = "ok"
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})

	type result struct {
		content string
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		reply, err := chatWithProgress(context.Background(), fx.Session, nil, nil)
		resCh <- result{reply.Content, err}
	}()

	receiveOrFail(t, fx.Provider.Waiting, "the model call to start")
	waitForOutput(t, out, "..") // the model is "thinking": hold it until dots have been printed
	close(fx.Provider.Gate)
	res := <-resCh

	if res.err != nil || res.content != "ok" {
		t.Fatalf("expected reply %q, got (%q, %v)", "ok", res.content, res.err)
	}
	if !regexp.MustCompile(`^\.{2,}\n$`).MatchString(out.String()) {
		t.Errorf("expected a line of dots ending in a newline, got %q", out.String())
	}
}

// - Verify Ctrl+C (a cancelled context) while dots are showing still ends the line, so [interrupted]
// starts on its own line
func Test_ChatWithProgress_Cancelled_EndsDotLine(t *testing.T) {
	shortDotInterval(t)
	fx := newChatSessionFixture(t, 10, 100)
	out := &syncBuffer{}
	fx.Session.OutBuffer = out
	fx.Session.ShowProgress = true
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := chatWithProgress(ctx, fx.Session, nil, nil)
		errCh <- err
	}()

	receiveOrFail(t, fx.Provider.Waiting, "the model call to start")
	waitForOutput(t, out, ".") // cancel only once a dot line has been started
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !regexp.MustCompile(`^\.+\n$`).MatchString(out.String()) {
		t.Errorf("expected the dot line to be ended, got %q", out.String())
	}
}

// - Verify no dots are printed when ShowProgress is off (tests, pipes, headless/oneshot)
func Test_ChatWithProgress_NoShowProgress_PrintsNothing(t *testing.T) {
	shortDotInterval(t)
	fx := newChatSessionFixture(t, 10, 100)
	fx.Provider.Reply = "ok"
	fx.Provider.Gate = make(chan struct{})
	fx.Provider.Waiting = make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		chatWithProgress(context.Background(), fx.Session, nil, nil)
	}()
	receiveOrFail(t, fx.Provider.Waiting, "the model call to start")
	time.Sleep(35 * time.Millisecond) // long enough for several dots if they were on
	close(fx.Provider.Gate)
	receiveOrFail(t, done, "the model call to return")

	if fx.Out.String() != "" {
		t.Errorf("expected no output with ShowProgress off, got %q", fx.Out.String())
	}
}

// - Verify dots are only enabled for chat mode writing to a terminal: never for headless/oneshot,
// and never when output is a buffer or a file
func Test_ShouldShowProgress(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer file.Close()

	cases := []struct {
		name string
		mode string
		out  io.Writer
	}{
		{"chat mode, buffer", config.ModeChat, &bytes.Buffer{}},
		{"chat mode, file on disk", config.ModeChat, file},
		{"headless mode, file on disk", config.ModeHeadless, file},
		{"oneshot mode, buffer", config.ModeOneshot, &bytes.Buffer{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.GetDefaultConfig()
			cfg.AgentMode = tc.mode
			cfg.OutBuffer = tc.out
			if shouldShowProgress(cfg) {
				t.Errorf("expected no progress dots for %s", tc.name)
			}
		})
	}
}
