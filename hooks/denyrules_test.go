package hooks

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/model"
)

// call builds a tool call with the given name and arguments.
func call(name, args string) model.ToolCall {
	return model.ToolCall{ID: "call_1", Function: model.ToolCallFunc{Name: name, Arguments: args}}
}

// newRules builds DenyRules and fails the test on a constructor error.
func newRules(t *testing.T, extra []string) *DenyRules {
	t.Helper()
	d, err := NewDenyRules(extra)
	if err != nil {
		t.Fatalf("NewDenyRules(%q): %v", extra, err)
	}
	return d
}

// checkCall asserts that tool name is denied by wantRule, or allowed when wantRule is "".
func checkCall(t *testing.T, d *DenyRules, tc model.ToolCall, wantRule string) {
	t.Helper()
	err := d.BeforeTool(context.Background(), tc)
	if wantRule == "" {
		if err != nil {
			t.Errorf("%q: want allowed, got %v", tc.Function.Name, err)
		}
		return
	}
	want := `denied by rule "` + wantRule + `"`
	if err == nil || err.Error() != want {
		t.Errorf("%q: want error %q, got %v", tc.Function.Name, want, err)
	}
}

func Test_Unit_DenyRules_Defaults(t *testing.T) {
	cases := []struct {
		name     string
		wantRule string // "" = allowed
	}{
		{"github_delete_file", "*delete*"},
		{"DeleteFile", "*delete*"},
		{"fs_remove_dir", "*remove*"},
		{"bulk_delete", "*delete*"},
		{"filesystem_move_file", ""},
		{"filesystem_write_file", ""},
		{"read_file", ""},
	}
	for _, extra := range [][]string{nil, {}} {
		d := newRules(t, extra)
		for _, c := range cases {
			checkCall(t, d, call(c.name, "{}"), c.wantRule)
		}
	}
}

func Test_Unit_DenyRules_ExtrasAddToDefaults(t *testing.T) {
	d := newRules(t, []string{"*move*"})
	checkCall(t, d, call("filesystem_move_file", "{}"), "*move*")
	checkCall(t, d, call("github_delete_file", "{}"), "*delete*")
	checkCall(t, d, call("fs_remove_dir", "{}"), "*remove*")
	checkCall(t, d, call("read_file", "{}"), "")
}

func Test_Unit_DenyRules_CaseInsensitive(t *testing.T) {
	d := newRules(t, []string{"*MOVE*"})
	checkCall(t, d, call("filesystem_move_file", "{}"), "*move*")
	checkCall(t, d, call("Filesystem_MOVE_File", "{}"), "*move*")
}

func Test_Unit_DenyRules_BareName(t *testing.T) {
	d := newRules(t, []string{"mv"})
	cases := []struct {
		name     string
		wantRule string
	}{
		{"mv", "mv"},
		{"MV", "mv"},
		{"mv_file", ""},
		{"fs_mv", ""},
	}
	for _, c := range cases {
		checkCall(t, d, call(c.name, "{}"), c.wantRule)
	}
}

func Test_Unit_DenyRules_FirstMatchWins(t *testing.T) {
	d := newRules(t, []string{"fs_*"})
	checkCall(t, d, call("fs_delete_x", "{}"), "*delete*") // default comes before the extra
	checkCall(t, d, call("fs_list", "{}"), "fs_*")
}

