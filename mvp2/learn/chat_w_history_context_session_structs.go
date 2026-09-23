//
// This builds on chat_w_memory_compaction_refactor.go to accomodate any needed or desired changes discovered through testing.
//

package main

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const ResponseTimeout = 5 * time.Minute
const WelcomeMsg = "Running agent with configuration: %+v\n\nType /clear to clear chat history.\nType /exit to exit\n"
const WindowStrategy = "offset" // default context window strategy: "offset", "in-place", "ring-buffer", "linked-list"
const SummarizeSystemPrompt = "You are a helpful assistant that summarizes chat history. Summarize the key conversational points and important details concisely."
const MaxContextWindow = 20          // maximum number of messages to keep in the sliding context window
const AutoCompactThresholdFrac = 0.9 // fraction threshold of the context window at which automatic compaction is triggered

//////////////////////////////////////////////
// Interfaces and structs
//////////////////////////////////////////////

type Provider interface {
	Chat(ctx context.Context, chatHistory []ChatMessage) (string, error)
}

type OpenAICompat struct {
	BaseURL string
	Model   string
	ApiKey  string // empty for Ollama
}

type Config struct {
	// User configuration
	UserID string `json:"user_id"`

	// LLM service provider configuration
	Provider   string `json:"provider"` // "openai" (any OpenAI-compatible server) | "anthropic"
	Model      string `json:"model"`
	BaseURL    string `json:"base_url"`
	ApiKeyName string `json:"api_key_name"`

	// I/O configuration for the chat session
	InBuffer  io.Reader `json:"in_buffer"`
	OutBuffer io.Writer `json:"out_buffer"`

	// system prompt
	SystemPrompt string `json:"system_prompt"`

	// memory configuration
	ChatStoreType string `json:"chat_store_type"` // "in-memory" | "persistent"

	// tools and MCP servers configuration
	Tools []string `json:"tools"`
}

func getDefaultConfig() Config {
	return Config{
		UserID:        "default_user",
		Provider:      "openai", // Ollama exposes an OpenAI-compatible endpoint
		Model:         "gpt-4o-mini",
		BaseURL:       "https://api.openai.com/v1",
		ApiKeyName:    "OPENAI_API_KEY", // no auth needed for a local Ollama server
		SystemPrompt:  "You are a helpful assistant. Keep your responses concise and relevant.",
		ChatStoreType: "in-memory",
		InBuffer:      os.Stdin,
		OutBuffer:     os.Stdout,
	}
}

// func getDefaultConfig() Config {
// 	return Config{
// 		UserID:        "default_user",
// 		Provider:     "openai", // Ollama exposes an OpenAI-compatible endpoint
// 		Model:        "qwen2.5:0.5b",
// 		BaseURL:      "http://127.0.0.1:11434/v1",
// 		ApiKeyName:   "", // no auth needed for a local Ollama server
// 		SystemPrompt: "You are a helpful assistant. Keep your responses concise and relevant.",
// 		ChatStoreType: "in-memory",
// 		InBuffer:      os.Stdin,
// 		OutBuffer:     os.Stdout,
// 	}
// }

type ChatMessage struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"` // `json:"role"` is a tag indicating the JSON key for this field
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

type ChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
}

// ContextWindow defines the interface for managing the chat history within the context window, allowing different strategies for handling the chat history.
type ContextWindow interface {
	AddMessages(msgs []ChatMessage)
	GetMessages() []ChatMessage
	RemoveLast(n int)
	Clear()
	GetSize() int    // gets the current number of messages in the context window
	GetMaxSize() int // gets the maximum number of messages the context window can hold
}

func NewContextWindow(maxContextWindow int, windowStrategy string) (ContextWindow, error) {
	const buffer = 2 // leave headroom for the system prompt + pending user message added outside window
	switch windowStrategy {
	case "offset":
		return NewOffsetWindow(maxContextWindow - buffer), nil
	case "in-place":
		return NewInPlaceWindow(maxContextWindow - buffer), nil
	case "ring-buffer":
		return NewRingBufferWindow(maxContextWindow - buffer), nil
	case "linked-list":
		return NewLLWindow(maxContextWindow - buffer), nil
	default:
		return nil, fmt.Errorf("unsupported window strategy: %s", windowStrategy)
	}
}

