package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerryschen31/minimal-agent/model"
)

// errAllowanceUsed is returned (wrapped) by a metered model call when the worker or director has used its token
// allowance. The caller (runTask) decides what it means: a failed attempt (taskTokenBudget was the limit) or the end of
// the run (the run's budget was).
var errAllowanceUsed = errors.New("token allowance used up")

// usageMeter counts the tokens of one worker (or one director) and refuses model calls once its allowance is used.
// [agent] One meter per worker attempt, used by that worker's goroutine only, so it needs no lock. Parallel workers
// (step 3) each get their own and report their total when they end; nothing is shared while they run.
type usageMeter struct {
	allowance  int         // most tokens (model.Usage.Total) this worker may use; 0 = no limit
	used       model.Usage // what its model calls reported so far
	unreported int         // calls whose server sent no usage (counted as 0)
}

// check returns an error wrapping errAllowanceUsed when the allowance is used up.
//
// Notes:
//   - Called before a call: a call's size is known only after it, so a worker can go over by one call.
func (m *usageMeter) check() error {
	if m.allowance > 0 && m.used.Total() >= m.allowance {
		return fmt.Errorf("%w (%d of %d tokens)", errAllowanceUsed, m.used.Total(), m.allowance)
	}
	return nil
}

// add counts one call's usage.
//
// Notes:
//   - A call with no usage (Reported false) adds 0 and is counted in unreported.
func (m *usageMeter) add(u model.Usage) {
	m.used = m.used.Add(u)
	if !u.Reported {
		m.unreported++
	}
}

// meteredProvider wraps a model.Provider so every call is checked against and counted by meter.
type meteredProvider struct {
	inner model.Provider
	meter *usageMeter
}

// Chat refuses the call when the meter's allowance is used up, else calls the wrapped provider and counts its usage.
//
// Notes:
//   - A refused call never reaches the server. Under the tracer (getSessionProvider), it is traced as a model_error.
//   - A failed call counts nothing (its Response has no usage).
func (p meteredProvider) Chat(ctx context.Context, msgs []model.ChatMessage, tools []model.ToolDef) (model.Response, error) {
	if err := p.meter.check(); err != nil {
		return model.Response{}, err
	}
	resp, err := p.inner.Chat(ctx, msgs, tools)
	if err != nil {
		return resp, err
	}
	p.meter.add(resp.Usage)
	return resp, nil
}
