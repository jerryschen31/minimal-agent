package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jerryschen31/minimal-agent/model"
	"github.com/jerryschen31/minimal-agent/state"
	"github.com/jerryschen31/minimal-agent/tools/builtin"
	"github.com/jerryschen31/minimal-agent/trace"
	"github.com/jerryschen31/minimal-agent/utils/textutils"
)

// Resume (PLAN-auto-mode.md, 1h). A run whose journal has no run_ended was cut off (a crash, a kill, a power cut).
// Starting the same plan again settles each call the crash cut off (a tool_before with no tool_after) before any task
// runs, then continues that run, or starts a new one if the plan or config changed. The cut-off task runs again from
// the start with a fresh worker, told what happened to its calls; done tasks are skipped as always.

// errResumeWaiting is returned when a cut-off call needs a person's answer and none could be had (no terminal, -yes,
// Ctrl+D): nothing was done or recorded, and the next start asks again.
var errResumeWaiting = errors.New("a cut-off call needs an answer")

// noEffectTools are the tools whose calls change nothing, so a cut-off one needs no settling.
var noEffectTools = map[string]bool{"read_file": true, "list_files": true, "search_files": true, "task_done": true, "task_failed": true}

// fileToolNames are the built-in tools that change files (tools.FileTool); a cut-off one is settled by its hashes.
var fileToolNames = map[string]bool{"write_file": true, "edit_file": true, "create_directory": true, "copy_file": true, "move_file": true}

// settleAction is what resume will do about one cut-off call.
type settleAction int

const (
	settleNothing settleAction = iota // it changes nothing
	settleFiles                       // compare its files with their "before" hashes; remove leftover temporary files
	settleResend                      // send the exact call again, with the same idempotency key
	settleDone                        // a person said it happened: treat it as done
)

// settlePlan is one cut-off call and what resume will do about it (decideSettlement).
type settlePlan struct {
	call   state.OpenCall
	action settleAction
}

// resumeRun settles the cut-off calls of the plan's last run, which never ended (st.Latest), and returns that run to
// continue when the plan file and the config are byte for byte what it started with; otherwise it ends that run and
// returns nil (the caller starts a new one). now is the manifest this start would write (its tools and MCP servers). It also returns a note per task about its settled calls (crashNotes).
//
// Notes:
//   - First every call's fate is decided, asking a person where needed (decideSettlement); errResumeWaiting means an
//     answer was needed and couldn't be had, and then nothing is done or written.
//   - Then the run's journal is reopened (state.Home.ContinueRun cuts a torn last line), cs.ToolJournal records into
//     it (a re-send is a recorded call), the calls are settled (settle), and a resumed event lists the outcomes.
//   - [agent] Continuing keeps one run one run, so step 2's budgets carry across a crash. When the plan, the config or
//     the tools changed (toolsDiffer), the old run gets run_ended (interrupted) and the history carries over to the new
//     run through Load.
//   - Tools changed: no call is re-sent without asking (PR #44 review: a re-send must not go to a different server
//     unseen). A server that changed behind an unchanged interface is not detected (accepted, Jerry).
func resumeRun(ctx context.Context, cs *ChatSession, home state.Home, st state.PlanState, now state.Manifest, planData, config []byte, ask promptReader) (*autoRun, map[string]string, error) {
	w := cs.OutBuffer
	last := st.Latest
	fmt.Fprintf(w, "Run %04d did not finish (a crash or a kill).\n", last.Run.Number)
	toolsChanged := toolsDiffer(last.Manifest, now)
	if toolsChanged {
		fmt.Fprintln(w, "The tools or MCP servers changed since then: nothing is sent again without asking, and a new run starts.")
	}
	plans, err := decideSettlement(ctx, cs, last.OpenCalls, toolsChanged, ask, w)
	if err != nil {
		return nil, nil, err
	}
	sessionStart := time.Now() // [agent] before any re-send: those calls belong to this session's running time
	j, err := home.ContinueRun(last.Run)
	if err != nil {
		return nil, nil, err
	}
	rj, err := newRunToolJournal(j, cs.Config.RedactionLevel)
	if err != nil {
		j.Close()
		return nil, nil, err
	}
	cs.ToolJournal = rj
	settled, notes := settle(ctx, cs, rj, plans, w)
	if jerr := rj.err(); jerr != nil { // a re-send's record failed: leave the run open, settle again next start
		cs.ToolJournal = nil
		j.Close()
		return nil, nil, fmt.Errorf("run %04d: the journal failed while settling: %w", last.Run.Number, jerr)
	}
	host, _ := os.Hostname()
	resumed := state.Resumed{PID: os.Getpid(), Host: host, TraceRunID: cs.Recorder.RunID(), StartedAt: sessionStart.UTC(), Settled: settled}
	if err := j.Append(state.EventResumed, "", resumed); err != nil {
		cs.ToolJournal = nil
		j.Close()
		return nil, nil, err
	}
	if last.Manifest.PlanSHA256 == state.SHA256(planData) && last.Manifest.ConfigSHA256 == state.SHA256(config) && !toolsChanged {
		fmt.Fprintf(w, "Resuming run %04d.\n", last.Run.Number)
		return &autoRun{run: last.Run, journal: j, tools: rj, sessionStart: sessionStart}, notes, nil
	}
	cs.ToolJournal = nil
	ended := state.RunEnded{Reason: state.StopInterrupted, Error: "cut off by a crash; the plan, config or tools changed since, so a new run continues it"}
	err = j.Append(state.EventRunEnded, "", ended)
	j.Close()
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(w, "The plan, config or tools changed since run %04d started, so a new run continues it.\n", last.Run.Number)
	return nil, notes, nil
}

