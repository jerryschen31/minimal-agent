package trace

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

// renderDigest records events through a real recorder with a digest subscriber and returns the digest file's text.
func renderDigest(t *testing.T, record func(rec Recorder)) (digest, jsonl string, dir string) {
	t.Helper()
	cfg := config.GetDefaultConfig()
	cfg.TraceDir = filepath.Join(t.TempDir(), "traces")
	rec, err := SetupRecorder(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	record(rec)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	d, _ := filepath.Glob(filepath.Join(cfg.TraceDir, "*.digest.md"))
	j, _ := filepath.Glob(filepath.Join(cfg.TraceDir, "*.jsonl"))
	if len(d) != 1 || len(j) != 1 {
		t.Fatalf("want one digest and one jsonl, got %v %v", d, j)
	}
	db, _ := os.ReadFile(d[0])
	jb, _ := os.ReadFile(j[0])
	return string(db), string(jb), cfg.TraceDir
}

func Test_Digest_GroupsToolCallsPerTask_AndShowsTextOnlyTurns(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		defer run.End()

		task := StartTask(rec, "fix script2.py")
		task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "read_file", Arguments: `{"path":"script1.py"}`})
		task.Record(ToolRequested, ToolRequestedData{CallID: "c2", Name: "read_file", Arguments: `{"path":".env"}`})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c2", Name: "read_file", Content: "error: access denied", Failed: true})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c1", Name: "read_file", Content: "ok"})
		task.Record(ToolRequested, ToolRequestedData{CallID: "c3", Name: "lint", Arguments: `{"files":["a.py"]}`})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c3", Name: "lint", Content: "ok"})
		task.End()

		text := StartTask(rec, "best Python linters?\n- injected bullet")
		text.Record(ModelResponse, ModelResponseData{Message: MessageRecord{Role: "assistant", Content: "ruff, pylint"}})
		text.End()
	})
	for _, want := range []string{
		"## fix script2.py",
		"3 tool calls:",
		"- read_file : read script1.py\n",
		"- read_file : read .env (failed: error: access denied)",
		`- lint : {"files":["a.py"]}`,
		"## best Python linters? - injected bullet", // newline collapsed: input cannot start a new bullet
		"answered, no tools (12 chars)",
		"redactions: 0",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
	if strings.Contains(digest, "\n- injected") {
		t.Errorf("user input broke out of its heading:\n%s", digest)
	}
}

func Test_Digest_File_IsOwnerOnly(t *testing.T) {
	_, _, dir := renderDigest(t, func(rec Recorder) { StartRun(rec, config.GetDefaultConfig()).End() })
	files, _ := filepath.Glob(filepath.Join(dir, "*.digest.md"))
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("digest mode = %v, %v; want 0600", info, err)
	}
}

func Test_Digest_ShowsFailuresUnfinishedCallsAndCounts(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		task := StartTask(rec, "go")
		task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "shell", Arguments: `{"command":"grep x f | wc -l"}`})
		task.Record(ModelError, ModelErrorData{Error: "boom"})
		task.Record(Compaction, CompactionData{Trigger: "auto"})
		task.Record(ToolRequested, ToolRequestedData{CallID: "c2", Name: "read_file", Arguments: `{"path":"k.txt"}`})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c2", Name: "read_file", Content: "error: bad key sk-ant-api03-abcdefghijklmnopqrstuvwx", Failed: true})
		task.Fail(os.ErrDeadlineExceeded)
		task.End()
		run.End()
	})
	if !strings.Contains(digest, "2 tool calls:") {
		t.Errorf("both calls should be counted:\n%s", digest)
	}
	if strings.Contains(digest, "sk-ant-api03-abcdefghijklmnopqrstuvwx") {
		t.Errorf("a secret reached the digest:\n%s", digest)
	}
	if !strings.Contains(digest, "1 requested but never finished") {
		t.Errorf("unfinished call not reported:\n%s", digest)
	}
	if !strings.Contains(digest, "task error: ") {
		t.Errorf("task failure not reported:\n%s", digest)
	}
	if !strings.Contains(digest, "model errors: 1, compactions: 1") {
		t.Errorf("counters wrong:\n%s", digest)
	}
}

func Test_Digest_ShellCommandsShownVerbatim(t *testing.T) {
	got := describe("shell", `{"command":"cd /etc && du -h  ."}`, "", false)
	if got != "shell : cd /etc && du -h ." {
		t.Errorf("describe = %q", got)
	}
}

func Test_Digest_UnfinishedRun_SaysSo(t *testing.T) {
	d := NewActivityDigest()
	var out bytes.Buffer
	if err := d.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unfinished (no run_end)") {
		t.Errorf("digest = %q", out.String())
	}
}

