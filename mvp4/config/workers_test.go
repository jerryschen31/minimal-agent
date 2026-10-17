package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// - Verify a worker type file changes only the keys it has (a systemPrompt-only file keeps the model and
// tools), replaces mcpServers instead of merging, and refuses the run's keys and unknown keys
func Test_ApplyWorkerFile(t *testing.T) {
	base := GetDefaultConfig()
	base.Model, base.SystemPrompt, base.BuiltinTools = "gpt-5-mini", "You are an engineer.", []string{"ReadFile", "WriteFile"}
	base.McpServers = map[string]McpServerConfig{"fs": {Command: "npx"}}

	tester := writeTempFile(t, "tester.json", []byte(`{"systemPrompt": "You are a tester."}`))
	got, err := ApplyWorkerFile(base, tester)
	if err != nil {
		t.Fatal(err)
	}
	if got.SystemPrompt != "You are a tester." || got.Model != "gpt-5-mini" || len(got.BuiltinTools) != 2 || len(got.McpServers) != 1 {
		t.Errorf("systemPrompt-only file: %+v", got)
	}

	other := writeTempFile(t, "other.json", []byte(`{"model": "big", "builtinTools": ["ReadFile"], "mcpServers": {"wiki": {"type": "http", "url": "https://x"}}}`))
	got, err = ApplyWorkerFile(base, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, merged := got.McpServers["fs"]; got.Model != "big" || len(got.BuiltinTools) != 1 || merged || len(base.McpServers) != 1 {
		t.Errorf("override file: model %s tools %v servers %v (base servers %v)", got.Model, got.BuiltinTools, got.McpServers, base.McpServers)
	}

	for body, want := range map[string]string{
		`{"director": {}}`:                 `"director" can't be set per worker type`,
		`{"workDir": "/"}`:                 `"workDir" can't be set per worker type`,
		`{"systemPromt": "typo"}`:          `unrecognized field "systemPromt"`,
		`not json`:                         "parse worker type file",
		`{"workers": [{"type": "x"}]}`:     `"workers" can't be set`,
		`{"objectiveFile": "o.md", "x":1}`: "can't be set per worker type",
	} {
		if _, err := ApplyWorkerFile(base, writeTempFile(t, "bad.json", []byte(body))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", body, err, want)
		}
	}
}

// - Verify WorkerPool: without "workers", director.maxParallel workers of the default type; with it, the
// listed types in order, counts at least 1
func Test_WorkerPool(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Director.MaxParallel = 3
	if p := cfg.WorkerPool(); len(p) != 1 || p[0].Type != DefaultWorkerType || p[0].Count != 3 {
		t.Errorf("no workers section: %+v", p)
	}
	cfg.Workers = []WorkerType{{Type: "engineer", Count: 2}, {Type: "tester"}}
	if p := cfg.WorkerPool(); len(p) != 2 || p[0].Type != "engineer" || p[0].Count != 2 || p[1].Count != 1 {
		t.Errorf("workers section: %+v", p)
	}
}

// - Verify the "workers" checks in Validate
func Test_Validate_Workers(t *testing.T) {
	orch := GetDefaultConfig()
	orch.AgentMode, orch.ObjectiveFile = ModeOrchestrator, "objective.md"
	two := []WorkerType{{Type: "engineer", Count: 2}, {Type: "tester"}}
	for name, c := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"ok":                {func(c *Config) { c.Workers = two }, ""},
		"with maxParallel":  {func(c *Config) { c.Workers, c.Director.MaxParallel = two, 2 }, "can't be combined"},
		"bad name":          {func(c *Config) { c.Workers = []WorkerType{{Type: "code reviewer"}} }, "must be a name"},
		"duplicate":         {func(c *Config) { c.Workers = []WorkerType{{Type: "a"}, {Type: "a"}} }, "listed twice"},
		"negative count":    {func(c *Config) { c.Workers = []WorkerType{{Type: "a", Count: -1}} }, "negative count"},
		"autonomous":        {func(c *Config) { c.Workers, c.AgentMode = two, ModeAutonomous }, "needs -mode orchestrator"},
		"worker mode is ok": {func(c *Config) { c.Workers, c.AgentMode = two, ModeWorker }, ""},
	} {
		cfg := orch
		c.edit(&cfg)
		err := Validate(cfg)
		if (c.want == "" && err != nil) || (c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want))) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// - Verify a worker type's "config" path is relative to the main config file, and that -mode worker
// -workerconfig loads the main config with the type's file over it
func Test_SetDefaultConfig_WorkerTypes(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.json")
	os.WriteFile(main, []byte(`{"model": "m", "systemPrompt": "engineer", "workers": [{"type": "tester", "config": "tester.json", "cout": 2}]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "tester.json"), []byte(`{"systemPrompt": "tester"}`), 0o644)

	cfg, err := SetDefaultConfig(Flags{Config: &main})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workers[0].Config; got != filepath.Join(dir, "tester.json") {
		t.Errorf("worker type file resolved to %q", got)
	}

	f, err := ParseFlags([]string{"-mode", "worker", "-config", main, "-workerconfig", cfg.Workers[0].Config}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = SetDefaultConfig(f)
	if err != nil || cfg.SystemPrompt != "tester" || cfg.Model != "m" || cfg.WorkerConfigFile == "" {
		t.Errorf("worker config = %q %q %q, %v", cfg.SystemPrompt, cfg.Model, cfg.WorkerConfigFile, err)
	}
	if _, err := ParseFlags([]string{"-mode", "orchestrator", "-config", main, "-workerconfig", main}, io.Discard); err == nil {
		t.Error("-workerconfig outside worker mode: want an error")
	}
}
