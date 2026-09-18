//
// This builds on chat_with_sliding_context_window_memory.go by flexibility with swapping different context window data structure management strategies
// 1. Sliding window strategy on a slice
// 2. "Slide back" strategy on a slice, essentially keeping the context window slice in-place
// 3. Ring-buffer strategy on a slice (where the slice functions to loop back around), allowing efficient use of a fixed-size buffer
// 4. A circular linked list data structure (using container/list in Go's native stdlib package) to manage the context window, allowing dynamic context window sizes and O(1) removal of old messages
//
// Again, future improvements could include persisting older messages to disk or a database,
// and/or implementing a more sophisticated memory management strategy like summarization.
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

const ResponseTimeout = 30 * time.Second
const WelcomeMsg = "Running agent with configuration: %+v\n\nType /clear to clear chat history.\nType /exit to exit\n"
const WindowStrategy = "offset" // default context window strategy: "offset", "in-place", "ring-buffer", "circular-linked-list"

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
}

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
		if err != nil || strings.TrimSpace(line) == "/exit" {
			return
		}
		// skip empty lines (user just pushes enter)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// user wants to clear the chat history
		if strings.HasPrefix(line, "/clear") {
			chatHistory, err = createNewChatHistory(maxContextWindow, WindowStrategy)
			if err != nil {
				fatal(err)
			}
			fmt.Println("Chat history cleared.")

			line = strings.TrimSpace(line[6:])
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
