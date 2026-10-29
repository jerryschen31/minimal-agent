package hooks

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jerryschen31/minimal-agent/model"
)

var _ Hook = (*DenyRules)(nil) // compile-time check: *DenyRules satisfies Hook

// defaultDenyRules are always on; config rules are added to them, never replace them.
var defaultDenyRules = []string{"*delete*", "*remove*"}

// DenyRules specifies the tool call name patterns that are denied; any tool call matching a rule will be vetoed.
type DenyRules struct {
	rules []string // lowercased, checked at construction
}

// SetupDenyRules constructs the deny rules from the configuration, combining default and extra rules.
func SetupDenyRules(denyRules []string) (*DenyRules, error) {
	return NewDenyRules(denyRules)
}

// NewDenyRules creates the combined list of default and extra rules, all lowercased and validated; an invalid rule will result in an error naming it
func NewDenyRules(extraRules []string) (*DenyRules, error) {
	d := &DenyRules{}
	for _, raw := range append(defaultDenyRules, extraRules...) {
		rule := strings.ToLower(strings.TrimSpace(raw)) // lowercase for case-insensitive matching; a stray space would match nothing
		// errors quote raw, the rule as the user wrote it
		if strings.Contains(rule, ":") {
			return nil, fmt.Errorf("invalid deny rule %q: tool:program form needs run_command, which doesn't exist yet", raw)
		}
		if _, err := path.Match(rule, ""); err != nil { // check for malformed search patterns such as invalid wildcards or open bracket without a closing bracket
			return nil, fmt.Errorf("invalid deny rule %q: %w", raw, err)
		}
		if slices.Contains(d.rules, rule) { // a duplicate (e.g. a config rule repeating a default) adds nothing
			continue
		}
		d.rules = append(d.rules, rule)
	}
	return d, nil
}

// BeforeTool checks the tool call against the deny rules; it returns an error if any rule matches the tool call name.
func (d *DenyRules) BeforeTool(ctx context.Context, call model.ToolCall) error {
	for _, rule := range d.rules {
		if RuleMatches(rule, call.Function.Name) {
			return fmt.Errorf("denied by rule %q", rule)
		}
	}
	return nil
}

// Rules returns the rules in the order BeforeTool checks them: the built-in defaults first, then the config ones.
func (d *DenyRules) Rules() []string {
	return slices.Clone(d.rules) // a copy, so a caller can't change the rules in use
}

// IsBuiltIn reports whether rule is one of the always-on defaults rather than one from the config.
func IsBuiltIn(rule string) bool {
	return slices.Contains(defaultDenyRules, rule)
}

// RuleMatches reports whether a rule (as returned by Rules) matches a model-facing tool name, ignoring case.
// BeforeTool and info mode both use it, so what info mode shows is what the gate does.
func RuleMatches(rule, toolName string) bool {
	matched, _ := path.Match(rule, strings.ToLower(toolName)) // the error can be ignored: NewDenyRules rejected malformed rules
	return matched
}
