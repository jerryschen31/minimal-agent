package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/model/modeltest"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/jerryschen31/minimal-agent/tools/builtin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// crashPlan is a two-task plan: t1 makes a file, t2 sends something (a run_command test tool).
const crashPlan = "# Plan: Send\n\n## Tasks\n\n### t1: Make\ndone when: made\n\nMake it.\n\n### t2: Send\ndepends: t1\ndone when: sent\n\nSend it.\n"

// crashExitCode is how the crash child process exits inside its tool call.
const crashExitCode = 3

// newCrashFixture returns an auto fixture for the plan at dir/plan.md with its state folder at dir/state, and a
// run_command test tool that appends a line to dir/sent.txt and then, if crash, exits the process mid-call.
func newCrashFixture(t *testing.T, dir string, crash bool) *chatSessionFixture {
	t.Helper()
	fx, _ := newAutoFixture(t, filepath.Join(dir, "plan.md"))
	fx.Session.Config.StateDir = filepath.Join(dir, "state")
	send := funcTool{"run_command", func(context.Context, json.RawMessage) (string, error) {
		f, err := os.OpenFile(filepath.Join(dir, "sent.txt"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return "", err
		}
		f.WriteString("sent\n")
		f.Close()
		if crash {
			os.Exit(crashExitCode) // the process dies inside the call: tool_before is written, tool_after never is
		}
		return "sent", nil
	}}
	if err := fx.Session.Tools.Register(send); err != nil {
		t.Fatal(err)
	}
	return fx
}

// - The crash child: runs crashPlan until t2's call kills the process. Runs only when the parent test starts it.
func Test_Unit_AutoResume_CrashChild(t *testing.T) {
	dir := os.Getenv("MINAGENT_CRASH_DIR")
	if dir == "" {
		t.Skip("run by Test_Unit_AutoResume_AfterCrash")
	}
	fx := newCrashFixture(t, dir, true)
	fx.Provider.Script = []model.ChatMessage{
		modeltest.AssistantText("made it"),                                     // t1's worker
		modeltest.AssistantToolCall("w2", "run_command", `{"command":"send"}`), // t2's worker: the call that crashes
	}
	runAutoYes(t, fx)
	t.Fatal("the child should have exited inside the tool call")
}

// crashAndWait writes crashPlan to dir and runs the crash child on it; it fails unless the child died in the call.
func crashAndWait(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte(crashPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^Test_Unit_AutoResume_CrashChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MINAGENT_CRASH_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != crashExitCode {
		t.Fatalf("crash child: %v, want exit code %d\n%s", err, crashExitCode, out)
	}
}

// resumeWith runs the resume of the crashed run in dir with answer as the typed input (-yes, so only the resume asks),
// and returns the fixture and the output.
func resumeWith(t *testing.T, dir, answer string, terminal bool) (*chatSessionFixture, string, error) {
	t.Helper()
	fx := newCrashFixture(t, dir, false)
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("checked: it was sent")} // t2's new worker
	fx.Session.Config.AssumeYes = true
	reader := newPlainReader(t.Context(), strings.NewReader(answer), fx.Out)
	gates := promptReader(reader)
	if !terminal {
		gates = nil
	}
	err := autoMode(t.Context(), nil, fx.Session, reader, gates)
	return fx, fx.Out.String(), err
}

// sentLines returns how many times the test tool sent.
func sentLines(t *testing.T, dir string) int {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "sent.txt"))
	return strings.Count(string(data), "sent\n")
}

// resumedEvent returns the resumed event in run 1's journal of the state folder in dir.
func resumedEvent(t *testing.T, dir string) (state.Resumed, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "state", "runs", "0001", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e state.Event
		json.Unmarshal([]byte(line), &e)
		if e.Type == state.EventResumed {
			var r state.Resumed
			json.Unmarshal(e.Data, &r)
			return r, true
		}
	}
	return state.Resumed{}, false
}

