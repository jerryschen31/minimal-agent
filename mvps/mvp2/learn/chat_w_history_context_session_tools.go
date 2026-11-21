//
// This builds on chat_w_history_context_session_structs.go by adding support for built-in tool calls
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const ResponseTimeout = 5 * time.Minute
const WelcomeMsg = "Thanks for using minimal agent!\nType /clear to clear the conversation.\nType /exit to exit\nType /compact to compact the conversation.\nType /config to see the current agent configuration.\n"
const WindowStrategy = "offset" // default context window strategy: "offset", "in-place", "ring-buffer", "linked-list"
const SummarizeSystemPrompt = "You are a helpful assistant that summarizes chat history. Summarize the key conversational points and important details concisely."
const MaxContextWindow = 10          // maximum number of messages to keep in the sliding context window
const AutoCompactThresholdFrac = 0.9 // fraction threshold of the context window at which automatic compaction is triggered
const MaxReActSteps = 10             // maximum number of steps in a single ReAct loop - prevents infinite reasoning cycles when model gets stuck

// ErrMaxSteps is returned (wrapped) by reActLoop when the model is still asking for tools after MaxReActSteps steps.
// A "sentinel error": one shared error value that callers recognize with errors.Is(err, ErrMaxSteps)
var ErrMaxSteps = errors.New("max steps for ReAct loop exceeded")

//////////////////////////////////////////////
// Interfaces and structs
//////////////////////////////////////////////