// This is a snapshot of the context window's state, including the current chat history and the generation counter.
type ContextState struct {
	Messages []ChatMessage
	Gen      int
}

// ChatHistory interface for managing running chat history storage and retrieval.
type ChatHistory interface {
	GetMessages() []ChatMessage
	Append(msgs []ChatMessage)
}

type InMemoryChatHistory struct {
	mu       sync.Mutex
	messages []ChatMessage
}

func NewInMemoryChatHistory() (*InMemoryChatHistory, error) {
	return &InMemoryChatHistory{
		messages: make([]ChatMessage, 0),
	}, nil
}

// gets a full copy of the chat history
func (h *InMemoryChatHistory) GetMessages() []ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ChatMessage(nil), h.messages...)
}

// appends new messages to the chat history
func (h *InMemoryChatHistory) Append(msgs []ChatMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// the ... appends all of the messages one at a time preserving order (equivalent to a for loop but cleaner syntax)
	h.messages = append(h.messages, msgs...)
}

// chatSession holds the state and configuration for an ongoing chat session
type ChatSession struct {
	Provider   Provider
	MsgHistory ChatHistory
	MsgContext *ChatContext
	SystemMsg  ChatMessage
	UserID     string
	InBuffer   io.Reader
	OutBuffer  io.Writer
}

func NewChatSession(provider Provider, chatHistory ChatHistory, chatContext *ChatContext, cfg Config) (*ChatSession, error) {
	return &ChatSession{
		Provider:   provider,
		MsgHistory: chatHistory,
		MsgContext: chatContext,
		SystemMsg:  createSystemMessage(cfg.SystemPrompt),
		UserID:     cfg.UserID,
		InBuffer:   cfg.InBuffer,
		OutBuffer:  cfg.OutBuffer,
	}, nil
}

func createSystemMessage(systemPrompt string) ChatMessage {
	return ChatMessage{
		Role:      "system",
		Content:   systemPrompt,
		ID:        createID(),
		Timestamp: time.Now().UTC(),
	}
}

// Context for a chat session
type ChatContext struct {
	mu                   sync.Mutex
	window               ContextWindow // The context window is the data structure that holds the actual chat messages
	gen                  int           // Generation counter - increments each time the context is compacted or cleared
	autoCompactThreshold int
	compactInProgress    atomic.Bool
}

func NewChatContext(w ContextWindow, autoCompactThreshold int) (*ChatContext, error) {
	return &ChatContext{
		window:               w,
		gen:                  0,
		autoCompactThreshold: autoCompactThreshold,
		compactInProgress:    atomic.Bool{},
	}, nil
}

func (c *ChatContext) GetWindow() ContextWindow {
	return c.window
}

func (c *ChatContext) ShouldStartCompaction() bool {
	return c.compactInProgress.CompareAndSwap(false, true)
}

func (c *ChatContext) EndCompaction() {
	c.compactInProgress.Store(false)
}

func (c *ChatContext) IsAutoCompactionNeeded() bool {
	return c.window.GetSize() >= c.autoCompactThreshold
}

func (c *ChatContext) AddMessages(msgs []ChatMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.AddMessages(msgs)
}

func (c *ChatContext) GetMessages() []ChatMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window.GetMessages()
}

func (c *ChatContext) RemoveLast(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.RemoveLast(n)
}

func (c *ChatContext) GetSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window.GetSize()
}

func (c *ChatContext) GetMaxSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window.GetMaxSize()
}

// Snapshot() gets a snapshot of the state for the current context window - for now this includes the chat history and the generation counter
func (c *ChatContext) Snapshot() ContextState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ContextState{
		Messages: c.window.GetMessages(),
		Gen:      c.gen,
	}
}

func (c *ChatContext) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.Clear()
	c.gen++
}

func (c *ChatContext) Compact(state ContextState, summaryMsg ChatMessage) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.gen != state.Gen {
		// if the context window has been cleared or compacted since the snapshot was taken, compaction cannot be safely performed
		return false
	}

	// get the IDs for the messages that are being compacted as a hash set (in case new messages are added concurrently while the summary is being generated by the LLM)
	stateMessageIDs := getMessageIDs(state.Messages)

	// create a new message slice, replacing the snapshot messages that were summarized with the summary message, and then any new messages that were inserted since the compaction started (preserving their order).
	newMessages := append([]ChatMessage(nil), summaryMsg)
	for _, msg := range c.window.GetMessages() {
		if !stateMessageIDs[msg.ID] {
			newMessages = append(newMessages, msg)
		}
	}
	// there is an edge case where maxSize new messages are added concurrently before compaction finishes, so we may need to trim the message slice to fit within the maximum size.
	newMessages = clampToMax(newMessages, c.window.GetMaxSize())

	// replace the messages in the context window with the new messages
	c.window.Clear()
	c.window.AddMessages(newMessages)
	c.gen++

	return true
}

