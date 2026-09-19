//
// This builds on chat_w_diff_context_window_mgmt_strategies.go by adding background and manual memory compaction
//
// Note that we are still measuring the context window using number of messages as the unit. This does not differentiate long messages from short ones.
// Measuring context window by token count is the correct approach, which we will tackle later. This will require a tokenizer or a way to know or estimate the number of tokens for a given message.
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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const ResponseTimeout = 5 * time.Minute
const WelcomeMsg = "Running agent with configuration: %+v\n\nType /clear to clear chat history.\nType /exit to exit\n"
const WindowStrategy = "offset" // default context window strategy: "offset", "in-place", "ring-buffer", "linked-list"
const SummarizeSystemPrompt = "You are a helpful assistant that summarizes chat history. Summarize the key conversational points and important details concisely."

type Provider interface {
	Chat(ctx context.Context, chatHistory []ChatMessage) (string, error)
}

type OpenAICompat struct {
	BaseURL string
	Model   string
	ApiKey  string // empty for Ollama
}

type Config struct {
	// LLM service provider configuration
	Provider   string `json:"provider"` // "openai" (any OpenAI-compatible server) | "anthropic"
	Model      string `json:"model"`
	BaseURL    string `json:"base_url"`
	ApiKeyName string `json:"api_key_name"`

	// system prompt
	SystemPrompt string `json:"system_prompt"`

	// tools and MCP servers configuration
	Tools []string `json:"tools"`
}

type ChatMessage struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"` // `json:"role"` is a tag indicating the JSON key for this field
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
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
	Snapshot() ContextState // gets a snapshot of metadata for the current context window - for now this includes the chat history and the generation counter
	Compact(state ContextState, summaryMsg ChatMessage) bool
	Clear()
}

// This is a snapshot of the context window's state, including the current chat history and the generation counter.
type ContextState struct {
	Messages []ChatMessage
	Gen      int
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

// OffsetWindow is a context window strategy that stores messages as a slice and just shifts the window,
// making the oldest messages at the beginning of the slice unreachable, when the maximum size is exceeded.
type OffsetWindow struct {
	mu       sync.Mutex
	messages []ChatMessage
	maxSize  int // maximum number of messages to retain in the context window
	gen      int // generation counter to track the number of times the window has been compacted or cleared
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
	newMessages := make([]ChatMessage, len(w.messages))
	copy(newMessages, w.messages)
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
	w.gen++
}

func (w *OffsetWindow) Snapshot() ContextState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return ContextState{
		// this appends to a nil slice, which results in a new slice (copy) being allocated - this is what we want so we don't share the underlying array with the original messages (which may get modified after Snapshot() returns, resulting in unexpected behavior if we didn't make a copy)
		// [agent] Go 1.21 the standard library has slices.Clone(w.messages), which does the same thing and says what it does. It's what I'd reach for if your go.mod allows it. slices.Clone also keeps a nil input as nil.
		// Note that Messages: w.messages would share the underlying array, so we make a copy to avoid external modifications.
		// Also Messages: w.GetMessages() would result in a deadlock since GetMessages() tries to lock an already locked mutex (and will block until the lock is free). Apparently go -race does not catch deadlocks, so this is a subtle bug to catch.
		Messages: append([]ChatMessage(nil), w.messages...),
		Gen:      w.gen,
	}
}

// Compact replaces the messages in the context window that were summarized with the summary message, preserving any new messages that were added since the snapshot. Returns true if compaction was successful.
func (w *OffsetWindow) Compact(state ContextState, summaryMsg ChatMessage) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gen != state.Gen {
		// if the context window has been cleared or compacted since the snapshot was taken, compaction cannot be safely performed
		return false
	}

	// get the IDs for the messages that are being compacted as a hash set (in case new messages are added concurrently while the summary is being generated by the LLM)
	stateMessageIDs := getMessageIDs(state.Messages)

	// create a new message slice, replacing the snapshot messages that were summarized with the summary message, and then any new messages that were inserted since the compaction started (preserving their order).
	newMessages := make([]ChatMessage, 0, len(w.messages))
	newMessages = append(newMessages, summaryMsg)
	for _, msg := range w.messages {
		if !stateMessageIDs[msg.ID] {
			newMessages = append(newMessages, msg)
		}
	}
	// there is an edge case where maxSize new messages are added concurrently before compaction finishes, so we may need to trim the message slice to fit within the maximum size.
	newMessages = clampToMax(newMessages, w.maxSize)
	w.messages = newMessages
	w.gen++
	return true
}

