//
// This builds on chat_with_simple_memory.go by adding a sliding context window memory mechanism for the chat history.
// We also future-proof our ChatMessage by adding an ID and Timestamp and a few other updates
// Note that this naive implementation completely loses older messages once the sliding window moves forward.
//
// Future improvements could include persisting older messages to disk or a database,
// and/or implementing a more sophisticated memory management strategy like summarization.
//

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const ResponseTimeout = 30 * time.Second
const WelcomeMsg = "Running agent with configuration: %+v\n\nType /clear to clear chat history.\nType /exit to exit\n"

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

func getContextWindow(chatHistory []ChatMessage, maxContextWindow int, windowStrategy string) []ChatMessage {
	switch windowStrategy {
	case "offset":
		// if the chat history exceeds a certain length, remove the oldest messages to maintain a sliding window
		// Note that the chopped off message elements are still in memory within the backing chatHistory array,
		// but we cannot access them anymore since we've effectively advanced the pointer to the first element of the sliding window.
		// This will eventually get garbage-collected if the chatHistory grows beyond the capacity of this current backing array and a new array at a separate memory location is allocated for chatHistory,
		// but the memory inefficiency / leakage here is something to keep in mind. Perhaps there's a better data structure for implementing a sophisticated sliding window efficiently.
		if len(chatHistory) > maxContextWindow {
			chatHistory = chatHistory[len(chatHistory)-maxContextWindow:]
		}
		return chatHistory
	case "in-place":
		// in-place copy feels better for SMALL context windows (maybe <10k) since it keeps the slice that is the context window in the same backing array, avoiding unnecessary allocations.
		if len(chatHistory) > maxContextWindow {
			num2drop := len(chatHistory) - maxContextWindow
			n := copy(chatHistory, chatHistory[num2drop:]) // copy(dst, src) returns the number of elements copied into the destination slice
			chatHistory = chatHistory[:n]
			// index:        0  1  2  3  4  5  6  7  8  9  10 11
			// chatHistory: [a, b, c, d, e, f, g, h, i, j, k, l]
			// src (drop=2):       [c, d, e, f, g, h, i, j, k, l]   (chatHistory[2:], len=10)
			// copy(chatHistory, src) copies min(12, 10) = 10 elements: src[0] (c) into chatHistory[0], src[1] (d) into chatHistory[1], and so on. Since src and dst overlap (same backing array, src is just offset by 2), this is exactly the scenario memmove exists for — and Go's copy is specified to handle overlapping slices correctly regardless of which direction you're shifting, so there's no risk of corrupting data mid-copy the way a naive byte-by-byte loop in the wrong direction could.
			// After the copy, the array looks like:
			// index:        0  1  2  3  4  5  6  7  8  9  10 11
			// chatHistory: [c, d, e, f, g, h, i, j, k, l, k, l]
			//                                            ^^^^ stale leftover data, but unreachable
		}
		return chatHistory
	default:
		return chatHistory
	}
}

func getDefaultConfig() Config {
	return Config{
		Provider:     "openai", // Ollama exposes an OpenAI-compatible endpoint
		Model:        "qwen2.5:0.5b",
		BaseURL:      "http://127.0.0.1:11434/v1",
		ApiKeyName:   "", // no auth needed for a local Ollama server
		SystemPrompt: "You are a helpful assistant. Keep your responses concise and relevant.",
	}
}

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

func runLoop(ctx context.Context, provider Provider, systemPrompt string) {
	// maximum context window size
	const maxContextWindow = 10 // maximum number of messages to keep in the sliding context window

	// initialize a buffered read for user input from stdin
	stdin := bufio.NewReader(os.Stdin)
	// initialize a slice to store the chat history (the last message is the most recent one)
	var chatHistory []ChatMessage
	// append the system prompt as the first message in the chat history
	chatHistory = append(chatHistory, ChatMessage{
		Role:      "system",
		Content:   systemPrompt,
		ID:        createID(),
		Timestamp: time.Now().UTC(),
	})

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
			chatHistory = chatHistory[:0]
			chatHistory = append(chatHistory, ChatMessage{
				Role:      "system",
				Content:   systemPrompt,
				ID:        createID(),
				Timestamp: time.Now().UTC(),
			})
			fmt.Println("Chat history cleared.")

			line = strings.TrimSpace(line[6:])
			if line == "" {
				continue
			}
		}

		// append the user message to the chat history
		chatHistory = append(chatHistory, ChatMessage{
			Role:      "user",
			Content:   line,
			ID:        createID(),
			Timestamp: time.Now().UTC(),
		})

		// [debug] print the current chat history before sending the prompt to the chat provider
		fmt.Println("[debug] --- context window ---")
		for i, msg := range chatHistory {
			fmt.Printf("[%d] %s: %s\n", i, msg.Role, msg.Content)
		}
		fmt.Println("[debug] --- end ---")

		// send the prompt to the chat provider
		response, err := provider.Chat(ctx, chatHistory)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			// remove the last user message since there was an error (response not saved)
			chatHistory = chatHistory[:len(chatHistory)-1]
			continue
		}

		// get the response content from the chat provider
		fmt.Printf("Chat response: %+v\n", response)

		// append the assistant's response to the chat history
		chatHistory = append(chatHistory, ChatMessage{
			Role:      "assistant",
			Content:   response,
			ID:        createID(),
			Timestamp: time.Now().UTC(),
		})

		// maintain the sliding window by keeping only the most recent messages within the max context window
		chatHistory = getContextWindow(chatHistory, maxContextWindow, "offset")
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
