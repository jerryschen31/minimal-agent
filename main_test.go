package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jerryschen31/minimal-agent/agent"
	"github.com/jerryschen31/minimal-agent/config"
)

// - Verify exitCode: nil 0, a (wrapped) cancel 130, a (wrapped) ErrNeedsPerson 3, any other error 1
func Test_Unit_ExitCode(t *testing.T) {
	stderr := os.Stderr
	os.Stderr, _ = os.Open(os.DevNull)
	defer func() { os.Stderr = stderr }()
	tests := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{fmt.Errorf("run 0001 interrupted: %w", context.Canceled), exitInterrupted},
		{fmt.Errorf("run 0001 stopped (waiting): %w", agent.ErrNeedsPerson), exitNeedsPerson},
		{errors.New("run 0001 stopped (task_failed): t1 not done"), exitFailed},
	}
	for _, tt := range tests {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

//   - Verify the config "Run now?" builds for the second agent (loadConfig of config.FlagsForAuto) is auto mode's: the
//     plan file, -yes, auto's ask rules and maxTaskAttempts, no objective, and the same config file's settings
func Test_Unit_LoadConfig_RunNowIsAutoMode(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"model": "my-model"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	flags, err := config.ParseFlags([]string{"-config", cfgPath, "-mode", "plan", "-objective", "goal.md", "-y"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	planCfg, err := loadConfig(flags)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(config.FlagsForAuto(flags, "goal.plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	auto := config.GetDefaultConfig(config.ModeAuto)
	if cfg.AgentMode != config.ModeAuto || cfg.PlanFile != "goal.plan.md" || !cfg.AssumeYes || cfg.ObjectiveFile != "" {
		t.Errorf("mode %q, plan %q, yes %v, objective %q", cfg.AgentMode, cfg.PlanFile, cfg.AssumeYes, cfg.ObjectiveFile)
	}
	if fmt.Sprint(cfg.AskRules) != fmt.Sprint(auto.AskRules) || cfg.MaxTaskAttempts != auto.MaxTaskAttempts || cfg.SystemPrompt == planCfg.SystemPrompt {
		t.Errorf("not auto's defaults: ask rules %v, attempts %d, plan mode's system prompt %v", cfg.AskRules, cfg.MaxTaskAttempts, cfg.SystemPrompt == planCfg.SystemPrompt)
	}
	if cfg.Model != "my-model" {
		t.Errorf("model %q, want the config file's", cfg.Model)
	}
}