// InPlaceWindow is a context window strategy that stores messages in place and overwrites the oldest messages when the maximum size is exceeded.
type InPlaceWindow struct {
	mu       sync.Mutex
	messages []ChatMessage
	maxSize  int
	gen      int // generation counter to track the number of times the window has been compacted or cleared
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
	newMessages := make([]ChatMessage, len(w.messages))
	copy(newMessages, w.messages)
	return newMessages
}

func (w *InPlaceWindow) RemoveLast(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.messages) >= n {
		w.messages = w.messages[:len(w.messages)-n]
	}
}

func (w *InPlaceWindow) Snapshot() ContextState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return ContextState{
		Messages: append([]ChatMessage(nil), w.messages...),
		Gen:      w.gen,
	}
}

func (w *InPlaceWindow) Compact(state ContextState, summaryMsg ChatMessage) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gen != state.Gen {
		// if the context window has been cleared or compacted since the snapshot was taken, compaction cannot be safely performed
		return false
	}

	// get the IDs for the messages that are being compacted as a hash set (in case new messages are added concurrently while the summary is being generated by the LLM)
	stateMessageIDs := getMessageIDs(state.Messages)

	// create a new message slice, replacing the snapshot messages that were summarized with the summary message, and then any new messages that were inserted since the compaction started (preserving their order).
	newMessages := make([]ChatMessage, 0, len(w.messages))
	newMessages = append(newMessages, summaryMsg)
	for _, msg := range w.messages {
		if !stateMessageIDs[msg.ID] {
			newMessages = append(newMessages, msg)
		}
	}
	newMessages = clampToMax(newMessages, w.maxSize)
	w.messages = newMessages
	w.gen++
	return true
}

// clears the context window, removing all messages and incrementing the generation counter.
func (w *InPlaceWindow) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages = w.messages[:0]
	w.gen++
}

// RingBufferWindow is a context window strategy that uses a ring buffer to store messages.
type RingBufferWindow struct {
	mu       sync.Mutex
	messages []ChatMessage // fixed size backing array (size = maxSize)
	maxSize  int
	head     int // index where the next message should be stored
	count    int // number of messages currently in the buffer
	gen      int // generation counter to track the number of times the window has been compacted or cleared
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

func (w *RingBufferWindow) Snapshot() ContextState {
	w.mu.Lock()
	defer w.mu.Unlock()
	// with a ring buffer we need to explicitly create a new slice and copy the messages in the correct order, because the underlying array may wrap around.
	messages := make([]ChatMessage, w.count)
	for i := 0; i < w.count; i++ {
		messages[i] = w.messages[((w.head-w.count)+w.maxSize+i)%w.maxSize]
	}
	return ContextState{
		Messages: messages,
		Gen:      w.gen,
	}
}

func (w *RingBufferWindow) Compact(state ContextState, summaryMsg ChatMessage) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gen != state.Gen {
		// if the context window has been cleared or compacted since the snapshot was taken, compaction cannot be safely performed
		return false
	}

	// get the IDs for the messages that are being compacted as a hash set (in case new messages are added concurrently while the summary is being generated by the LLM)
	stateMessageIDs := getMessageIDs(state.Messages)

	// create a new message slice, replacing the snapshot messages that were summarized with the summary message, and then any new messages that were inserted since the compaction started (preserving their order).
	newMessages := make([]ChatMessage, 0, w.count)
	newMessages = append(newMessages, summaryMsg)
	for i := 0; i < w.count; i++ {
		msg := w.messages[((w.head-w.count)+w.maxSize+i)%w.maxSize]
		if !stateMessageIDs[msg.ID] {
			newMessages = append(newMessages, msg)
		}
	}

	newMessages = clampToMax(newMessages, w.maxSize)

	// update the ring buffer with the new messages
	w.count = len(newMessages)
	w.head = w.count % w.maxSize
	for i := 0; i < w.count; i++ {
		w.messages[i] = newMessages[i]
	}
	w.gen++
	return true
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
	w.gen++
}