//   - Verify the plan's "done test": a process killed inside a run_command call; the resume asks about the cut-off call
//     before anything runs, "n" treats it as done (it is not repeated: sent once), done t1 is not run again, t2 runs
//     again with a worker told about the crash, and the same run (0001) is continued with a resumed event
//   - Verify "y" sends the exact call again (sent twice), recorded with resend_of and the kept arguments
func Test_Unit_AutoResume_AfterCrash(t *testing.T) {
	for _, tt := range []struct {
		answer, outcome string
		sent            int
	}{{"n\n", state.OutcomeUserSaidDone, 1}, {"y\n", state.OutcomeResent, 2}} {
		t.Run(tt.outcome, func(t *testing.T) {
			dir := t.TempDir()
			crashAndWait(t, dir)
			if n := sentLines(t, dir); n != 1 {
				t.Fatalf("before the resume: sent %d times", n)
			}
			fx, out, err := resumeWith(t, dir, tt.answer, true)
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, out)
			}
			for _, want := range []string{"Run 0001 did not finish", `run_command "{\"command\":\"send\"}" did not complete before the crash (task t2). Send it again? [y/N]`,
				"Resuming run 0001", "skip t1", "▶ t2", "Finished: 1 done, 1 skipped."} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if n := sentLines(t, dir); n != tt.sent {
				t.Errorf("sent %d times, want %d", n, tt.sent)
			}
			if len(fx.Provider.Calls) != 1 || !strings.Contains(fx.Provider.Calls[0][1].Content, "## An earlier attempt was cut off by a crash") ||
				!strings.Contains(fx.Provider.Calls[0][1].Content, "- run_command") {
				t.Errorf("worker calls %d; t2's message should have the crash note", len(fx.Provider.Calls))
			}
			r, ok := resumedEvent(t, dir)
			if !ok || len(r.Settled) != 1 || r.Settled[0].Outcome != tt.outcome || r.Settled[0].Task != "t2" || r.PID != os.Getpid() {
				t.Errorf("resumed = %+v (found %v)", r, ok)
			}
			if _, err := os.Stat(filepath.Join(dir, "state", "runs", "0002")); err == nil {
				t.Errorf("a new run folder was made; the run should have been continued")
			}
			if tt.outcome == state.OutcomeResent {
				var resend state.ToolBefore
				for _, e := range journal(t, fx, 1) {
					var b state.ToolBefore
					if e.Type == state.EventToolBefore && json.Unmarshal(e.Data, &b) == nil && b.ResendOf != "" {
						resend = b
					}
				}
				if resend.ResendOf != r.Settled[0].CallID || string(resend.Args) != `{"command":"send"}` {
					t.Errorf("re-send record = %+v", resend)
				}
			}
		})
	}
}

//   - Verify a cut-off call that needs an answer stops the resume with "waiting" (ErrNeedsPerson) when nobody can answer
//     (no terminal, even with -yes) or on Ctrl+D: nothing is run or recorded, and the next start asks again
func Test_Unit_AutoResume_WaitsForAnAnswer(t *testing.T) {
	dir := t.TempDir()
	crashAndWait(t, dir)
	for _, tt := range []struct {
		name     string
		terminal bool
		want     string
	}{{"no terminal", false, "there is no terminal to ask"}, {"Ctrl+D", true, "you'll be asked again next time"}} {
		fx, out, err := resumeWith(t, dir, "", tt.terminal)
		if !errors.Is(err, ErrNeedsPerson) || !strings.Contains(out, tt.want) || !strings.Contains(out, "Stopped: waiting. Nothing was run.") {
			t.Errorf("%s: %v\n%s", tt.name, err, out)
		}
		if len(fx.Provider.Calls) != 0 || sentLines(t, dir) != 1 {
			t.Errorf("%s: %d model calls, sent %d times; want nothing done", tt.name, len(fx.Provider.Calls), sentLines(t, dir))
		}
		if _, ok := resumedEvent(t, dir); ok {
			t.Errorf("%s: a resumed event was written", tt.name)
		}
	}
	if _, out, err := resumeWith(t, dir, "n\n", true); err != nil || !strings.Contains(out, "Finished:") {
		t.Errorf("after waiting, a resume on a terminal: %v\n%s", err, out)
	}
}