// decideSettlement decides what resume does about each cut-off call, asking a person where it must; nothing is done yet.
//
// Notes:
//   - A call that changes nothing (noEffectTools, an MCP tool with readOnlyHint, a file tool whose path was refused so
//     no files were recorded) → nothing. A file call → settleFiles.
//   - An MCP call with idempotentHint that can be re-sent (resendable) → re-sent without asking, unless toolsChanged
//     (the tools or MCP servers differ from the crashed run's): then it is asked like the rest.
//   - Anything else (other MCP calls, run_command, an unknown tool) → asked. "Send it again? [y/N]": y re-sends, n
//     treats it as done. If it can't be re-sent: "Carry on, treating it as done? [y/N]": y treats it as done, n stops.
//   - Nobody to ask (ask is nil: no terminal; -yes never answers this), Ctrl+D, or n to the second question →
//     errResumeWaiting, before anything is done. Ctrl+C → ctx's error.
//   - The call is shown with %q (it holds model text).
func decideSettlement(ctx context.Context, cs *ChatSession, open []state.OpenCall, toolsChanged bool, ask promptReader, w io.Writer) ([]settlePlan, error) {
	var plans []settlePlan
	for _, c := range open {
		switch {
		case noEffectTools[c.Tool] || c.ReadOnly || fileToolNames[c.Tool] && len(c.Files) == 0:
			plans = append(plans, settlePlan{c, settleNothing})
			continue
		case len(c.Files) > 0:
			plans = append(plans, settlePlan{c, settleFiles})
			continue
		}
		why := resendable(cs, c.ToolBefore)
		if why == "" && c.Idempotent && !toolsChanged {
			plans = append(plans, settlePlan{c, settleResend})
			continue
		}
		what := fmt.Sprintf("%s %q did not complete before the crash (task %s)", c.Tool, c.ArgsPreview, c.Task)
		if ask == nil {
			fmt.Fprintf(w, "%s. It needs your answer, and there is no terminal to ask: run again on a terminal.\n", what)
			return nil, errResumeWaiting
		}
		question := what + ". Send it again? [y/N]: "
		if toolsChanged {
			question = what + ". The tools or MCP servers changed since. Send it again? [y/N]: "
		}
		if why != "" {
			question = fmt.Sprintf("%s and can't be sent again (%s). Carry on, treating it as done? [y/N]: ", what, why)
		}
		answer, err := ask.AskLine(ctx, question)
		switch {
		case errors.Is(err, io.EOF):
			fmt.Fprintln(w, "\nNo answer: nothing was done; you'll be asked again next time.")
			return nil, errResumeWaiting
		case err != nil:
			return nil, err
		}
		yes := isYes(answer, false)
		switch {
		case why == "" && yes:
			plans = append(plans, settlePlan{c, settleResend})
		case why == "" || yes:
			plans = append(plans, settlePlan{c, settleDone})
		default:
			fmt.Fprintln(w, "Nothing was done; settle it yourself, then run again.")
			return nil, errResumeWaiting
		}
	}
	return plans, nil
}

// toolsDiffer reports whether the tools or MCP servers of manifest now differ from those of was: a different tool set,
// a changed description or parameters (ToolsSHA256), or a server with another name, transport or reported version.
//
// Notes:
//   - Server settings themselves (URL, command, args) aren't compared: config.json leaves them out because they can
//     hold tokens. A server that reports the same identity and tools from a new address passes (accepted).
func toolsDiffer(was, now state.Manifest) bool {
	return was.ToolsSHA256 != now.ToolsSHA256 || !slices.Equal(was.MCPServers, now.MCPServers)
}