// LLWindow is a context window strategy that uses a doubly linked list to store messages.
// A doubly linked list has head and tail pointers, so add and remove can happen from the front or back in O(1) time
// This is useful for when we reach the context max and the new message needs to wrap around to the front (requiring us to remove the current head node and inserting new message in the front)
type LLWindow struct {
	mu       sync.Mutex
	messages *list.List
	maxSize  int
	gen      int // generation counter to track the number of times the window has been compacted, cleared, or modified (e.g., due to maxing the context window)
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
	i := 0
	for e := w.messages.Front(); e != nil; e = e.Next() {
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

func (w *LLWindow) Snapshot() ContextState {
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
	return ContextState{
		Messages: messages,
		Gen:      w.gen,
	}
}

func (w *LLWindow) Compact(state ContextState, summaryMsg ChatMessage) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.gen != state.Gen {
		// if the context window has been cleared or compacted since the snapshot was taken, compaction cannot be safely performed
		return false
	}

	// get the IDs for the messages that are being compacted as a hash set (in case new messages are added concurrently while the summary is being generated by the LLM)
	stateMessageIDs := getMessageIDs(state.Messages)

	// create a new message slice, replacing the snapshot messages that were summarized with the summary message, and then any new messages that were inserted since the compaction started (preserving their order).
	newMessages := make([]ChatMessage, 0, w.messages.Len())
	newMessages = append(newMessages, summaryMsg)
	for e := w.messages.Front(); e != nil; e = e.Next() {
		msg := e.Value.(ChatMessage)
		if !stateMessageIDs[msg.ID] {
			newMessages = append(newMessages, msg)
		}
	}

	newMessages = clampToMax(newMessages, w.maxSize)

	// update the linked list with the new messages
	w.messages.Init()
	for _, msg := range newMessages {
		w.messages.PushBack(msg)
	}
	w.gen++
	return true
}

func (w *LLWindow) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messages.Init()
	w.gen++
}

