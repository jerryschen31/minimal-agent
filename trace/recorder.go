package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
)

// Recorder is the only thing the agent loop knows about tracing.
type Recorder interface {
	// Record queues one event. It returns no error: tracing must never fail a run.
	// data is a typed payload (RunStartData, ModelResponseData, ...) and must not be changed after the call.
	Record(taskID string, eventType EventType, data any)
	// Close stops accepting events, writes everything already queued, and closes the subscribers.
	// It returns the first error any subscriber hit.
	Close() error
}

// Subscriber receives each finished event as one JSON line (no trailing newline).
// [agent] The JSONL file writer is the first Subscriber; a console printer or a live digest could be others.
// The recorder calls Write from one goroutine only, so a Subscriber needs no locking.
type Subscriber interface {
	Write(line []byte) error
	Close() error
}

// Nop returns a Recorder that does nothing. It is what the loop gets when tracing is off, so the loop
// never has to check "is tracing enabled?".
func Nop() Recorder { return nopRecorder{} }

type nopRecorder struct{}

func (nopRecorder) Record(string, EventType, any) {}
func (nopRecorder) Close() error                  { return nil }

// queueSize is how many events can wait for the tracewriter goroutine before Record blocks (backpressure).
const queueSize = 256

// recorder is the real Recorder: Record puts events on a channel, one goroutine takes them off in order,
// redacts, numbers, marshals and hands them to the subscribers.
type recorder struct {
	runID    string
	redactor *Redactor
	subs     []Subscriber

	maxEventBytes int // a payload larger than this (as JSON) is replaced by a size marker

	mu     sync.Mutex    // guards closed and every send on ch
	closed bool          // whether the tracewriter channel has been closed or not
	ch     chan Event    // event trace channel
	done   chan struct{} // closed when the tracewriter goroutine has finished

	closeOnce sync.Once // the subscribers are closed once, however often Close is called

	seq uint64 // to keep track of event order; only the tracewriter goroutine touches it
	err error  // for subscriber errors; written by the tracewriter goroutine, read after <-done
}

// SetupRecorder returns the Recorder for this run: a no-op when tracing is disabled in the config,
// otherwise a real one that writes <traceDir>/<run_id>.jsonl with the configured redaction level.
// An unknown redaction level, or a trace file that can't be created, is an error.
func SetupRecorder(ctx context.Context, cfg config.Config) (Recorder, error) {
	if !cfg.TraceEnabled {
		return Nop(), nil
	}
	redactor, err := NewRedactor(Level(cfg.RedactionLevel))
	if err != nil {
		return nil, err
	}
	if cfg.TraceMaxEventBytes <= 0 {
		return nil, fmt.Errorf("traceMaxEventBytes must be positive, got %d", cfg.TraceMaxEventBytes)
	}
	if cfg.TraceDir == "" {
		return nil, fmt.Errorf("traceDir must not be empty")
	}
	// the run ID is made once: it names the trace file and stamps every event in it
	runID := NewRunID()
	jsonl, err := NewJSONLSubscriber(cfg.TraceDir, runID)
	if err != nil {
		return nil, err // can't create the trace file: fail at startup rather than run without an audit trail
	}
	return NewRecorder(runID, redactor, cfg.TraceMaxEventBytes, jsonl), nil
}

// NewRecorder builds a recorder from explicit pieces (no config) and starts its tracewriter goroutine, which takes
// events off the channel, redacts them, and writes them to subscribers (e.g. the JSONL log file).
// maxEventBytes caps the JSON size of one event data payload.
func NewRecorder(runID string, redactor *Redactor, maxEventBytes int, subs ...Subscriber) Recorder {
	rec := &recorder{
		runID:         runID,
		redactor:      redactor,
		subs:          subs,
		maxEventBytes: maxEventBytes,
		ch:            make(chan Event, queueSize),
		done:          make(chan struct{}),
	}
	go rec.runTraceWriter()
	return rec
}