// given a slice of chat messages,get message IDs back as a map
// this assumes that ID is unique and present for each message
func getMessageIDs(messages []ChatMessage) map[string]bool {

	messageIDs := make(map[string]bool, len(messages))
	for _, m := range messages {
		if m.ID == "" {
			continue
		}
		messageIDs[m.ID] = true
	}
	return messageIDs
}

// clampToMax trims a compacted message slice (summary at [0], then surviving messages oldest-to-newest) to at most maxSize messages.
// This only matters in the rare event that enough new messages were added while the summary was being generated that the summary
// plus every survivor no longer fits. The summary is always kept and the oldest survivors are dropped first.
// It assumes maxSize >= 1 and returns msgs unchanged (not copied) when it already fits.
func clampToMax(msgs []ChatMessage, maxSize int) []ChatMessage {
	if len(msgs) <= maxSize {
		return msgs
	}
	// allocate a fresh slice so we don't alias msgs' backing array
	trimmed := make([]ChatMessage, 0, maxSize)
	trimmed = append(trimmed, msgs[0])
	return append(trimmed, msgs[len(msgs)-(maxSize-1):]...)
}

//******************************************//
// Chat Setup Functions
//******************************************//

// setup provider
func setupProvider(cfg Config) (Provider, error) {
	if cfg.Provider == "openai" {
		return &OpenAICompat{
			Model:   cfg.Model,
			BaseURL: cfg.BaseURL,
			ApiKey:  os.Getenv(cfg.ApiKeyName),
		}, nil
	} else {
		// provider not supported
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
}

// creates and returns a new chat history instance
func setupMemoryStore(cfg Config) (ChatHistory, error) {
	if cfg.ChatStoreType == "in-memory" {
		c, err := NewInMemoryChatHistory()
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, fmt.Errorf("unsupported chat store type: %s", cfg.ChatStoreType)
}

// creates a new chat context instance
func setupChatContext(cfg Config) (*ChatContext, error) {
	// create context window data structure
	w, err := NewContextWindow(MaxContextWindow, WindowStrategy)
	if err != nil {
		return nil, err
	}

	// create chat context using context window
	threshold := int(float64(w.GetMaxSize()) * AutoCompactThresholdFrac)
	cc, err := NewChatContext(w, threshold)
	if err != nil {
		return nil, err
	}
	return cc, nil
}

// creates a new chat session instance
func setupChatSession(provider Provider, chatHistory ChatHistory, chatContext *ChatContext, cfg Config) (*ChatSession, error) {
	cs, err := NewChatSession(provider, chatHistory, chatContext, cfg)
	if err != nil {
		return nil, err
	}
	return cs, nil
}

//*********************************************************//
// struct types that implement the ContextWindow interface
//*********************************************************//

// OffsetWindow is a context window strategy that stores messages as a slice and just shifts the window,
// making the oldest messages at the beginning of the slice unreachable, when the maximum size is exceeded.
type OffsetWindow struct {
	mu       sync.Mutex
	messages []ChatMessage
	maxSize  int // maximum number of messages to retain in the context window
}

func NewOffsetWindow(contextWindowSize int) *OffsetWindow {
	return &OffsetWindow{
		messages: make([]ChatMessage, 0, contextWindowSize),
		maxSize:  contextWindowSize,
	}
}

// adds one or more messages to the context window
func (w *OffsetWindow) AddMessages(msgs []ChatMessage) {
	// We need to lock the mutex to ensure thread-safe access to the messages slice.
	w.mu.Lock()
	defer w.mu.Unlock()
	// append the new messages
	for _, msg := range msgs {
		w.messages = append(w.messages, msg)
	}
	// After we have added the messages, we check if the total number of messages exceeds the maximum size and trim the oldest messages if necessary.
	// In practice we want the maxSize to be a bit less than the max context window.
	// If several messages are added, we don't want to hit the actual max context window before we trim.
	if len(w.messages) > w.maxSize {
		w.messages = w.messages[len(w.messages)-w.maxSize:]
	}
}

// gets a copy of the chat history within the current context window
func (w *OffsetWindow) GetMessages() []ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	// this appends to a nil slice, which results in a new slice (copy) being allocated - this is what we want so we don't share the underlying array (which may get modified after GetMessages() returns, resulting in unexpected behavior if we didn't make a copy)
	// [agent] Go 1.21 the standard library has slices.Clone(w.messages), which does the same thing and says what it does. It's what I'd reach for if your go.mod allows it. slices.Clone also keeps a nil input as nil.
	// Note that Messages: w.messages would share the underlying array, so we make a copy to avoid external modifications.
	newMessages := append([]ChatMessage(nil), w.messages...)
	return newMessages
}

// removes the last n messages from the context window - like an 'undo'
func (w *OffsetWindow) RemoveLast(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.messages) >= n {
		w.messages = w.messages[:len(w.messages)-n]
	}
}

