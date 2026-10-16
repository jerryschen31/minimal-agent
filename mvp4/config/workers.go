package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

// Worker types (orchestrator mode). See PLAN-orchestrator-worker-types.md.

// DefaultWorkerType is the worker type used when the config has no "workers" section.
const DefaultWorkerType = "general"

// WorkerType is one entry of the "workers" section: a kind of worker, how many processes of it to start,
// and the file whose settings make it different from the top-level config.
type WorkerType struct {
	Type        string `json:"type"`        // the name the director uses in set_tasks' worker_type, e.g. "engineer"
	Count       int    `json:"count"`       // worker processes to start (0 or missing = 1); each runs one task at a time
	Config      string `json:"config"`      // optional: a partial config file applied over the top-level config (relative to this config file)
	Description string `json:"description"` // optional: what this type is for, shown to the director
}

// workerFileForbidden are the keys a worker-type file may not set: they belong to the run, not to a worker.
var workerFileForbidden = []string{"agentMode", "objectiveFile", "timeout", "workDir", "director", "workers"}

var workerTypeName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// WorkerPool returns the worker types to start in orchestrator mode, in order (the first is the default
// type), with every count at least 1. Without a "workers" section it is director.maxParallel workers of
// DefaultWorkerType, configured by the top-level config alone (the slice D behaviour).
func (cfg Config) WorkerPool() []WorkerType {
	if len(cfg.Workers) == 0 {
		return []WorkerType{{Type: DefaultWorkerType, Count: max(cfg.Director.MaxParallel, 1)}}
	}
	pool := make([]WorkerType, len(cfg.Workers))
	for i, w := range cfg.Workers {
		w.Count = max(w.Count, 1)
		pool[i] = w
	}
	return pool
}

// validateWorkers checks the "workers" section of an orchestrator config.
func validateWorkers(cfg Config) error {
	if len(cfg.Workers) == 0 {
		return nil
	}
	// [agent] with worker types, the counts say how many tasks can run at once; a second knob for the same
	// thing would only invite contradictions
	if cfg.Director.MaxParallel > 1 {
		return fmt.Errorf(`director.maxParallel can't be combined with "workers": set each worker type's "count" instead`)
	}
	seen := map[string]bool{}
	for i, w := range cfg.Workers {
		switch {
		case !workerTypeName.MatchString(w.Type):
			return fmt.Errorf("workers[%d]: type %q must be a name like \"engineer\" (letters, digits, - and _)", i, w.Type)
		case seen[w.Type]:
			return fmt.Errorf("workers: type %q is listed twice", w.Type)
		case w.Count < 0:
			return fmt.Errorf("workers: type %q has a negative count %d", w.Type, w.Count)
		}
		seen[w.Type] = true
	}
	return nil
}

// resolveWorkerFiles makes each worker type's relative "config" path relative to the main config file's
// directory, so "worker-tester.json" next to the main config works from any working directory.
func resolveWorkerFiles(cfg *Config, mainFile string) {
	for i, w := range cfg.Workers {
		if w.Config != "" && !filepath.IsAbs(w.Config) {
			cfg.Workers[i].Config = filepath.Join(filepath.Dir(mainFile), w.Config)
		}
	}
}

// ApplyWorkerFile returns cfg with a worker-type file's settings applied over it: only the keys present
// in the file change, so a file holding just {"systemPrompt": "..."} is a complete worker type.
// "builtinTools" and "mcpServers" replace the top-level lists (they are not merged). The run's own keys
// (agentMode, objectiveFile, timeout, workDir, director, workers) are refused.
func ApplyWorkerFile(cfg Config, path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("worker type file: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse worker type file %s: %w", path, err)
	}
	known := jsonKeys(reflect.TypeOf(Config{}))
	for key := range raw {
		lower := strings.ToLower(key)
		for _, f := range workerFileForbidden {
			if lower == strings.ToLower(f) {
				return Config{}, fmt.Errorf("worker type file %s: %q can't be set per worker type; set it in the main config", path, key)
			}
		}
		if !known[lower] {
			return Config{}, fmt.Errorf("worker type file %s: unrecognized field %q", path, key)
		}
		if lower == "mcpservers" {
			cfg.McpServers = nil // replace, don't merge (and never write into the map the caller's config shares)
		}
		if lower == "builtintools" {
			cfg.BuiltinTools = nil // likewise: decoding into the shared backing array would overwrite the caller's slice
		}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse worker type file %s: %w", path, err)
	}
	return cfg, nil
}
