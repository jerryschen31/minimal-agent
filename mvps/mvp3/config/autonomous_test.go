package config

import (
	"io"
	"strings"
	"testing"
)

// - Verify a partial "director" block keeps the default director config for the missing fields,
// and an unknown key inside it is reported
func Test_Unit_SetDefaultConfig_DirectorBlock_MergesWithDefaults(t *testing.T) {
	path := writeTempFile(t, "config.json", []byte(`{
		"model": "worker-model",
		"director": {"model": "director-model", "maxTurnPerTask": 3}
	}`))
	var cfg Config
	var err error
	out := captureStdout(t, func() { cfg, err = SetDefaultConfig(Flags{Config: &path}) })
	if err != nil {
		t.Fatal(err)
	}
	want := GetDefaultDirectorConfig()
	want.Model = "director-model"
	if cfg.Director != want {
		t.Errorf("Director = %+v, want %+v", cfg.Director, want)
	}
	if !strings.Contains(out, `"director.maxTurnPerTask"`) {
		t.Errorf("expected a warning for the misspelled director key, got %q", out)
	}
}

// - Verify Validate checks the merged config, including values that came only from a file
func Test_Unit_Validate(t *testing.T) {
	ok := GetDefaultConfig()
	auto := GetDefaultConfig()
	auto.AgentMode, auto.ObjectiveFile = ModeAutonomous, "objective.md"

	cases := map[string]struct {
		edit func(*Config)
		base Config
		want string // "" = valid
	}{
		"chat default":          {func(*Config) {}, ok, ""},
		"autonomous ok":         {func(*Config) {}, auto, ""},
		"unknown mode":          {func(c *Config) { c.AgentMode = "headless" }, ok, "agentMode must be one of"},
		"autonomous no file":    {func(c *Config) { c.ObjectiveFile = "" }, auto, "requires an objective file"},
		"turns per task zero":   {func(c *Config) { c.Director.MaxTurnsPerTask = 0 }, auto, "maxTurnsPerTask"},
		"director steps zero":   {func(c *Config) { c.Director.MaxSteps = 0 }, auto, "director.maxSteps"},
		"max tasks zero":        {func(c *Config) { c.Director.MaxTasks = 0 }, auto, "director.maxTasks"},
		"max iterations zero":   {func(c *Config) { c.Director.MaxIterations = 0 }, auto, "director.maxIterations"},
		"bad timeout":           {func(c *Config) { c.Timeout = "soon" }, auto, "timeout"},
		"negative timeout":      {func(c *Config) { c.Timeout = "-1m" }, auto, "negative"},
		"worker steps zero":     {func(c *Config) { c.MaxSteps = 0 }, ok, "maxSteps"},
		"chat ignores director": {func(c *Config) { c.Director.MaxTurnsPerTask = 0 }, ok, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := tc.base
			tc.edit(&cfg)
			err := Validate(cfg)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("want valid, got %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// - Verify the autonomous flags parse, are refused with another explicit mode, and overlay the config
func Test_Unit_ParseFlags_AutonomousFlags(t *testing.T) {
	obj := writeTempFile(t, "objective.md", []byte("# Objective\nx\n"))
	f, err := ParseFlags([]string{"-mode", "autonomous", "-objective", obj, "-maxturnspertask", "3", "-timeout", "10m", "-directormodel", "big"}, io.Discard)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg := FlagsOverlay(GetDefaultConfig(), f)
	if cfg.AgentMode != ModeAutonomous || cfg.ObjectiveFile != obj || cfg.Director.MaxTurnsPerTask != 3 || cfg.Timeout != "10m" || cfg.Director.Model != "big" {
		t.Errorf("overlaid config: mode=%s objective=%s turns=%d timeout=%s directorModel=%s",
			cfg.AgentMode, cfg.ObjectiveFile, cfg.Director.MaxTurnsPerTask, cfg.Timeout, cfg.Director.Model)
	}

	for _, args := range [][]string{
		{"-mode", "chat", "-objective", obj},
		{"-mode", "chat", "-timeout", "1m"},
		{"-maxturnspertask", "0"},
		{"-timeout", "soon"},
		{"-objective", obj + ".missing"},
		{"-mode", "headless"},
		{"-mode", "autonomous", "-objective", obj, "--json"},
	} {
		if _, err := ParseFlags(args, io.Discard); err == nil {
			t.Errorf("ParseFlags(%q): want an error", args)
		}
	}
}