//   - Verify a changed plan settles the cut-off call in the old run, ends it (run_ended interrupted), and continues in
//     a new run where done t1 is still skipped
func Test_Unit_AutoResume_PlanChangedStartsNewRun(t *testing.T) {
	dir := t.TempDir()
	crashAndWait(t, dir)
	os.WriteFile(filepath.Join(dir, "plan.md"), []byte(strings.Replace(crashPlan, "# Plan: Send\n", "# Plan: Send\n\nNow with a summary.\n", 1)), 0o644)
	fx, out, err := resumeWith(t, dir, "n\n", true)
	if err != nil || !strings.Contains(out, "a new run continues it") || !strings.Contains(out, "Run 0002") || !strings.Contains(out, "skip t1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if r := lastRunEnded(t, journal(t, fx, 1)); r.Reason != state.StopInterrupted {
		t.Errorf("run 1 run_ended = %+v, want interrupted", r)
	}
	if r, ok := resumedEvent(t, dir); !ok || r.Settled[0].Outcome != state.OutcomeUserSaidDone {
		t.Errorf("run 1 resumed = %+v", r)
	}
}

// settleOne decides and settles the cut-off call c in a recorded session rs (answer: what a person types).
func settleOne(t *testing.T, rs *recordedSession, c state.OpenCall, answer string) (state.SettledCall, map[string]string, error) {
	t.Helper()
	ask := newPlainReader(t.Context(), strings.NewReader(answer), rs.cs.OutBuffer)
	plans, err := decideSettlement(t.Context(), rs.cs, []state.OpenCall{c}, false, ask, rs.cs.OutBuffer)
	if err != nil {
		return state.SettledCall{}, nil, err
	}
	settled, notes := settle(t.Context(), rs.cs, rs.cs.ToolJournal.(*runToolJournal), plans, rs.cs.OutBuffer)
	return settled[0], notes, nil
}

//   - Verify a cut-off file call: files as before → not_happened, and the leftover temporary file is removed; a file
//     changed → happened, naming it; a read → no_effect; a file tool with no recorded files → no_effect
func Test_Unit_Settle_FilesAndReads(t *testing.T) {
	rs := newRecordedSession(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, ".a.txt.minagent-0123abcd.tmp"), []byte("half"), 0o644)
	call := func(tool string, files ...state.FileHashes) state.OpenCall {
		return state.OpenCall{Task: "t1", ToolBefore: state.ToolBefore{CallID: "c1", Tool: tool, Files: files}}
	}
	s, notes, err := settleOne(t, rs, call("write_file", state.FileHashes{Path: p, Before: state.SHA256([]byte("old"))}), "")
	if err != nil || s.Outcome != state.OutcomeNotHappened || !strings.Contains(s.Note, "removed 1") || !strings.Contains(notes["t1"], "did not happen") {
		t.Errorf("unchanged: %+v, %v, %q", s, err, notes["t1"])
	}
	if left, _ := builtin.RemoveTempFiles(p); len(left) != 0 {
		t.Errorf("temporary file left: %v", left)
	}
	os.WriteFile(p, []byte("new"), 0o644)
	if s, _, _ := settleOne(t, rs, call("write_file", state.FileHashes{Path: p, Before: state.SHA256([]byte("old"))}), ""); s.Outcome != state.OutcomeHappened {
		t.Errorf("changed: %+v", s)
	}
	for _, c := range []state.OpenCall{call("read_file"), call("task_done"), call("edit_file")} {
		if s, _, err := settleOne(t, rs, c, ""); err != nil || s.Outcome != state.OutcomeNoEffect {
			t.Errorf("%s: %+v, %v", c.Tool, s, err)
		}
	}
}

