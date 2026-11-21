# Objective
Build a transpiler in Go that translates a typed, single-function Python script into equivalent, runnable Go code.

## Quality bar
- Good enough, not perfect. A task is done when `go build ./...` and `go vet ./...` pass and a few
  basic tests pass (3–5 cases on the main path). Don't ask for edge-case tests, refactors or style changes.
- Fix only what fails the build or the tests. After at most 8 fix rounds, call task_done anyway and
  record the remaining issues in the note.

## Constraints
- Implementation: Pure Go (standard libraries only, no external dependencies).
- Input Scope: A single Python file containing exactly one function with explicit type annotations (`int`, `bool`, `list[int]`), basic control flow (`if/else`, `while`, `return`), arithmetic/comparison operators, and an optional top-level `print(fn(...))` call.
- Output Scope: A valid `main.go` file (`package main` with `func main()`) that compiles and runs cleanly using `go run`.
- Verification: Must include `go test ./...` which transpiles a test script (e.g., binary search), executes the generated Go code via `go run`, and asserts the output is correct.