type Provider interface {
	Chat(ctx context.Context, chatHistory []ChatMessage, tools []ToolDef) (ChatMessage, error)
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
	InBuffer  io.Reader
	OutBuffer io.Writer

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

func printConfig(cfg Config) {
	fmt.Fprintf(cfg.OutBuffer, "Current agent configuration: %+v\n", cfg)
}

type ChatMessage struct {
	ID         string     `json:"id"`
	Role       string     `json:"role"` // `json:"role"` is a tag indicating the JSON key for this field
	Content    string     `json:"content"`
	Timestamp  time.Time  `json:"timestamp"`
	Type       string     `json:"type,omitempty"`         // flexible type field that indicates what kind of message this is (e.g., "user", "system", "summary")
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // records any tool invocations associated with this message
	ToolCallID string     `json:"tool_call_id,omitempty"` // ID of the associated tool call, if any
}

// shape of a tool definition that is sent as context to the LLM - this shape matches what is expected by most models (OpenAI chat completions standard)
type ToolDefFunc struct {
	Name   string          `json:"name"`
	Desc   string          `json:"description"`
	Params json.RawMessage `json:"parameters"`
	Strict bool            `json:"strict,omitempty"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolDefFunc `json:"function"`
}

// shape of the LLM response when it asks for a tool call - this shape matches what is returned by most models
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

// chat request and response structs
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ToolDef     `json:"tools,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
}

// Tool interface - Tool is any tool function the model can call (built-in tools for now, an MCP tool later)
// Note that when getting a tools list from an MCP server, the tools will be instantiated as many Tool values, one per listed tool.
type Tool interface {
	GetToolDefinition() ToolDef
	CallTool(ctx context.Context, args json.RawMessage) (string, error)
}

// instantiates a new tool definition instance
func NewToolDef(name string, description string, params json.RawMessage) ToolDef {
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return ToolDef{
		Type:     "function",
		Function: ToolDefFunc{Name: name, Desc: description, Params: params},
	}
}

// tool registry - keeps track of all registered tools that the model can call
type ToolRegistry struct {
	mu       sync.RWMutex         // allows concurrent reads with RLock() - good for frequent-read, rare-write structs (like a tool registry)
	tools    map[string]toolEntry // maps tool names to their corresponding tool entries for O(1) access
	tooldefs []ToolDef            // ordered list of registered tool definitions - this is what is sent to the LLM as context - this is better for caching purposes on the model end (order is same each time)
}

// an entry in the tool registry
type toolEntry struct {
	tool Tool
	def  ToolDef // captured once, at Register time
}

func NewToolRegistry(tools []Tool) (*ToolRegistry, error) {
	// initialize an empty tool registry
	reg := &ToolRegistry{
		tools:    make(map[string]toolEntry),
		tooldefs: []ToolDef{},
	}
	// then register each tool
	for _, tool := range tools {
		if err := reg.Register(tool); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// isNilTool reports whether t is nil, including the "typed nil" case.
// [agent] An interface value is a (type, value) pair. `var p *MyTool; var t Tool = p` gives t the
// type *MyTool and the value nil, so `t == nil` is false even though calling a method that
// dereferences the receiver would panic. reflect is the only way to look inside the pair.
// Trade-off: reflect is slower and less obvious than a plain nil check, but this runs only at
// registration (rare), never per request. Downside: a tool that deliberately supports a nil
// receiver would be rejected too; nothing here does.
// func isNilTool(t Tool) bool {
// 	if t == nil {
// 		return true
// 	}
// 	v := reflect.ValueOf(t)
// 	switch v.Kind() {
// 	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
// 		return v.IsNil()
// 	}
// 	return false
// }

// Adds a new tool to the tool registry. It returns an error if the tool name is empty or if a tool with the same name is already registered.
func (r *ToolRegistry) Register(t Tool) error {
	// if passed Tool is nil, error out
	// [agent] This catches only a nil interface; a typed nil pointer (e.g. (*MyTool)(nil)) passes and would panic below. Deliberately not handled: nothing here creates one.
	if t == nil {
		return fmt.Errorf("cannot register a nil tool")
	}
	toolDef := t.GetToolDefinition()
	toolName := toolDef.Function.Name
	if toolName == "" {
		return fmt.Errorf("tool name cannot be empty")
	}
	// tools that match an existing tool name do NOT overwrite - Lock() is called on writes
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[toolName]; exists {
		return fmt.Errorf("tool with name %q already registered", toolName)
	}
	r.tools[toolName] = toolEntry{
		tool: t,
		def:  toolDef,
	}
	r.rebuildToolDefList()
	return nil
}

// assuming that tool map exists, builds / rebuilds the ordered (sorted) list of tool definitions for the LLM context
// since this modifies the registry, ANY method that calls this MUST already hold the write lock (Lock) on the registry.
func (r *ToolRegistry) rebuildToolDefList() {
	r.tooldefs = make([]ToolDef, 0, len(r.tools))
	for _, toolEntry := range r.tools {
		r.tooldefs = append(r.tooldefs, toolEntry.def)
	}
	// sort the tool definitions by name to ensure consistent order
	sort.Slice(r.tooldefs, func(i, j int) bool {
		return r.tooldefs[i].Function.Name < r.tooldefs[j].Function.Name
	})
}

// Deletes a tool from the registry by name. It returns true if the tool was found and removed, false otherwise.
func (r *ToolRegistry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	r.rebuildToolDefList()
	return true
}

// Looks up a tool by name. It returns the tool and true if found, or nil and false otherwise.
func (r *ToolRegistry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t.tool, ok
}

// Returns the cached tool definitions, sorted by name.
// Callers must not modify the returned slice.
func (r *ToolRegistry) GetToolDefs() []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tooldefs
}

// In the agent kernel, we define a built-in tool that can read contents of text files, capped at ReadFileMaxBytes.
const ReadFileMaxBytes = 64 * 1024

type ReadFileTool struct{}

func (ReadFileTool) GetToolDefinition() ToolDef {
	return NewToolDef(
		"read_file",
		"Reads the contents of a UTF-8 text file. Files larger than 64 KB are truncated, with a note at the end.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "The path to the file to read"
				}
			},
			"required": ["path"]
		}`),
	)
}

func (ReadFileTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	// decode args into a small struct with a Path field; bad JSON returns an error. Empty path also returns an error
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("missing required argument: path")
	}

	f, err := os.Open(params.Path)
	if err != nil {
		return "", err // e.g. "open go.mod: no such file or directory" (clear enough for the model)
	}
	defer f.Close()

	// LimitReader is used to cap how much of the file is read into memory, preventing huge files from being fully loaded.
	// Reading one byte past the cap is how we can tell that the file was cut off with the comparison below.
	// A directory opens fine but fails here with "is a directory".
	data, err := io.ReadAll(io.LimitReader(f, ReadFileMaxBytes+1))
	if err != nil {
		return "", err
	}
	// a NUL byte means binary; returning it would just be noise to the model
	if bytes.IndexByte(data, 0) != -1 {
		return "", fmt.Errorf("%s looks like a binary file, not text", params.Path)
	}
	if len(data) > ReadFileMaxBytes {
		// add a note clearly indicating the file output was truncated since the file exceeded the maximum allowed size
		return string(data[:ReadFileMaxBytes]) + "\n[truncated: file is larger than 64 KB]", nil
	}
	return string(data), nil
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
	Config     Config
	Provider   Provider
	MsgHistory ChatHistory
	MsgContext *ChatContext
	Tools      *ToolRegistry
	SystemMsg  ChatMessage
	UserID     string
	InBuffer   io.Reader
	OutBuffer  io.Writer
}

func NewChatSession(provider Provider, chatHistory ChatHistory, chatContext *ChatContext, tools *ToolRegistry, cfg Config) (*ChatSession, error) {
	return &ChatSession{
		Config:     cfg,
		Provider:   provider,
		MsgHistory: chatHistory,
		MsgContext: chatContext,
		Tools:      tools,
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
	compactWG            sync.WaitGroup // Used for triggering some code that waits for compaction to complete (e.g., for graceful shutdown)
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

// WaitForCompaction blocks until any in-flight background compaction finishes. Safe to call
// even when none is running — Wait() on a zero counter returns immediately, no blocking.
// This is useful for code that needs to ensure all background compaction has completed before proceeding, such as during graceful shutdown.
func (c *ChatContext) WaitForCompaction() {
	c.compactWG.Wait()
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
func setupChatSession(provider Provider, chatHistory ChatHistory, chatContext *ChatContext, tools *ToolRegistry, cfg Config) (*ChatSession, error) {
	cs, err := NewChatSession(provider, chatHistory, chatContext, tools, cfg)
	if err != nil {
		return nil, err
	}
	return cs, nil
}

// creates a tool registry
func setupToolRegistry(cfg Config) (*ToolRegistry, error) {
	tools := []Tool{}
	// first add the built-in tools (ReadFile Tool to start)
	tools = append(tools, ReadFileTool{})

	// later we will add MCP tools specified in the config file

	// create a registry from the tools list
	reg, err := NewToolRegistry(tools)
	if err != nil {
		return nil, err
	}
	return reg, nil
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
func summarizeChatContext(ctx context.Context, p Provider, msgs []ChatMessage, addlInstructions string) (string, error) {
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
	if addlInstructions != "" {
		req = append(req, ChatMessage{Role: "system", Content: addlInstructions})
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

// compacts the chat context by summarizing the current messages and replacing them with a single system message containing the summary.
// in the future, the compaction could be more sophisticated (compacting a subset of messages, merging similar messages, having categories for messages, etc.)
// compactionType values for compactChatContext, distinguishing a manual /compact from an
// auto-triggered one (only affects the "triggered..." message printed to the user).
const (
	CompactionManual = "manual"
	CompactionAuto   = "auto"
)

// addlInstructions provide additional information for the summarization process, guiding how the chat context should be summarized.
func compactChatContext(ctx context.Context, cs *ChatSession, compactionType string, addlInstructions string) (ChatMessage, error) {
	defer cs.MsgContext.EndCompaction() // ensure the compaction lock is released after compaction is done
	if compactionType == CompactionAuto {
		fmt.Fprintln(cs.OutBuffer, "[system] Auto-compaction triggered...")
	} else {
		fmt.Fprintln(cs.OutBuffer, "[system] Compaction triggered...")
	}

	// take a snapshot of the current chat history
	state := cs.MsgContext.Snapshot()

	// summarize the chat history
	summary, err := summarizeChatContext(ctx, cs.Provider, state.Messages, addlInstructions)
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
		fmt.Fprintln(cs.OutBuffer, "[system] Chat context compacted successfully.")
		return summaryMsg, nil
	} else {
		return ChatMessage{}, fmt.Errorf("[system] Chat context compaction failed")
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

func (p OpenAICompat) Chat(ctx context.Context, chatHistory []ChatMessage, tools []ToolDef) (ChatMessage, error) {
	// make a request to the OpenAI-compatible server
	// 1. build a chatRequest with p.Model and the provided chat history
	reqRaw := ChatRequest{
		Model:    p.Model,
		Messages: chatHistory,
		Tools:    tools, // omitempty: nil tools send no "tools" key at all
	}

	// 2. json.Marshal it into a []byte body
	reqBody, err := json.Marshal(reqRaw)
	if err != nil {
		return ChatMessage{}, err
	}

	// 3. build an *http.Request with http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", ...)
	ctx, cancel := context.WithTimeout(ctx, ResponseTimeout)
	defer cancel()
	reqHttp, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return ChatMessage{}, err
	}

	// 4. set header "Content-Type": "application/json"
	reqHttp.Header.Set("Content-Type", "application/json")
	if p.ApiKey != "" {
		reqHttp.Header.Set("Authorization", "Bearer "+p.ApiKey)
	}

	// 5. do the request with a http.DefaultClient.Do(req) and TIMEOUT
	resp, err := http.DefaultClient.Do(reqHttp)
	if err != nil {
		return ChatMessage{}, err
	}
	defer resp.Body.Close()

	// 5b. Check if the HTTP response status code indicates an error
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatMessage{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// 6. read the response body, json.Unmarshal into a chatResponse
	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return ChatMessage{}, err
	}

	// 7. return resp.Choices[0].Message.Content, nil
	if len(chatResp.Choices) == 0 {
		return ChatMessage{}, fmt.Errorf("no choices in response")
	}
	return chatResp.Choices[0].Message, nil
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

// prepareChatRequest prepares the chat messages to be sent to the provider by combining the system message, the context, and the messages this turn into a single slice of ChatMessage.
// [note2agent] turnMsgs []ChatMessage breaks old tests on this
func prepareChatRequest(msgContext []ChatMessage, systemMsg ChatMessage, turnMsgs []ChatMessage) []ChatMessage {
	chat2send := make([]ChatMessage, len(msgContext)+len(turnMsgs)+1)
	chat2send[0] = systemMsg
	copy(chat2send[1:], msgContext)
	copy(chat2send[len(msgContext)+1:], turnMsgs)
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
			fmt.Fprintln(cs.OutBuffer, "[system] Chat history cleared")
		case "/summary", "/summarize":
			summaryString, err := summarizeChatContext(ctx, cs.Provider, msgContext, strings.TrimSpace(lineRest))
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "[error] Summarization error:", err)
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Chat summary:", summaryString)
		case "/compact":
			// we need to check if a compaction is already happening so we don't trigger a second compaction concurrently
			if !cs.MsgContext.ShouldStartCompaction() {
				fmt.Fprintln(cs.OutBuffer, "[system] Compaction already in progress. Skipping this compaction request.")
				return false
			}
			summaryMsg, err := compactChatContext(ctx, cs, CompactionManual, strings.TrimSpace(lineRest))
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "[error] Compaction error:", err)
				return false
			}
			fmt.Fprintln(cs.OutBuffer, "[system] Chat summary:", summaryMsg.Content)
		case "/config":
			printConfig(cs.Config)
		default:
			// if the command is not recognized, print an error message
			fmt.Fprintln(cs.OutBuffer, "[system] Unrecognized command:", lineFirst)
		}
		return false
		// user may have entered a prompt following the slash command (e.g., /clear <A brand new prompt>)
		// line = strings.TrimSpace(lineRest)
		// if line == "" {
		// 	return false
		// }
	}

	// [debug] print the current chat history before sending the prompt to the chat provider
	debugChatContext(msgContext)

	// initial user message
	userMsg := createChatMessage("user", "user", line, "", nil)

	// maybe consider passing a pointer to msgContext in future - to save on a full-copy of the context to reActLoop()
	turnMsgs, err := reActLoop(ctx, cs, msgContext, userMsg)
	if err != nil {
		fmt.Fprintln(cs.OutBuffer, "[error] ReAct loop error:", err)
		return false
	}

	// add new messages to history and context
	cs.MsgHistory.Append(turnMsgs)
	cs.MsgContext.AddMessages(turnMsgs)

	// check if the chat history has reached the auto-compaction threshold and a compaction is not already in progress - if so, trigger auto-compaction in a separate goroutine
	if cs.MsgContext.IsAutoCompactionNeeded() && cs.MsgContext.ShouldStartCompaction() {
		cs.MsgContext.compactWG.Add(1) // must be here synchronously BEFORE we enter the goroutine
		go func() {
			defer cs.MsgContext.compactWG.Done() // ensure the WaitGroup counter is decremented when the goroutine finishes
			_, err := compactChatContext(ctx, cs, CompactionAuto, "")
			if err != nil {
				fmt.Fprintln(cs.OutBuffer, "[error] Auto-compaction error:", err)
			}
		}()
	}
	return false
}

func reActLoop(ctx context.Context, cs *ChatSession, priorMsgs []ChatMessage, initialMsg ChatMessage) ([]ChatMessage, error) {
	// implementation of the ReAct loop goes here
	// // [PLAN] ReAct Loop
	// loop up to MaxSteps:
	// reply := Chat(msgs, cs.Tools.GetToolDefs())
	// append reply to the turn's messages
	// if len(reply.ToolCalls) == 0 → done
	// for each call in reply.ToolCalls:
	//     append runToolCall(ctx, cs, call)

	requestMsgs := []ChatMessage{initialMsg}
	toolDefs := cs.Tools.GetToolDefs()
	for stepNum := 0; stepNum < MaxReActSteps; stepNum++ {
		// if user cancels the operation (maybe the ReAct loop is taking too long), exit the loop immediately
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// prepare the request to send to the chat provider - includes the system message, the context, the request message, and tool definition info
		request2send := prepareChatRequest(priorMsgs, cs.SystemMsg, requestMsgs)

		// send the chat request to the LLM provider
		response, err := cs.Provider.Chat(ctx, request2send, toolDefs)
		if err != nil {
			fmt.Fprintln(cs.OutBuffer, "error:", err)
			return nil, err
		}

		// append the response as a ChatMessage to the request messages for the next iteration
		fmt.Fprintf(cs.OutBuffer, "[assistant] %s\n", response)
		responseMsg := createChatMessage("assistant", "assistant", response.Content, "", response.ToolCalls)
		requestMsgs = append(requestMsgs, responseMsg)

		// if there are no tool calls in the response, we are done
		if len(response.ToolCalls) == 0 {
			return requestMsgs, nil
		}
		// otherwise there are tool calls to be made
		for _, tc := range response.ToolCalls {
			fmt.Fprintln(cs.OutBuffer, "[tool] Tool called:", tc.Function.Name)
			toolResultMsg := runToolCall(ctx, cs, tc)
			requestMsgs = append(requestMsgs, toolResultMsg)
		}
	}

	return nil, fmt.Errorf("%w (limit %d)", ErrMaxSteps, MaxReActSteps)
}

// ***********************************************************//
// Run loop for handling chat session
// ***********************************************************//
func gracefulShutdown(cancel context.CancelFunc, chatSession *ChatSession) {
	// signal the context to stop any ongoing operations
	cancel()
	// wait for any in-flight background compaction to finish
	chatSession.MsgContext.WaitForCompaction()
}

func createChatMessage(role string, msgType string, content string, toolCallID string, toolCalls []ToolCall) ChatMessage {
	return ChatMessage{
		Role:       role,
		ToolCallID: toolCallID,
		Content:    content,
		ID:         createID(),
		Timestamp:  time.Now().UTC(),
		Type:       msgType,
		ToolCalls:  toolCalls,
	}
}

// callToolSafely runs tool.CallTool and recovers from a panic inside a tool (since we want model execution to continue on a failed tool)
func callToolSafely(ctx context.Context, tool Tool, args json.RawMessage) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return tool.CallTool(ctx, args)
}

// executes a single tool call and returns the result as a ChatMessage. In the next iteration of the ReAct loop, this chat message is sent back to the model
func runToolCall(ctx context.Context, cs *ChatSession, call ToolCall) ChatMessage {
	// 1. Look up call.Function.Name with cs.Tools.Lookup. If the tool is unknown, the result is an error message.
	tool, ok := cs.Tools.Lookup(call.Function.Name)
	if !ok {
		return createChatMessage("tool", "tool", fmt.Sprintf("error: unknown tool %q", call.Function.Name), call.ID, nil)
	}
	// 2. Check that call.Function.Arguments is a JSON object. Arguments is a string holding JSON. If it isn't an object, the result is an error message. Empty "" for no-arg tools.
	if call.Function.Arguments != "" {
		var argsMap map[string]interface{}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &argsMap); err != nil {
			return createChatMessage("tool", "tool", fmt.Sprintf("error: invalid JSON arguments: %v", err), call.ID, nil)
		}
		if argsMap == nil {
			return createChatMessage("tool", "tool", "error: arguments must be a JSON object", call.ID, nil)
		}
	}

	// 3. Run tool.CallTool(ctx, json.RawMessage(args)). If it returns an error, put "error: ..." in Content.
	args := json.RawMessage(call.Function.Arguments)
	// if call.Function.Arguments is empty, json.RawMessage returns an empty byte slice - create a proper empty JSON string in this case.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := callToolSafely(ctx, tool, args)
	if err != nil {
		return createChatMessage("tool", "tool", fmt.Sprintf("error: %v", err), call.ID, nil)
	}

	return createChatMessage("tool", "tool", result, call.ID, nil)
}

func runLoop(ctx context.Context, chatSession *ChatSession) {
	ctx, cancel := context.WithCancel(ctx)
	defer gracefulShutdown(cancel, chatSession)

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

	// 4. setup tools (if applicable)
	tools, err := setupToolRegistry(cfg)
	if err != nil {
		return err
	}

	// 5. setup this chat session
	chatSession, err := setupChatSession(provider, chatHistory, chatContext, tools, cfg)
	if err != nil {
		return err
	}

	// print a welcome message
	fmt.Fprintf(cfg.OutBuffer, WelcomeMsg)
	fmt.Fprintf(cfg.OutBuffer, "Using model %s\n", cfg.Model)

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
