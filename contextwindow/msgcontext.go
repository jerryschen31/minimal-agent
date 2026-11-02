package contextwindow

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/model"
)

// [agent] the window size, strategy, auto-compaction and summarizer prompt are settings in config (GetDefaultConfig holds the defaults);
// this one stays in code because it is part of how a summary is stored, not a choice
const SummaryRequest = "Summarize our conversation up to this point." // the user half of the summary pair that replaces compacted messages

// MinContextWindowMessages is the smallest window allowed: the summary pair plus one user/assistant turn.
// Below 2, the summary pair alone doesn't fit and clampToMax would panic.
const MinContextWindowMessages = 4

// Context for a chat session
type ChatContext struct {
	mu                   sync.Mutex
	window               ContextWindow // The context window is the data structure that holds the actual chat messages
	gen                  int           // Generation counter - increments each time the context is compacted or cleared
	autoCompactThreshold int
	compactInProgress    atomic.Bool
	CompactWG            sync.WaitGroup // Used for triggering some code that waits for compaction to complete (e.g., for graceful shutdown)
}

// This is a snapshot of the context window's state, including the current chat history and the generation counter.
type ContextState struct {
	Messages []model.ChatMessage
	Gen      int
}

// NewChatContext wraps w. Auto-compaction is needed once the window holds autoCompactThreshold messages.
//
// Notes:
//   - a threshold of 0 (or less) means auto-compaction is off. Config never says 0: it has its own autoCompact switch,
//     which SetupChatContext turns into 0
//   - the error result is always nil today
func NewChatContext(w ContextWindow, autoCompactThreshold int) (*ChatContext, error) {
	return &ChatContext{
		window:               w,
		gen:                  0,
		autoCompactThreshold: autoCompactThreshold,
		compactInProgress:    atomic.Bool{},
	}, nil
}

// GetWindow returns the underlying ContextWindow.
//
// Notes:
//   - reads c.window without taking c.mu; the window has its own lock, but calls made on it directly bypass the ChatContext lock
//     (and so its generation counter)
func (c *ChatContext) GetWindow() ContextWindow {
	return c.window
}

// ShouldStartCompaction reports whether the caller may start a compaction, claiming the in-progress flag if so.
//
// Notes:
//   - it is an atomic compare-and-swap, so of several concurrent callers exactly one gets true
//   - the winner must call EndCompaction when done (CompactChatContext does this with defer), or no compaction can start again
func (c *ChatContext) ShouldStartCompaction() bool {
	return c.compactInProgress.CompareAndSwap(false, true)
}

// EndCompaction releases the in-progress flag that ShouldStartCompaction set.
//
// Notes:
//   - safe to call when the flag is not set
func (c *ChatContext) EndCompaction() {
	c.compactInProgress.Store(false)
}

// IsAutoCompactionNeeded reports whether the window has reached the auto-compaction threshold.
//
// Notes:
//   - always false when the threshold is 0 or less (auto-compaction off)
//   - only reads the size; it does not claim the in-progress flag, so pair it with ShouldStartCompaction
func (c *ChatContext) IsAutoCompactionNeeded() bool {
	return c.autoCompactThreshold > 0 && c.window.GetSize() >= c.autoCompactThreshold
}

// WaitForCompaction blocks until any in-flight background compaction finishes.
//
// Notes:
//   - safe to call even when none is running: Wait() on a zero counter returns immediately, no blocking
//   - useful for code that needs to ensure all background compaction has completed before proceeding, such as during graceful
//     shutdown
func (c *ChatContext) WaitForCompaction() {
	c.CompactWG.Wait()
}

// AddMessages appends msgs to the window under the context lock.
//
// Notes:
//   - the window decides what to evict when it is over its maximum size; see ContextWindow.AddMessages for the user-first rule
func (c *ChatContext) AddMessages(msgs []model.ChatMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.AddMessages(msgs)
}

// GetMessages returns a copy of the messages in the window under the context lock.
//
// Notes:
//   - the slice is a copy, so the caller may keep or modify it
func (c *ChatContext) GetMessages() []model.ChatMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window.GetMessages()
}

// RemoveLast removes the last n messages from the window under the context lock.
//
// Notes:
//   - does not change the generation counter
func (c *ChatContext) RemoveLast(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.RemoveLast(n)
}

// GetSize returns how many messages the window holds now.
//
// Notes: none.
func (c *ChatContext) GetSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window.GetSize()
}

