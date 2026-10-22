package trace

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Detector aims to find secrets, keys, credentials and passwords in agent logs.
// It is an interface so the detection engine can be swapped. The built-in detector below is a lightweight set of regexp and field rules.
// Something like gitleaks, if swapped in as the detector, would consider context and redact more, at the possible expense of an increase in false positives.
type Detector interface {
	// Name identifies the detector engine ("builtin", "gitleaks"); builtin is the default, others selected via config
	Name() string
	// Find returns any found secrets. key is the JSON field name s sits under ("" at the root or in a slice).
	Find(key, s string) []Finding
}

// Finding is one secret a Detector found, and the name of the rule that matched it.
type Finding struct {
	Rule   string // the type of secret - e.g. "aws_access_key", "github_token"
	Secret string // the exact text to mask (redacted in output)
}

// String keeps the secret out of accidental %v / %+v printing.
func (f Finding) String() string {
	return fmt.Sprintf("finding(%s)", f.Rule)
}

// Level sets the stringency of secret detection. The user-facing config file picks a level by name.
type Level string

const (
	LevelStandard Level = "standard"
	// TODO later: LevelStringent, LevelMetadata
)

// placeholderPrefix starts every placeholder.
const placeholderPrefix = "[REDACTED:"

// exactPlaceholderRe matches one complete placeholder and nothing else. A finding that is exactly a placeholder is
// already redacted and is skipped, so redacting twice (L0, then the digest pass) never nests placeholders.
// A finding that merely starts with the prefix ("[REDACTED:x#12345678]hunter2xyz") still carries credential text and is masked.
var exactPlaceholderRe = regexp.MustCompile(`^\[REDACTED:[a-z0-9_]+#[0-9a-f]{8}\]$`)

// rule is one thing the built-in detector looks for. Set one of the two patterns.
type rule struct {
	name string
	// valueRe finds a secret by its SHAPE, anywhere inside a string (e.g. an API key format).
	// If the pattern has capture groups, the one that matched is the secret and the rest is context (e.g. "Bearer ").
	// Several groups mean alternatives (a double-quoted, single-quoted or bare value): only one of them matches.
	valueRe *regexp.Regexp
	// keyRe finds a secret by the NAME of the field it sits under (e.g. "db_password": "hunter2");
	// the whole string value is the secret.
	keyRe *regexp.Regexp
}

// builtinDetector applies a list of rules.
type builtinDetector struct {
	rules []rule
}

func (d builtinDetector) Name() string { return "builtin" }

func (d builtinDetector) Find(key, s string) []Finding {
	var found []Finding
	for _, r := range d.rules {
		if r.valueRe != nil {
			for _, m := range r.valueRe.FindAllStringSubmatch(s, -1) {
				secret := m[0]
				if len(m) > 1 {
					secret = firstNonEmpty(m[1:])
					if secret == "" {
						continue // nothing to mask
					}
				}
				found = append(found, Finding{Rule: r.name, Secret: secret})
			}
		}
		if r.keyRe != nil && s != "" && r.keyRe.MatchString(key) {
			found = append(found, Finding{Rule: r.name, Secret: s})
		}
	}
	return found
}

func firstNonEmpty(groups []string) string {
	for _, g := range groups {
		if g != "" {
			return g
		}
	}
	return ""
}

// assignedValue is the value after "name=" or "name:", for the assignment rules below. In order of preference:
// a complete double-quoted value, a complete single-quoted value (both may contain spaces and backslash-escaped characters,
// and run to the end of the text if the closing quote is missing, so a cut-off value is still masked), then a bare token.
// Each alternative is its own capture group, and the one that matched is the secret.
const assignedValue = `\s*(?:"((?:[^"\\]|\\(?s:.)){4,})(?:"|$)|'((?:[^'\\]|\\(?s:.)){4,})(?:'|$)|["']?([^\s"',;&}]{4,}))`

// secretFieldPattern: (?i) part = separator style, (?-i:...) part = camelCase (a lower-case letter, then a capitalised word).
const secretFieldPattern = `(?i)(?:^|[_.\-])(?:pass(?:word|wd|phrase)?|pwd|pw|secret|token|key|credentials?|auth|authorization|cookie)$` +
	`|(?i)api[_-]?(?:key|token)$|(?i)private[_-]?key$` +
	`|(?-i:[a-z](?:Pass|Password|Passwd|Passphrase|Pwd|Pw|Secret|Token|Key|Credentials?|Auth|Authorization|Cookie)$)`

