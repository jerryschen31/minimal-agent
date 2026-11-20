---
format: minagent-plan/v1
objective: objective.md
objective_hash: sha256:3f2a9c0d1b2e4f5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c
plan_version: 2
---

# Plan: Write a short review of X
Three sources, then an outline, then a draft.

## Assumptions
- No length was given; aiming for about 2,000 words.

## Notes
A section the reader does not know, kept in Other as written.

## Tasks
Work through these in order.

### t1: Survey the sources
needs: academic search, PDF reading
done when: notes/sources.md lists at least 3 sources

Search for recent reviews of X.

### t2: Outline
depends: t1
reason: the objective now asks for an outline first
done when: outline.md exists

Write outline.md from notes/sources.md.

### t3: Publish
depends: t2
effects: external
gate: ask before running (publishes under your name)
needs: the blog's API, logged in as you
done when: the post URL is in published.md

#### Steps
- post the draft
- save the URL
