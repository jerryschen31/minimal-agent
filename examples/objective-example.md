---
format: minagent-objective/v1
---

# Objective
Build `wordfreq`, a small Go command-line tool that reads a text file and prints the ten most frequent words with
their counts. It is a first project for someone learning Go, so the code should be easy to read.

## Instructions
- Work in the working directory; create a Go module named `wordfreq`.
- After each task, run `go build ./...` and `go test ./...`; a task is done only when both pass.
- Prefer small functions with clear names over clever code.
- Done when `go run . testdata/sample.txt` prints ten lines of the form `word count`, most frequent first, and
  `README.md` explains how to run the tool and its tests.

## Constraints
- Go standard library only; no third-party modules.
- Do not touch files outside the working directory.

## Context
- A word is a run of letters (any language's letters count); words are compared in lower case.
- Ties are broken alphabetically, so the output is the same on every run.

## Tasks
Work through the tasks in order; each one builds on the tasks it depends on.

### t1: Create the module and a sample input

Run `go mod init wordfreq`. Write `testdata/sample.txt` with a paragraph of ordinary English text (about 100 words)
that repeats a few words, so the top ten is easy to check by eye.

### t2: Count words
depends: t1

Write `count.go` with `func countWords(r io.Reader) (map[string]int, error)`, using the definition of a word in
Context. Write a table-driven test in `count_test.go` that covers:

- an empty input
- mixed case ("Go go GO" counts as 3 for "go")
- punctuation and digits between words

### t3: Pick the top ten
depends: t2

Write `func topN(counts map[string]int, n int) []Entry` in `top.go`, sorted by count (highest first), then
alphabetically. Test the tie-breaking, and a map with fewer than n words.

### t4: Wire up main and write the README
depends: t2, t3

`main.go` takes one argument, the file to read, prints the top ten as `word count` lines, and exits with status 1 and
a short message if the file can't be read. Then write `README.md`: what the tool does, how to run it, and how to run
the tests.