// clears the context window, removing all messages and incrementing the generation counter.
func (w *OffsetWindow) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = w.messages[:0]
}

func (w *OffsetWindow) GetSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.messages)
}

func (w *OffsetWindow) GetMaxSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxSize
}

// InPlaceWindow is a context window strategy that stores messages in place and overwrites the oldest messages when the maximum size is exceeded.
type InPlaceWindow struct {
	mu       sync.Mutex
	messages []ChatMessage
	maxSize  int
}

func NewInPlaceWindow(contextWindowSize int) *InPlaceWindow {
	return &InPlaceWindow{
		messages: make([]ChatMessage, 0, contextWindowSize),
		maxSize:  contextWindowSize,
	}
}

func (w *InPlaceWindow) AddMessages(msgs []ChatMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// append the new messages
	for _, msg := range msgs {
		w.messages = append(w.messages, msg)
	}
	// similar to the offset window, we want maxSize < maxContextWindow so that there is a buffer and we never hit the actual max context window before trimming.
	if len(w.messages) > w.maxSize {
		// shift all messages to the left by one position to make room for the new message at the end
		num2drop := len(w.messages) - w.maxSize
		n := copy(w.messages, w.messages[num2drop:])
		w.messages = w.messages[:n]
	}
}

func (w *InPlaceWindow) GetMessages() []ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	newMessages := append([]ChatMessage(nil), w.messages...)
	return newMessages
}

func (w *InPlaceWindow) RemoveLast(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.messages) >= n {
		w.messages = w.messages[:len(w.messages)-n]
	}
}

// clears the context window, removing all messages and incrementing the generation counter.
func (w *InPlaceWindow) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = w.messages[:0]
}

func (w *InPlaceWindow) GetSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.messages)
}

func (w *InPlaceWindow) GetMaxSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxSize
}

// RingBufferWindow is a context window strategy that uses a ring buffer to store messages.
type RingBufferWindow struct {
	mu       sync.Mutex
	messages []ChatMessage // fixed size backing array (size = maxSize)
	maxSize  int
	head     int // index where the next message should be stored
	count    int // number of messages currently in the buffer
}

func NewRingBufferWindow(contextWindowSize int) *RingBufferWindow {
	return &RingBufferWindow{
		messages: make([]ChatMessage, contextWindowSize, contextWindowSize),
		maxSize:  contextWindowSize,
		head:     0,
		count:    0,
	}
}

func (w *RingBufferWindow) AddMessages(msgs []ChatMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, msg := range msgs {
		w.messages[w.head] = msg
		w.head = (w.head + 1) % w.maxSize // if index exceed maxSize this will wrap around
		if w.count < w.maxSize {
			w.count++
		}
	}
}

func (w *RingBufferWindow) GetMessages() []ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	// with a ring buffer we need to explicitly create a new slice and copy the messages in the correct order, because the underlying array may wrap around.
	newMessages := make([]ChatMessage, w.count)
	for i := 0; i < w.count; i++ {
		newMessages[i] = w.messages[((w.head-w.count)+w.maxSize+i)%w.maxSize]
	}
	return newMessages
}

func (w *RingBufferWindow) RemoveLast(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.count >= n {
		w.count -= n
		w.head = (w.head - n + w.maxSize) % w.maxSize
	}
}

