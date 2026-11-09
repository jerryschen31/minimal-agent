# Objective
Build `wordfreq`, a small Go command-line tool that reads a text file and prints the ten most frequent words with
their counts. It is a first project for someone learning Go, so the code should be easy to read.

# Instructions
- Work in the working directory; create a Go module named `wordfreq`.
- Go standard library only; no third-party modules. Don't touch files outside the working directory.
- After each step, `go build ./...` and `go test ./...` must pass.
- Prefer small functions with clear names over clever code.
- Done when `go run . testdata/sample.txt` prints ten lines of the form `word count`, most frequent first, and
  `README.md` explains how to run the tool and its tests.

## Rough steps
1. Create the module and a sample input (`testdata/sample.txt`, about 100 words of ordinary English).
2. Count words (`count.go`), with a table-driven test: empty input, mixed case, punctuation and digits.
3. Pick the top ten (`top.go`), testing the tie-breaking and a map with fewer than ten words.
4. Wire up `main.go` and write the README.

# Context
- A word is a run of letters (any language's letters count); words are compared in lower case.
- Ties are broken alphabetically, so the output is the same on every run.