func Test_Unit_DenyRules_InvalidRules(t *testing.T) {
	badPatterns := []struct {
		desc  string
		extra []string
		rule  string // the rule the error must name
	}{
		{"unclosed bracket after star", []string{"*delete["}, "*delete["},
		{"lone bracket", []string{"["}, "["},
		{"trailing backslash", []string{`abc\`}, `abc\`},
		{"invalid after valid ones", []string{"*move*", "mv", "["}, "["},
	}
	for _, c := range badPatterns {
		t.Run(c.desc, func(t *testing.T) {
			d, err := NewDenyRules(c.extra)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if d != nil {
				t.Errorf("want nil DenyRules on error, got %+v", d)
			}
			if !errors.Is(err, path.ErrBadPattern) {
				t.Errorf("want error wrapping path.ErrBadPattern, got %v", err)
			}
			if !strings.Contains(err.Error(), c.rule) {
				t.Errorf("error %q does not name rule %q", err, c.rule)
			}
		})
	}

	t.Run("tool:program form", func(t *testing.T) {
		d, err := NewDenyRules([]string{"run_command:rm"})
		if err == nil || !strings.Contains(err.Error(), "run_command") {
			t.Fatalf("want error mentioning run_command, got %v", err)
		}
		if d != nil {
			t.Errorf("want nil DenyRules on error, got %+v", d)
		}
	})
}

func Test_Unit_DenyRules_SeparateInstancesDontShareRules(t *testing.T) {
	before := slices.Clone(defaultDenyRules)

	a := newRules(t, []string{"aaa"})
	b := newRules(t, []string{"bbb"})

	checkCall(t, a, call("aaa", "{}"), "aaa")
	checkCall(t, a, call("bbb", "{}"), "")
	checkCall(t, b, call("bbb", "{}"), "bbb")
	checkCall(t, b, call("aaa", "{}"), "")

	if !slices.Equal(defaultDenyRules, before) {
		t.Errorf("defaultDenyRules changed: got %q, want %q", defaultDenyRules, before)
	}
}

func Test_Unit_DenyRules_ArgumentsIgnored(t *testing.T) {
	d := newRules(t, nil)
	cases := []struct {
		name, args string
		wantRule   string
	}{
		{"github_delete_file", `{"path":"a.txt"}`, "*delete*"},
		{"github_delete_file", "", "*delete*"},
		{"github_delete_file", "not json", "*delete*"},
		{"read_file", `{"path":"delete_me.txt"}`, ""},
		{"run_tool", `{"action":"remove","target":"*delete*"}`, ""},
	}
	for _, c := range cases {
		checkCall(t, d, call(c.name, c.args), c.wantRule)
	}
}

// - Verify Rules returns the defaults first, then the config rules lowercased and trimmed, in that order
func Test_Unit_DenyRules_Rules_Order(t *testing.T) {
	d := newRules(t, []string{"  *MOVE* ", "mv"})
	want := []string{"*delete*", "*remove*", "*move*", "mv"}
	if got := d.Rules(); !slices.Equal(got, want) {
		t.Errorf("Rules() = %q, want %q", got, want)
	}
	if got := newRules(t, nil).Rules(); !slices.Equal(got, defaultDenyRules) {
		t.Errorf("Rules() with no extras = %q, want %q", got, defaultDenyRules)
	}
}

// - Verify changing the slice Rules returns changes neither the rules BeforeTool uses nor a later Rules call
func Test_Unit_DenyRules_Rules_ReturnsCopy(t *testing.T) {
	d := newRules(t, []string{"*move*"})
	got := d.Rules()
	for i := range got {
		got[i] = "nothing_matches_this"
	}
	checkCall(t, d, call("github_delete_file", "{}"), "*delete*")
	checkCall(t, d, call("filesystem_move_file", "{}"), "*move*")
	if again := d.Rules(); !slices.Equal(again, []string{"*delete*", "*remove*", "*move*"}) {
		t.Errorf("Rules() after mutating a copy = %q", again)
	}
}

// - Verify IsBuiltIn is true only for the exact default rules
func Test_Unit_DenyRules_IsBuiltIn(t *testing.T) {
	cases := map[string]bool{
		"*delete*": true,
		"*remove*": true,
		"*move*":   false,
		"*DELETE*": false, // [agent] takes a rule as Rules returns it (lowercased), so no case folding here
		"delete":   false,
		"":         false,
	}
	for rule, want := range cases {
		if got := IsBuiltIn(rule); got != want {
			t.Errorf("IsBuiltIn(%q) = %v, want %v", rule, got, want)
		}
	}
}

// - Verify RuleMatches ignores the tool name's case and uses glob matching
func Test_Unit_DenyRules_RuleMatches(t *testing.T) {
	cases := []struct {
		rule, name string
		want       bool
	}{
		{"*delete*", "github_delete_file", true},
		{"*delete*", "GitHub_DELETE_File", true},
		{"read_file", "READ_FILE", true},
		{"read_file", "read_files", false},
		{"fs_?", "FS_A", true},
		{"*move*", "read_file", false},
	}
	for _, c := range cases {
		if got := RuleMatches(c.rule, c.name); got != c.want {
			t.Errorf("RuleMatches(%q, %q) = %v, want %v", c.rule, c.name, got, c.want)
		}
	}
}

// - Verify BeforeTool denies a tool exactly when some rule from Rules matches it by RuleMatches
func Test_Unit_DenyRules_BeforeToolAgreesWithRuleMatches(t *testing.T) {
	d := newRules(t, []string{"*MOVE*", "fs_?", "read_file"})
	names := []string{"github_delete_file", "Fs_Remove", "filesystem_move_file", "FS_A", "fs_ab", "READ_FILE",
		"read_files", "write_file", "list"}
	for _, name := range names {
		matched := slices.ContainsFunc(d.Rules(), func(rule string) bool { return RuleMatches(rule, name) })
		denied := d.BeforeTool(context.Background(), call(name, "{}")) != nil
		if matched != denied {
			t.Errorf("%q: RuleMatches says matched=%v, BeforeTool says denied=%v", name, matched, denied)
		}
	}
}
