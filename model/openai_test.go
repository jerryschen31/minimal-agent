package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// - Verify that an error in the chat request object errors gracefull
func Test_ChatRequest_ErrorsGracefully(t *testing.T) {
	// server is closed before use, so any request against its URL fails at the transport
	// level (connection refused) — deterministic, unlike guessing an unused port.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("handler should never be reached: server was closed before the request")
	}))
	srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err == nil {
		t.Fatalf("expected an error when the provider is unreachable, got reply %q", reply.Content)
	}
}

// - Verify a non-2xx response code errors gracefully
func Test_ChatRequest_Non2xxResponseErrorsGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err == nil {
		t.Fatalf("expected an error for a non-2xx response, got reply %q", reply.Content)
	}
}

// - Verify an empty response errors gracefully
func Test_ChatRequest_EmptyResponseErrorsGracefully(t *testing.T) {
	// Two distinct ways a response can be "empty" — each exercises a different failure
	// path in Chat(), so both are worth covering rather than picking one.
	t.Run("no body at all", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// write nothing: json.Decode on the other end hits EOF
		}))
		defer srv.Close()

		p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

		if err == nil {
			t.Fatalf("expected an error for an empty response body, got reply %q", reply.Content)
		}
	})

	t.Run("valid JSON but no choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResponse{Choices: nil})
		}))
		defer srv.Close()

		p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

		if err == nil {
			t.Fatalf("expected an error for a response with no choices, got reply %q", reply.Content)
		}
	})
}

// - Verify a proper request-response cycle that returns a 2xx response is successful
func Test_ChatRequest_ProperRequestResponseCycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected a POST request, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected path %q, got %q", "/chat/completions", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{
			Choices: []struct {
				Message ChatMessage `json:"message"`
			}{
				{Message: ChatMessage{Role: "assistant", Content: "hi there"}},
			},
		})
	}))
	defer srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err != nil {
		t.Fatalf("Chat() returned an unexpected error: %v", err)
	}
	if reply.Content != "hi there" {
		t.Errorf("expected reply %q, got %q", "hi there", reply.Content)
	}
}

// - Verify the request sent to the provider carries only the API's message fields: the agent's own
// ID, Timestamp and Type are never put on the wire, while role, content, tool_calls and tool_call_id are
func Test_ChatRequest_OnlyAPIFieldsSentOnMessages(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{Choices: []struct {
			Message ChatMessage `json:"message"`
		}{{Message: ChatMessage{Role: "assistant", Content: "ok"}}}})
	}))
	defer srv.Close()

	history := []ChatMessage{
		{ID: "id-1", Timestamp: time.Now(), Type: "user", Role: "user", Content: "hi"},
		{ID: "id-2", Timestamp: time.Now(), Type: "assistant", Role: "assistant",
			ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: ToolCallFunc{Name: "read_file", Arguments: `{"path":"x"}`}}}},
		{ID: "id-3", Timestamp: time.Now(), Type: "tool", Role: "tool", Content: "file text", ToolCallID: "call-1"},
	}
	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	if _, err := p.Chat(context.Background(), history, nil); err != nil {
		t.Fatalf("Chat() returned an error: %v", err)
	}

	var sent struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body is not valid JSON: %v\n%s", err, body)
	}
	if len(sent.Messages) != len(history) {
		t.Fatalf("expected %d messages on the wire, got %d", len(history), len(sent.Messages))
	}
	allowed := map[string]bool{"role": true, "content": true, "tool_calls": true, "tool_call_id": true}
	for i, m := range sent.Messages {
		for key := range m {
			if !allowed[key] {
				t.Errorf("message %d: field %q must not be sent to the provider", i, key)
			}
		}
	}
	// the fields the API needs survive
	if _, ok := sent.Messages[1]["tool_calls"]; !ok {
		t.Errorf("expected tool_calls on the assistant message, got %v", sent.Messages[1])
	}
	if got := string(sent.Messages[2]["tool_call_id"]); got != `"call-1"` {
		t.Errorf("expected tool_call_id %q on the tool message, got %s", "call-1", got)
	}
}

// - Verify a provider response still decodes into a ChatMessage (role, content, tool_calls) and
// ignores any extra keys the provider includes, such as an "id"
func Test_ChatRequest_ResponseDecodesAPIFieldsAndIgnoresExtras(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"id":"provider-id","role":"assistant","content":"hello","refusal":null,
			"tool_calls":[{"id":"call-9","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`)
	}))
	defer srv.Close()

	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)

	if err != nil {
		t.Fatalf("Chat() returned an error: %v", err)
	}
	if reply.Role != "assistant" || reply.Content != "hello" {
		t.Errorf("expected assistant %q, got %+v", "hello", reply)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ID != "call-9" || reply.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("expected the read_file tool call call-9 to decode, got %+v", reply.ToolCalls)
	}
	if reply.ID != "" {
		t.Errorf("expected the provider's message id not to populate our internal ID, got %q", reply.ID)
	}
}
