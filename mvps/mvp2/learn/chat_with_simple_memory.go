//
// This builds on chat_simple.go by adding simple memory functionality
// Memory is implemented as a running chat history slice that stores all previous messages between the user and the assistant.
//

package main

import (
	"bufio"
	"bytes"
	"context"
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
	Role    string `json:"role"` // `json:"role"` is a tag indicating the JSON key for this field
	Content string `json:"content"`
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
	// initialize a buffered read for user input from stdin
	stdin := bufio.NewReader(os.Stdin)
	// initialize a slice to store the chat history (the last message is the most recent one)
	var chatHistory []ChatMessage
	// append the system prompt as the first message in the chat history
	chatHistory = append(chatHistory, ChatMessage{
		Role:    "system",
		Content: systemPrompt,
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
				Role:    "system",
				Content: systemPrompt,
			})
			fmt.Println("Chat history cleared.")

			line = strings.TrimSpace(line[6:])
			if line == "" {
				continue
			}
		}

		// append the user message to the chat history
		chatHistory = append(chatHistory, ChatMessage{
			Role:    "user",
			Content: line,
		})

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
			Role:    "assistant",
			Content: response,
		})
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
