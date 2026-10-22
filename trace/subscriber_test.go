package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

func Test_JSONL_WritesLinesWithOwnerOnlyPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "traces")
	sub, err := NewJSONLSubscriber(dir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`{"a":1}`, `{"b":2}`} {
		if err := sub.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "run-1.jsonl"))
	if err != nil || string(got) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Errorf("file = %q, %v", got, err)
	}
	dirInfo, _ := os.Stat(dir)
	fileInfo, _ := os.Stat(filepath.Join(dir, "run-1.jsonl"))
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("permissions: dir %v, file %v; want 0700 and 0600", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func Test_JSONL_RefusesToReuseAFileOrAPathInTheRunID(t *testing.T) {
	dir := t.TempDir()
	first, err := NewJSONLSubscriber(dir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	if _, err := NewJSONLSubscriber(dir, "run-1"); err == nil {
		t.Error("a second run with the same ID must not open the first run's file")
	}
	for _, id := range []string{"", "../escape", "a/b"} {
		if _, err := NewJSONLSubscriber(dir, id); err == nil {
			t.Errorf("run ID %q should be rejected", id)
		}
	}
}

func Test_SetupRecorder_WritesATraceFile(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.TraceDir = filepath.Join(t.TempDir(), "traces")
	rec, err := SetupRecorder(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record("", RunStart, RunStartData{AgentName: "minagent"})
	rec.Record("", RunEnd, GetRunEndData("ok", "", 5))
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(cfg.TraceDir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("trace files = %v, want exactly one", files)
	}
	data, _ := os.ReadFile(files[0])
	runID := strings.TrimSuffix(filepath.Base(files[0]), ".jsonl")
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"event_type":"run_start"`) || !strings.Contains(lines[1], `"event_type":"run_end"`) {
		t.Errorf("lines = %q", lines)
	}
	if !strings.Contains(lines[0], `"run_id":"`+runID+`"`) {
		t.Errorf("the file name %q and the events' run_id disagree: %s", runID, lines[0])
	}
}

func Test_SetupRecorder_DisabledWritesNothing_UncreatableDirFails(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.TraceDir = filepath.Join(t.TempDir(), "traces")
	cfg.TraceEnabled = false
	rec, err := SetupRecorder(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record("", RunStart, nil)
	rec.Close()
	if _, err := os.Stat(cfg.TraceDir); !os.IsNotExist(err) {
		t.Errorf("tracing is off but %s was created", cfg.TraceDir)
	}

	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	cfg.TraceEnabled = true
	cfg.TraceDir = filepath.Join(blocker, "traces") // a directory can't be created under a regular file
	if _, err := SetupRecorder(t.Context(), cfg); err == nil {
		t.Error("expected an error when the trace directory can't be created")
	}
	cfg.TraceDir = ""
	if _, err := SetupRecorder(t.Context(), cfg); err == nil {
		t.Error("expected an error for an empty traceDir")
	}
}