// clears the context window, removing all messages and incrementing the generation counter.
func (w *RingBufferWindow) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// reset the fixed size backing array without changing its capacity (initializes all elements to their zero value - O(num messages) operation
	// If we ever want the O(1) version, delete this loop and add a comment saying the stale slots are unreachable, but still hold references.
	// Either way, clear(w.messages) does the same job in one line, if we're on Go 1.21 or later - still is an O(num messages) operation.
	for i := range w.messages {
		w.messages[i] = ChatMessage{}
	}
	w.head = 0
	w.count = 0
}

func (w *RingBufferWindow) GetSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

func (w *RingBufferWindow) GetMaxSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxSize
}

// LLWindow is a context window strategy that uses a doubly linked list to store messages.
// A doubly linked list has head and tail pointers, so add and remove can happen from the front or back in O(1) time
// This is useful for when we reach the context max and the new message needs to wrap around to the front (requiring us to remove the current head node and inserting new message in the front)
type LLWindow struct {
	mu       sync.Mutex
	messages *list.List
	maxSize  int
}

// initialize an empty linked list
func NewLLWindow(contextWindowSize int) *LLWindow {
	return &LLWindow{
		messages: list.New(),
		maxSize:  contextWindowSize,
	}
}

func (w *LLWindow) AddMessages(msgs []ChatMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// add nodes to the linked list
	for _, msg := range msgs {
		if w.messages.Len() >= w.maxSize {
			w.messages.Remove(w.messages.Front())
		}
		w.messages.PushBack(msg)
	}
}

func (w *LLWindow) GetMessages() []ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	messages := make([]ChatMessage, w.messages.Len())
	// traverse the linked list from front to back and copy the messages into the slice
	i := 0
	for e := w.messages.Front(); e != nil; e = e.Next() {
		// LL node values are type Any (i.e., interface{}) and this instance holds ChatMessage values, so we need to type assert it to ChatMessage before copying it into the slice
		messages[i] = e.Value.(ChatMessage)
		i++
	}
	return messages
}

func (w *LLWindow) RemoveLast(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 0; i < n && w.messages.Len() > 0; i++ {
		w.messages.Remove(w.messages.Back())
	}
}

func (w *LLWindow) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages.Init()
}

func (w *LLWindow) GetSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.messages.Len()
}

func (w *LLWindow) GetMaxSize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxSize
}

//***********************************************************//
// Methods that operate on the chat context (e.g., compaction)
//***********************************************************//

// makes a call to the provider to summarize the given chat messages
func summarizeChatContext(ctx context.Context, p Provider, msgs []ChatMessage) (string, error) {
	if len(msgs) == 0 {
		return "", fmt.Errorf("no messages to summarize")
	}
	// render the messages as plain text, one block per message
	var transcript strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&transcript, "%s: %s\n", m.Role, m.Content)
	}

	// Create a brand-new chatMessages slice, so we never touch the original backing array.
	req := []ChatMessage{
		{Role: "system", Content: SummarizeSystemPrompt},
		{Role: "user", Content: "<transcript>\n" + transcript.String() + "</transcript>"},
	}

	summary, err := p.Chat(ctx, req)
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}

	// Chat response summary could be nil, which should be considered an error.
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", fmt.Errorf("summarize: model returned an empty summary")
	}
	return summary, nil
}

// compacts the chat context by summarizing the current messages and replacing them with a single system message containing the summary.
// in the future, the compaction could be more sophisticated (compacting a subset of messages, merging similar messages, having categories for messages, etc.)
// compactionType values for compactChatContext, distinguishing a manual /compact from an
// auto-triggered one (only affects the "triggered..." message printed to the user).
const (
	CompactionManual = "manual"
	CompactionAuto   = "auto"
)

