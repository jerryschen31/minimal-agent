package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
