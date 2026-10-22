// Package trace records what the agent does during a run, as an event log you can audit afterwards.
//
// # Overview
//
// Generally speaking, there are specific points in the agent's execution (the agentic loop) where tracing is relevant:
//
// - the very beginning and very end of the agentic loop
// - the start and end of a task assigned to an agent, or a request-response turn
// - each model turn - a request event and a response event
// - each tool call turn - a tool call request and a tool execution response
//
// Let's use a simplified example of an agent run where the user asks, "what is the weather today in Paris?"
// Pictorially, the tracing points can be visualized as follows:
//
//	    agent setup
//	        |
//	        v
//		agent execution loop start ==> record "run-start" event trace
//		    |
//		    v
//		task start (task: "get today's weather in Paris") => record "task-start" event trace
//		    |
//		    v
//		model request (prompt: "what is the weather today in Paris?") => record "model-request" trace
//		    |
//		    v
//	    model response (response: "Use get_weather MCP tool call") => record "model-response" trace
//		    |
//		    v
//		tool call request (tool: "get_weather", args: ["Paris"]) => record "tool_requested" trace
//	        |
//	        v
//	    tool execution response (result: "Sunny in Paris") => record "tool_finished" trace
//		    |
//		    v
//		task end => record "task-end" trace
//	        |
//	        v
//	    agent execution loop end => record "run-end" trace
//
// Note that event traces during normal execution are paired, meaning each start event has a corresponding end event.
//
// Errors are also logged as part of the event trace, typically with an "error" event type and relevant error details.
// An application crash is an exception that leaves no run_end, so a reader treats the end of the file as an unclean stop.
//
// At these event trace touchpoints, the agent records the corresponding event trace via an implementation of a Recorder interface, which has a Record() method.
//
// Note that tracing can be turned off using the config, in which case each event is a No-op (Nop), which does nothing.
//
// # Agent event tracing Flow
//
//	agent loop                       the loop's goroutine
//	    |
//	    |  Record(scopeID, eventType, data) - a quick event trace recording that takes a snapshot of the event data
//	    v
//	+---------------------------------+
//	| timestamp: time, run ID         |
//	| snapshot: json.Marshal(data)    |  a private copy (i.e., a snapshot), so the loop can reuse its slices and maps
//	| size cap: too big -> marker     |  we don't want a massive data block to overwhelm the trace (and potentially block execution)
//	+---------------------------------+
//	    |
//	    v
//	[ event trace queue ]             allows a certain amount of backpressure instead of dropping events, when the agent loop is running really fast
//	    |
//	    v
//	+----------------------------------+
//	| tracewriter goroutine (just one) |  handles trace events one at a time, in order
//	|   1. redact secrets              |  we do NOT want actual secrets leaking into the trace logs
//	|   2. number it (Counter)         |  so that we can keep track of the order of events
//	|   3. marshal to one JSON line    |
//	+---------------------------------+
//	    |
//	    v
//	[ Subscribers ]                   the JSON log file, and the L1 digest (digest.md) when digestEnabled; a console print stream could follow
//
// # Redacting Secrets
//
// Nothing is written raw. This requires an extra layer in tracing, but I feel it's worth it for the security.
// A redactor walks the payload as generic JSON and replaces each secret with a placeholder such as [REDACTED:aws_access_key#9f3a41c7].
// The same secret gets the same tag within a run, so repeated secrets can be matched without revealing the value.
// The trace file is also created with owner-only permissions (0600) in a owner-only (0700) directory, to protect it from other users tampering with it.
//
// # Runs
//
// A "run" represents the entire lifecycle of an agent session from start to finish.
// It begins with a run_start event and ends with a run_end event, encompassing all tasks and events in between.
// Run IDs look like r20261021T101500Z-9f3a41c7 (prefix "r").
// For agents running concurrency, each agent instance will have its own run, with separate run_start and run_end events.
//
// # Tasks
//
// From the perspective of an agent trace, a "task" represents a unit of work or an operation that the agent is performing.
//
// Tasks in Chat mode: a task is one user message through to the final answer, with any number of model steps and tool calls in between.
//
//	This includes slash commands that trigger model calls or change agent state (/clear, /compact, /summary, /summarize): each is a task of its own,
//	with the command text as the task_start input.
//	If a task ends early (Ctrl+C or an error), the task_end event says how it ended, so a cancelled turn still closes. All events inside the turn share one scope_id (which is the task_id - see # Scope section).
//
// Tasks in Autonomous mode: one task from objective.md is one task, no matter how many steps the agent takes.
// Task IDs look like t20261021T101530Z-5be0d2a1 (prefix "t")
// Task IDs help readers of the trace, such as the L1 digest or when grepping the trace log file - so that events can be grouped / collected by task.
// Info-only commands (/config, unknown commands) and /quit are not tasks and are not traced.
//
// # Scope
//
// A "scope" represents a traced larger unit of agentic work, currently either a run or a task.
// Within a scope, the Scope object tracks the start time, status, and any errors, and ensures that all events within the scope are associated with the correct task / run ID.
// Sub-units of work within a scope, such as a model request / response within a task, are mapped to the same scope ID (the task ID for tasks, the run ID for runs).
//
// # Files
//
//   - event.go: the trace event envelope, event types and payload structs
//   - redact.go: secrets detector and redactor
//   - recorder.go: Recorder, Subscriber, channel queue plus tracewriter goroutine
//   - digest.go, digest_activity.go: L1 digest (Digester interface, live Subscriber adapter, Replay for old .jsonl files; ActivityDigest is the first implementation), a condensed summary built from the redacted trace events; this is meant for quick inspection and analysis of the trace, e.g. within a pull request or a run review
//   - subscriber.go: defines the Subscriber interface and built-in implementations for handling trace events; first is the JSONL log file subscriber
//   - scope.go: defines the Scope object that tracks the start time, status, and errors for a scope (run or task), ensuring all events within the scope are correctly associated with the scope ID
package trace