func compactChatContext(ctx context.Context, cs *ChatSession, compactionType string) (ChatMessage, error) {
	defer cs.MsgContext.EndCompaction() // ensure the compaction lock is released after compaction is done
	if compactionType == CompactionAuto {
		fmt.Fprintln(cs.OutBuffer, "Auto-compaction triggered...")
	} else {
		fmt.Fprintln(cs.OutBuffer, "Compaction triggered...")
	}

	// take a snapshot of the current chat history
	state := cs.MsgContext.Snapshot()

	// summarize the chat history
	summary, err := summarizeChatContext(ctx, cs.Provider, state.Messages)
	if err != nil {
		return ChatMessage{}, err
	}

	// create a new message history, removing all of the messages that were in the snapshot that was summarized
	summaryMsg := ChatMessage{
		Role:      "user",
		Content:   summary,
		Timestamp: time.Now().UTC(), // .Format(time.RFC3339),
		ID:        createID(),       // uuid.New().String(),
		Type:      "summary",
	}
	isCompacted := cs.MsgContext.Compact(state, summaryMsg)
	if isCompacted {
		// optionally, you could log or perform some action when compaction succeeds
		fmt.Fprintln(cs.OutBuffer, "Chat context compacted successfully.")
		return summaryMsg, nil
	} else {
		return ChatMessage{}, fmt.Errorf("Chat context compaction failed")
	}
}

// print the current chat context, just for debugging purposes - prints to the standard output (console)
func debugChatContext(msgContext []ChatMessage) {
	fmt.Println("[debug] --- context window ---")
	for i, msg := range msgContext {
		fmt.Printf("[%d] %s: %s\n", i, msg.Role, msg.Content)
	}
	fmt.Println("[debug] --- end ---")
}

//***********************************************************//
// Chat request and response
//***********************************************************//

func (p OpenAICompat) Chat(ctx context.Context, chatHistory []ChatMessage) (string, error) {
	// make a request to the OpenAI-compatible server
	// 1. build a chatRequest with p.Model and the provided chat history
	reqRaw := ChatRequest{
		Model:    p.Model,
		Messages: chatHistory,
	}

	// 2. json.Marshal it into a []byte body
	reqBody, err := json.Marshal(reqRaw)
	if err != nil {
		return "", err
	}

	// 3. build an *http.Request with http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", ...)
	ctx, cancel := context.WithTimeout(ctx, ResponseTimeout)
	defer cancel()
	reqHttp, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}

	// 4. set header "Content-Type": "application/json"
	reqHttp.Header.Set("Content-Type", "application/json")
	if p.ApiKey != "" {
		reqHttp.Header.Set("Authorization", "Bearer "+p.ApiKey)
	}

	// 5. do the request with a http.DefaultClient.Do(req) and TIMEOUT
	resp, err := http.DefaultClient.Do(reqHttp)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 5b. Check if the HTTP response status code indicates an error
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// 6. read the response body, json.Unmarshal into a chatResponse
	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", err
	}

	// 7. return resp.Choices[0].Message.Content, nil
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return chatResp.Choices[0].Message.Content, nil
}

// helper function for handling a fatal error and exiting the program immediately
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}

// generate a (fairly) unique ID using 16 random bytes and encode it as a hex string
// github.com/google/uuid is better but generates a dependency. We can consider if we truly need RFC-4122 UUIDs
func createID() string {
	b := make([]byte, 16)
	rand.Read(b) // crypto/rand
	return hex.EncodeToString(b)
}

// prepareChatRequest prepares the chat messages to be sent to the provider by combining the system message, the chat history, and the user's message into a single slice of ChatMessage.
func prepareChatRequest(msgContext []ChatMessage, systemMsg, userMsg ChatMessage) []ChatMessage {
	chat2send := make([]ChatMessage, len(msgContext)+2)
	chat2send[0] = systemMsg
	copy(chat2send[1:], msgContext)
	chat2send[len(chat2send)-1] = userMsg
	return chat2send
}

//***********************************************************//
// Parse and handle user input
//***********************************************************//