func Test_Replay_RebuildsTheSameDigestAsTheLiveOne(t *testing.T) {
	live, jsonl, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		task := StartTask(rec, "read it")
		task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c1", Name: "read_file", Content: "x"})
		task.End()
		run.End()
	})
	d := NewActivityDigest()
	if err := Replay(strings.NewReader(jsonl), d); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d.Render(&out)
	if out.String() != live {
		t.Errorf("replayed digest differs from the live one:\n%s\n---\n%s", out.String(), live)
	}
}

func Test_Replay_HandlesVeryLongLines_AndRejectsGarbage(t *testing.T) {
	long := `{"ts":"2026-10-22T00:00:00Z","run_id":"r","counter":0,"event_type":"task_start","scope_id":"t1","event_data":{"input":"` +
		strings.Repeat("a", 1<<20) + `"}}` // 1 MB: bigger than bufio.Scanner's default limit
	d := NewActivityDigest()
	if err := Replay(strings.NewReader(long), d); err != nil { // no trailing newline on the last line
		t.Fatalf("long line: %v", err)
	}
	if len(d.tasks) != 1 {
		t.Errorf("tasks = %d, want 1", len(d.tasks))
	}
	if err := Replay(strings.NewReader("not json\n"), NewActivityDigest()); err == nil {
		t.Error("expected an error for a line that is not an event")
	}
}

func Test_SetupRecorder_DigestDisabled_WritesNoDigest(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.DigestEnabled = false
	cfg.TraceDir = filepath.Join(t.TempDir(), "traces")
	rec, err := SetupRecorder(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	StartRun(rec, cfg).End()
	rec.Close()
	if files, _ := filepath.Glob(filepath.Join(cfg.TraceDir, "*.digest.md")); len(files) != 0 {
		t.Errorf("digest disabled but wrote %v", files)
	}
	if files, _ := filepath.Glob(filepath.Join(cfg.TraceDir, "*.jsonl")); len(files) != 1 {
		t.Errorf("the L0 trace must still be written, got %v", files)
	}
}

func Test_Digest_SlashCommandsShowTheirEffect(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		clear := StartTask(rec, "/clear")
		clear.Record(ContextCleared, GetContextClearedData(4))
		clear.End()
		compact := StartTask(rec, "/compact")
		compact.Record(Compaction, CompactionData{Trigger: "manual", Status: "ok", MessagesBefore: 12, MessagesAfter: 3})
		compact.End()
		run.Record(Compaction, CompactionData{Trigger: "auto", Status: "ok", MessagesBefore: 30, MessagesAfter: 5}) // under the run ID: counted, not a task
		run.End()
	})
	for _, want := range []string{"## /clear\n\ncontext cleared (4 messages)", "## /compact\n\ncompaction ok: 12 -> 3 messages", "compactions: 2"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
	if strings.Count(digest, "\n## ") != 2 {
		t.Errorf("the auto compaction must not appear as a task:\n%s", digest)
	}
}

// - Verify a truncated or dropped payload is reported as incomplete data, not read as a nameless finished call
// while the real request is reported as never finished
func Test_Digest_MissingPayloadMarkers_AreReportedAsIncomplete(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.TraceMaxEventBytes = 200 // small, so the big tool result below is over the cap
	cfg.TraceDir = filepath.Join(t.TempDir(), "traces")
	rec, err := SetupRecorder(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := StartRun(rec, config.GetDefaultConfig())
	task := StartTask(rec, "read big file")
	task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "read_file", Arguments: `{"path":"big.txt"}`})
	task.Record(ToolFinished, ToolFinishedData{CallID: "c1", Name: "read_file", Content: strings.Repeat("x", 1000)}) // truncated by the recorder
	task.End()
	run.End()
	rec.Close()
	files, _ := filepath.Glob(filepath.Join(cfg.TraceDir, "*.digest.md"))
	b, _ := os.ReadFile(files[0])
	digest := string(b)

	for _, want := range []string{"1 tool call:", "1 requested, result missing (trace incomplete)", "1 event had incomplete data"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
	if strings.Contains(digest, "never finished") || strings.Contains(digest, "- :") || strings.Contains(digest, "2 tool") {
		t.Errorf("the truncated completion was read as activity:\n%s", digest)
	}
}

func Test_Digest_DroppedAndRunLevelMarkers(t *testing.T) {
	d := NewActivityDigest()
	d.Add(Event{RunID: "r", ScopeID: "r", EventType: RunStart, EventData: []byte(`{"dropped":"redaction failed"}`)})
	d.Add(Event{RunID: "r", ScopeID: "t1", EventType: ModelResponse, EventData: []byte(`{"dropped":"redaction failed"}`)})
	var out bytes.Buffer
	d.Render(&out)
	s := out.String()
	if !strings.Contains(s, "1 run-level event had incomplete data") || !strings.Contains(s, "1 event had incomplete data") {
		t.Errorf("digest = %s", s)
	}
	if strings.Contains(s, "answered, no tools") { // a dropped model_response is not a text-only reply
		t.Errorf("a dropped model_response was read as an empty reply:\n%s", s)
	}
}