// GetMaxSize returns the most messages the window can hold.
//
// Notes: none.
func (c *ChatContext) GetMaxSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window.GetMaxSize()
}

// Snapshot returns the window's messages together with the generation counter, taken under one lock.
//
// Notes:
//   - for now the state is the chat history and the generation counter
//   - pass the state to Compact later: the generation tells Compact whether the window was cleared or compacted in between
func (c *ChatContext) Snapshot() ContextState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ContextState{
		Messages: c.window.GetMessages(),
		Gen:      c.gen,
	}
}

// Clear empties the window and increments the generation counter.
//
// Notes:
//   - the generation bump makes any compaction that took a Snapshot before the clear fail instead of resurrecting old messages
func (c *ChatContext) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.Clear()
	c.gen++
}

// Compact replaces the messages in state with summaryMsgs, keeping any messages added since state was taken.
//
// Notes:
//   - returns false, changing nothing, if the generation has moved on since the snapshot (the window was cleared or compacted
//     meanwhile), because compaction cannot be done safely then
//   - messages are matched by ID (getMessageIDs), because new messages may be added concurrently while the LLM writes the summary;
//     the result is the summary first, then the new messages in their original order
//   - if enough new messages arrived that summary plus survivors no longer fit, clampToMax trims the survivors
//   - on success the generation counter is incremented
func (c *ChatContext) Compact(state ContextState, summaryMsgs []model.ChatMessage) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.gen != state.Gen {
		// if the context window has been cleared or compacted since the snapshot was taken, compaction cannot be safely performed
		return false
	}

	// get the IDs for the messages that are being compacted as a hash set (in case new messages are added concurrently while the summary is being generated by the LLM)
	stateMessageIDs := getMessageIDs(state.Messages)

	// create a new message slice, replacing the snapshot messages that were summarized with the summary messages, and then any new messages that were inserted since the compaction started (preserving their order).
	newMessages := append([]model.ChatMessage(nil), summaryMsgs...)
	for _, msg := range c.window.GetMessages() {
		if !stateMessageIDs[msg.ID] {
			newMessages = append(newMessages, msg)
		}
	}
	// there is an edge case where maxSize new messages are added concurrently before compaction finishes, so we may need to trim the message slice to fit within the maximum size.
	newMessages = clampToMax(newMessages, len(summaryMsgs), c.window.GetMaxSize())

	// replace the messages in the context window with the new messages
	c.window.Clear()
	c.window.AddMessages(newMessages)
	c.gen++

	return true
}

// getMessageIDs returns the IDs of messages as a set.
//
// Notes:
//   - assumes ID is unique and present for each message; messages with an empty ID are skipped, so they are never treated as
//     already summarized
func getMessageIDs(messages []model.ChatMessage) map[string]bool {

	messageIDs := make(map[string]bool, len(messages))
	for _, m := range messages {
		if m.ID == "" {
			continue
		}
		messageIDs[m.ID] = true
	}
	return messageIDs
}

// clampToMax trims a compacted message slice to at most maxSize messages.
//
// Notes:
//   - msgs is the first numSummary summary messages, then surviving messages oldest-to-newest
//   - only matters in the rare event that enough new messages were added while the summary was being generated that the summary
//     plus every survivor no longer fits
//   - the summary is always kept and the oldest survivors are dropped first, then any survivors before the next user message, so the
//     kept survivors start at a whole turn (possibly leaving just the summary)
//   - skipping to the next user message is needed because the cut may land mid-turn, leaving orphaned tool results or an assistant
//     reply whose question is gone
//   - assumes maxSize >= numSummary and returns msgs unchanged (not copied) when it already fits; otherwise it allocates a fresh
//     slice so it does not alias msgs' backing array
func clampToMax(msgs []model.ChatMessage, numSummary, maxSize int) []model.ChatMessage {
	if len(msgs) <= maxSize {
		return msgs
	}
	// skip survivors up to the next user message: the cut may land mid-turn, leaving orphaned tool results or an assistant reply whose question is gone
	start := len(msgs) - (maxSize - numSummary)
	for start < len(msgs) && msgs[start].Role != "user" {
		start++
	}
	// allocate a fresh slice so we don't alias msgs' backing array
	trimmed := make([]model.ChatMessage, 0, maxSize)
	trimmed = append(trimmed, msgs[:numSummary]...)
	return append(trimmed, msgs[start:]...)
}

