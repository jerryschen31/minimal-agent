---
format: minagent-plan/v1
objective: objective-example.md
objective_hash: sha256:a19b76139fad61d6eebefd4c4d771f23ec1ea2c5c874c7ac2227b94efe626e4b
plan_version: 1
---

# Plan: wordfreq, a word-frequency CLI in Go
Four tasks in the order the objective's rough steps give.

## Assumptions
- Go 1.26 is installed and `go` is on the PATH.
- "Most frequent" counts every occurrence; there is no stop-word list.

## Tasks

### t1: Create the module and a sample input
needs: a shell or file tools in the working directory, a Go toolchain
done when: go.mod declares module wordfreq and testdata/sample.txt has about 100 words

Run `go mod init wordfreq`. Write `testdata/sample.txt` with a paragraph of ordinary English text that repeats a few
words, so the top ten is easy to check by eye.

### t2: Count words
depends: t1
needs: file tools, a Go toolchain
done when: go test ./... passes with a table-driven test for countWords

Write `count.go` with `func countWords(r io.Reader) (map[string]int, error)`, using the definition of a word in the
objective's Context. Test an empty input, mixed case ("Go go GO" counts 3 for "go"), and punctuation and digits between
words.

### t3: Pick the top ten
depends: t2
needs: file tools, a Go toolchain
done when: go test ./... passes, including the tie-breaking test

Write `func topN(counts map[string]int, n int) []Entry` in `top.go`, sorted by count (highest first), then
alphabetically. Test the tie-breaking and a map with fewer than n words.

### t4: Wire up main and write the README
depends: t2, t3
needs: file tools, a Go toolchain
done when: go run . testdata/sample.txt prints ten "word count" lines and README.md explains how to run it and its tests

`main.go` takes one argument, the file to read, prints the top ten, and exits with status 1 and a short message if the
file can't be read. Then write `README.md`.
