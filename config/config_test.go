package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout swaps os.Stdout for a temp file while fn runs and returns what was written.
// GetDefaultConfig reads os.Stdout when it is called, so a Config built inside fn writes its
// OutBuffer output here. Tests using this must not run in parallel (os.Stdout is process-wide).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("creating stdout capture file: %v", err)
	}
	orig := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = orig }() // restore even if fn calls t.Fatalf (runtime.Goexit still runs defers)
	fn()
	f.Close()

	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(out)
}

// writeTempFile creates a file with the given contents in a per-test temp dir (deleted by Go
// after the test) and returns its path.
func writeTempFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}

// - Verify SetDefaultConfig applies the values in the file, keeps defaults for keys the file
// omits, and prints no warning when every key is recognized
func Test_Unit_SetDefaultConfig_PopulatesFromFile_KeepsDefaultsForMissingKeys(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"userId": "jerry",
		"model": "qwen2.5:0.5b",
		"baseUrl": "http://127.0.0.1:11434/v1",
		"builtinTools": ["read_file"]
	}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = SetDefaultConfig(Flags{Config: &path}) })

	if err != nil {
		t.Fatalf("SetDefaultConfig() returned an error: %v", err)
	}
	if cfg.UserID != "jerry" || cfg.Model != "qwen2.5:0.5b" || cfg.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Errorf("expected file values to be applied, got UserID=%q Model=%q BaseURL=%q", cfg.UserID, cfg.Model, cfg.BaseURL)
	}
	if len(cfg.BuiltinTools) != 1 || cfg.BuiltinTools[0] != "read_file" {
		t.Errorf("expected BuiltinTools [read_file], got %v", cfg.BuiltinTools)
	}

	defaults := GetDefaultConfig()
	if cfg.Provider != defaults.Provider || cfg.SystemPrompt != defaults.SystemPrompt || cfg.ChatStoreType != defaults.ChatStoreType {
		t.Errorf("expected omitted keys to keep their defaults, got Provider=%q SystemPrompt=%q ChatStoreType=%q", cfg.Provider, cfg.SystemPrompt, cfg.ChatStoreType)
	}
	if cfg.InBuffer == nil || cfg.OutBuffer == nil {
		t.Errorf("expected InBuffer and OutBuffer to stay wired, got %v and %v", cfg.InBuffer, cfg.OutBuffer)
	}
	if out != "" {
		t.Errorf("expected no output when all keys are recognized, got %q", out)
	}
}

// - Verify SetDefaultConfig returns an error (that errors.Is os.ErrNotExist recognizes) when the file is missing
func Test_Unit_SetDefaultConfig_MissingFile_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")

	_, err := SetDefaultConfig(Flags{Config: &path})

	if err == nil {
		t.Fatalf("expected an error for a missing config file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected the error to wrap os.ErrNotExist, got %v", err)
	}
}

// - Verify keys Config has no field for produce one stdout warning each (sorted, naming the key),
// are ignored, and do not stop the recognized keys from being applied or cause an error
func Test_Unit_SetDefaultConfig_UnknownFields_WarnsAndIgnores(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"userId": "jerry",
		"zebra": 1,
		"subagents": true,
		"InBuffer": "not a reader"
	}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = SetDefaultConfig(Flags{Config: &path}) })

	if err != nil {
		t.Fatalf("expected unknown fields to be a warning, not an error, got: %v", err)
	}
	if cfg.UserID != "jerry" {
		t.Errorf("expected recognized key userId to still apply, got %q", cfg.UserID)
	}
	if cfg.InBuffer == nil {
		t.Errorf("expected InBuffer to be untouched by the file, got nil")
	}

	// sorted by key: "InBuffer" < "subagents" < "zebra" (uppercase sorts before lowercase)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 warning lines, got %d: %q", len(lines), out)
	}
	for i, key := range []string{"InBuffer", "subagents", "zebra"} {
		if !strings.HasPrefix(lines[i], "[warning]") || !strings.Contains(lines[i], fmt.Sprintf("%q", key)) {
			t.Errorf("line %d: expected a [warning] naming %q, got %q", i, key, lines[i])
		}
	}
}

// - Verify keys match case-insensitively like encoding/json does, so a key that was applied is not also reported as unrecognized
func Test_Unit_SetDefaultConfig_KeyCaseInsensitive_AppliedWithoutWarning(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{"UserId": "jerry"}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = SetDefaultConfig(Flags{Config: &path}) })

	if err != nil {
		t.Fatalf("SetDefaultConfig() returned an error: %v", err)
	}
	if cfg.UserID != "jerry" {
		t.Errorf("expected UserId to apply to UserID, got %q", cfg.UserID)
	}
	if out != "" {
		t.Errorf("expected no warning for a case-variant of a known key, got %q", out)
	}
}

// - Verify config keys are camelCase: the old snake_case spellings are no longer recognized, so
// they warn and are ignored instead of silently applying
func Test_Unit_SetDefaultConfig_SnakeCaseKeys_WarnAndIgnored(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{"user_id": "jerry", "base_url": "http://x/v1", "apiKeyName": "MY_KEY"}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = SetDefaultConfig(Flags{Config: &path}) })

	if err != nil {
		t.Fatalf("SetDefaultConfig() returned an error: %v", err)
	}
	defaults := GetDefaultConfig()
	if cfg.UserID != defaults.UserID || cfg.BaseURL != defaults.BaseURL {
		t.Errorf("expected snake_case keys to be ignored (defaults kept), got UserID=%q BaseURL=%q", cfg.UserID, cfg.BaseURL)
	}
	if cfg.ApiKeyName != "MY_KEY" {
		t.Errorf("expected camelCase apiKeyName to apply, got %q", cfg.ApiKeyName)
	}
	for _, key := range []string{"user_id", "base_url"} {
		if !strings.Contains(out, fmt.Sprintf("%q", key)) {
			t.Errorf("expected a warning naming %q, got %q", key, out)
		}
	}
}

