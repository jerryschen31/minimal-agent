package hooks

import (
	"context"
	"encoding/json"
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

// CommandRulePrefix starts a rule that applies to run_command calls and matches the call's command line (see
// CommandLine), not the tool name. A rule without it matches tool names only.
const CommandRulePrefix = "run_command:"

// runCommandName is the model-facing name of the run_command tool (builtin.RunCommandTool's definition uses the same).
const runCommandName = "run_command"

// ToolRules holds the deny and ask rules, glob patterns on model-facing tool names, and on the command line of
// run_command calls (rules starting with CommandRulePrefix). A call that matches a deny rule is denied even if it also
// matches an ask rule; a call that matches neither runs.
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
//   - A rule containing ":" is rejected unless it starts with CommandRulePrefix (the run_command form); MCP tool names
//     never contain ":".
//   - A malformed glob (an invalid wildcard, an open bracket with no closing bracket) is caught by path.Match.
//   - A duplicate (e.g. the same rule in two cases, *Delete* and *delete*) adds nothing and is skipped.
func checkRules(kind string, rulesIn []string) ([]string, error) {
	var rules []string
	for _, raw := range rulesIn {
		rule := strings.ToLower(strings.TrimSpace(raw)) // lowercase for case-insensitive matching; a stray space would match nothing
		// errors quote raw, the rule as the user wrote it
		if strings.Contains(rule, ":") && !strings.HasPrefix(rule, CommandRulePrefix) {
			return nil, fmt.Errorf("invalid %s rule %q: ':' is only allowed in a rule that starts with %q", kind, raw, CommandRulePrefix)
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
//   - The level comes from LevelFor, so a run_command call is judged by its command line too.
//   - A deny match returns `denied by rule "…"`, an ask match returns an error wrapping ErrNeedsApproval (`needs
//     approval by rule "…"`), and no match returns nil.
//   - Only the ErrNeedsApproval veto can be overridden by the user or by approval allow-all; a deny error is final.
func (t *ToolRules) BeforeTool(ctx context.Context, call model.ToolCall) error {
	switch level, rule := t.LevelFor(call); level {
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

// LevelFor returns the level a tool call gets and the rule that set it.
//
// Notes:
//   - A rule without CommandRulePrefix is matched against the tool name, as in Level. A rule with it applies to
//     run_command calls only and is matched against the call's command line, e.g. "run_command:go test ./..." (see
//     CommandLine). "run_command:*" is every command; the plain rule "run_command" is every call by name.
//   - The first matching deny rule wins, then the first matching ask rule, so deny beats ask.
//   - A run_command call whose arguments can't be read (no string command) fails closed: if no rule matched it is
//     asked, with the rule shown as "(unreadable run_command arguments)". Command rules can't be checked for it, so a
//     deny rule written for a command cannot be relied on there; the tool rejects such arguments anyway.
func (t *ToolRules) LevelFor(call model.ToolCall) (level, rule string) {
	name := call.Function.Name
	subject, readable := "", false
	if strings.EqualFold(name, runCommandName) {
		subject, readable = CommandLine(call)
	}
	matches := func(r string) bool {
		if strings.HasPrefix(r, CommandRulePrefix) {
			return readable && RuleMatches(r, subject)
		}
		return RuleMatches(r, name)
	}
	for _, r := range t.deny {
		if matches(r) {
			return LevelDeny, r
		}
	}
	for _, r := range t.ask {
		if matches(r) {
			return LevelAsk, r
		}
	}
	if strings.EqualFold(name, runCommandName) && !readable {
		return LevelAsk, "(unreadable run_command arguments)"
	}
	return LevelRun, ""
}

// CommandLine returns the text command rules are matched against for a run_command call: "run_command:" followed by
// the program and its arguments joined with single spaces, e.g. "run_command:go test ./...".
//
// Notes:
//   - ok is false when the arguments are not a JSON object with a non-empty string "command" and an optional list of
//     strings "args". Keys are read as the tool reads them (encoding/json: case-insensitive).
//   - [agent] An argument that contains a space looks like two arguments here. A glob over this line is not a detector:
//     "go -C dir run x" does not match "run_command:go run*". See DECISIONS.md § run_command plan.
func CommandLine(call model.ToolCall) (line string, ok bool) {
	var in struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &in); err != nil || in.Command == "" {
		return "", false
	}
	return CommandRulePrefix + strings.Join(append([]string{in.Command}, in.Args...), " "), true
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
