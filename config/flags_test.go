package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// - Verify -workdir (single or double dash) accepts an existing directory and is recorded as passed
func Test_Unit_ParseFlags_WorkDir_ExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, flagName := range []string{"-workdir", "--workdir"} {
		flags, err := ParseFlags([]string{flagName, dir}, io.Discard)
		if err != nil {
			t.Fatalf("%s %s: unexpected error: %v", flagName, dir, err)
		}
		if flags.WorkDir == nil || *flags.WorkDir != dir {
			t.Errorf("%s: expected WorkDir %q, got %v", flagName, dir, flags.WorkDir)
		}
	}
}

// - Verify an unspecified -workdir stays nil, so it never overrides the config file
func Test_Unit_ParseFlags_WorkDir_Unspecified_IsNil(t *testing.T) {
	flags, err := ParseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flags.WorkDir != nil {
		t.Errorf("expected nil WorkDir, got %q", *flags.WorkDir)
	}
}

// - Verify -workdir rejects a missing path, a plain file, and an empty value
func Test_Unit_ParseFlags_WorkDir_Invalid_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", file, err)
	}
	cases := map[string]struct {
		value   string
		wantErr string
	}{
		"missing": {filepath.Join(dir, "missing"), "no such file"},
		"a file":  {file, "not a directory"},
		"empty":   {"", "must not be empty"},
		"blank":   {"   ", "must not be empty"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFlags([]string{"-workdir", tc.value}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// - Verify FlagsOverlay lets -workdir override the config value, and leaves it alone when not passed
func Test_Unit_FlagsOverlay_WorkDir(t *testing.T) {
	cfg := GetDefaultConfig(ModeChat)
	cfg.WorkDir = "from-config"

	if got := FlagsOverlay(cfg, Flags{}).WorkDir; got != "from-config" {
		t.Errorf("expected WorkDir to stay %q when -workdir is not passed, got %q", "from-config", got)
	}
	override := "from-flag"
	if got := FlagsOverlay(cfg, Flags{WorkDir: &override}).WorkDir; got != "from-flag" {
		t.Errorf("expected -workdir to win over the config file, got %q", got)
	}
}

// - Verify -debug is nil when not passed, true when passed, and false with -debug=false (so a flag
// can switch off "debug": true from the config file)
func Test_Unit_ParseFlags_Debug(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want *bool
	}{
		{"not passed", nil, nil},
		{"-debug", []string{"-debug"}, new(true)},
		{"--debug", []string{"--debug"}, new(true)},
		{"-debug=false", []string{"-debug=false"}, new(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags, err := ParseFlags(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			switch {
			case tc.want == nil && flags.Debug != nil:
				t.Errorf("expected nil Debug, got %v", *flags.Debug)
			case tc.want != nil && (flags.Debug == nil || *flags.Debug != *tc.want):
				t.Errorf("expected Debug %v, got %v", *tc.want, flags.Debug)
			}
		})
	}
}

// - Verify FlagsOverlay lets -debug override the config value, and leaves it alone when not passed
func Test_Unit_FlagsOverlay_Debug(t *testing.T) {
	cfg := GetDefaultConfig(ModeChat)
	cfg.Debug = true // as if "debug": true came from the config file

	if !FlagsOverlay(cfg, Flags{}).Debug {
		t.Errorf("expected Debug to stay true when -debug is not passed")
	}
	if FlagsOverlay(cfg, Flags{Debug: new(false)}).Debug {
		t.Errorf("expected -debug=false to win over the config file")
	}
}

// - Verify -mode info parses on its own, and rejects the flags that only other modes use
func Test_Unit_ParseFlags_ModeInfo(t *testing.T) {
	mission := filepath.Join(t.TempDir(), "mission.md")
	if err := os.WriteFile(mission, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", mission, err)
	}
	cases := []struct {
		name    string
		args    []string
		wantErr string // "" means no error expected
	}{
		{"alone", []string{"-mode", "info"}, ""},
		{"with --json", []string{"-mode", "info", "--json"}, "--json only applies"},
		{"with -q", []string{"-mode", "info", "-q", "x"}, "-query only applies"},
		{"with -mission", []string{"-mode", "info", "-mission", mission}, "-mission only applies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags, err := ParseFlags(tc.args, io.Discard)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if flags.Mode == nil || *flags.Mode != ModeInfo {
					t.Errorf("expected Mode %q, got %v", ModeInfo, flags.Mode)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// - Verify the unknown-mode error lists info among the valid modes
func Test_Unit_ParseFlags_UnknownMode_ListsInfo(t *testing.T) {
	_, err := ParseFlags([]string{"-mode", "bogus"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "|info") {
		t.Errorf("expected the error to list info, got %v", err)
	}
}

// - Verify ValidateMode checks the effective mode (from the config file or -mode) against the flags given
func Test_Unit_ValidateMode(t *testing.T) {
	withMode := func(mode string) Config {
		cfg := GetDefaultConfig(ModeChat)
		cfg.AgentMode = mode
		return cfg
	}
	cases := []struct {
		name    string
		cfg     Config
		flags   Flags
		wantErr string // "" means no error expected
	}{
		{"config info with --json", withMode(ModeInfo), Flags{JSON: new(true)}, "--json only applies"},
		{"config info with -q", withMode(ModeInfo), Flags{Query: new("x")}, "-query only applies"},
		{"config info with -mission", withMode(ModeInfo), Flags{Mission: new("mission.md")}, "-mission only applies"},
		{"config info alone", withMode(ModeInfo), Flags{}, ""},
		{"config chat alone", withMode(ModeChat), Flags{}, ""},
		{"config plan alone", withMode(ModePlan), Flags{}, ""},
		{"config plan with -objective", withMode(ModePlan), Flags{Objective: new("draft.md")}, ""},
		{"config chat with -objective", withMode(ModeChat), Flags{Objective: new("draft.md")}, "-objective only applies"},
		{"config plan with -yes", withMode(ModePlan), Flags{Yes: new(true)}, ""},
		{"config chat with -yes", withMode(ModeChat), Flags{Yes: new(true)}, "-yes only applies"},
		{"config info with -yes=false", withMode(ModeInfo), Flags{Yes: new(false)}, "-yes only applies"},
		{"config bogus", withMode("bogus"), Flags{}, "-mode must be one of"},
		{"-mode info overlaid, with --json", FlagsOverlay(GetDefaultConfig(ModeChat), Flags{Mode: new(ModeInfo)}),
			Flags{Mode: new(ModeInfo), JSON: new(true)}, "--json only applies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMode(tc.cfg, tc.flags)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// - Verify -approve is nil when not passed and records ask or allow-all (single or double dash) when passed
func Test_Unit_ParseFlags_Approve_Valid(t *testing.T) {
	flags, err := ParseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flags.Approve != nil {
		t.Errorf("expected nil Approve when not passed, got %q", *flags.Approve)
	}
	for _, args := range [][]string{{"-approve", "ask"}, {"-approve", "allow-all"}, {"--approve=allow-all"}} {
		flags, err := ParseFlags(args, io.Discard)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", args, err)
		}
		want := args[len(args)-1]
		if i := strings.Index(want, "="); i >= 0 {
			want = want[i+1:]
		}
		if flags.Approve == nil || *flags.Approve != want {
			t.Errorf("%q: expected Approve %q, got %v", args, want, flags.Approve)
		}
	}
}

// - Verify -approve rejects anything but ask or allow-all (including other cases and an empty value), naming the choices
func Test_Unit_ParseFlags_Approve_Invalid(t *testing.T) {
	for _, value := range []string{"bogus", "ALLOW-ALL", "allow_all", "Ask", "deny", "", "   "} {
		t.Run(value, func(t *testing.T) {
			flags, err := ParseFlags([]string{"-approve", value}, io.Discard)
			if err == nil {
				t.Fatalf("expected an error for -approve %q, got Approve %v", value, flags.Approve)
			}
			if !strings.Contains(err.Error(), "approve") {
				t.Errorf("expected the error to name -approve, got %v", err)
			}
		})
	}
	_, err := ParseFlags([]string{"-approve", "bogus"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ask") || !strings.Contains(err.Error(), "allow-all") || !strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("expected the error to list ask and allow-all and quote the value, got %v", err)
	}
}

// - Verify FlagsOverlay lets -approve override the config value, and leaves it alone when not passed
func Test_Unit_FlagsOverlay_Approve(t *testing.T) {
	cfg := GetDefaultConfig(ModeChat)
	cfg.Approval = ApprovalAllowAll // as if "approval": "allow-all" came from the config file

	if got := FlagsOverlay(cfg, Flags{}).Approval; got != ApprovalAllowAll {
		t.Errorf("expected Approval to stay %q when -approve is not passed, got %q", ApprovalAllowAll, got)
	}
	if got := FlagsOverlay(cfg, Flags{Approve: new(ApprovalAsk)}).Approval; got != ApprovalAsk {
		t.Errorf("expected -approve ask to win over the config file, got %q", got)
	}
	if got := FlagsOverlay(GetDefaultConfig(ModeChat), Flags{Approve: new(ApprovalAllowAll)}).Approval; got != ApprovalAllowAll {
		t.Errorf("expected -approve allow-all to win over the default, got %q", got)
	}
}

// - Verify -objective/-obj (one flag) accepts an existing file or plain text, and FlagsOverlay copies it
// - Verify -yes/-y (one flag): plan mode only, =false accepted, and FlagsOverlay copies it to AssumeYes
func Test_Unit_ParseFlags_PlanMode(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "draft.md")
	if err := os.WriteFile(draft, []byte("# Objective\nx\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", draft, err)
	}

	for _, name := range []string{"-objective", "-obj", "--obj"} {
		flags, err := ParseFlags([]string{"-mode", "plan", name, draft, "-yes"}, io.Discard)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if flags.Objective == nil || *flags.Objective != draft || flags.Yes == nil || !*flags.Yes {
			t.Fatalf("%s: Objective %v, Yes %v", name, flags.Objective, flags.Yes)
		}
		cfg := FlagsOverlay(GetDefaultConfig(ModeChat), flags)
		if cfg.AgentMode != ModePlan || cfg.ObjectiveFile != draft || !cfg.AssumeYes {
			t.Errorf("%s: overlay gave mode %q, ObjectiveFile %q, AssumeYes %v", name, cfg.AgentMode, cfg.ObjectiveFile, cfg.AssumeYes)
		}
	}

	// plan mode alone is fine: it asks for a new objective at the prompt
	flags, err := ParseFlags([]string{"-mode", "plan"}, io.Discard)
	if err != nil || flags.Objective != nil || flags.Yes != nil {
		t.Errorf("-mode plan alone: flags %+v, err %v", flags, err)
	}
	if cfg := FlagsOverlay(GetDefaultConfig(ModePlan), flags); cfg.AssumeYes || cfg.ObjectiveFile != "" {
		t.Errorf("-mode plan alone: overlay gave AssumeYes %v, ObjectiveFile %q", cfg.AssumeYes, cfg.ObjectiveFile)
	}

	cases := map[string]struct {
		args    []string
		wantErr string
	}{
		"empty objective":    {[]string{"-mode", "plan", "-objective", " "}, "must not be empty"},
		"different values":   {[]string{"-mode", "plan", "-objective", draft, "-obj", "other.md"}, "different values"},
		"objective in chat":  {[]string{"-mode", "chat", "-obj", draft}, "-objective only applies"},
		"yes in info":        {[]string{"-mode", "info", "-yes"}, "-yes only applies"},
		"y in chat":          {[]string{"-mode", "chat", "-y"}, "-yes only applies"},
		"yes and y disagree": {[]string{"-mode", "plan", "-yes", "-y=false"}, "different values"},
		"json in plan":       {[]string{"-mode", "plan", "--json"}, "--json only applies"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFlags(tc.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// - Verify -objective/-obj accepts text that names no file (the objective's text), and a directory path, as given
func Test_Unit_ParseFlags_Objective_TextAccepted(t *testing.T) {
	dir := t.TempDir()
	for _, value := range []string{"Write a word counter in Go", filepath.Join(dir, "nope.md"), dir} {
		flags, err := ParseFlags([]string{"-mode", "plan", "-obj", value}, io.Discard)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", value, err)
		}
		if flags.Objective == nil || *flags.Objective != value {
			t.Errorf("%q: Objective %v", value, flags.Objective)
		}
		if got := FlagsOverlay(GetDefaultConfig(ModePlan), flags).ObjectiveFile; got != value {
			t.Errorf("%q: ObjectiveFile %q", value, got)
		}
	}
}

// - Verify -yes and -y are one flag: nil when not passed, true when passed, false with =false, and both with the same value accepted
func Test_Unit_ParseFlags_Yes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want *bool
	}{
		{"not passed", nil, nil},
		{"-yes", []string{"-yes"}, new(true)},
		{"-y", []string{"-y"}, new(true)},
		{"--yes", []string{"--yes"}, new(true)},
		{"-yes=false", []string{"-yes=false"}, new(false)},
		{"-y=false", []string{"-y=false"}, new(false)},
		{"-yes -y", []string{"-yes", "-y"}, new(true)},
		{"-yes=false -y=false", []string{"-yes=false", "-y=false"}, new(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags, err := ParseFlags(append([]string{"-mode", "plan"}, tc.args...), io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			switch {
			case tc.want == nil && flags.Yes != nil:
				t.Errorf("expected nil Yes, got %v", *flags.Yes)
			case tc.want != nil && (flags.Yes == nil || *flags.Yes != *tc.want):
				t.Errorf("expected Yes %v, got %v", *tc.want, flags.Yes)
			}
		})
	}
}

// - Verify FlagsOverlay sets AssumeYes from -yes, and leaves it alone when not passed
func Test_Unit_FlagsOverlay_Yes(t *testing.T) {
	cfg := GetDefaultConfig(ModePlan)
	if FlagsOverlay(cfg, Flags{}).AssumeYes {
		t.Errorf("expected AssumeYes false when -yes is not passed")
	}
	if !FlagsOverlay(cfg, Flags{Yes: new(true)}).AssumeYes {
		t.Errorf("expected -yes to set AssumeYes")
	}
	cfg.AssumeYes = true
	if FlagsOverlay(cfg, Flags{Yes: new(false)}).AssumeYes {
		t.Errorf("expected -yes=false to clear AssumeYes")
	}
}