//   - Verify a cut-off MCP call whose tool says it is idempotent is re-sent without asking, with the exact arguments
//     and the same idempotency key (the server sees them)
//   - Verify a re-send goes through the rules again: with a deny rule added since, it is refused (resend_failed)
func Test_Unit_Settle_IdempotentMCPResent(t *testing.T) {
	ctx := context.Background()
	type got struct {
		key  any
		item string
	}
	var received []got
	server := mcp.NewServer(&mcp.Implementation{Name: "shop", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "order", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Item string `json:"item"`
		}) (*mcp.CallToolResult, any, error) {
			received = append(received, got{req.Params.Meta[mcpconnect.IdempotencyMetaKey], in.Item})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ordered"}}}, nil, nil
		})
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, _ := server.Connect(ctx, serverT, nil)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); ss.Close() })
	mcpTools, err := mcpconnect.SetupMCPTools(ctx, []*mcpconnect.McpConnection{{Name: "shop", Config: config.McpServerConfig{Command: "x"}, Session: session}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	rs := newRecordedSession(t, mcpTools...)
	args := json.RawMessage(`{"item":"book"}`)
	cut := state.OpenCall{Task: "t2", ToolBefore: state.ToolBefore{CallID: "c7", Tool: "shop_order", Args: args, ArgsSHA256: state.SHA256(args),
		IdempotencyKey: "minagent-0011223344556677", Idempotent: true}}
	s, notes, err := settleOne(t, rs, cut, "") // no answer typed: it must not ask
	if err != nil || s.Outcome != state.OutcomeResent || !strings.Contains(notes["t2"], "succeeded") {
		t.Fatalf("settled %+v, %v", s, err)
	}
	if len(received) != 1 || received[0] != (got{"minagent-0011223344556677", "book"}) {
		t.Errorf("server received %+v", received)
	}

	rules, _ := hooks.NewToolRules([]string{"shop_*"}, nil)
	rs.cs.Hooks = []hooks.Hook{rules}
	if s, _, _ := settleOne(t, rs, cut, ""); s.Outcome != state.OutcomeResendFailed || !strings.Contains(s.Note, "denied") || len(received) != 1 {
		t.Errorf("with a deny rule: %+v (server calls %d)", s, len(received))
	}
}

//   - Verify a call that can't be re-sent (arguments that don't match their hash, or none kept) asks "Carry on, treating
//     it as done?": y → user_said_done, n → waiting
func Test_Unit_Settle_CantResend(t *testing.T) {
	rs := newRecordedSession(t, funcTool{"email_send", func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	cut := state.OpenCall{Task: "t1", ToolBefore: state.ToolBefore{CallID: "c1", Tool: "email_send", Args: json.RawMessage(`{"to":"b"}`),
		ArgsSHA256: state.SHA256([]byte(`{"to":"a"}`))}}
	if s, _, err := settleOne(t, rs, cut, "y\n"); err != nil || s.Outcome != state.OutcomeUserSaidDone {
		t.Errorf("y: %+v, %v", s, err)
	}
	if !strings.Contains(rs.cs.OutBuffer.(interface{ String() string }).String(), "can't be sent again (its kept arguments don't match their hash)") {
		t.Errorf("question lacks the reason")
	}
	if _, _, err := settleOne(t, rs, cut, "n\n"); !errors.Is(err, errResumeWaiting) {
		t.Errorf("n: err %v, want waiting", err)
	}
}

// - Verify args are kept for run_command calls (exact bytes), not for other built-in calls, and not when too big
func Test_Unit_ToolJournal_KeepsArgsForResend(t *testing.T) {
	noop := func(context.Context, json.RawMessage) (string, error) { return "", nil }
	rs := newRecordedSession(t, funcTool{"run_command", noop}, funcTool{"ok_tool", noop})
	rs.call(t, "run_command", `{"command":"go"}`)
	rs.call(t, "ok_tool", `{"a":1}`)
	rs.call(t, "run_command", `{"big":"`+strings.Repeat("x", maxResendArgsBytes)+`"}`)
	ev := rs.events(t)
	if b := ev[0].(*state.ToolBefore); string(b.Args) != `{"command":"go"}` {
		t.Errorf("run_command args = %s", b.Args)
	}
	if b := ev[2].(*state.ToolBefore); b.Args != nil {
		t.Errorf("ok_tool args kept: %s", b.Args)
	}
	if b := ev[4].(*state.ToolBefore); b.Args != nil {
		t.Errorf("over-cap args kept (%d bytes)", len(b.Args))
	}
}

// - Verify toolsDiffer: the same tools and servers → false; another tools hash, or a server reporting another version → true
func Test_Unit_ToolsDiffer(t *testing.T) {
	base := state.Manifest{ToolsSHA256: "sha256:a", MCPServers: []state.MCPServer{{Name: "shop", Transport: "stdio", Server: "shop", Version: "1"}}}
	same := base
	same.MCPServers = slices.Clone(base.MCPServers)
	if toolsDiffer(base, same) {
		t.Errorf("identical manifests differ")
	}
	tools := same
	tools.ToolsSHA256 = "sha256:b"
	version := same
	version.MCPServers = []state.MCPServer{{Name: "shop", Transport: "stdio", Server: "shop", Version: "2"}}
	if !toolsDiffer(base, tools) || !toolsDiffer(base, version) {
		t.Errorf("a changed tools hash or server version was not noticed")
	}
}

//   - Verify that when the tools changed since the crash, even an idempotent MCP call is asked about, not re-sent
//     automatically (PR #44 review); "n" treats it as done and nothing is sent
func Test_Unit_Settle_ToolsChanged_AsksBeforeResend(t *testing.T) {
	sent := 0
	rs := newRecordedSession(t, funcTool{"shop_order", func(context.Context, json.RawMessage) (string, error) { sent++; return "", nil }})
	args := json.RawMessage(`{"item":"book"}`)
	cut := state.OpenCall{Task: "t2", ToolBefore: state.ToolBefore{CallID: "c7", Tool: "shop_order", Args: args, ArgsSHA256: state.SHA256(args),
		IdempotencyKey: "minagent-0011223344556677", Idempotent: true}}
	ask := newPlainReader(t.Context(), strings.NewReader("n\n"), rs.cs.OutBuffer)
	plans, err := decideSettlement(t.Context(), rs.cs, []state.OpenCall{cut}, true, ask, rs.cs.OutBuffer)
	if err != nil || len(plans) != 1 || plans[0].action != settleDone {
		t.Fatalf("plans %+v, %v; want asked and treated as done", plans, err)
	}
	if out := rs.cs.OutBuffer.(interface{ String() string }).String(); !strings.Contains(out, "changed since. Send it again?") {
		t.Errorf("question lacks the reason:\n%s", out)
	}
	if sent != 0 {
		t.Errorf("sent %d times", sent)
	}
}

//   - Verify a resume where the tools changed (a tool was added since the crash) starts a new run instead of
//     continuing the old one, and says why
func Test_Unit_AutoResume_ToolsChangedStartsNewRun(t *testing.T) {
	dir := t.TempDir()
	crashAndWait(t, dir)
	fx := newCrashFixture(t, dir, false)
	if err := fx.Session.Tools.Register(funcTool{"new_tool", func(context.Context, json.RawMessage) (string, error) { return "", nil }}); err != nil {
		t.Fatal(err)
	}
	fx.Provider.Script = []model.ChatMessage{modeltest.AssistantText("checked")}
	fx.Session.Config.AssumeYes = true
	reader := newPlainReader(t.Context(), strings.NewReader("n\n"), fx.Out)
	err := autoMode(t.Context(), nil, fx.Session, reader, reader)
	out := fx.Out.String()
	if err != nil || !strings.Contains(out, "The tools or MCP servers changed since then") || !strings.Contains(out, "Run 0002") {
		t.Errorf("%v\n%s", err, out)
	}
}
