package main

// Test cases for chat with memory compaction functionality.
//
//
// Basic prompt-response mechanics
// - Verify the system prompt is always first in the message list within the request and is never evicted.
// - Verify that the system prompt is still the first message in the message list even after memory compaction.
// - Verify that recognized slash commands are not included in chat request messages.
// - Verify that unrecognized slash commands error out and do not send any request to the Provider
//
// Basic chat history mechanics
// - Verify that a new chat message is correctly added as the most recent entry in the chat history (exists and is most recent)
// - Verify that a recent chat message can be removed from the chat history.
// - Verify that successive chat messages retain the same order in the chat history.
// - Verify that a full successful prompt-response cycle adds both the prompt and the response to the chat history.
// - Verify that a failed response (timeout or error) does not add an incomplete entry to the chat history.
// - Verify that a failed response (timeout or error) does not add the corresponding user prompt to the chat history.
// - Verify that a user can successfully clear the chat history with an appropriate slash command.
//
// Basic context window mechanics
// - Verify that adding a new message to a full context window does not exceed the maximum context window size.
// - Verify that adding a new message to a full context window correctly removes the oldest message to make space for the new one.
// - Verify that adding multiple new messages to a full context window does not exceed the maximum context window size
// - Verify that adding multiple new messages to a full context window correctly removes the oldest messages to make space for the new ones.
//
// Basic memory compaction tests
// - Verify that memory compaction correctly creates a summary message and inserts it into the chat history.
// - Verify that unsuccessful memory compaction (timeout or error) does not alter the chat history.
// - Verify that concurrent memory compactions cannot occur simultaneously (only the first compaction is executed).
// - Verify that memory compaction correctly reduces the memory footprint without losing essential chat history.
// - Verify that memory compaction does not remove messages added after compaction started but before compaction completes.
// - Verify that memory compaction maintains the correct order of the chat messages added after compaction already started.
// - Verify that auto-compaction is correctly triggered when the memory footprint exceeds a certain threshold.
// - Verify that a user can manually trigger memory compaction with an appropriate slash command.
// - Verify that an empty or whitespace-only summary is treated as a failure and leaves the history unchanged
// - Verify that compacting an empty history returns an error and leaves the history unchanged.
// - Verify that a cancelled context (e.g. with Ctrl+C) during summarization leaves the history unchanged.
// - Verify that after a failed compaction, chat keeps working (i.e., new messages can still be added to the chat history).
// - Verify that after compaction, the next chat request contains the summary plus any new messages added after compaction
// - Verify that after compaction, the next chat request does not contain old compacted messages.
// - Verify that multiple auto-compactions do not trigger concurrently
//
// Basic context window type mechanics
// - Verify all of the above chat history and context window and memory compaction mechanics specifically for each of the current window types
//   (OffsetWindow, InPlaceWindow, RingBufferWindow, LLWindow)
//
// Window type performance tests
// - Measure and compare the performance of the current window types under various chat and context window operations
//   (OffsetWindow, InPlaceWindow, RingBufferWindow, LLWindow)
//
// Unit tests
// - Verify ring-buffer wraparound behavior within the data structure
// - Verify LLWindow eviction behavior within the data structure
// - Verify OffsetWindow eviction behavior within the data structure
// - Verify InPlaceWindow eviction behavior within the data structure
// - An operation on messages returned by GetMessages() does not modify the current chat history
// - An operation on messages returned by Snapshot() does not modify the current chat history
//
// Other tests
// - Run all tests with -race and make sure the program is free of race conditions.
//
