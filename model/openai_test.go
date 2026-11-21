package model

import (
	"context"
	"encoding/json"
	"errors"
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
		t.Fatalf("expected an error when the provider is unreachable, got reply %q", reply.Message.Content)
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
		t.Fatalf("expected an error for a non-2xx response, got reply %q", reply.Message.Content)
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
			t.Fatalf("expected an error for an empty response body, got reply %q", reply.Message.Content)
		}
	})

	t.Run("valid JSON but no choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(openAIResponse{Choices: nil})
		}))
		defer srv.Close()

		p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}
		reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

		if err == nil {
			t.Fatalf("expected an error for a response with no choices, got reply %q", reply.Message.Content)
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
		json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{
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
	if reply.Message.Content != "hi there" {
		t.Errorf("expected reply %q, got %q", "hi there", reply.Message.Content)
	}
}

// - Verify the request sent to the provider carries only the API's message fields: the agent's own
// ID, Timestamp and Type are never put on the wire, while role, content, tool_calls and tool_call_id are
func Test_ChatRequest_OnlyAPIFieldsSentOnMessages(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openAIResponse{Choices: []openAIChoice{{Message: ChatMessage{Role: "assistant", Content: "ok"}}}})
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
	if reply.Message.Role != "assistant" || reply.Message.Content != "hello" {
		t.Errorf("expected assistant %q, got %+v", "hello", reply)
	}
	if len(reply.Message.ToolCalls) != 1 || reply.Message.ToolCalls[0].ID != "call-9" || reply.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("expected the read_file tool call call-9 to decode, got %+v", reply.Message.ToolCalls)
	}
	if reply.Message.ID != "" {
		t.Errorf("expected the provider's message id not to populate our internal ID, got %q", reply.Message.ID)
	}
}

// delayedReplyServer answers "hi there" after delay, or gives up early when the client goes away (so Close won't wait).
func delayedReplyServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // [agent] the server notices a client hang-up (r.Context done) only once the body is read
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{
				{Message: ChatMessage{Role: "assistant", Content: "hi there"}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// - Verify Chat gives up after Timeout: a server slower than the timeout makes it return a deadline error promptly
func Test_ChatRequest_Timeout_SlowServerFailsPromptly(t *testing.T) {
	srv := delayedReplyServer(t, 5*time.Second)
	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model", Timeout: 50 * time.Millisecond}

	start := time.Now()
	_, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a deadline exceeded error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("expected Chat to return soon after the 50ms timeout, took %v", elapsed)
	}
}

// - Verify a zero Timeout sets no limit of its own: a reply that takes a little while still arrives
func Test_ChatRequest_ZeroTimeout_NoLimit(t *testing.T) {
	srv := delayedReplyServer(t, 100*time.Millisecond)
	p := OpenAICompat{BaseURL: srv.URL, Model: "test-model"}

	reply, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hello"}}, nil)

	if err != nil {
		t.Fatalf("Chat() returned an unexpected error: %v", err)
	}
	if reply.Message.Content != "hi there" {
		t.Errorf("expected reply %q, got %q", "hi there", reply.Message.Content)
	}
}

// replyServer answers every request with body, verbatim.
func replyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// - Verify Chat reads the server's usage (cached tokens included) and finish_reason into the Response
func Test_ChatRequest_ReadsUsageAndFinishReason(t *testing.T) {
	srv := replyServer(t, `{"choices":[{"message":{"role":"assistant","content":"",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":1200,"completion_tokens":85,"total_tokens":1285,
		"prompt_tokens_details":{"cached_tokens":1024},"completion_tokens_details":{"reasoning_tokens":40}}}`)

	got, err := OpenAICompat{BaseURL: srv.URL, Model: "m"}.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat() returned an error: %v", err)
	}
	want := Usage{InputTokens: 1200, OutputTokens: 85, CacheReadTokens: 1024, Reported: true}
	if got.Usage != want {
		t.Errorf("expected usage %+v, got %+v", want, got.Usage)
	}
	if got.StopReason != StopToolCalls {
		t.Errorf("expected stop reason %q, got %q", StopToolCalls, got.StopReason)
	}
}