// - Verify malformed JSON (here a trailing comma) is an error that names the file
func Test_Unit_SetDefaultConfig_InvalidJSON_ReturnsError(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{"userId": "jerry",}`))

	_, err := SetDefaultConfig(Flags{Config: &path})

	if err == nil {
		t.Fatalf("expected an error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("expected the error to name the file %q, got %v", path, err)
	}
}

// - Verify transportType infers the transport from the fields, honors a matching explicit type,
// and rejects ambiguous, unknown, contradicting and inapplicable-field entries (wantErr is a
// substring of the expected error; "" means success)
func Test_Unit_McpServerConfig_TransportType(t *testing.T) {
	env := map[string]string{"TOKEN": "x"}
	hdr := map[string]string{"Authorization": "Bearer x"}

	tests := []struct {
		name    string
		cfg     McpServerConfig
		want    string
		wantErr string
	}{
		// inferred from fields
		{"command only is stdio", McpServerConfig{Command: "npx"}, "stdio", ""},
		{"command, args and env is stdio", McpServerConfig{Command: "npx", Args: []string{"-y", "pkg"}, Env: env}, "stdio", ""},
		{"url only is http", McpServerConfig{URL: "https://x/mcp"}, "http", ""},
		{"url with headers is http", McpServerConfig{URL: "https://x/mcp", Headers: hdr}, "http", ""},
		// explicit type agreeing with the fields
		{"type stdio with command", McpServerConfig{Type: "stdio", Command: "npx"}, "stdio", ""},
		{"type http with url", McpServerConfig{Type: "http", URL: "https://x/mcp"}, "http", ""},
		{"type sse with url is passed through", McpServerConfig{Type: "sse", URL: "https://x/sse"}, "sse", ""},
		// shape errors
		{"both command and url", McpServerConfig{Command: "npx", URL: "https://x/mcp"}, "", "both"},
		{"neither command nor url", McpServerConfig{}, "", "set either"},
		{"only args set", McpServerConfig{Args: []string{"a"}}, "", "set either"},
		// type errors
		{"unknown type", McpServerConfig{Type: "htpp", URL: "https://x/mcp"}, "", "unknown type"},
		{"type is case-sensitive", McpServerConfig{Type: "HTTP", URL: "https://x/mcp"}, "", "unknown type"},
		{"type stdio without command", McpServerConfig{Type: "stdio", URL: "https://x/mcp"}, "", `needs "command"`},
		{"type http without url", McpServerConfig{Type: "http", Command: "npx"}, "", `needs "url"`},
		{"type sse without url", McpServerConfig{Type: "sse", Command: "npx"}, "", `needs "url"`},
		// fields that don't apply to the transport
		{"headers on stdio", McpServerConfig{Command: "npx", Headers: hdr}, "", `"headers"`},
		{"env on http", McpServerConfig{URL: "https://x/mcp", Env: env}, "", `"env"`},
		{"args on http", McpServerConfig{URL: "https://x/mcp", Args: []string{"a"}}, "", `"args"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.TransportType()

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got (%q, %v)", tt.wantErr, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("expected transport %q, got %q", tt.want, got)
			}
		})
	}
}

// - Verify transportType does not modify the config: Type stays what the user wrote (empty here)
func Test_Unit_McpServerConfig_TransportType_DoesNotMutate(t *testing.T) {
	cfg := McpServerConfig{Command: "npx"}

	if _, err := cfg.TransportType(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Type != "" {
		t.Errorf("expected Type to stay empty, got %q", cfg.Type)
	}
}

// - Verify an mcpServers block decodes from a config file in the usual MCP shape and each entry's transport is inferred
func Test_Unit_SetDefaultConfig_McpServers_DecodeAndInferTransport(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"mcpServers": {
			"fs":     {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"], "env": {"A": "b"}},
			"remote": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer x"}},
			"legacy": {"type": "sse", "url": "https://example.com/sse"}
		}
	}`))

	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = SetDefaultConfig(Flags{Config: &path}) })

	if err != nil {
		t.Fatalf("SetDefaultConfig() returned an error: %v", err)
	}
	if out != "" {
		t.Errorf("expected no warnings (mcpServers and its inner keys are known), got %q", out)
	}
	want := map[string]string{"fs": "stdio", "remote": "http", "legacy": "sse"}
	if len(cfg.McpServers) != len(want) {
		t.Fatalf("expected %d servers, got %d: %+v", len(want), len(cfg.McpServers), cfg.McpServers)
	}
	for label, wantKind := range want {
		got, err := cfg.McpServers[label].TransportType()
		if err != nil || got != wantKind {
			t.Errorf("server %q: expected %q, got (%q, %v)", label, wantKind, got, err)
		}
	}
	if fs := cfg.McpServers["fs"]; len(fs.Args) != 3 || fs.Env["A"] != "b" {
		t.Errorf("expected fs args and env to decode, got %+v", fs)
	}
}

// - Verify the built-in default for the ReAct step limit is 10
func Test_Unit_GetDefaultConfig_MaxSteps_Is10(t *testing.T) {
	if got := GetDefaultConfig().MaxSteps; got != 10 {
		t.Errorf("expected default MaxSteps 10, got %d", got)
	}
}