// makes a call to the provider to summarize the given chat messages
func summarizeChatHistory(ctx context.Context, p Provider, msgs []ChatMessage) (string, error) {
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

// compacts the chat history by summarizing the current messages and replacing them with a single system message containing the summary.
// in the future, the compaction could be more sophisticated (compacting a subset of messages, merging similar messages, having categories for messages, etc.)
func compactChatHistory(ctx context.Context, p Provider, w ContextWindow) (ChatMessage, error) {
	// take a snapshot of the current chat history
	state := w.Snapshot()

	// summarize the chat history
	summary, err := summarizeChatHistory(ctx, p, state.Messages)
	if err != nil {
		return ChatMessage{}, err
	}

	// create a new message history, removing all of the messages that were in the snapshot that was summarized
	summaryMsg := ChatMessage{
		Role:      "user",
		Content:   summary,
		Timestamp: time.Now().UTC(), // .Format(time.RFC3339),
		ID:        createID(),       // uuid.New().String(),
	}
	isCompacted := w.Compact(state, summaryMsg)
	if isCompacted {
		// optionally, you could log or perform some action when compaction succeeds
		fmt.Println("Chat history compacted successfully.")
		return summaryMsg, nil
	} else {
		fmt.Println("Chat history compaction failed.")
		return ChatMessage{}, fmt.Errorf("chat history compaction failed")
	}
}

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

func getDefaultConfig() Config {
	return Config{
		Provider:     "openai", // Ollama exposes an OpenAI-compatible endpoint
		Model:        "gpt-4o-mini",
		BaseURL:      "https://api.openai.com/v1",
		ApiKeyName:   "OPENAI_API_KEY", // no auth needed for a local Ollama server
		SystemPrompt: "You are a helpful assistant. Keep your responses concise and relevant.",
	}
}

// func getDefaultConfig() Config {
// 	return Config{
// 		Provider:     "openai", // Ollama exposes an OpenAI-compatible endpoint
// 		Model:        "qwen2.5:0.5b",
// 		BaseURL:      "http://127.0.0.1:11434/v1",
// 		ApiKeyName:   "", // no auth needed for a local Ollama server
// 		SystemPrompt: "You are a helpful assistant. Keep your responses concise and relevant.",
// 	}
// }

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

func createNewChatHistory(maxContextWindow int, windowStrategy string) (ContextWindow, error) {
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

// prepareChatRequest prepares the chat messages to be sent to the provider by combining the system message, the chat history, and the user's message into a single slice of ChatMessage.
func prepareChatRequest(chatHistory ContextWindow, systemMsg, userMsg ChatMessage) []ChatMessage {
	msgs := chatHistory.GetMessages()
	chat2send := make([]ChatMessage, len(msgs)+2)
	chat2send[0] = systemMsg
	copy(chat2send[1:], msgs)
	chat2send[len(chat2send)-1] = userMsg
	return chat2send
}

func debugChatHistory(chatHistory ContextWindow) {
	fmt.Println("[debug] --- context window ---")
	chatMsgs := chatHistory.GetMessages()
	for i, msg := range chatMsgs {
		fmt.Printf("[%d] %s: %s\n", i, msg.Role, msg.Content)
	}
	fmt.Println("[debug] --- end ---")
}

func runLoop(ctx context.Context, provider Provider, systemPrompt string) {
	// maximum context window size
	const maxContextWindow = 10 // maximum number of messages to keep in the sliding context window

	// initialize a buffered read for user input from stdin
	stdin := bufio.NewReader(os.Stdin)
	// initialize a sliding context window for managing chat history efficiently
	chatHistory, err := createNewChatHistory(maxContextWindow, WindowStrategy)
	if err != nil {
		fatal(err)
	}

	// the system prompt should ALWAYS be the first message in the chat history. For chat agents, this would be an AGENTS.md, CLAUDE.md, etc.
	systemMsg := ChatMessage{
		Role:      "system",
		Content:   systemPrompt,
		ID:        createID(),
		Timestamp: time.Now().UTC(),
	}

	for {
		// read user input from stdin - /exit to quit
		fmt.Print("\n> ")
		line, err := stdin.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimSpace(line)
		lineFirst, lineRest, _ := strings.Cut(line, " ")
		// skip empty lines (user just pushes enter or just has spaces)
		if lineFirst == "" {
			continue
		}
		// parse slash commands
		if strings.HasPrefix(lineFirst, "/") {
			switch lineFirst {
			case "/exit":
				return
			case "/clear":
				chatHistory.Clear()
				fmt.Println("Chat history cleared.")
			case "/summary", "/summarize":
				summaryString, err := summarizeChatHistory(ctx, provider, chatHistory.GetMessages())
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					continue
				}
				fmt.Println("Chat summary:", summaryString)
			case "/compact":
				summaryMsg, err := compactChatHistory(ctx, provider, chatHistory)
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					continue
				}
				fmt.Println("Chat summary:", summaryMsg.Content)
			default:
				// if the command is not recognized, print an error message
				fmt.Println("Unrecognized command:", lineFirst)
				continue
			}
			// user may have entered a prompt following the slash command (e.g., /clear <A brand new prompt>)
			line = strings.TrimSpace(lineRest)
			if line == "" {
				continue
			}
		}

		// append the user message to the chat history
		userMsg := ChatMessage{
			Role:      "user",
			Content:   line,
			ID:        createID(),
			Timestamp: time.Now().UTC(),
		}

		// [debug] print the current chat history before sending the prompt to the chat provider
		debugChatHistory(chatHistory)

		// prepare the request to send to the chat provider - includes the system message, the chat history, and the user's message
		chat2send := prepareChatRequest(chatHistory, systemMsg, userMsg)

		// send the chat request to the LLM provider
		response, err := provider.Chat(ctx, chat2send)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}

		// get the response content from the chat provider
		fmt.Printf("Chat response: %+v\n", response)

		// append the user prompt and AI assistant's response to the chat history
		responseMsg := ChatMessage{
			Role:      "assistant",
			Content:   response,
			ID:        createID(),
			Timestamp: time.Now().UTC(),
		}

		chatHistory.AddMessages([]ChatMessage{userMsg, responseMsg})
	}
}

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

	// 2. setup memory and context (if applicable)
	// 3. setup tools and MCP servers (if applicable)

	// print the configuration for debugging purposes
	fmt.Printf(WelcomeMsg, cfg)

	// 4. chat with the LLM provider in a loop
	runLoop(ctx, provider, cfg.SystemPrompt)

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
