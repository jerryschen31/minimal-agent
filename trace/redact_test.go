package trace

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Fake secrets are built from repeated characters so no secret scanner flags this file.
var (
	fakeAnthropic = "sk-ant-api03-" + strings.Repeat("aB3", 12)
	fakeOpenAI    = "sk-proj-" + strings.Repeat("xY7", 14)
	fakeAWS       = "AKIA" + strings.Repeat("Q7", 8)
	fakeGitHub    = "ghp_" + strings.Repeat("a1B2", 9)
	fakeSlack     = "xoxb-" + strings.Repeat("1234567890", 2)
	fakeGitLab    = "glpat-" + strings.Repeat("zZ9", 8)
	fakeGoogle    = "AIza" + strings.Repeat("kL4", 12)[:35]
	fakeStripe    = "sk_live_" + strings.Repeat("Ab1", 8)
	fakeJWT       = "eyJ" + strings.Repeat("a", 12) + ".eyJ" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12)
	fakePEM       = "-----BEGIN RSA PRIVATE KEY-----\nZmFrZS1rZXktYm9keQ==\nZmFrZS1rZXktYm9keQ==\n-----END RSA PRIVATE KEY-----" // the body is base64 of "fake-key-body"
)

var placeholderRe = regexp.MustCompile(`\[REDACTED:[a-z_]+#[0-9a-f]{8}\]`)

func newTestRedactor(t *testing.T) *Redactor {
	t.Helper()
	r, err := NewRedactor(LevelStandard)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func redactToString(t *testing.T, r *Redactor, data any) (string, []string) {
	t.Helper()
	out, fired, err := r.Redact(data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), fired
}

func Test_Redact_ValueRules(t *testing.T) {
	tests := []struct {
		name, text, secret, rule string
	}{
		{"anthropic", "ANTHROPIC_API_KEY=" + fakeAnthropic, fakeAnthropic, "anthropic_key"},
		{"openai", "key " + fakeOpenAI + " here", fakeOpenAI, "openai_key"},
		{"aws", "id: " + fakeAWS + ".", fakeAWS, "aws_access_key"},
		{"github", "git clone https://x/" + fakeGitHub, fakeGitHub, "github_token"},
		{"slack", "hook " + fakeSlack, fakeSlack, "slack_token"},
		{"gitlab", "token is " + fakeGitLab, fakeGitLab, "gitlab_token"},
		{"google", "k=" + fakeGoogle, fakeGoogle, "google_api_key"},
		{"stripe", fakeStripe, fakeStripe, "stripe_key"},
		{"jwt", "session " + fakeJWT, fakeJWT, "jwt"},
		{"bearer", `curl -H "Authorization: Bearer abcDEF0123456789xyz"`, "abcDEF0123456789xyz", "bearer_token"},
		{"pem", "key:\n" + fakePEM + "\ndone", fakePEM, "pem_private_key"},
		{"pem truncated", "key:\n-----BEGIN PRIVATE KEY-----\nZmFrZS1rZXktYm9keQ", "ZmFrZS1rZXktYm9keQ", "pem_private_key"},
		{"url credentials", "postgres://admin:s3cretpw@db.internal/app", "admin:s3cretpw", "url_credentials"},
		{"key=value", "export DB_PASSWORD=hunter2xyz", "hunter2xyz", "key_value"},
		{"yaml", "api_key: abcd1234efgh", "abcd1234efgh", "key_value"},
		{"pw", "login pw=zx81qk9m", "zx81qk9m", "key_value"},
		{"api_token", `{"api_token":"tok98zxcv"}`, "tok98zxcv", "key_value"},
		{"bare key with =", "run --key=abcd1234efgh", "abcd1234efgh", "key_assignment"},
		{"bare key quoted", `{"key": "abcd1234efgh"}`, "abcd1234efgh", "key_assignment"},
	}
	r := newTestRedactor(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, fired := redactToString(t, r, map[string]any{"output": tt.text})
			if strings.Contains(out, tt.secret) {
				t.Errorf("secret survived: %s", out)
			}
			if !placeholderRe.MatchString(out) {
				t.Errorf("no placeholder in %s", out)
			}
			if !slices.Contains(fired, tt.rule) {
				t.Errorf("fired = %v, want it to contain %q", fired, tt.rule)
			}
		})
	}
}

func Test_Redact_BearerKeepsContext(t *testing.T) {
	out, _ := redactToString(t, newTestRedactor(t), "Authorization: Bearer abcDEF0123456789xyz")
	if !strings.Contains(out, "Authorization: Bearer [REDACTED:") {
		t.Errorf("want the Bearer prefix kept, got %s", out)
	}
}

