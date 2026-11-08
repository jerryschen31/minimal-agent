---
format: minagent-objective/v1
---

# Objective
Find out how the modelcontextprotocol/go-sdk repository implements the client side of the streamable
HTTP transport, and write a one-page explanation to NOTES.md for a Go developer.

## Instructions
- NOTES.md names the main types, explains how a request is sent and how sessions are handled, in under 40 lines.
- If deepwiki answers in general terms, that is enough: name the types and files it mentions.

## Context
- The repository is NOT in the working directory: learn about it only through the deepwiki tools
  (they take a GitHub repository name such as "modelcontextprotocol/go-sdk"). Never use read_file on
  the repository's paths; read_file and write_file are only for the working directory.

## Notes
An extra section with a name the parser does not know, kept in Other as written.

## Tasks

### t1: Find the transport code
Use the deepwiki tools to find which files and types implement the streamable HTTP client transport in
modelcontextprotocol/go-sdk.

### t2: Write NOTES.md
depends: t1

Write NOTES.md: the main types, how a request is sent, and how sessions are handled.
- Keep it under 40 lines.