func handleUserInput(ctx context.Context, cs *ChatSession, line string) bool {
	// Implementation for handling user input goes here
	msgContext := cs.MsgContext.GetMessages()
	line = strings.TrimSpace(line)
	lineFirst, lineRest, _ := strings.Cut(line, " ")
	// skip empty lines (user just pushes enter or just has spaces)
	if lineFirst == "" {
		return false
	}
	// parse slash commands
	if strings.HasPrefix(lineFirst, "/") {
		switch lineFirst {
		case "/exit":
			return true
		case "/clear":
			cs.MsgContext.Clear()
			fmt.Fprintln(cs.OutBuffer, "Chat history cleared.")
		case "/summary", "/summarize":
			summaryString, err := summarizeChatContext(ctx, cs.Provider, msgContext)
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "Summarization error:", err)
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "Chat summary:", summaryString)
		case "/compact":
			// we need to check if a compaction is already happening so we don't trigger a second compaction concurrently
			if !cs.MsgContext.ShouldStartCompaction() {
				fmt.Fprintln(cs.OutBuffer, "Compaction already in progress. Skipping this compaction request.")
				return false
			}
			summaryMsg, err := compactChatContext(ctx, cs, CompactionManual)
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "Compaction error:", err)
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "Chat summary:", summaryMsg.Content)
		default:
			// if the command is not recognized, print an error message
			fmt.Fprintln(cs.OutBuffer, "Unrecognized command:", lineFirst)
			return false
		}
		// user may have entered a prompt following the slash command (e.g., /clear <A brand new prompt>)
		line = strings.TrimSpace(lineRest)
		if line == "" {
			return false
		}
	}

	// append the user message to the chat history
	userMsg := ChatMessage{
		Role:      "user",
		Content:   line,
		ID:        createID(),
		Timestamp: time.Now().UTC(),
		Type:      "user",
	}

	// [debug] print the current chat history before sending the prompt to the chat provider
	debugChatContext(msgContext)

	// prepare the request to send to the chat provider - includes the system message, the chat history, and the user's message
	chat2send := prepareChatRequest(msgContext, cs.SystemMsg, userMsg)

	// send the chat request to the LLM provider
	response, err := cs.Provider.Chat(ctx, chat2send)
	if err != nil {
		fmt.Fprintln(cs.OutBuffer, "error:", err)
		return false
	}

	// get the response content from the chat provider
	fmt.Fprintln(cs.OutBuffer, "Chat response:", response)

	// append the user prompt and AI assistant's response to the chat history
	responseMsg := ChatMessage{
		Role:      "assistant",
		Content:   response,
		ID:        createID(),
		Timestamp: time.Now().UTC(),
	}

	// add new messages to history and context
	cs.MsgHistory.Append([]ChatMessage{userMsg, responseMsg})
	cs.MsgContext.AddMessages([]ChatMessage{userMsg, responseMsg})

	// check if the chat history has reached the auto-compaction threshold and a compaction is not already in progress - if so, trigger auto-compaction in a separate goroutine
	if cs.MsgContext.IsAutoCompactionNeeded() && cs.MsgContext.ShouldStartCompaction() {
		go func() {
			_, err := compactChatContext(ctx, cs, CompactionAuto)
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "Auto-compaction error:", err)
			}
		}()
	}
	return false
}

//***********************************************************//
// Run loop for handling chat session
//***********************************************************//

func runLoop(ctx context.Context, chatSession *ChatSession) {

	stdin := bufio.NewReader(chatSession.InBuffer)

	for {
		// read user input from stdin - /exit to quit
		fmt.Print("\n> ")
		line, err := stdin.ReadString('\n')
		if err != nil {
			return
		}

		quitSignal := handleUserInput(ctx, chatSession, line)
		if quitSignal {
			return
		}

	}
}

//***********************************************************//
// Main entry point for the agent application
//***********************************************************//

func runAgent(ctx context.Context, cfg Config) error {
	// placeholder for the main agent logic
	// this function should implement the core functionality of the agent
	// using the provided context and configuration

	//// setup the agent object ////
	// 1. setup LLM provider
	provider, err := setupProvider(cfg)
	if err != nil {
		return err
	}

	// 2. setup memory store for chat history
	chatHistory, err := setupMemoryStore(cfg)
	if err != nil {
		return err
	}

	// 3. setup context struct for managing active conversation context
	chatContext, err := setupChatContext(cfg)
	if err != nil {
		return err
	}

	// 4. setup tools and MCP servers (if applicable)

	// 5. setup this chat session
	chatSession, err := setupChatSession(provider, chatHistory, chatContext, cfg)
	if err != nil {
		return err
	}

	// print the configuration for debugging purposes
	fmt.Printf(WelcomeMsg, cfg)

	// 6. chat with the LLM provider in a loop
	runLoop(ctx, chatSession)

	return nil
}

func main() {

	// setup hard-coded default configuration
	cfg := getDefaultConfig()

	// catch OS signals (e.g., Ctrl+C or process termination signal (kill)) and terminate gracefully
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// setup the agent object and run the main agent logic with the provided context and configuration
	if err := runAgent(ctx, cfg); err != nil {
		fatal(err)
	}
}
