package trace

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSub is an in-memory Subscriber. If block is set, the first Write waits for it to be closed.
type fakeSub struct {
	mu       sync.Mutex
	lines    []string
	closed   int
	writeErr error
	panics   bool
	block    chan struct{}
	started  chan struct{} // closed when the first Write begins
	once     sync.Once
}

func (f *fakeSub) Write(line []byte) error {
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.block != nil {
		<-f.block
	}
	if f.panics {
		panic("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, string(line))
	return f.writeErr
}

func (f *fakeSub) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

// events decodes every line the subscriber received.
func (f *fakeSub) events(t *testing.T) []Event {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.lines))
	for i, l := range f.lines {
		if err := json.Unmarshal([]byte(l), &out[i]); err != nil {
			t.Fatalf("line %d is not an Event: %v\n%s", i, err, l)
		}
	}
	return out
}

func newTestRecorder(t *testing.T, subs ...Subscriber) Recorder {
	t.Helper()
	return NewRecorder("run-1", newTestRedactor(t), 1<<20, subs...)
}

func Test_Recorder_OversizedPayloadIsReplacedByMarker(t *testing.T) {
	sub := &fakeSub{}
	rec := NewRecorder("run-1", newTestRedactor(t), 100, sub)
	big := strings.Repeat("x", 500)
	rec.Record("t", ToolFinished, map[string]string{"result": big, "secret_note": fakeAnthropic})
	rec.Record("t", ToolFinished, map[string]string{"result": "small"})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	evs := sub.events(t)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (the oversized event is kept)", len(evs))
	}
	var marker struct {
		Truncated     bool `json:"truncated"`
		OriginalBytes int  `json:"original_bytes"`
	}
	if err := json.Unmarshal(evs[0].EventData, &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Truncated || marker.OriginalBytes < 500 {
		t.Errorf("marker = %+v, want truncated with original_bytes >= 500", marker)
	}
	if strings.Contains(sub.lines[0], "xxxx") || strings.Contains(sub.lines[0], fakeAnthropic) {
		t.Errorf("oversized payload leaked into the trace: %s", sub.lines[0])
	}
	if !strings.Contains(string(evs[1].EventData), "small") {
		t.Errorf("a payload under the cap must pass through: %s", evs[1].EventData)
	}
}

func Test_Recorder_WritesEventsInOrder(t *testing.T) {
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	before := time.Now().UTC()
	rec.Record("", RunStart, RunStartData{AgentName: "minagent"})
	rec.Record("task-1", ToolFinished, ToolCallRecord{CallID: "c1", Name: "read_file"})
	rec.Record("task-1", RunEnd, nil)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	evs := sub.events(t)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	wantTypes := []EventType{RunStart, ToolFinished, RunEnd}
	for i, ev := range evs {
		if ev.Counter != uint64(i) {
			t.Errorf("event %d: counter = %d", i, ev.Counter)
		}
		if ev.EventType != wantTypes[i] {
			t.Errorf("event %d: type = %s, want %s", i, ev.EventType, wantTypes[i])
		}
		if ev.RunID != "run-1" {
			t.Errorf("event %d: run_id = %q", i, ev.RunID)
		}
		if ev.TimeStamp.Before(before) || ev.TimeStamp.Location() != time.UTC {
			t.Errorf("event %d: ts = %v, want a UTC time at or after %v", i, ev.TimeStamp, before)
		}
	}
	if evs[0].ScopeID != "" || evs[1].ScopeID != "task-1" {
		t.Errorf("scope ids = %q, %q", evs[0].ScopeID, evs[1].ScopeID)
	}
	if !strings.Contains(string(evs[0].EventData), `"agent_name":"minagent"`) {
		t.Errorf("payload = %s", evs[0].EventData)
	}
	if string(evs[2].EventData) != "null" {
		t.Errorf("nil payload = %s, want null", evs[2].EventData)
	}
}

