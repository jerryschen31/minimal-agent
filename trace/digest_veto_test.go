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
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
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
	if strings.Contains(digest, "allow-all") || strings.Contains(digest, "unknown decision") {
		t.Errorf("expected no allow-all or unknown-decision count when there are none:\n%s", digest)
	}
}

// - Verify an allowed_by_config veto gets its own note on the call's line, is counted in the footer as "allowed by
// allow-all", and is not counted as an unknown decision
func Test_Digest_AllowedByConfig_NoteAndFooter(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
		defer run.End()
		task := StartTask(rec, "tidy up")
		defer task.End()

		task.Record(ToolRequested, ToolRequestedData{CallID: "c1", Name: "write_file", Arguments: `{"path":"a.txt"}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c1", Name: "write_file", Reason: `needs approval by rule "write_file"`, Decision: VetoAllowedByConfig})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c1", Name: "write_file", Content: "wrote"})
		task.Record(ToolRequested, ToolRequestedData{CallID: "c2", Name: "fs_delete_file", Arguments: `{"path":"b.txt"}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c2", Name: "fs_delete_file", Reason: `needs approval by rule "*delete*"`, Decision: VetoAllowedByConfig})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c2", Name: "fs_delete_file", Content: "deleted"})
		task.Record(ToolRequested, ToolRequestedData{CallID: "c3", Name: "git_push", Arguments: `{}`})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c3", Name: "git_push", Reason: `denied by rule "*push*"`, Decision: VetoDenied})
		task.Record(ToolFinished, ToolFinishedData{CallID: "c3", Name: "git_push", Content: `error: denied by rule "*push*"`, Failed: true})
	})

	for _, want := range []string{
		`- write_file : wrote [allowed without asking (allow-all) over: needs approval by rule "write_file"]` + "\n",
		`[allowed without asking (allow-all) over: needs approval by rule "*delete*"]` + "\n",
		`[vetoed, user not asked: denied by rule "*push*"]` + "\n",
		"vetoes: 3 (allowed once by user: 0, denied by user: 0, denied without asking: 1, allowed by allow-all: 2)\n",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest is missing %q:\n%s", want, digest)
		}
	}
	if strings.Contains(digest, "unknown decision") {
		t.Errorf("allowed_by_config must not count as an unknown decision:\n%s", digest)
	}
}

// - Verify a decision the digest doesn't know is still counted as "unknown decision", after the allow-all count
func Test_Digest_UnknownDecision_CountedSeparately(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
		task := StartTask(rec, "go")
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c1", Name: "write_file", Reason: "fake", Decision: VetoAllowedByConfig})
		task.Record(ToolVetoed, ToolVetoedData{CallID: "c2", Name: "write_file", Reason: "fake", Decision: "from_the_future"})
		task.End()
		run.End()
	})
	want := "vetoes: 2 (allowed once by user: 0, denied by user: 0, denied without asking: 0, allowed by allow-all: 1, unknown decision: 1)\n"
	if !strings.Contains(digest, want) {
		t.Errorf("digest is missing %q:\n%s", want, digest)
	}
}

// - Verify a run with no vetoes has no veto line in the footer
func Test_Digest_NoVetoes_NoFooterLine(t *testing.T) {
	digest, _, _ := renderDigest(t, func(rec Recorder) {
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
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
		run := StartRun(rec, config.GetDefaultConfig(config.ModeChat))
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

// - Verify run_start carries the deny rules in force: no key with the defaults (there are no default deny rules), the
// config's list when it sets one, and no key at all when there are none (nil or [])
func Test_GetRunStartData_DenyRules(t *testing.T) {
	cfg := config.GetDefaultConfig(config.ModeChat)
	b, err := json.Marshal(GetRunStartData(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "deny_rules") {
		t.Errorf("expected no deny_rules key with the defaults: %s", b)
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
	cfg := config.GetDefaultConfig(config.ModeChat)
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

// - Verify run_start carries the ask rules and approval in force: the defaults, a config's values, and no ask_rules key
// when there are none (nil or [])
func Test_GetRunStartData_AskRulesAndApproval(t *testing.T) {
	cfg := config.GetDefaultConfig(config.ModeChat)
	data := GetRunStartData(cfg)
	if data.Approval != config.ApprovalAsk || len(data.AskRules) != 7 {
		t.Errorf("expected the default ask rules and approval, got ask_rules %q, approval %q", data.AskRules, data.Approval)
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"ask_rules":["*delete*","*remove*","write_file","edit_file","create_directory","copy_file","move_file"]`,
		`"approval":"ask"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("run_start JSON missing %s: %s", want, b)
		}
	}

	cfg.AskRules, cfg.Approval = []string{"*push*"}, config.ApprovalAllowAll
	b, err = json.Marshal(GetRunStartData(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ask_rules":["*push*"]`, `"approval":"allow-all"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("run_start JSON missing %s: %s", want, b)
		}
	}

	for _, rules := range [][]string{nil, {}} {
		cfg.AskRules = rules
		b, err = json.Marshal(GetRunStartData(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "ask_rules") {
			t.Errorf("expected no ask_rules key for %#v: %s", rules, b)
		}
	}
}