// builtinRules is the catalogue of "common secrets"
// Go's regexp (RE2) has no lookahead or lookbehind, so context like "Bearer " is matched and the secret is picked out with a capture group.
// Redaction prefers false positives to misses, so patterns are a little loose on purpose.
// Order matters when two rules find the same text: the first one named in a Finding wins the placeholder.
var builtinRules = []rule{
	{name: "anthropic_key", valueRe: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{name: "openai_key", valueRe: regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{32,}`)},
	{name: "aws_access_key", valueRe: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{name: "github_token", valueRe: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{20,}`)},
	{name: "slack_token", valueRe: regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`)},
	{name: "gitlab_token", valueRe: regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{name: "google_api_key", valueRe: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{name: "stripe_key", valueRe: regexp.MustCompile(`[sr]k_live_[0-9A-Za-z]{16,}`)},
	{name: "jwt", valueRe: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{name: "bearer_token", valueRe: regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{16,})`)},
	{name: "pem_private_key", valueRe: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)},
	{name: "url_credentials", valueRe: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://([^\s/:@]+:[^\s/@]+)@`)},
	// key=value or "key":"value" where the key name ends with a secret word.
	// Ending matters: max_tokens=1024 must survive, access_token=abc123 must not. Also catches secrets inside a tool call's JSON-string arguments.
	{name: "key_value", valueRe: regexp.MustCompile(`(?i)(?:^|[^a-z])(?:pass(?:word|wd|phrase)?|pwd|pw|secret|token|api[_-]?key|private[_-]?key)["']?\s*[:=]` + assignedValue)},
	// bare "key" is common in prose ("key: the main idea"), so it only counts with "=" or a quoted "key" -> api_key=..., --key=..., {"key": "..."} etc.
	{name: "key_assignment", valueRe: regexp.MustCompile(`(?i)(?:^|[^a-z])(?:[a-z0-9]+[_-])?key(?:["']\s*:|\s*=)` + assignedValue)},
	// a value under a JSON field whose name is, or ends with, a secret word is secret, whatever it looks like.
	// The word must be the whole name, follow a separator (db_password, x-api-key) or follow a camelCase boundary (apiKey, dbPassword),
	// so words like compass, monkey, max_tokens and input_tokens do not match.
	{name: "secret_field", keyRe: regexp.MustCompile(secretFieldPattern)},
}

// levelRules lists the rules each level turns on, by name. Standard is every built-in rule. A stricter level later is another row.
var levelRules = map[Level][]string{
	LevelStandard: ruleNames(),
}

func ruleNames() []string {
	names := make([]string, len(builtinRules))
	for i, r := range builtinRules {
		names[i] = r.name
	}
	return names
}

// Redactor masks secrets in event data. Build it once per run. Treat it as single-caller: the Recorder
// will serialise calls under its own mutex, so the Redactor needs none.
type Redactor struct {
	detectors []Detector
	key       []byte // random per-run HMAC key for placeholder tags
}

// NewRedactor builds a Redactor for a level. Unknown levels fail loudly since user should know if redaction is misconfigured.
// Later: extra detectors (add) and an allow list (remove rules) are plain slice edits.
func NewRedactor(level Level) (*Redactor, error) {
	names, ok := levelRules[level]
	if !ok {
		return nil, fmt.Errorf("unknown redaction level %q", level)
	}
	byName := make(map[string]rule, len(builtinRules))
	for _, r := range builtinRules {
		byName[r.name] = r
	}
	rules := make([]rule, 0, len(names))
	for _, n := range names {
		r, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("redaction level %q names unknown rule %q", level, n)
		}
		rules = append(rules, r)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("redaction key: %w", err)
	}
	return &Redactor{detectors: []Detector{builtinDetector{rules: rules}}, key: key}, nil
}

// Redact returns a redacted copy of data and the sorted, de-duplicated rule names that fired.
// Steps: json.Marshal(data) -> json.Unmarshal into `any` -> walk -> return the walked value.
// On any error it returns the error and NO data: the caller must drop the payload, never write it raw.
func (r *Redactor) Redact(data any) (any, []string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, nil, fmt.Errorf("redact: marshal: %w", err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, nil, fmt.Errorf("redact: unmarshal: %w", err)
	}
	fired := map[string]bool{}
	tree = r.walk(tree, "", fired)
	return tree, slices.Sorted(maps.Keys(fired)), nil
}

// walk returns v (event data) with every detected secret string redacted. key is the JSON field name v sits under ("" at the root).
// fired collects rule names as a set.
func (r *Redactor) walk(v any, key string, fired map[string]bool) any {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			v[k] = r.walk(child, k, fired)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = r.walk(child, key, fired)
		}
		return v
	case string:
		return r.redactString(v, key, fired)
	default:
		return v
	}
}

// redactString uses the detectors to find any secrets, then replaces those secrets with placeholders and records the rules that fired.
func (r *Redactor) redactString(s, key string, fired map[string]bool) string {
	var found []Finding
	for _, d := range r.detectors {
		found = append(found, d.Find(key, s)...)
	}
	slices.SortStableFunc(found, func(a, b Finding) int { return len(b.Secret) - len(a.Secret) })
	for _, f := range found {
		if f.Secret == "" || exactPlaceholderRe.MatchString(f.Secret) || !strings.Contains(s, f.Secret) {
			continue
		}
		s = strings.ReplaceAll(s, f.Secret, r.placeholder(f.Rule, f.Secret))
		fired[f.Rule] = true
	}
	return s
}

// placeholder builds "[REDACTED:rule#tag]". tag = first 4 bytes of HMAC-SHA256(r.key, secret), hex.
// The same secret gets the same tag within a run, so repeats can be matched without revealing them.
func (r *Redactor) placeholder(rule, secret string) string {
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(secret))
	return fmt.Sprintf("%s%s#%s]", placeholderPrefix, rule, hex.EncodeToString(mac.Sum(nil)[:4]))
}