// SetupChatContext creates a new chat context instance from the context window settings in cfg.
//
// Notes:
//   - validates cfg with CheckConfig first, so a bad setting fails here rather than later
//   - the auto-compaction threshold is 0 (off) unless cfg.AutoCompact is set; otherwise it is autoCompactAt(cfg)
func SetupChatContext(cfg config.Config) (*ChatContext, error) {
	if err := CheckConfig(cfg); err != nil {
		return nil, err
	}
	// create context window data structure
	w, err := NewContextWindow(cfg.ContextWindowMessages, cfg.ContextStrategy)
	if err != nil {
		return nil, err
	}

	// create chat context using context window; threshold 0 = auto-compaction off
	threshold := 0
	if cfg.AutoCompact {
		threshold = autoCompactAt(cfg)
	}
	cc, err := NewChatContext(w, threshold)
	if err != nil {
		return nil, err
	}
	return cc, nil
}

// CheckConfig rejects context window settings that would fail later or quietly misbehave.
//
// Notes:
//   - called by SetupChatContext, and by the agent's own config check, so info mode reports the same errors chat mode would
//   - the window must hold at least MinContextWindowMessages, the strategy must be one of WindowStrategies, and the summarizer
//     prompt must not be blank
//   - autoCompactThreshold is checked even when autoCompact is false, so a typo is caught before auto-compaction is turned back on
//   - [agent] with autoCompact on, the start point must be above 4 messages: compaction leaves 2 messages (the summary pair) and a
//     plain turn adds at least 2 more, so a threshold of 4 or less would compact after nearly every turn
func CheckConfig(cfg config.Config) error {
	if cfg.ContextWindowMessages < MinContextWindowMessages {
		return fmt.Errorf("contextWindowMessages must be at least %d (the summary pair plus one turn), got %d", MinContextWindowMessages, cfg.ContextWindowMessages)
	}
	if !slices.Contains(WindowStrategies, cfg.ContextStrategy) {
		return fmt.Errorf("contextStrategy %q is not one of %s", cfg.ContextStrategy, strings.Join(WindowStrategies, ", "))
	}
	// checked even when autoCompact is false, so a typo is caught before auto-compaction is turned back on
	if !(cfg.AutoCompactThreshold > 0 && cfg.AutoCompactThreshold <= 1) {
		return fmt.Errorf("autoCompactThreshold must be above 0 and at most 1 (a fraction of the window), got %g", cfg.AutoCompactThreshold)
	}
	// [agent] compaction leaves 2 messages (the summary pair) and a plain turn adds at least 2 more, so a threshold of 4 or less
	// would compact after nearly every turn
	if cfg.AutoCompact && autoCompactAt(cfg) <= 4 {
		return fmt.Errorf("autoCompactThreshold %g of %d messages starts auto-compaction at %d messages; it must be above 4 (the summary pair plus one turn)",
			cfg.AutoCompactThreshold, cfg.ContextWindowMessages, autoCompactAt(cfg))
	}
	if strings.TrimSpace(cfg.SummarizerPrompt) == "" {
		return fmt.Errorf("summarizerPrompt must not be empty")
	}
	return nil
}

// autoCompactAt is the message count at which auto-compaction starts.
//
// Notes:
//   - the product of the window size and the threshold fraction, truncated toward zero
func autoCompactAt(cfg config.Config) int {
	return int(float64(cfg.ContextWindowMessages) * cfg.AutoCompactThreshold)
}

