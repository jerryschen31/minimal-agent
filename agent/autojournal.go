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

// hashContentMaxBytes is the largest file hashPath reads in full. A larger one (a big copy_file or move_file) gets a
// "meta:<size>:<mtime>" fingerprint instead, so recording a call never turns a rename into reading gigabytes (PR #44
// review). A var so tests can lower it.
var hashContentMaxBytes int64 = 16 << 20

// maxResendArgsBytes caps the exact arguments kept for a call resume may re-send (tool_before's args). A call with
// larger arguments can't be re-sent: resume asks about it instead.
const maxResendArgsBytes = 64 << 10

// resendCtx is the context key resendCall uses to tell recordCall that a call repeats a cut-off one.
type resendCtx struct{}

// withResend returns ctx marking the next recorded call as a re-send of the cut-off call orig: recordCall reuses its
// idempotency key and records resend_of.
//
// Notes: none.
func withResend(ctx context.Context, orig state.ToolBefore) context.Context {
	return context.WithValue(ctx, resendCtx{}, orig)
}

// runToolJournal records every tool call of an auto run in its journal: tool_before when the call is about to run,
// tool_after when it returns (the session's ToolJournal). [agent] A call cut off by a crash is a tool_before with no
// tool_after; resume (1h) settles those.
type runToolJournal struct {
	journal  *state.Journal
	redactor *trace.Redactor // masks secrets in the argument preview, as the trace does
	mu       sync.Mutex
	task     string // the task the calls belong to (setTask)
	n        int    // calls recorded so far; the next call ID is "c<n+1>"
	// broken is the first journal write that failed. From then on no call runs (not recorded = not run) and the run
	// stops without run_ended, so a call whose end wasn't recorded stays open for the next start to settle.
	broken error
}

// err returns the journal write that failed, or nil while every call has been recorded.
//
// Notes:
//   - runTask checks it after each worker and director, and end skips run_ended when it is set (PR #44 review).
func (rj *runToolJournal) err() error {
	rj.mu.Lock()
	defer rj.mu.Unlock()
	return rj.broken
}

// fail keeps err as the journal's first failed write and returns it.
//
// Notes: none.
func (rj *runToolJournal) fail(err error) error {
	rj.mu.Lock()
	defer rj.mu.Unlock()
	if rj.broken == nil {
		rj.broken = err
	}
	return err
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
//     its request's _meta. Built-in calls get none. An MCP tool's idempotentHint and readOnlyHint are recorded with it.
//   - MCP and run_command calls also keep their exact arguments (args, up to maxResendArgsBytes), so resume can re-send
//     them. They are unredacted; the journal is private (0600 in the state folder).
//   - A re-send (ctx from withResend) records resend_of and reuses the cut-off call's idempotency key.
//   - A file tool (tools.FileTool) records each path it would change with the path's hash now (hashPath); tool_after
//     records each one's hash after the call. If the tool can't say (bad arguments, a refused path), no files are
//     recorded: the call will fail the same way.
//   - The preview is the arguments with secrets masked (the trace's redactor), cut to maxArgsPreviewBytes. The hash is
//     of the exact arguments.
//   - A tool_before or tool_after that can't be written marks the journal broken (err): no further call runs, and the
//     run stops without run_ended, so the call stays open for the next start to settle. done returns that error.
func (rj *runToolJournal) recordCall(ctx context.Context, call model.ToolCall, tool tools.Tool, args json.RawMessage) (context.Context, func(failed bool) error, error) {
	if err := rj.err(); err != nil {
		return ctx, nil, fmt.Errorf("the run's journal failed earlier: %w", err)
	}
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
	hints, isMCP := mcpconnect.HintsOf(tool)
	if isMCP {
		before.IdempotencyKey = newIdempotencyKey()
		before.Idempotent, before.ReadOnly = hints.Idempotent, hints.ReadOnly
	}
	if orig, ok := ctx.Value(resendCtx{}).(state.ToolBefore); ok {
		before.ResendOf = orig.CallID
		if orig.IdempotencyKey != "" {
			before.IdempotencyKey = orig.IdempotencyKey // the same key: a server that deduplicates runs it once
		}
	}
	if before.IdempotencyKey != "" {
		ctx = mcpconnect.WithIdempotencyKey(ctx, before.IdempotencyKey)
	}
	if (isMCP || call.Function.Name == "run_command") && len(args) <= maxResendArgsBytes {
		before.Args = args // [agent] the calls resume may re-send; file tools are settled by their hashes instead
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
		return ctx, nil, rj.fail(err)
	}
	done := func(failed bool) error {
		after := state.ToolAfter{CallID: id, Failed: failed}
		for _, p := range paths {
			after.Files = append(after.Files, state.FileHashes{Path: p, After: hashPath(p)})
		}
		if err := rj.journal.Append(state.EventToolAfter, task, after); err != nil {
			return rj.fail(err)
		}
		return nil
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
//   - A file up to hashContentMaxBytes is read in full, streamed. A larger one gets "meta:<size>:<mtime in ns>" instead:
//     resume only asks "is it as it was before the call?", and size and modification time answer that for big files
//     (a same-size change within the same nanosecond would be missed: accepted).
//   - Anything else, or a read error, is "unreadable: <reason>": a hash that matches nothing, so resume treats the path
//     as changed.
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
	case info.Size() > hashContentMaxBytes:
		return fmt.Sprintf("meta:%d:%d", info.Size(), info.ModTime().UnixNano())
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
