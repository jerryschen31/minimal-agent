package contextwindow

import (
	"container/list"
	"fmt"
	"sync"

	"github.com/jerryschen31/minimal-agent/model"
)

// ****************************************
// ContextWindow
// ****************************************

// ContextWindow defines the interface for managing the chat history within the context window, allowing different strategies for handling the chat history.
type ContextWindow interface {
	AddMessages(msgs []model.ChatMessage)
	GetMessages() []model.ChatMessage
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

//*********************************************************//
// struct types that implement the ContextWindow interface
//*********************************************************//

// OffsetWindow is a context window strategy that stores messages as a slice and just shifts the window,
// making the oldest messages at the beginning of the slice unreachable, when the maximum size is exceeded.
type OffsetWindow struct {
	mu       sync.Mutex
	messages []model.ChatMessage
	maxSize  int // maximum number of messages to retain in the context window
}

func NewOffsetWindow(contextWindowSize int) *OffsetWindow {
	return &OffsetWindow{
		messages: make([]model.ChatMessage, 0, contextWindowSize),
		maxSize:  contextWindowSize,
	}
}

// adds one or more messages to the context window
func (w *OffsetWindow) AddMessages(msgs []model.ChatMessage) {
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
func (w *OffsetWindow) GetMessages() []model.ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	// this appends to a nil slice, which results in a new slice (copy) being allocated - this is what we want so we don't share the underlying array (which may get modified after GetMessages() returns, resulting in unexpected behavior if we didn't make a copy)
	// [agent] Go 1.21 the standard library has slices.Clone(w.messages), which does the same thing and says what it does. It's what I'd reach for if your go.mod allows it. slices.Clone also keeps a nil input as nil.
	// Note that Messages: w.messages would share the underlying array, so we make a copy to avoid external modifications.
	newMessages := append([]model.ChatMessage(nil), w.messages...)
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
	messages []model.ChatMessage
	maxSize  int
}

func NewInPlaceWindow(contextWindowSize int) *InPlaceWindow {
	return &InPlaceWindow{
		messages: make([]model.ChatMessage, 0, contextWindowSize),
		maxSize:  contextWindowSize,
	}
}

func (w *InPlaceWindow) AddMessages(msgs []model.ChatMessage) {
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

func (w *InPlaceWindow) GetMessages() []model.ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	newMessages := append([]model.ChatMessage(nil), w.messages...)
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
	messages []model.ChatMessage // fixed size backing array (size = maxSize)
	maxSize  int
	head     int // index where the next message should be stored
	count    int // number of messages currently in the buffer
}

func NewRingBufferWindow(contextWindowSize int) *RingBufferWindow {
	return &RingBufferWindow{
		messages: make([]model.ChatMessage, contextWindowSize, contextWindowSize),
		maxSize:  contextWindowSize,
		head:     0,
		count:    0,
	}
}

func (w *RingBufferWindow) AddMessages(msgs []model.ChatMessage) {
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

func (w *RingBufferWindow) GetMessages() []model.ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	// with a ring buffer we need to explicitly create a new slice and copy the messages in the correct order, because the underlying array may wrap around.
	newMessages := make([]model.ChatMessage, w.count)
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
		w.messages[i] = model.ChatMessage{}
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

func (w *LLWindow) AddMessages(msgs []model.ChatMessage) {
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

func (w *LLWindow) GetMessages() []model.ChatMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	messages := make([]model.ChatMessage, w.messages.Len())
	// traverse the linked list from front to back and copy the messages into the slice
	i := 0
	for e := w.messages.Front(); e != nil; e = e.Next() {
		// LL node values are type Any (i.e., interface{}) and this instance holds ChatMessage values, so we need to type assert it to ChatMessage before copying it into the slice
		messages[i] = e.Value.(model.ChatMessage)
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
