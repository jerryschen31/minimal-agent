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
	cfg := GetDefaultConfig()
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
	cfg := GetDefaultConfig()
	cfg.Debug = true // as if "debug": true came from the config file

	if !FlagsOverlay(cfg, Flags{}).Debug {
		t.Errorf("expected Debug to stay true when -debug is not passed")
	}
	if FlagsOverlay(cfg, Flags{Debug: new(false)}).Debug {
		t.Errorf("expected -debug=false to win over the config file")
	}
}