// - Verify a server that sends no usage (key missing or null) gives Reported false, and one without
// prompt_tokens_details still reports input and output with no cached tokens
func Test_ChatRequest_MissingUsage_NotReported(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{
		{"no usage key", `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, Usage{}},
		{"usage null", `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":null}`, Usage{}},
		{"no details", `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":30,"completion_tokens":5}}`, Usage{InputTokens: 30, OutputTokens: 5, Reported: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := replyServer(t, c.body)
			got, err := OpenAICompat{BaseURL: srv.URL, Model: "m"}.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil)
			if err != nil {
				t.Fatalf("Chat() returned an error: %v", err)
			}
			if got.Usage != c.want {
				t.Errorf("expected usage %+v, got %+v", c.want, got.Usage)
			}
			if got.StopReason != StopEnd {
				t.Errorf("expected stop reason %q, got %q", StopEnd, got.StopReason)
			}
		})
	}
}

// - Verify finish_reason values map to the shared stop reasons, and unknown or missing ones pass through unchanged
func Test_Unit_OpenAIStopReason_Mapping(t *testing.T) {
	cases := map[string]string{
		"stop":           StopEnd,
		"tool_calls":     StopToolCalls,
		"length":         StopMaxTokens,
		"content_filter": "content_filter",
		"":               "",
	}
	for in, want := range cases {
		if got := openAIStopReason(in); got != want {
			t.Errorf("openAIStopReason(%q): expected %q, got %q", in, want, got)
		}
	}
}

// requestKeys sends one Chat with p (its BaseURL set to a fake server) and returns the top-level keys of the request body.
func requestKeys(t *testing.T, p OpenAICompat) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body does not decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	if _, err := p.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("Chat() returned an error: %v", err)
	}
	return body
}

// - Verify MaxTokens 0 sends neither limit key, and a positive MaxTokens is sent only under MaxTokensField
func Test_ChatRequest_MaxTokens_SentUnderOneKey(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		field     string
		wantKey   string // "" = neither key
	}{
		{"zero sends nothing", 0, "max_tokens", ""},
		{"max_tokens", 512, "max_tokens", "max_tokens"},
		{"max_completion_tokens", 512, "max_completion_tokens", "max_completion_tokens"},
		{"unset field falls back to max_tokens", 512, "", "max_tokens"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := requestKeys(t, OpenAICompat{Model: "m", MaxTokens: c.maxTokens, MaxTokensField: c.field})
			for _, key := range []string{"max_tokens", "max_completion_tokens"} {
				raw, present := body[key]
				switch {
				case key == c.wantKey && (!present || string(raw) != "512"):
					t.Errorf("expected %q: 512 in the request, got %q (present %v)", key, raw, present)
				case key != c.wantKey && present:
					t.Errorf("expected no %q in the request, got %s", key, raw)
				}
			}
		})
	}
}

// - Verify the host rule: OpenAI and Azure OpenAI get max_completion_tokens, every other host max_tokens, and a
// configured override wins over the host
func Test_Unit_MaxTokensFieldFor_ByHostAndOverride(t *testing.T) {
	cases := []struct {
		baseURL, override, want string
	}{
		{"https://api.openai.com/v1", "", "max_completion_tokens"},
		{"https://API.OpenAI.com/v1", "", "max_completion_tokens"},
		{"https://myres.openai.azure.com/openai/deployments/x", "", "max_completion_tokens"},
		{"http://localhost:11434/v1", "", "max_tokens"},
		{"https://openrouter.ai/api/v1", "", "max_tokens"},
		{"http://litellm.internal:4000", "", "max_tokens"},
		{"https://api.openai.com.evil.example/v1", "", "max_tokens"}, // only the real host, not a look-alike prefix
		{"http://litellm.internal:4000", "max_completion_tokens", "max_completion_tokens"},
		{"https://api.openai.com/v1", "max_tokens", "max_tokens"},
		{"://not a url", "", "max_tokens"},
	}
	for _, c := range cases {
		if got := maxTokensFieldFor(c.baseURL, c.override); got != c.want {
			t.Errorf("maxTokensFieldFor(%q, %q) = %q, want %q", c.baseURL, c.override, got, c.want)
		}
	}
}
