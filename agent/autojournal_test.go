package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jerryschen31/minimal-agent/config"
	"github.com/jerryschen31/minimal-agent/hooks"
	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// funcTool is a tool named name that runs fn.
type funcTool struct {
	name string
	fn   func(ctx context.Context, args json.RawMessage) (string, error)
}

// GetToolDefinition returns the tool's definition.
func (f funcTool) GetToolDefinition() model.ToolDef {
	return model.NewToolDef(f.name, "test tool", json.RawMessage(`{"type":"object","properties":{}}`))
}

// CallTool runs fn.
func (f funcTool) CallTool(ctx context.Context, args json.RawMessage) (string, error) {
	return f.fn(ctx, args)
}

// recordedSession is a session whose tool calls are recorded in a fresh run journal, under task t1.
type recordedSession struct {
	cs      *ChatSession
	journal *state.Journal
	path    string // the journal file
}

// newRecordedSession returns a session with toolList as its tools, recording into a new run's journal.
func newRecordedSession(t *testing.T, toolList ...tools.Tool) *recordedSession {
	t.Helper()
	fx := newChatSessionFixture(t, 10, 100)
	reg, err := tools.NewToolRegistry(toolList)
	if err != nil {
		t.Fatal(err)
	}
	fx.Session.Tools = reg
	home, _ := state.OpenHome(t.TempDir())
	j, r, err := home.NewRun(state.Manifest{PlanPath: "plan.md"}, []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	rj, err := newRunToolJournal(j, "standard")
	if err != nil {
		t.Fatal(err)
	}
	rj.setTask("t1")
	fx.Session.ToolJournal = rj
	return &recordedSession{cs: fx.Session, journal: j, path: filepath.Join(r.Dir, "journal.jsonl")}
}

// call runs one tool call through executeToolCall.
func (rs *recordedSession) call(t *testing.T, name, args string) (string, error) {
	t.Helper()
	tc := model.ToolCall{ID: "m-" + name, Function: model.ToolCallFunc{Name: name, Arguments: args}}
	return executeToolCall(t.Context(), rs.cs, trace.StartTask(trace.Nop(), "x"), tc)
}

// events returns the journal's tool_before and tool_after events, decoded, in order: a *ToolBefore or a *ToolAfter.
func (rs *recordedSession) events(t *testing.T) []any {
	t.Helper()
	data, err := os.ReadFile(rs.path)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := state.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, e := range events {
		switch e.Type {
		case state.EventToolBefore:
			var b state.ToolBefore
			json.Unmarshal(e.Data, &b)
			if e.Task != "t1" {
				t.Errorf("tool_before %s under task %q, want t1", b.CallID, e.Task)
			}
			out = append(out, &b)
		case state.EventToolAfter:
			var a state.ToolAfter
			json.Unmarshal(e.Data, &a)
			out = append(out, &a)
		}
	}
	return out
}

//   - Verify every call that runs gets tool_before then tool_after with the same run-unique call ID (c1, c2), the
//     model's ID, the arguments' hash, and failed set only for a call that failed
//   - Verify a call that never ran gets no record: denied by a rule, an unknown tool, arguments that aren't an object
func Test_Unit_ToolJournal_RecordsEveryCallThatRuns(t *testing.T) {
	ok := funcTool{"ok_tool", func(context.Context, json.RawMessage) (string, error) { return "fine", nil }}
	bad := funcTool{"bad_tool", func(context.Context, json.RawMessage) (string, error) { return "", errors.New("broke") }}
	denied := funcTool{"denied_tool", func(context.Context, json.RawMessage) (string, error) { return "", nil }}
	rs := newRecordedSession(t, ok, bad, denied)
	rules, err := hooks.NewToolRules([]string{"denied_tool"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rs.cs.Hooks = []hooks.Hook{rules}

	rs.call(t, "ok_tool", `{"a":1}`)
	rs.call(t, "denied_tool", `{}`)
	rs.call(t, "no_such_tool", `{}`)
	rs.call(t, "ok_tool", `[1]`)
	rs.call(t, "bad_tool", ``)

	ev := rs.events(t)
	if len(ev) != 4 {
		t.Fatalf("%d tool events, want 4 (two calls ran): %+v", len(ev), ev)
	}
	b1, a1, b2, a2 := ev[0].(*state.ToolBefore), ev[1].(*state.ToolAfter), ev[2].(*state.ToolBefore), ev[3].(*state.ToolAfter)
	if b1.CallID != "c1" || a1.CallID != "c1" || b2.CallID != "c2" || a2.CallID != "c2" {
		t.Errorf("call IDs = %s %s %s %s, want c1 c1 c2 c2", b1.CallID, a1.CallID, b2.CallID, a2.CallID)
	}
	if b1.Tool != "ok_tool" || b1.ModelCallID != "m-ok_tool" || b1.ArgsSHA256 != state.SHA256([]byte(`{"a":1}`)) || b1.IdempotencyKey != "" {
		t.Errorf("tool_before = %+v", b1)
	}
	if a1.Failed || !a2.Failed {
		t.Errorf("failed = %v, %v; want false, true", a1.Failed, a2.Failed)
	}
	if b2.ArgsSHA256 != state.SHA256([]byte(`{}`)) {
		t.Errorf("no arguments: hash of %q, want of {} (what the tool got)", b2.ArgsPreview)
	}
}

//   - Verify tool_before is in the journal before the tool runs (the tool finds it, with no tool_after), and a call that
//     can't be recorded (journal closed) doesn't run
func Test_Unit_ToolJournal_RecordedBeforeItRuns(t *testing.T) {
	var rs *recordedSession
	ran := 0
	probe := funcTool{"probe", func(context.Context, json.RawMessage) (string, error) {
		ran++
		ev := rs.events(t)
		if len(ev) != 1 {
			t.Errorf("during the call: %d tool events, want only its tool_before", len(ev))
		} else if _, ok := ev[0].(*state.ToolBefore); !ok {
			t.Errorf("during the call: %T, want its tool_before", ev[0])
		}
		return "", nil
	}}
	rs = newRecordedSession(t, probe)
	rs.call(t, "probe", `{}`)
	if ran != 1 {
		t.Fatalf("probe ran %d times", ran)
	}
	rs.journal.Close()
	if _, err := rs.call(t, "probe", `{}`); err == nil || !strings.Contains(err.Error(), "could not be recorded") || ran != 1 {
		t.Errorf("closed journal: err %v, ran %d times; want not run", err, ran)
	}
}

//   - Verify each file tool records the paths it changes (absolute, symlinks resolved) with their hash before
//     (tool_before) and after (tool_after): a new file absent → content hash; an edit; a new directory absent → dir; a
//     copy's destination only; a move's source and destination
//   - Verify a path the tool refuses (.env) records no files, and the call fails
func Test_Unit_ToolJournal_FileHashes(t *testing.T) {
	dir := t.TempDir()
	real, _ := filepath.EvalSymlinks(dir)
	cfg := config.GetDefaultConfig(config.ModeAuto)
	cfg.WorkDir, cfg.StateDir, cfg.TraceDir = dir, t.TempDir(), filepath.Join(t.TempDir(), "trace")
	cfg.BuiltinTools = []string{"write_file", "edit_file", "create_directory", "copy_file", "move_file"}
	builtins, err := tools.SetupBuiltinTools(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rs := newRecordedSession(t, builtins...)
	at := func(name string) string { return filepath.Join(real, name) }
	sha := func(s string) string { return state.SHA256([]byte(s)) }
	fh := func(path, before, after string) state.FileHashes {
		return state.FileHashes{Path: path, Before: before, After: after}
	}

	tests := []struct {
		tool, args string
		want       []state.FileHashes // Path, Before, After
	}{
		{"write_file", `{"path":"a.txt","content":"hello"}`, []state.FileHashes{fh(at("a.txt"), state.HashAbsent, sha("hello"))}},
		{"edit_file", `{"path":"a.txt","old_string":"hello","new_string":"bye"}`, []state.FileHashes{fh(at("a.txt"), sha("hello"), sha("bye"))}},
		{"create_directory", `{"path":"d"}`, []state.FileHashes{fh(at("d"), state.HashAbsent, state.HashDir)}},
		{"copy_file", `{"source":"a.txt","destination":"d/b.txt"}`, []state.FileHashes{fh(at("d/b.txt"), state.HashAbsent, sha("bye"))}},
		{"move_file", `{"source":"a.txt","destination":"c.txt"}`, []state.FileHashes{
			fh(at("a.txt"), sha("bye"), state.HashAbsent), fh(at("c.txt"), state.HashAbsent, sha("bye"))}},
	}
	for _, tt := range tests {
		if _, err := rs.call(t, tt.tool, tt.args); err != nil {
			t.Fatalf("%s: %v", tt.tool, err)
		}
	}
	if _, err := rs.call(t, "write_file", `{"path":".env","content":"x"}`); err == nil {
		t.Errorf("write_file .env: no error")
	}
	ev := rs.events(t)
	if len(ev) != 2*(len(tests)+1) {
		t.Fatalf("%d tool events, want %d", len(ev), 2*(len(tests)+1))
	}
	for i, tt := range tests {
		b, a := ev[2*i].(*state.ToolBefore), ev[2*i+1].(*state.ToolAfter)
		if len(b.Files) != len(tt.want) || len(a.Files) != len(tt.want) {
			t.Errorf("%s: before %+v, after %+v, want %d paths", tt.tool, b.Files, a.Files, len(tt.want))
			continue
		}
		for k, w := range tt.want {
			if b.Files[k].Path != w.Path || b.Files[k].Before != w.Before || a.Files[k].Path != w.Path || a.Files[k].After != w.After {
				t.Errorf("%s path %d: before %+v, after %+v; want %+v", tt.tool, k, b.Files[k], a.Files[k], w)
			}
		}
	}
	refused := ev[len(ev)-2].(*state.ToolBefore)
	if len(refused.Files) != 0 || !ev[len(ev)-1].(*state.ToolAfter).Failed {
		t.Errorf("refused path: files %+v; want none and a failed call", refused.Files)
	}
}

// - Verify the argument preview masks a secret and is cut, while the hash is of the exact arguments
func Test_Unit_ToolJournal_ArgsPreview(t *testing.T) {
	rs := newRecordedSession(t, funcTool{"ok_tool", func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	token := "ghp_" + strings.Repeat("A", 36)
	args := `{"note":"use ` + token + `","long":"` + strings.Repeat("x", 400) + `"}`
	rs.call(t, "ok_tool", args)
	b := rs.events(t)[0].(*state.ToolBefore)
	if strings.Contains(b.ArgsPreview, token) || !strings.HasSuffix(b.ArgsPreview, " …") || len(b.ArgsPreview) > maxArgsPreviewBytes+len(" …") {
		t.Errorf("preview (%d bytes) = %s", len(b.ArgsPreview), b.ArgsPreview)
	}
	if b.ArgsSHA256 != state.SHA256([]byte(args)) {
		t.Errorf("args hash is not of the exact arguments")
	}
}

//   - Verify an MCP call gets a random idempotency key ("minagent-" + 16 hex), recorded in tool_before with the tool's
//     idempotentHint, and the server receives the same key in the request's _meta; each call gets a new key
func Test_Unit_ToolJournal_MCPIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	var received []any
	server := mcp.NewServer(&mcp.Implementation{Name: "shop", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "order", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
			received = append(received, req.Params.Meta[mcpconnect.IdempotencyMetaKey])
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ordered"}}}, nil, nil
		})
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); ss.Close() })
	conn := &mcpconnect.McpConnection{Name: "shop", Config: config.McpServerConfig{Command: "x"}, Session: session}
	mcpTools, err := mcpconnect.SetupMCPTools(ctx, []*mcpconnect.McpConnection{conn}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	rs := newRecordedSession(t, mcpTools...)
	for range 2 {
		if _, err := rs.call(t, "shop_order", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	ev := rs.events(t)
	b1, b2 := ev[0].(*state.ToolBefore), ev[2].(*state.ToolBefore)
	if !regexp.MustCompile(`^minagent-[0-9a-f]{16}$`).MatchString(b1.IdempotencyKey) || !b1.Idempotent {
		t.Errorf("tool_before = %+v, want a minagent key and idempotent", b1)
	}
	if len(received) != 2 || received[0] != b1.IdempotencyKey || received[1] != b2.IdempotencyKey || b1.IdempotencyKey == b2.IdempotencyKey {
		t.Errorf("server received %v; recorded %q and %q (want the same, different per call)", received, b1.IdempotencyKey, b2.IdempotencyKey)
	}
}

// - Verify in a real run the director's calls are recorded under the task it judges, and the run's session stops
// recording when the run ends
func Test_Unit_ToolJournal_DirectorCallsInARun(t *testing.T) {
	fx := newOneTaskFixture(t)
	if out, err := runAutoYes(t, fx); err != nil {
		t.Fatalf("autoMode: %v\n%s", err, out)
	}
	var found bool
	for _, e := range journal(t, fx, 1) {
		var b state.ToolBefore
		if e.Type == state.EventToolBefore && json.Unmarshal(e.Data, &b) == nil && b.Tool == "task_done" {
			found = e.Task == "t1"
		}
	}
	if !found {
		t.Errorf("no tool_before for the director's task_done under t1")
	}
	if fx.Session.ToolJournal != nil {
		t.Errorf("ToolJournal still set after the run")
	}
}