// resendable returns why the cut-off call c can't be sent again, or "" if it can.
//
// Notes:
//   - Its exact arguments must have been kept (MCP and run_command calls, up to maxResendArgsBytes), must still match
//     their recorded hash, and the tool must exist in this session (its MCP server connected).
func resendable(cs *ChatSession, c state.ToolBefore) string {
	switch {
	case len(c.Args) == 0:
		return "its arguments weren't kept"
	case state.SHA256(c.Args) != c.ArgsSHA256:
		return "its kept arguments don't match their hash"
	}
	if _, ok := cs.Tools.Lookup(c.Tool); !ok {
		return "the tool isn't available now"
	}
	return ""
}

// settle does what decideSettlement decided, and returns each call's record (for the resumed event) and a note per task
// for that task's next worker (taskMessage's crash section).
//
// Notes:
//   - settleFiles: each recorded path is hashed now (hashPath) and compared with its "before" hash: all equal → it
//     didn't happen; any different → it happened, or something else changed the file. Temporary files a crash left
//     next to each path (builtin.RemoveTempFiles) are removed.
//   - settleResend: resendCall, recorded under the call's task (rj.setTask); its result or error is in the note.
//   - One line per call is printed.
func settle(ctx context.Context, cs *ChatSession, rj *runToolJournal, plans []settlePlan, w io.Writer) ([]state.SettledCall, map[string]string) {
	var settled []state.SettledCall
	lines := map[string][]string{}
	for _, p := range plans {
		c := p.call
		s := state.SettledCall{CallID: c.CallID, Tool: c.Tool, Task: c.Task}
		var forWorker string
		switch p.action {
		case settleNothing:
			s.Outcome, forWorker = state.OutcomeNoEffect, "changes nothing; nothing to check"
		case settleFiles:
			var changed []string
			removed := 0
			for _, f := range c.Files {
				if hashPath(f.Path) != f.Before {
					changed = append(changed, f.Path)
				}
				n, _ := builtin.RemoveTempFiles(f.Path)
				removed += len(n)
			}
			if len(changed) == 0 {
				s.Outcome, forWorker = state.OutcomeNotHappened, "did not happen: its files are as they were before it"
			} else {
				s.Outcome = state.OutcomeHappened
				forWorker = "happened (or something else changed its files): changed now: " + strings.Join(changed, ", ")
			}
			if removed > 0 {
				s.Note = fmt.Sprintf("removed %d leftover temporary file(s)", removed)
			}
		case settleResend:
			rj.setTask(c.Task)
			result, err := resendCall(ctx, cs, c.ToolBefore)
			if err != nil {
				s.Outcome, s.Note = state.OutcomeResendFailed, err.Error()
				forWorker = "was sent again after the crash and failed: " + oneLine(err.Error(), 300)
			} else {
				s.Outcome = state.OutcomeResent
				forWorker = "was sent again after the crash (same arguments and idempotency key) and succeeded: " + oneLine(result, 300)
			}
		case settleDone:
			s.Outcome, forWorker = state.OutcomeUserSaidDone, "a person said it was done: don't do it again"
		}
		settled = append(settled, s)
		shown := fmt.Sprintf("%s %q", c.Tool, c.ArgsPreview)
		fmt.Fprintf(w, "  %s — %s", shown, s.Outcome)
		if s.Note != "" {
			fmt.Fprintf(w, " (%s)", oneLine(s.Note, 200))
		}
		fmt.Fprintln(w)
		lines[c.Task] = append(lines[c.Task], fmt.Sprintf("- %s %s: %s", c.Tool, c.ArgsPreview, forWorker))
	}
	notes := map[string]string{}
	for task, ls := range lines {
		notes[task] = strings.Join(ls, "\n")
	}
	return settled, notes
}

// resendCall sends the cut-off call c again through the normal path (executeToolCall): the same tool and exact
// arguments, the same idempotency key, recorded with resend_of.
//
// Notes:
//   - The deny and ask rules apply again (a rule added since the crash still counts; an ask rule is denied, nobody is
//     asked during a run), and so does the tool timeout. Traced as a task "resend <call ID>".
func resendCall(ctx context.Context, cs *ChatSession, c state.ToolBefore) (string, error) {
	scope := trace.StartTask(cs.Recorder, "resend "+c.CallID)
	defer scope.End()
	call := model.ToolCall{ID: c.ModelCallID, Type: "function", Function: model.ToolCallFunc{Name: c.Tool, Arguments: string(c.Args)}}
	result, err := executeToolCall(withResend(ctx, c), cs, scope, call)
	if err != nil {
		scope.Fail(err)
	}
	return result, err
}

// oneLine returns s on one line (newlines as spaces), cut to max bytes with " …".
//
// Notes: none.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if cut, ok := textutils.CutBytes(s, max); ok {
		return cut + " …"
	}
	return s
}
