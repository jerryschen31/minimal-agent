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

// exampleRules mirrors the config defaults (config.GetDefaultConfig); hooks has no defaults of its own, so tests
// pass them explicitly.
var exampleRules = []string{"*delete*", "*remove*"}

// newRules builds ToolRules with the given deny rules and no ask rules, and fails the test on a constructor error.
func newRules(t *testing.T, rulesIn []string) *ToolRules {
	t.Helper()
	d, err := NewToolRules(rulesIn, nil)
	if err != nil {
		t.Fatalf("NewToolRules(%q, nil): %v", rulesIn, err)
	}
	return d
}

// checkCall asserts that tool name is denied by wantRule, or allowed when wantRule is "".
func checkCall(t *testing.T, d *ToolRules, tc model.ToolCall, wantRule string) {
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

// - Verify the rules given are the rules enforced: *delete* and *remove* deny matching names, any case, and allow the rest
func Test_Unit_ToolRules_Deny_GivenRulesEnforced(t *testing.T) {
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
	d := newRules(t, exampleRules)
	for _, c := range cases {
		checkCall(t, d, call(c.name, "{}"), c.wantRule)
	}
}

// - Verify nil and an empty list both mean no rules: DenyRules is empty and BeforeTool allows every name
func Test_Unit_ToolRules_Deny_NoRules_AllowsEverything(t *testing.T) {
	for _, rulesIn := range [][]string{nil, {}} {
		d := newRules(t, rulesIn)
		if got := d.DenyRules(); len(got) != 0 {
			t.Errorf("NewToolRules(%#v, nil).DenyRules() = %q, want none", rulesIn, got)
		}
		for _, name := range []string{"github_delete_file", "fs_remove_dir", "filesystem_move_file", "read_file", ""} {
			checkCall(t, d, call(name, "{}"), "")
		}
	}
}

// - Verify only the rules given apply: a list without *delete* and *remove* adds nothing implicit
func Test_Unit_ToolRules_Deny_OnlyGivenRules(t *testing.T) {
	d := newRules(t, []string{"*rename*"})
	checkCall(t, d, call("filesystem_rename_file", "{}"), "*rename*")
	checkCall(t, d, call("github_delete_file", "{}"), "")
	checkCall(t, d, call("fs_remove_dir", "{}"), "")
	checkCall(t, d, call("read_file", "{}"), "")
	if got, want := d.DenyRules(), []string{"*rename*"}; !slices.Equal(got, want) {
		t.Errorf("DenyRules() = %q, want %q", got, want)
	}
}

// - Verify a repeated rule (after trimming and lowercasing) is kept once, at its first position
func Test_Unit_ToolRules_Deny_DuplicatesSkipped(t *testing.T) {
	d := newRules(t, []string{"*move*", "mv", " *MOVE* ", "mv"})
	if got, want := d.DenyRules(), []string{"*move*", "mv"}; !slices.Equal(got, want) {
		t.Errorf("DenyRules() = %q, want %q", got, want)
	}
}

func Test_Unit_ToolRules_Deny_CaseInsensitive(t *testing.T) {
	d := newRules(t, []string{"*MOVE*"})
	checkCall(t, d, call("filesystem_move_file", "{}"), "*move*")
	checkCall(t, d, call("Filesystem_MOVE_File", "{}"), "*move*")
}

func Test_Unit_ToolRules_Deny_BareName(t *testing.T) {
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

func Test_Unit_ToolRules_Deny_FirstMatchWins(t *testing.T) {
	d := newRules(t, []string{"*delete*", "fs_*"})
	checkCall(t, d, call("fs_delete_x", "{}"), "*delete*") // the earlier rule wins
	checkCall(t, d, call("fs_list", "{}"), "fs_*")
}

func Test_Unit_ToolRules_Deny_InvalidRules(t *testing.T) {
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
			d, err := NewToolRules(c.extra, nil)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if d != nil {
				t.Errorf("want nil ToolRules on error, got %+v", d)
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
		d, err := NewToolRules([]string{"run_command:rm"}, nil)
		if err == nil || !strings.Contains(err.Error(), "run_command") {
			t.Fatalf("want error mentioning run_command, got %v", err)
		}
		if d != nil {
			t.Errorf("want nil ToolRules on error, got %+v", d)
		}
	})
}

// - Verify two instances keep their own rules, and NewToolRules does not change the slice it was given
func Test_Unit_ToolRules_Deny_SeparateInstancesDontShareRules(t *testing.T) {
	inA := []string{" AAA "}
	inB := []string{"bbb"}

	a := newRules(t, inA)
	b := newRules(t, inB)

	checkCall(t, a, call("aaa", "{}"), "aaa")
	checkCall(t, a, call("bbb", "{}"), "")
	checkCall(t, b, call("bbb", "{}"), "bbb")
	checkCall(t, b, call("aaa", "{}"), "")

	if !slices.Equal(inA, []string{" AAA "}) || !slices.Equal(inB, []string{"bbb"}) {
		t.Errorf("input slices changed: got %q and %q", inA, inB)
	}

	// [agent] a shared backing array is the bug to catch: append into one input's spare capacity must not leak
	shared := make([]string, 1, 4)
	shared[0] = "ccc"
	c := newRules(t, shared)
	d := newRules(t, append(shared, "ddd"))
	checkCall(t, c, call("ddd", "{}"), "")
	checkCall(t, d, call("ddd", "{}"), "ddd")
}

func Test_Unit_ToolRules_Deny_ArgumentsIgnored(t *testing.T) {
	d := newRules(t, exampleRules)
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

// - Verify DenyRules returns exactly the rules given, lowercased and trimmed, in the order given
func Test_Unit_ToolRules_Deny_Rules_Order(t *testing.T) {
	d := newRules(t, []string{"  *MOVE* ", "mv", "*Delete*"})
	want := []string{"*move*", "mv", "*delete*"}
	if got := d.DenyRules(); !slices.Equal(got, want) {
		t.Errorf("DenyRules() = %q, want %q", got, want)
	}
	if got := newRules(t, exampleRules).DenyRules(); !slices.Equal(got, exampleRules) {
		t.Errorf("DenyRules() = %q, want %q", got, exampleRules)
	}
}

// - Verify changing the slice DenyRules returns changes neither the rules BeforeTool uses nor a later DenyRules call
func Test_Unit_ToolRules_Deny_Rules_ReturnsCopy(t *testing.T) {
	d := newRules(t, []string{"*delete*", "*remove*", "*move*"})
	got := d.DenyRules()
	for i := range got {
		got[i] = "nothing_matches_this"
	}
	checkCall(t, d, call("github_delete_file", "{}"), "*delete*")
	checkCall(t, d, call("filesystem_move_file", "{}"), "*move*")
	if again := d.DenyRules(); !slices.Equal(again, []string{"*delete*", "*remove*", "*move*"}) {
		t.Errorf("DenyRules() after mutating a copy = %q", again)
	}
}

// - Verify RuleMatches ignores the tool name's case and uses glob matching
func Test_Unit_ToolRules_Deny_RuleMatches(t *testing.T) {
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

// - Verify BeforeTool denies a tool exactly when some rule from DenyRules matches it by RuleMatches
func Test_Unit_ToolRules_Deny_BeforeToolAgreesWithRuleMatches(t *testing.T) {
	d := newRules(t, []string{"*MOVE*", "fs_?", "read_file"})
	names := []string{"github_delete_file", "Fs_Remove", "filesystem_move_file", "FS_A", "fs_ab", "READ_FILE",
		"read_files", "write_file", "list"}
	for _, name := range names {
		matched := slices.ContainsFunc(d.DenyRules(), func(rule string) bool { return RuleMatches(rule, name) })
		denied := d.BeforeTool(context.Background(), call(name, "{}")) != nil
		if matched != denied {
			t.Errorf("%q: RuleMatches says matched=%v, BeforeTool says denied=%v", name, matched, denied)
		}
	}
}

// - Verify * and ? match across '/': a tool name is an opaque string, so a server label like "team/fs" can't put a
// delete tool out of reach of "*delete*" (path.Match alone stops * at '/')
func Test_Unit_ToolRules_Deny_SlashInToolName(t *testing.T) {
	cases := []struct {
		rule, name string
		want       bool
	}{
		{"*delete*", "team/fs_delete_file", true},
		{"*delete*", "Team/FS_Delete_File", true},
		{"*", "a/b/c", true},
		{"team/*", "team/fs_write_file", true},
		{"team/fs_write_file", "team/fs_write_file", true}, // a literal '/' in a rule still matches '/'
		{"team?fs_write_file", "team/fs_write_file", true}, // ? matches '/' too
		{"team/*", "teamx_write_file", false},
		{"*delete*", "team/fs_move_file", false},
	}
	for _, c := range cases {
		if got := RuleMatches(c.rule, c.name); got != c.want {
			t.Errorf("RuleMatches(%q, %q) = %v, want %v", c.rule, c.name, got, c.want)
		}
	}

	d, err := NewToolRules(exampleRules, nil)
	if err != nil {
		t.Fatalf("NewToolRules: %v", err)
	}
	call := model.ToolCall{ID: "call_1"}
	call.Function.Name = "team/fs_delete_file"
	if err := d.BeforeTool(context.Background(), call); err == nil {
		t.Errorf("BeforeTool(%q) = nil, want denied by the *delete* rule", call.Function.Name)
	}
}

// ---- ask rules and levels ----

// newToolRules builds ToolRules from deny and ask lists and fails the test on a constructor error.
func newToolRules(t *testing.T, deny, ask []string) *ToolRules {
	t.Helper()
	r, err := NewToolRules(deny, ask)
	if err != nil {
		t.Fatalf("NewToolRules(%q, %q): %v", deny, ask, err)
	}
	return r
}

// - Verify Level gives deny, ask or run with the rule that set it, and a name matching both a deny and an ask rule is
// denied whichever list names it first or however the rules are ordered
func Test_Unit_ToolRules_Level_DenyWinsOverAsk(t *testing.T) {
	cases := []struct {
		desc      string
		deny, ask []string
		name      string
		wantLevel string
		wantRule  string
	}{
		{"deny only", []string{"*push*"}, nil, "git_push", LevelDeny, "*push*"},
		{"ask only", nil, []string{"*delete*"}, "fs_delete_file", LevelAsk, "*delete*"},
		{"no match runs", []string{"*push*"}, []string{"*delete*"}, "read_file", LevelRun, ""},
		{"both: specific deny, broad ask", []string{"fs_delete_file"}, []string{"*delete*"}, "fs_delete_file", LevelDeny, "fs_delete_file"},
		{"both: broad deny, specific ask", []string{"*delete*"}, []string{"fs_delete_file"}, "fs_delete_file", LevelDeny, "*delete*"},
		{"both: same rule in each list", []string{"*delete*"}, []string{"*delete*"}, "FS_Delete_File", LevelDeny, "*delete*"},
		{"ask matches, deny list has another rule", []string{"*push*"}, []string{"*delete*"}, "fs_delete_file", LevelAsk, "*delete*"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			r := newToolRules(t, c.deny, c.ask)
			level, rule := r.Level(c.name)
			if level != c.wantLevel || rule != c.wantRule {
				t.Errorf("Level(%q) = (%q, %q), want (%q, %q)", c.name, level, rule, c.wantLevel, c.wantRule)
			}
		})
	}
}

// - Verify BeforeTool agrees with Level: a deny match is `denied by rule "r"` and does NOT wrap ErrNeedsApproval, an ask
// match wraps ErrNeedsApproval with the exact text `needs approval by rule "r"`, and no match returns nil
func Test_Unit_ToolRules_BeforeTool_ErrorsPerLevel(t *testing.T) {
	r := newToolRules(t, []string{"*push*", "fs_delete_file"}, []string{"*delete*", "write_file"})
	cases := []struct {
		name         string
		wantText     string // "" = allowed
		wantApproval bool
	}{
		{"git_push", `denied by rule "*push*"`, false},
		{"fs_delete_file", `denied by rule "fs_delete_file"`, false}, // also matches the ask rule *delete*: deny wins
		{"github_delete_branch", `needs approval by rule "*delete*"`, true},
		{"WRITE_FILE", `needs approval by rule "write_file"`, true},
		{"read_file", "", false},
	}
	for _, c := range cases {
		err := r.BeforeTool(context.Background(), call(c.name, "{}"))
		if c.wantText == "" {
			if err != nil {
				t.Errorf("%q: want allowed, got %v", c.name, err)
			}
			continue
		}
		if err == nil || err.Error() != c.wantText {
			t.Errorf("%q: want error %q, got %v", c.name, c.wantText, err)
			continue
		}
		if got := errors.Is(err, ErrNeedsApproval); got != c.wantApproval {
			t.Errorf("%q: errors.Is(err, ErrNeedsApproval) = %v, want %v", c.name, got, c.wantApproval)
		}
	}
}

// - Verify ask rules are lowercased, trimmed and de-duplicated like deny rules, and the first matching ask rule wins
func Test_Unit_ToolRules_Ask_NormalizedAndFirstMatchWins(t *testing.T) {
	r := newToolRules(t, nil, []string{" *DELETE* ", "fs_*", "*delete*"})
	if got, want := r.AskRules(), []string{"*delete*", "fs_*"}; !slices.Equal(got, want) {
		t.Errorf("AskRules() = %q, want %q", got, want)
	}
	if got := r.DenyRules(); len(got) != 0 {
		t.Errorf("DenyRules() = %q, want none", got)
	}
	if level, rule := r.Level("fs_delete_x"); level != LevelAsk || rule != "*delete*" {
		t.Errorf("Level(fs_delete_x) = (%q, %q), want (ask, *delete*)", level, rule)
	}
	if level, rule := r.Level("fs_list"); level != LevelAsk || rule != "fs_*" {
		t.Errorf("Level(fs_list) = (%q, %q), want (ask, fs_*)", level, rule)
	}
}

// - Verify an invalid rule fails construction with an error that names its list ("deny" or "ask") and the rule
func Test_Unit_ToolRules_InvalidRule_NamesTheList(t *testing.T) {
	cases := []struct {
		desc       string
		deny, ask  []string
		wantPrefix string
	}{
		{"bad deny", []string{"["}, []string{"*delete*"}, `invalid deny rule "["`},
		{"bad ask", []string{"*push*"}, []string{"*delete*", "fs_["}, `invalid ask rule "fs_["`},
		{"ask tool:program form", nil, []string{"run_command:rm"}, `invalid ask rule "run_command:rm"`},
		{"both bad: deny reported", []string{"["}, []string{"]["}, `invalid deny rule "["`},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			r, err := NewToolRules(c.deny, c.ask)
			if err == nil || !strings.HasPrefix(err.Error(), c.wantPrefix) {
				t.Fatalf("want error starting %q, got %v", c.wantPrefix, err)
			}
			if r != nil {
				t.Errorf("want nil ToolRules on error, got %+v", r)
			}
		})
	}
}

// - Verify DenyRules and AskRules return copies: changing them changes neither the rules in use nor a later call
func Test_Unit_ToolRules_RuleLists_ReturnCopies(t *testing.T) {
	r := newToolRules(t, []string{"*push*"}, []string{"*delete*"})
	deny, ask := r.DenyRules(), r.AskRules()
	deny[0], ask[0] = "nothing_matches_this", "nothing_matches_this"

	if level, _ := r.Level("git_push"); level != LevelDeny {
		t.Errorf("Level(git_push) = %q after changing a copy, want deny", level)
	}
	if level, _ := r.Level("fs_delete_file"); level != LevelAsk {
		t.Errorf("Level(fs_delete_file) = %q after changing a copy, want ask", level)
	}
	if got := r.DenyRules(); !slices.Equal(got, []string{"*push*"}) {
		t.Errorf("DenyRules() = %q after changing a copy", got)
	}
	if got := r.AskRules(); !slices.Equal(got, []string{"*delete*"}) {
		t.Errorf("AskRules() = %q after changing a copy", got)
	}
}
