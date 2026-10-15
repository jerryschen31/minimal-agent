# Objective
Create a tiny Go program in the working directory that prints the first 10 Fibonacci numbers.

## Tasks
- Create a Go module named fib (go mod init fib) and write fib.go: package main, a function fib(n int) []int, and a main that prints fib(10), one number per line. Run it with go run . to check the output.
- Write fib_test.go with a table-driven test covering fib(0), fib(1) and fib(10), and run go test ./... until it passes.
- Write README.md explaining in a few lines how to run the program and the test.

## Done when
- fib.go, fib_test.go and README.md exist and are consistent with each other.
- go test ./... passes and go run . prints 0 1 1 2 3 5 8 13 21 34, one per line.

## Constraints
- Use only the Go standard library.
