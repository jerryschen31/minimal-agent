package worker

import (
	"slices"
	"testing"

	"github.com/jerryschen31/minimal-agent/mvp4/config"
)

// - Verify the worker process gets worker mode plus the merged worker settings, the config file only
// when there is one, and -debug only when it is on
func Test_ChildArgs(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.Model, cfg.MaxSteps, cfg.WorkDir = "small-model", 7, "/tmp/work"
	want := []string{"-mode", "worker", "-workdir", "/tmp/work", "-model", "small-model", "-maxsteps", "7"}
	if got := childArgs(cfg); !slices.Equal(got, want) {
		t.Errorf("childArgs = %q, want %q", got, want)
	}
	cfg.ConfigFile, cfg.Debug = "my.json", true
	want = append(want, "-config", "my.json", "-debug")
	if got := childArgs(cfg); !slices.Equal(got, want) {
		t.Errorf("childArgs = %q, want %q", got, want)
	}
}