func Test_Recorder_RedactsPayloads(t *testing.T) {
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	rec.Record("t", ToolFinished, map[string]string{"result": "key " + fakeAnthropic, "note": "fine"})
	rec.Record("t", ToolFinished, map[string]string{"result": "nothing secret"})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	evs := sub.events(t)
	if strings.Contains(sub.lines[0], fakeAnthropic) {
		t.Fatalf("secret reached the subscriber: %s", sub.lines[0])
	}
	if !placeholderRe.MatchString(string(evs[0].EventData)) {
		t.Errorf("no placeholder: %s", evs[0].EventData)
	}
	if !slices.Equal(evs[0].Redactions, []string{"anthropic_key"}) {
		t.Errorf("redactions = %v", evs[0].Redactions)
	}
	if strings.Contains(sub.lines[1], "redactions") {
		t.Errorf("clean event should have no redactions field: %s", sub.lines[1])
	}
}

// The snapshot must be taken in Record: a slice changed after Record must not change what is written.
func Test_Recorder_SnapshotsPayload(t *testing.T) {
	sub := &fakeSub{block: make(chan struct{}), started: make(chan struct{})}
	rec := newTestRecorder(t, sub)

	rec.Record("t", ModelRequest, ModelRequestData{Model: "first"})
	<-sub.started // the tracewriter is now stuck inside the first Write, so the next event waits in the queue

	names := []string{"read_file"}
	rec.Record("t", ModelRequest, ModelRequestData{Model: "second", ToolNames: names})
	names[0] = "changed_after_record"

	close(sub.block)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	evs := sub.events(t)
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	got := string(evs[1].EventData)
	if !strings.Contains(got, `"read_file"`) || strings.Contains(got, "changed_after_record") {
		t.Errorf("payload was not snapshotted: %s", got)
	}
}

func Test_Recorder_UnencodablePayloadIsKeptWithMarshalError(t *testing.T) {
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	rec.Record("t", ToolFinished, map[string]any{"bad": make(chan int)})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	evs := sub.events(t)
	if len(evs) != 1 || !strings.Contains(string(evs[0].EventData), "marshal_error") {
		t.Errorf("events = %v", sub.lines)
	}
}

func Test_Recorder_CloseWritesEverythingQueued(t *testing.T) {
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	n := queueSize*3 + 7 // more than the queue holds, so Record has to wait for the tracewriter
	for range n {
		rec.Record("t", ToolFinished, ToolCallRecord{Name: "x"})
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	evs := sub.events(t)
	if len(evs) != n {
		t.Fatalf("got %d events, want %d", len(evs), n)
	}
	for i, ev := range evs {
		if ev.Counter != uint64(i) {
			t.Fatalf("event %d has counter %d", i, ev.Counter)
		}
	}
}

func Test_Recorder_CloseTwiceAndRecordAfterClose(t *testing.T) {
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	rec.Record("t", RunStart, nil)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	rec.Record("t", RunEnd, nil) // dropped, must not panic
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sub.events(t)) != 1 {
		t.Errorf("event recorded after Close was written: %v", sub.lines)
	}
	if sub.closed != 1 {
		t.Errorf("subscriber closed %d times, want 1", sub.closed)
	}
}

func Test_Recorder_ConcurrentRecord(t *testing.T) {
	sub := &fakeSub{}
	rec := newTestRecorder(t, sub)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				rec.Record("t", ToolFinished, map[string]int{"g": g})
			}
		}()
	}
	wg.Wait()
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	evs := sub.events(t)
	if len(evs) != 800 {
		t.Fatalf("got %d events", len(evs))
	}
	for i, ev := range evs {
		if ev.Counter != uint64(i) {
			t.Fatalf("event %d has counter %d", i, ev.Counter)
		}
	}
}

func Test_Recorder_OneFailingSubscriberDoesNotStarveTheOthers(t *testing.T) {
	bad := &fakeSub{writeErr: errors.New("disk full")}
	good := &fakeSub{}
	rec := newTestRecorder(t, bad, good)
	rec.Record("t", RunStart, nil)
	rec.Record("t", RunEnd, nil)
	err := rec.Close()
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Close error = %v", err)
	}
	if len(good.events(t)) != 2 {
		t.Errorf("the healthy subscriber got %d events", len(good.events(t)))
	}
}

func Test_Recorder_SubscriberPanicDoesNotCrashTheRun(t *testing.T) {
	rec := newTestRecorder(t, &fakeSub{panics: true})
	rec.Record("t", RunStart, nil)
	rec.Record("t", RunEnd, nil)
	err := rec.Close()
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Errorf("Close error = %v, want the panic reported", err)
	}
}

