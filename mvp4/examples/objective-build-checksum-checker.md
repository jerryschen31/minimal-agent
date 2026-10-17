# Objective
Build a CLI tool that computes and verifies file checksums across multiple hashing algorithms simultaneously using a single streaming pass.

## Quality bar
- Deterministic correctness over performance micro-optimizations. A task is considered done when `go build ./...` and `go vet ./...` pass and the driver/package tests pass against standard test vectors.
- Golden Vector Grounding:
  - Verification for hash implementations must rely strictly on standard test vectors (e.g., empty string `""` and `"The quick brown fox jumps over the lazy dog"`).
  - Do not ask the worker to invent complex random fuzzing harnesses or benchmark memory allocations (`-bench`, allocs/op).
- Single-Pass Stream Verification:
  - For the combined pipeline, verify that the input stream is read exactly once (e.g., using a spy reader or `io.MultiWriter`). 
  - Do not require chunked streaming buffers to handle multi-gigabyte mock disk files in memory during unit tests; a standard in-memory `strings.Reader` or `bytes.Buffer` test fixture is sufficient.
- Parallel Task Autonomy:
  - When evaluating individual algorithm drivers (`pkg/digests/*`), verify only the driver under test. Do not block or fail a worker if an unrelated sibling worker's package is not yet merged or has a pending stub.
- Fix Threshold & Fallback:
  - Fix compiler errors, panics, incorrect hash hex outputs, or data races (`-race`).
  - After at most 4 worker iterations on a task, if the core hashing algorithm works and produces correct output for standard vectors but secondary CLI formatting or flag edge-cases fail, call `task_done` anyway, record the minor issue in the note, and move on.

## Constraints
- Implementation: Pure Go (1.21+), standard library only (`crypto/sha256`, `crypto/sha512`, `crypto/md5`, `hash/crc32`, `hash/adler32`).
- Interface & Concurrency Architecture:
  - Phase 1 defines `type DigestComputer interface { Name() string; Write([]byte) (int, error); Sum() string }`.
  - Phase 2 parallel workers implement distinct algorithm wrappers:
    - Worker 1: `pkg/digests/sha.go` (SHA-256 and SHA-512)
    - Worker 2: `pkg/digests/checksum.go` (CRC32 IEEE and Adler32)
    - Worker 3: `pkg/digests/md5.go` (MD5 legacy)
    - Worker 4: `pkg/formatter/table.go` (Formats multi-hash output into aligned text / JSON)
  - Phase 3 combines them with `io.MultiWriter` so the file is read only once from disk.
- Verification: Unit tests verifying all algorithms against RFC standard test vectors ("The quick brown fox...").
