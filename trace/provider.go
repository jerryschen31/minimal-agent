package trace

import (
	"context"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
)

// TracedProvider wraps a model.Provider and records a model_request before each Chat call and a
// model_response (or model_error) after it, under one scope ID.
// This is the decorator pattern: it implements the model.Provider interface, so every caller (the ReAct loop, /summary, compaction) gets tracing
// by being handed the wrapper, and none of them has to remember to record.
type TracedProvider struct {
	Step     int            // ReAct step number stamped on the next model_request. The loop sets it before each call (1-based); callers outside the loop leave it 0, which is omitted from the event
	inner    model.Provider // the actual provider being wrapped
	rec      Recorder
	scopeID  string
	cfg      config.Config
	TurnMsgs []model.ChatMessage // the messages added by this turn; in the ReAct loop, only trace these messages (not the previous context)
}

// GetTracedProvider returns a TracedProvider that wraps p so its calls are recorded under scopeID: a task ID for a turn or a command,
// the run ID for background work such as auto-compaction. Use one wrapper per scope, from one goroutine.
func GetTracedProvider(p model.Provider, rec Recorder, scopeID string, cfg config.Config) *TracedProvider {
	return &TracedProvider{inner: p, rec: rec, scopeID: scopeID, cfg: cfg}
}

// Chat records the request, calls the wrapped provider, then records the response or the error.
func (t *TracedProvider) Chat(ctx context.Context, msgs []model.ChatMessage, toolDefs []model.ToolDef) (model.ChatMessage, error) {
	recorded, contextCount := msgs, 0
	if t.TurnMsgs != nil {
		recorded, contextCount = t.TurnMsgs, len(msgs)-len(t.TurnMsgs)
	}
	t.rec.Record(t.scopeID, ModelRequest, GetModelRequestData(t.cfg, t.Step, contextCount, recorded, toolDefs))
	start := time.Now()
	resp, err := t.inner.Chat(ctx, msgs, toolDefs)
	if err != nil {
		t.rec.Record(t.scopeID, ModelError, GetModelErrorData(err, time.Since(start)))
		return resp, err
	}
	t.rec.Record(t.scopeID, ModelResponse, GetModelResponseData(resp, time.Since(start)))
	return resp, nil
}