// SummarizeChatContext makes a call to the provider to summarize the given chat messages, with systemPrompt (config's
// summarizerPrompt) as the instructions.
//
// Notes:
//   - returns an error for no messages, a provider error, or an empty summary (a Chat response with no text is an error)
//   - the messages are rendered as plain text, one block per message (tool calls and tool call IDs included), and sent inside
//     <transcript> tags in a brand-new request slice, so the original backing array is never touched
//   - the user's extra instructions (addlInstructions) go in the one system message, because some providers reject or ignore a system
//     message that isn't first
//   - [agent] the system message also ranks the user's own text above anything inside the transcript (which can hold text from files
//     or tools)
//   - no tools are offered to the model for this call
func SummarizeChatContext(ctx context.Context, p model.Provider, msgs []model.ChatMessage, systemPrompt, addlInstructions string) (string, error) {
	if len(msgs) == 0 {
		return "", fmt.Errorf("no messages to summarize")
	}
	// render the messages as plain text, one block per message
	var transcript strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&transcript, "%s: %s\n", m.Role, m.Content)
		for _, call := range m.ToolCalls {
			fmt.Fprintf(&transcript, "tool_call %s: %s(%s)\n", call.ID, call.Function.Name, call.Function.Arguments)
		}
		if m.ToolCallID != "" {
			fmt.Fprintf(&transcript, "tool_call_id: %s\n", m.ToolCallID)
		}
	}

	// the user's extra instructions go in the one system message: some providers reject or ignore a system message that isn't first
	// [agent] the system message also ranks the user's own text above anything inside the transcript (which can hold text from files or tools)
	if addlInstructions != "" {
		systemPrompt += "\n\nAdditional instructions from the user: " + addlInstructions
	}

	// Create a brand-new model.ChatMessages slice, so we never touch the original backing array.
	req := []model.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "<transcript>\n" + transcript.String() + "</transcript>"},
	}

	resp, err := p.Chat(ctx, req, nil)
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}

	// Chat response summary could be nil, which should be considered an error.
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return "", fmt.Errorf("summarize: model returned an empty summary")
	}
	return summary, nil
}

// CompactChatContext compacts the chat context by summarizing the current messages and replacing them with a user/assistant
// pair: a request for a summary, and the summary. It returns the assistant message, which holds the summary.
//
// Notes:
//   - addlInstructions provide additional information for the summarization process, guiding how the chat context should be
//     summarized
//   - releases the in-progress flag (EndCompaction) on every return path, so the caller must have claimed it first with
//     ShouldStartCompaction
//   - takes a Snapshot first and Compact applies the summary against it, so messages added while the LLM was working survive; if the
//     window was cleared or compacted meanwhile it returns an error ([system] Chat context compaction failed)
//   - [agent] a pair, not a lone user message: as a user message the model could read the summary as a new request, and the next user
//     turn would make two user messages in a row. As the assistant's own answer, text injected into the summary (from files or
//     tools) also carries less authority than if it looked like the user said it
//   - the user half is made up (the user never typed it); chat history keeps what really happened, only the context is replaced
//   - in the future, the compaction could be more sophisticated (compacting a subset of messages, merging similar messages, having
//     categories for messages, etc.)
func CompactChatContext(ctx context.Context, chatctx *ChatContext, p model.Provider, systemPrompt, addlInstructions string) (model.ChatMessage, error) {
	defer chatctx.EndCompaction() // ensure the compaction lock is released after compaction is done

	// take a snapshot of the current chat history
	state := chatctx.Snapshot()

	// summarize the chat history
	summary, err := SummarizeChatContext(ctx, p, state.Messages, systemPrompt, addlInstructions)
	if err != nil {
		return model.ChatMessage{}, err
	}

	// [agent] a pair, not a lone user message: as a user message the model could read the summary as a new request, and the next
	// user turn would make two user messages in a row. As the assistant's own answer, text injected into the summary (from files or
	// tools) also carries less authority than if it looked like the user said it.
	// The user half is made up (the user never typed it); chat history keeps what really happened, only the context is replaced.
	now := time.Now().UTC()
	requestMsg := model.ChatMessage{Role: "user", Content: SummaryRequest, Timestamp: now, ID: model.CreateID(), Type: "summary"}
	summaryMsg := model.ChatMessage{Role: "assistant", Content: summary, Timestamp: now, ID: model.CreateID(), Type: "summary"}

	// create a new message history, removing all of the messages that were in the snapshot that was summarized
	isCompacted := chatctx.Compact(state, []model.ChatMessage{requestMsg, summaryMsg})
	if isCompacted {
		// optionally, you could log or perform some action when compaction succeeds
		return summaryMsg, nil
	} else {
		return model.ChatMessage{}, fmt.Errorf("[system] Chat context compaction failed")
	}
}

// DebugChatContext prints the current chat context to w, for debugging.
//
// Notes:
//   - shown only with -debug / "debug": true
func DebugChatContext(w io.Writer, msgContext []model.ChatMessage) {
	fmt.Fprintln(w, "[debug] --- context window ---")
	for i, msg := range msgContext {
		fmt.Fprintf(w, "[%d] %s: %s\n", i, msg.Role, msg.Content)
	}
	fmt.Fprintln(w, "[debug] --- end ---")
}