func Test_Redact_FieldNames(t *testing.T) {
	r := newTestRedactor(t)
	data := map[string]any{
		"db_password":   "hunter2",
		"apiKey":        "k-12345",
		"Authorization": "whatever",
		"access_token":  "tok-one",
		"nested":        map[string]any{"client_secret": "sec-three"},
		"list":          map[string]any{"password": []any{"lst-one", "lst-two"}},
		"password":      "p-1",
		"pw":            "p-2",
		"pwd":           "p-3",
		"db_pass":       "p-4",
		"key":           "p-5",
		"api_key":       "p-6",
		"x-api-key":     "p-7",
		"apikey":        "p-8",
		"token":         "p-9",
		"api_token":     "p-10",
		"accessToken":   "p-11",
		"dbPassword":    "p-12",
		"private_key":   "p-13",
	}
	out, fired := redactToString(t, r, data)
	for _, secret := range []string{"hunter2", "k-12345", "whatever", "tok-one", "sec-three", "lst-one", "lst-two",
		"p-1", "p-2", "p-3", "p-4", "p-5", "p-6", "p-7", "p-8", "p-9", `"p-10"`, "p-11", "p-12", "p-13"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q survived: %s", secret, out)
		}
	}
	if !slices.Equal(fired, []string{"secret_field"}) {
		t.Errorf("fired = %v", fired)
	}
}

// Our own token-count fields and ordinary prose must survive.
func Test_Redact_LeavesNonSecretsAlone(t *testing.T) {
	r := newTestRedactor(t)
	data := ModelResponseData{
		Message:    MessageRecord{Role: "assistant", Content: "I set max_tokens=1024 and the token budget is fine. See https://example.com/a:b"},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 1234, OutputTokens: 567, CacheReadTokens: 89},
		DurationMS: 42,
	}
	want, _ := json.Marshal(data)
	var wantTree any
	_ = json.Unmarshal(want, &wantTree)
	wantJSON, _ := json.Marshal(wantTree)

	out, fired := redactToString(t, r, data)
	if out != string(wantJSON) {
		t.Errorf("data changed:\n got %s\nwant %s", out, wantJSON)
	}
	if len(fired) != 0 {
		t.Errorf("fired = %v, want none", fired)
	}
}

func Test_Redact_FieldNamesThatOnlyLookSimilar(t *testing.T) {
	data := map[string]any{
		"max_tokens": "abcdefgh", "input_tokens": "12345", "compass": "north", "monkey": "banana",
		"keyboard": "qwerty", "passenger": "alice", "tokens": "many", "bypass": "yes", "keys": "ring",
		"system_prompt": "be helpful", "Monkey": "banana2",
	}
	out, fired := redactToString(t, newTestRedactor(t), data)
	if strings.Contains(out, "REDACTED") || len(fired) != 0 {
		t.Errorf("non-secret field names were treated as secrets: %s %v", out, fired)
	}
}

func Test_Redact_ProseIsNotAKeyAssignment(t *testing.T) {
	text := "Key: the main idea. The monkey=banana1 and a keyboard: qwerty, compass: north"
	out, fired := redactToString(t, newTestRedactor(t), text)
	if strings.Contains(out, "REDACTED") || len(fired) != 0 {
		t.Errorf("prose was redacted: %s %v", out, fired)
	}
}

func Test_Redact_JSONStringArguments(t *testing.T) {
	// ToolCallRecord.Arguments is a string holding JSON; the walker sees one string.
	data := ToolCallRecord{CallID: "c1", Name: "login", Arguments: `{"user":"jerry","password":"hunter2xyz"}`}
	out, fired := redactToString(t, newTestRedactor(t), data)
	if strings.Contains(out, "hunter2xyz") {
		t.Errorf("password survived: %s", out)
	}
	if !slices.Contains(fired, "key_value") {
		t.Errorf("fired = %v", fired)
	}
}

func Test_Redact_TypedPayload(t *testing.T) {
	data := RunStartData{SystemPrompt: "use key " + fakeAnthropic, McpServers: map[string]string{"fs": "stdio"}, MaxTokens: 100}
	out, _ := redactToString(t, newTestRedactor(t), data)
	if strings.Contains(out, fakeAnthropic) {
		t.Errorf("secret survived: %s", out)
	}
	if !strings.Contains(out, `"max_tokens":100`) || !strings.Contains(out, `"fs":"stdio"`) {
		t.Errorf("non-secret fields changed: %s", out)
	}
}