func Test_Recorder_RedactionFailureDropsThePayload(t *testing.T) {
	sub := &fakeSub{}
	r := &recorder{redactor: newTestRedactor(t), subs: []Subscriber{sub}}                                        // no tracewriter goroutine: we call traceWrite directly
	r.traceWrite(Event{EventType: ToolFinished, EventData: json.RawMessage(`{"secret": "oops` + fakeAnthropic)}) // invalid JSON
	if r.err == nil {
		t.Error("want the redaction failure remembered")
	}
	if len(sub.lines) != 1 || strings.Contains(sub.lines[0], fakeAnthropic) || !strings.Contains(sub.lines[0], "dropped") {
		t.Errorf("lines = %v", sub.lines)
	}
}

func Test_Nop(t *testing.T) {
	rec := Nop()
	rec.Record("t", RunStart, nil)
	if err := rec.Close(); err != nil {
		t.Error(err)
	}
}

func Test_NewRunID(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == b {
		t.Error("two run IDs are equal")
	}
	if !regexpRunID.MatchString(a) {
		t.Errorf("run ID %q does not look like r20261020T101500Z-9f3a41c7", a)
	}
	if id := NewTaskID(); !regexpTaskID.MatchString(id) {
		t.Errorf("task ID %q does not look like t20261020T101530Z-5be0d2a1", id)
	}
}

var (
	regexpRunID  = regexp.MustCompile(`^r\d{8}T\d{6}Z-[0-9a-f]{8}$`)
	regexpTaskID = regexp.MustCompile(`^t\d{8}T\d{6}Z-[0-9a-f]{8}$`)
)

// - Verify that credentials written in quoted form, or next to a fake placeholder, never reach a subscriber, in whole or in part.
// The values are made-up words, not real credentials.
func Test_Recorder_QuotedAndPlaceholderLookalikeCredentialsNeverReachASubscriber(t *testing.T) {
	cases := []struct {
		name  string
		data  any
		parts []string // no part of the credential may appear in the written line
	}{
		{"single-quoted env assignment", TaskStartData{Input: "DB_PASSWORD='hunter2xyz'"}, []string{"hunter2xyz"}},
		{"single-quoted flag", TaskStartData{Input: "run --key='opaque123456'"}, []string{"opaque123456"}},
		{"double-quoted value with spaces", TaskStartData{Input: `{"password":"correct horse battery staple"}`}, []string{"correct", "horse", "battery", "staple"}},
		{"double-quoted shell assignment", TaskStartData{Input: `export TOKEN="zebra quartz mango"`}, []string{"zebra", "quartz", "mango"}},
		{"escaped quote inside the value", TaskStartData{Input: `password="pa\"ss zebra word"`}, []string{"zebra", "ss zebra"}},
		{"single-quoted value with spaces", TaskStartData{Input: "secret: 'alpha bravo charlie'"}, []string{"alpha", "bravo", "charlie"}},
		{"quoted key assignment with spaces", TaskStartData{Input: `config {"api_key": "falcon harbor lantern"}`}, []string{"falcon", "harbor", "lantern"}},
		{"closing quote missing", TaskStartData{Input: `password="never closed ending`}, []string{"never", "closed", "ending"}},
		{"secret field after a fake placeholder", map[string]string{"password": "[REDACTED:secret_field#12345678]hunter2xyz"}, []string{"hunter2xyz"}},
		{"assignment after a fake placeholder", TaskStartData{Input: "password=[REDACTED:not-a-placeholder]hunter2xyz"}, []string{"hunter2xyz"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub := &fakeSub{}
			rec := newTestRecorder(t, sub)
			rec.Record("t", TaskStart, c.data)
			if err := rec.Close(); err != nil {
				t.Fatal(err)
			}
			if len(sub.lines) != 1 {
				t.Fatalf("got %d lines", len(sub.lines))
			}
			line := sub.lines[0]
			for _, part := range c.parts {
				if strings.Contains(line, part) {
					t.Errorf("part %q of the credential reached the subscriber: %s", part, line)
				}
			}
			if !strings.Contains(line, placeholderPrefix) {
				t.Errorf("nothing was redacted: %s", line)
			}
		})
	}
}
