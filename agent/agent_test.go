package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/trace"
)

// - Verify run_end is recorded by ShutdownAgent after background work has drained: a compaction still running when
// RunAgent returns must land its event before run_end, and RunAgent alone must not end the run
func Test_Shutdown_RecordsRunEndAfterBackgroundCompaction(t *testing.T) {
	fx := newChatSessionFixture(t, 10, 100)
	spy := &recorderSpy{}
	fx.Session.Recorder = spy
	a := &Agent{session: fx.Session, agentMode: "chat"}

	if err := a.RunAgent(context.Background(), nil); err != nil { // empty input: the loop ends at EOF
		t.Fatal(err)
	}
	if slices.Contains(spy.eventTypes(), trace.RunEnd) {
		t.Fatalf("RunAgent recorded run_end before shutdown: %v", spy.eventTypes())
	}

	release := make(chan struct{})
	fx.Context.CompactWG.Add(1)
	go func() { // stands in for an auto-compaction that is still waiting on the model
		defer fx.Context.CompactWG.Done()
		<-release
		spy.Record(spy.RunID(), trace.Compaction, trace.CompactionData{Trigger: "auto"})
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.ShutdownAgent(context.Background())
	}()
	close(release)
	<-done

	types := spy.eventTypes()
	want := []trace.EventType{trace.RunStart, trace.Compaction, trace.RunEnd}
	if !slices.Equal(types, want) {
		t.Errorf("events = %v, want %v", types, want)
	}
	a.ShutdownAgent(context.Background()) // a second shutdown writes nothing more
	if len(spy.eventTypes()) != len(want) {
		t.Errorf("second ShutdownAgent recorded more events: %v", spy.eventTypes())
	}
}

// - Verify a non-positive mcpResultMaxBytes fails setup instead of silently dropping every MCP tool
func Test_SetupAgent_RejectsNonPositiveMCPResultCap(t *testing.T) {
	for _, n := range []int{0, -1} {
		var a Agent
		err := a.SetupAgent(context.Background(), config.Config{MaxSteps: 10, MCPResultMaxBytes: n})
		if err == nil || !strings.Contains(err.Error(), "mcpResultMaxBytes") {
			t.Errorf("MCPResultMaxBytes %d: err = %v, want an mcpResultMaxBytes error", n, err)
		}
	}
}