// Record is how the agent loop records (snapshots) an event. The event is queued into a channel for the tracewriter goroutine to process asynchronously.
// The tracewriter goroutine does the heavy lifting of processing the trace events
func (r *recorder) Record(taskID string, eventType EventType, data any) {
	ev := Event{
		TimeStamp: time.Now().UTC(), // timestamp records when this agent event actually happened; tracewriter will log it separately in its own goroutine
		RunID:     r.runID,
		TaskID:    taskID,
		EventType: eventType,
	}

	// json.Marshal creates a bytes copy of the event data payload.
	// When this data payload is a reference to a slice or map, for example, copying it allows the agent loop to continue to modify this slice or map without affecting the event trace.
	raw, err := json.Marshal(data)
	if err != nil {
		// tracing never fails the run: keep the event and say the payload could not be encoded
		raw, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}

	// Size cap: the whole payload is swapped for a marker when size exceeds this cap
	if len(raw) > r.maxEventBytes {
		raw, _ = json.Marshal(map[string]any{"truncated": true, "original_bytes": len(raw)})
	}
	ev.EventData = json.RawMessage(raw)

	// A channel-write (in this case, writing an event) on a closed channel panics, so we first need to check if the channel is closed before we write to the channel.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.ch <- ev
}

// runTraceWriter is the tracewriter goroutine that essentially writes event traces.
// A queued channel handles backpressure, and the tracewriter processes queued events one at a time, in the order they were recorded.
func (r *recorder) runTraceWriter() {
	defer close(r.done)
	for ev := range r.ch { // range over the values (i.e. trace events) in the channel: waits for the next event; leaves the loop only when ch is empty AND closed
		r.safeTraceWrite(ev)
	}
}

// safeTraceWrite is write with a safety net: we recover gracefully from panics inside the trace writer.
func (r *recorder) safeTraceWrite(ev Event) {
	defer func() {
		if p := recover(); p != nil {
			r.fail(fmt.Errorf("tracewriter panic: %v\n%s", p, debug.Stack()))
		}
	}()
	r.traceWrite(ev)
}

// traceWrite finishes one event (redact secrets, assign trace number, marshal) and hands it to every subscriber.
func (r *recorder) traceWrite(ev Event) {
	// 1. redact secrets. On failure the payload is dropped, never written raw, and the gap shows in the trace.
	tree, fired, err := r.redactor.Redact(ev.EventData)
	var redacted []byte
	if err == nil {
		redacted, err = json.Marshal(tree)
	}
	if err != nil {
		r.fail(fmt.Errorf("event %s: redaction failed: %w", ev.EventType, err))
		redacted = []byte(`{"dropped":"redaction failed"}`)
		fired = nil
	}

	// 2. put the redacted payload back and record which rules fired
	ev.EventData = redacted
	ev.Redactions = fired

	// 3. number the trace. Note that r.seq only advances once the line is built, so a skipped event leaves no hole.
	ev.Counter = r.seq
	line, err := json.Marshal(ev)
	if err != nil {
		r.fail(fmt.Errorf("event %s: marshal failed: %w", ev.EventType, err))
		return
	}
	r.seq++

	// 4. Write the trace to all subscribers (e.g., a log file, network endpoint, a stdout stream, etc.)
	for _, sub := range r.subs {
		if err := sub.Write(line); err != nil {
			r.fail(err)
		}
	}
}

// fail registers the first error encountered into the recorder. Only the single tracewriter goroutine calls this, so no lock is needed.
func (r *recorder) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// Close stops accepting events, waits until every queued event has been written, closes the subscribers, and returns the first error the tracewriter or a subscriber hit.
// This is safe to call more than once (idempotent).
func (r *recorder) Close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.ch) // signal the tracewriter to stop after draining the queue
	}
	r.mu.Unlock()

	<-r.done                // blocks - waits for the tracewriter goroutine to finish processing all events, after which close(r.done) is called in tracewriter, which unblocks this
	r.closeOnce.Do(func() { // close all subscribers exactly once
		for _, sub := range r.subs {
			if err := sub.Close(); err != nil {
				r.fail(err)
			}
		}
	})
	return r.err
}

// NewRunID returns a unique, time-sortable run ID, e.g. "20261020T101500Z-9f3a41c7".
// The time prefix makes trace files sort by start time; the random suffix keeps two runs started in the
// same second apart.
func NewRunID() string {
	b := make([]byte, 4)
	rand.Read(b) // crypto/rand; never returns an error since Go 1.24
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}
