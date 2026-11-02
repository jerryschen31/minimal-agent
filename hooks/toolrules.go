package hooks

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
)

var _ Hook = (*ToolRules)(nil) // compile-time check: *ToolRules satisfies Hook

// Levels a tool call can get from ToolRules.
const (
	LevelDeny = "deny" // refused, never asked, in every mode
	LevelAsk  = "ask"  // the user decides; with nobody to ask (no terminal, autonomous) it is refused, unless approval is "allow-all"
	LevelRun  = "run"  // no rule matched: the call runs
)

// ErrNeedsApproval marks a veto the user may override: BeforeTool wraps it when an ask rule matches. Any other hook
// error is final.
var ErrNeedsApproval = errors.New("needs approval")

// ToolRules holds the deny and ask rules, glob patterns on model-facing tool names. A name that matches a deny rule is
// denied even if it also matches an ask rule; a name that matches neither runs.
// [agent] Two lists rather than one list of {match, level} objects: the order of entries never matters (deny always
// wins), and each list replaces its own default in a config file. Downside: no "ask for fs_* except fs_read" until an
// allow list exists (PLAN-approval-gates.md, "Later").
type ToolRules struct {
	deny, ask []string // lowercased, checked at construction
}

// NewToolRules builds the rules from cfg.DenyRules and cfg.AskRules.
//
// Notes:
//   - The lists are the defaults from config.GetDefaultConfig, or a config file's lists that replaced them.
//   - Each rule is lowercased and validated; an invalid rule is an error naming it.
func NewToolRules(denyRules, askRules []string) (*ToolRules, error) {
	deny, err := checkRules("deny", denyRules)
	if err != nil {
		return nil, err
	}
	ask, err := checkRules("ask", askRules)
	if err != nil {
		return nil, err
	}
	return &ToolRules{deny: deny, ask: ask}, nil
}

// checkRules lowercases and validates one list of rules, dropping duplicates.
//
// Notes:
//   - kind ("deny" or "ask") names the list in errors.
//   - Errors quote the rule as the user wrote it; matching uses the trimmed, lowercased form (a stray space would
//     match nothing).
//   - A rule containing ":" is rejected: the tool:program form needs run_command, which does not exist yet.
//   - A malformed glob (an invalid wildcard, an open bracket with no closing bracket) is caught by path.Match.
//   - A duplicate (e.g. the same rule in two cases, *Delete* and *delete*) adds nothing and is skipped.
func checkRules(kind string, rulesIn []string) ([]string, error) {
	var rules []string
	for _, raw := range rulesIn {
		rule := strings.ToLower(strings.TrimSpace(raw)) // lowercase for case-insensitive matching; a stray space would match nothing
		// errors quote raw, the rule as the user wrote it
		if strings.Contains(rule, ":") {
			return nil, fmt.Errorf("invalid %s rule %q: tool:program form needs run_command, which doesn't exist yet", kind, raw)
		}
		if _, err := path.Match(rule, ""); err != nil { // check for malformed search patterns such as invalid wildcards or open bracket without a closing bracket
			return nil, fmt.Errorf("invalid %s rule %q: %w", kind, raw, err)
		}
		if slices.Contains(rules, rule) { // a duplicate (e.g. the same rule in two cases, *Delete* and *delete*) adds nothing
			continue
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// BeforeTool checks the tool call against the rules.
//
// Notes:
//   - A deny match returns `denied by rule "…"`, an ask match returns an error wrapping ErrNeedsApproval (`needs
//     approval by rule "…"`), and no match returns nil.
//   - Only the ErrNeedsApproval veto can be overridden by the user or by approval allow-all; a deny error is final.
func (t *ToolRules) BeforeTool(ctx context.Context, call model.ToolCall) error {
	switch level, rule := t.Level(call.Function.Name); level {
	case LevelDeny:
		return fmt.Errorf("denied by rule %q", rule)
	case LevelAsk:
		return fmt.Errorf("%w by rule %q", ErrNeedsApproval, rule)
	}
	return nil
}

// Level returns the level a tool name gets and the rule that set it.
//
// Notes:
//   - The rule is "" for LevelRun.
//   - The first matching deny rule wins, then the first matching ask rule, so deny beats ask.
//   - BeforeTool and info mode both use it, so what info mode shows is what the gate does.
func (t *ToolRules) Level(toolName string) (level, rule string) {
	for _, r := range t.deny {
		if RuleMatches(r, toolName) {
			return LevelDeny, r
		}
	}
	for _, r := range t.ask {
		if RuleMatches(r, toolName) {
			return LevelAsk, r
		}
	}
	return LevelRun, ""
}

// DenyRules returns the deny rules in the order Level checks them.
//
// Notes:
//   - It returns a copy, so a caller can't change the rules in use.
func (t *ToolRules) DenyRules() []string {
	return slices.Clone(t.deny) // a copy, so a caller can't change the rules in use
}

// AskRules returns the ask rules in the order Level checks them.
//
// Notes:
//   - It returns a copy, so a caller can't change the rules in use.
func (t *ToolRules) AskRules() []string {
	return slices.Clone(t.ask)
}

// RuleMatches reports whether a rule (as returned by DenyRules or AskRules) matches a model-facing tool name,
// ignoring case.
//
// Notes:
//   - A tool name is an opaque string, not a path: path.Match's * and ? stop at '/', so "*delete*" would miss a name
//     like "team/fs_delete_file" (an MCP server label can contain '/').
//   - '/' is mapped to a byte that has no special meaning in a pattern, on both sides, so * and ? match across it and
//     a literal '/' in a rule still matches '/'.
//   - The path.Match error is ignored because NewToolRules rejected malformed rules.
func RuleMatches(rule, toolName string) bool {
	const opaqueSep = "\x00"
	rule = strings.ReplaceAll(rule, "/", opaqueSep)
	name := strings.ReplaceAll(strings.ToLower(toolName), "/", opaqueSep)
	matched, _ := path.Match(rule, name) // the error can be ignored: NewToolRules rejected malformed rules
	return matched
}
