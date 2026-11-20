package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/jerryschen31/minimal-agent/mcpconnect"
	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/jerryschen31/minimal-agent/textutils"
	"github.com/jerryschen31/minimal-agent/tools"
	"github.com/jerryschen31/minimal-agent/trace"
)

// maxArgsPreviewBytes caps a tool call's argument preview in the journal (tool_before's args_preview).
const maxArgsPreviewBytes = 300

// runToolJournal records every tool call of an auto run in its journal: tool_before when the call is about to run,
// tool_after when it returns (the session's ToolJournal). [agent] A call cut off by a crash is a tool_before with no
// tool_after; resume (1h) settles those.
type runToolJournal struct {
	journal  *state.Journal
	redactor *trace.Redactor // masks secrets in the argument preview, as the trace does
	mu       sync.Mutex
	task     string // the task the calls belong to (setTask)
	n        int    // calls recorded so far; the next call ID is "c<n+1>"
}

// newRunToolJournal returns the tool recorder for a run with journal j, masking previews at redaction level level.
//
// Notes:
//   - An unknown level is an error (the same check as the trace's).
func newRunToolJournal(j *state.Journal, level string) (*runToolJournal, error) {
	r, err := trace.NewRedactor(trace.Level(level))
	if err != nil {
		return nil, err
	}
	return &runToolJournal{journal: j, redactor: r}, nil
}

// setTask makes the calls recorded from now on belong to task id.
//
// Notes: none.
func (rj *runToolJournal) setTask(id string) {
	rj.mu.Lock()
	rj.task = id
	rj.mu.Unlock()
}

// recordCall writes tool_before for call and returns ctx carrying the call's idempotency key and a function that writes
// tool_after.
//
// Notes:
//   - The call ID is the run's own ("c1", "c2", …): a model's IDs can repeat. The model's is kept as model_call_id.
//   - Every MCP call gets a random idempotency key (newIdempotencyKey), recorded and put on ctx; the tool sends it in
//     its request's _meta. Built-in calls get none. An MCP tool's idempotentHint is recorded with it.
//   - A file tool (tools.FileTool) records each path it would change with the path's hash now (hashPath); tool_after
//     records each one's hash after the call. If the tool can't say (bad arguments, a refused path), no files are
//     recorded: the call will fail the same way.
//   - The preview is the arguments with secrets masked (the trace's redactor), cut to maxArgsPreviewBytes. The hash is
//     of the exact arguments.
//   - A tool_after that can't be written is ignored here: the next journal write fails too, and stops the run.
func (rj *runToolJournal) recordCall(ctx context.Context, call model.ToolCall, tool tools.Tool, args json.RawMessage) (context.Context, func(failed bool), error) {
	rj.mu.Lock()
	rj.n++
	id, task := fmt.Sprintf("c%d", rj.n), rj.task
	rj.mu.Unlock()

	before := state.ToolBefore{
		CallID:      id,
		ModelCallID: call.ID,
		Tool:        call.Function.Name,
		ArgsSHA256:  state.SHA256(args),
		ArgsPreview: rj.preview(args),
	}
	if hints, isMCP := mcpconnect.HintsOf(tool); isMCP {
		before.IdempotencyKey = newIdempotencyKey()
		before.Idempotent = hints.Idempotent
		ctx = mcpconnect.WithIdempotencyKey(ctx, before.IdempotencyKey)
	}
	var paths []string
	if ft, ok := tool.(tools.FileTool); ok {
		if p, err := ft.ChangedPaths(args); err == nil {
			paths = p
		}
	}
	for _, p := range paths {
		before.Files = append(before.Files, state.FileHashes{Path: p, Before: hashPath(p)})
	}
	if err := rj.journal.Append(state.EventToolBefore, task, before); err != nil {
		return ctx, nil, err
	}
	done := func(failed bool) {
		after := state.ToolAfter{CallID: id, Failed: failed}
		for _, p := range paths {
			after.Files = append(after.Files, state.FileHashes{Path: p, After: hashPath(p)})
		}
		rj.journal.Append(state.EventToolAfter, task, after)
	}
	return ctx, done, nil
}

// preview returns args with secrets masked, cut to maxArgsPreviewBytes.
//
// Notes:
//   - Arguments that can't be redacted (not JSON) are not shown at all: "(arguments not shown)". Never the raw text.
func (rj *runToolJournal) preview(args json.RawMessage) string {
	redacted, _, err := rj.redactor.Redact(args)
	if err != nil {
		return "(arguments not shown)"
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		return "(arguments not shown)"
	}
	if s, cut := textutils.CutBytes(string(data), maxArgsPreviewBytes); cut {
		return s + " …"
	}
	return string(data)
}

// newIdempotencyKey returns a random key for one tool call: "minagent-" and 16 hex characters.
//
// Notes:
//   - Random, so it is unique across machines and state folders; a re-send (1h) reuses the recorded key.
func newIdempotencyKey() string {
	b := make([]byte, 8)
	rand.Read(b) // never returns an error (crypto/rand, Go 1.24+)
	return "minagent-" + hex.EncodeToString(b)
}

// hashPath returns what is at path now: "sha256:<hex>" of a file's content, state.HashAbsent or state.HashDir.
//
// Notes:
//   - The file is read in full, streamed (copy_file's limit is 100 MB). Anything else, or a read error, is
//     "unreadable: <reason>": a hash that matches nothing, so resume treats the path as changed.
//   - The path was checked by the tool (FileTool.ChangedPaths), so it is inside workDir and not deny-listed.
func hashPath(path string) string {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return state.HashAbsent
	case err != nil:
		return "unreadable: " + err.Error()
	case info.IsDir():
		return state.HashDir
	case !info.Mode().IsRegular():
		return "unreadable: not a regular file"
	}
	f, err := os.Open(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unreadable: " + err.Error()
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