func Test_Redact_SameSecretSameTag(t *testing.T) {
	r := newTestRedactor(t)
	out, _ := redactToString(t, r, []string{fakeAnthropic, "again " + fakeAnthropic, "other " + fakeGitHub})
	tags := placeholderRe.FindAllString(out, -1)
	if len(tags) != 3 {
		t.Fatalf("tags = %v", tags)
	}
	if tags[0] != tags[1] {
		t.Errorf("same secret got different tags: %v", tags)
	}
	if tags[0] == tags[2] {
		t.Errorf("different secrets got the same tag: %v", tags)
	}

	other := newTestRedactor(t)
	out2, _ := redactToString(t, other, fakeAnthropic)
	if placeholderRe.FindString(out2) == tags[0] {
		t.Error("two runs (keys) produced the same tag")
	}
}

func Test_Redact_FiredIsSortedAndDeduplicated(t *testing.T) {
	_, fired := redactToString(t, newTestRedactor(t), []string{fakeGitHub, fakeAnthropic, fakeGitHub + "x", "x " + fakeAnthropic})
	if !slices.Equal(fired, []string{"anthropic_key", "github_token"}) {
		t.Errorf("fired = %v", fired)
	}
}

func Test_Redact_OverlappingRulesNameOnlyTheOneThatMasked(t *testing.T) {
	// The Anthropic key also fits the broader OpenAI shape; only one rule masks it.
	_, fired := redactToString(t, newTestRedactor(t), fakeAnthropic)
	if len(fired) != 1 {
		t.Errorf("fired = %v, want exactly one rule", fired)
	}
}

func Test_Redact_Idempotent(t *testing.T) {
	r := newTestRedactor(t)
	first, _ := redactToString(t, r, map[string]any{"a": "password=hunter2xyz", "b": fakeJWT, "db_password": "pw12345"})
	var tree any
	_ = json.Unmarshal([]byte(first), &tree)
	second, fired := redactToString(t, r, tree)
	if first != second {
		t.Errorf("second pass changed output:\n%s\n%s", first, second)
	}
	if len(fired) != 0 {
		t.Errorf("second pass fired %v", fired)
	}
}

func Test_Redact_FailsClosed(t *testing.T) {
	out, fired, err := newTestRedactor(t).Redact(map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("want an error for data that cannot be marshalled")
	}
	if out != nil || fired != nil {
		t.Errorf("got data back with an error: %v %v", out, fired)
	}
}

func Test_Redact_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{"x": fakeAnthropic}
	_, _ = redactToString(t, newTestRedactor(t), in)
	if in["x"] != fakeAnthropic {
		t.Error("input was modified")
	}
}

func Test_NewRedactor_UnknownLevel(t *testing.T) {
	if _, err := NewRedactor("paranoid"); err == nil {
		t.Error("want an error for an unknown level")
	}
}

func Test_Finding_StringHidesSecret(t *testing.T) {
	f := Finding{Rule: "github_token", Secret: fakeGitHub}
	for _, s := range []string{fmt.Sprint(f), fmt.Sprintf("%v", f), fmt.Sprintf("%+v", f)} {
		if strings.Contains(s, fakeGitHub) {
			t.Errorf("secret leaked in %q", s)
		}
	}
}

// - Verify only a complete placeholder is skipped: a value that is exactly a placeholder is left alone (so a second
// pass changes nothing), while text after or around a placeholder-looking prefix is still masked
func Test_Redact_OnlyACompletePlaceholderIsExempt(t *testing.T) {
	r := newTestRedactor(t)

	for _, in := range []string{
		"password=[REDACTED:key_value#12345678]",
		"password='[REDACTED:key_value#12345678]'",
	} {
		out, fired := redactToString(t, r, in)
		if !strings.Contains(out, in) || len(fired) != 0 { // out is the JSON encoding of in
			t.Errorf("complete placeholder changed: %q -> %q (fired %v)", in, out, fired)
		}
	}
	if out, _ := redactToString(t, r, map[string]string{"password": "[REDACTED:secret_field#12345678]"}); !strings.Contains(out, "[REDACTED:secret_field#12345678]") {
		t.Errorf("a field that is exactly a placeholder was re-masked: %s", out)
	}

	for _, in := range []string{
		"password=[REDACTED:key_value#12345678]hunter2xyz",
		"password=[REDACTED:not-a-placeholder]hunter2xyz",
		"password=[REDACTED:key_value#1234]hunter2xyz",
	} {
		out, _ := redactToString(t, r, in)
		if strings.Contains(out, "hunter2xyz") {
			t.Errorf("credential after a placeholder-looking prefix survived: %q -> %q", in, out)
		}
	}
}
