package agent

import (
	"context"
	"time"

	"github.com/jerryschen31/minimal-agent/contextwindow"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/trace"
)

// compactAndTrace runs one context compaction and records a compaction event with what it did, whether it worked or not.
// trigger is "manual" (/compact, scopeID is that command's scope) or "auto" (scopeID is the run ID: no user task owns it).
func compactAndTrace(ctx context.Context, cs *ChatSession, scopeID, trigger, instructions string) (model.ChatMessage, error) {
	start := time.Now()
	before := cs.MsgContext.GetSize()

	summaryMsg, err := contextwindow.CompactChatContext(ctx, cs.MsgContext, cs.getSessionProvider(scopeID), cs.Config.SummarizerPrompt, instructions)

	status, errText := trace.OutcomeOf(err)
	cs.Recorder.Record(scopeID, trace.Compaction, trace.CompactionData{
		Trigger:        trigger,
		Status:         status,
		Error:          errText,
		MessagesBefore: before,
		MessagesAfter:  cs.MsgContext.GetSize(),
		SummaryID:      summaryMsg.ID, // empty when compaction failed
		DurationMS:     time.Since(start).Milliseconds(),
	})
	return summaryMsg, err
}
