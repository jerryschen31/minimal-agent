package trace

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
)

// - Verify each veto decision puts its note on the right call's line, a denied call keeps its "(failed: ...)",
// two vetoes on one call give two notes, and the footer counts every decision
func Test_Digest_VetoNotesAndFooter(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		defer run.End()
		task := StartTask(rec, "tidy up")
		defer task.End()

		// c1: no prompt, the veto stands
		task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "delete_file", Arguments: `{"path":"a.txt"}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c1", Name: "delete_file", Reason: `denied by rule "*delete*"`, Decision: VetoDenied})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c1", Name: "delete_file", Content: `error: denied by rule "*delete*"`, Failed: true})
		// c2: the user said no
		task.Record(ToolRequested, ToolRequestedData{CallID: "c2", Name: "delete_file", Arguments: `{"path":"b.txt"}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c2", Name: "delete_file", Reason: `denied by rule "*delete*"`, Decision: VetoDeniedByUser, WaitMS: 1200})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c2", Name: "delete_file", Content: `error: denied by rule "*delete*"`, Failed: true})
		// c3: the user allowed it once
		task.Record(ToolRequested, ToolRequestedData{CallID: "c3", Name: "move_file", Arguments: `{"from":"c.txt"}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c3", Name: "move_file", Reason: `denied by rule "*move*"`, Decision: VetoAllowedByUser, WaitMS: 800})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c3", Name: "move_file", Content: "moved"})
		// c4: two vetoes, first allowed, second refused
		task.Record(ToolRequested, ToolRequestedData{CallID: "c4", Name: "delete_file", Arguments: `{"path":"d.txt"}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c4", Name: "delete_file", Reason: `denied by rule "*delete*"`, Decision: VetoAllowedByUser})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c4", Name: "delete_file", Reason: `denied by rule "*file*"`, Decision: VetoDeniedByUser})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c4", Name: "delete_file", Content: `error: denied by rule "*file*"`, Failed: true})
		// c5: a plain call with no veto
		task.Record(ToolRequested, ToolRequestedData{CallID: "c5", Name: "read_file", Arguments: `{"path":"e.txt"}`})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c5", Name: "read_file", Content: "ok"})
	})

	for _, want := range []string{
		`- delete_file : {"path":"a.txt"} (failed: error: denied by rule "*delete*") [vetoed, user not asked: denied by rule "*delete*"]` + "\n",
		`- delete_file : {"path":"b.txt"} (failed: error: denied by rule "*delete*") [user said no to: denied by rule "*delete*"]` + "\n",
		`- move_file : {"from":"c.txt"} [allowed once by user over: denied by rule "*move*"]` + "\n",
		`- delete_file : {"path":"d.txt"} (failed: error: denied by rule "*file*") [allowed once by user over: denied by rule "*delete*"] [user said no to: denied by rule "*file*"]` + "\n",
		"- read_file : read e.txt\n",
		"vetoes: 5 (allowed once by user: 2, denied by user: 2, denied without asking: 1)\n",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
}

// - Verify a run with no vetoes has no veto line in the footer
func Test_Digest_NoVetoes_NoFooterLine(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		task := StartTask(rec, "read")
		task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c1", Name: "read_file", Content: "x"})
		task.End()
		run.End()
	})
	if strings.Contains(digest, "vetoed") {
		t.Errorf("expected no veto line:\n%s", digest)
	}
}

// - Verify a tool_vetoed with no matching tool_requested is still counted, and nothing breaks
func Test_Digest_VetoWithoutRequest_CountedInFooter(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig())
		task := StartTask(rec, "go")
		task.Record(ToolVetoed, ToolVetoedData{CallID: "orphan", Name: "delete_file", Reason: "denied by rule \"*delete*\"", Decision: VetoDenied})
		task.Record(ToolFinished, ToolFinishedData{CallID: "orphan", Name: "delete_file", Content: "error: denied", Failed: true})
		task.End()
		run.End()
	})
	if !strings.Contains(digest, "vetoes: 1 (allowed once by user: 0, denied by user: 0, denied without asking: 1)") {
		t.Errorf("footer should count the orphan veto:\n%s", digest)
	}
	if !strings.Contains(digest, "- delete_file : (failed: error: denied)") {
		t.Errorf("the finished call should still be listed:\n%s", digest)
	}
}

// - Verify run_start carries the deny rules in force: the defaults when the config leaves them alone, the config's
// list when it replaces them, and no key at all when there are none (nil or [])
func Test_GetRunStartData_DenyRules(t *testing.T) {
	cfg := config.GetDefaultConfig()
	b, err := json.Marshal(GetRunStartData(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"deny_rules":["*delete*","*remove*"]`) {
		t.Errorf("run_start JSON missing the default deny_rules: %s", b)
	}

	cfg.DenyRules = []string{"*delete*", "*move*"}
	b, err = json.Marshal(GetRunStartData(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"deny_rules":["*delete*","*move*"]`) {
		t.Errorf("run_start JSON missing deny_rules: %s", b)
	}

	for _, rules := range [][]string{nil, {}} {
		cfg.DenyRules = rules
		b, err = json.Marshal(GetRunStartData(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "deny_rules") {
			t.Errorf("expected no deny_rules key for %#v: %s", rules, b)
		}
	}
}

// - Verify run_start carries the context window settings from the config, under their JSON keys; auto_compact
// is written even when false, so a reader can tell "off" from "not recorded"
func Test_GetRunStartData_ContextWindowSettings(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.ContextWindowMessages = 7
	cfg.ContextStrategy = "ring-buffer"
	cfg.AutoCompact = false
	cfg.AutoCompactThreshold = 0.75
	cfg.SummarizerPrompt = "fake test summarizer prompt"

	data := GetRunStartData(cfg)
	if data.ContextWindowMessages != 7 || data.ContextStrategy != "ring-buffer" || data.AutoCompact ||
		data.AutoCompactThreshold != 0.75 || data.SummarizerPrompt != "fake test summarizer prompt" {
		t.Errorf("expected the context window settings copied from the config, got %+v", data)
	}

	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"context_window_messages":7`,
		`"context_strategy":"ring-buffer"`,
		`"auto_compact":false`,
		`"auto_compact_threshold":0.75`,
		`"summarizer_prompt":"fake test summarizer prompt"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("run_start JSON missing %s: %s", want, b)
		}
	}
}
