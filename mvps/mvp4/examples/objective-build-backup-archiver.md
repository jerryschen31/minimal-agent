# Objective
Build a deduplicating directory backup and snapshot utility in Go that saves and restores directory trees using content-addressable storage.

## Quality bar
- Functional recovery over filesystem edge cases. A task is considered done when `go build ./...` and `go vet ./...` succeed and its unit or component tests pass.
- Verification Standards:
  - Byte-for-Byte Integrity: The core definition of success is that a restored directory tree matches the original test fixture's file names, subdirectories, and file contents (`bytes.Equal` or SHA-256 match).
  - Deduplication Invariant: Tests must assert that backing up two identical files (or unchanged files across two snapshot runs) results in only one copy of those chunks being written to the content store.
  - Test Fixtures: Use small, in-memory or `t.TempDir()` fixtures with plain text or small binary files (1KB to 128KB). Do not require synthetic multi-gigabyte files or disk-filling benchmarks.
- Out of Scope (Do Not Block On):
  - Do not ask for symlink loop resolution, hard links, Windows/POSIX file permission bitmask conversions, extended file attributes (xattrs), or cross-device mount point handling. Plain regular files and standard nested directories are sufficient.
  - Do not require dynamic Rabin/CDC (Content-Defined Chunking) algorithms if a fixed-size chunking strategy (e.g., 64KB blocks) satisfies the tests.
- Parallel Task Isolation:
  - When evaluating individual worker packages (`pkg/chunker`, `pkg/manifest`, `pkg/compress`, `pkg/restore`), evaluate only against each component's local interface and mock stores. Do not fail a worker task because a sibling worker's package is incomplete or unmerged.
- Fix Threshold & Fallback:
  - Fix compiler errors, panics, broken directory traversals, or corrupted restored files.
  - After at most 5 worker iterations on a task, if the component fulfills its interface contract and round-trips data correctly but secondary CLI flag parsing or progress-bar formatting fails, call `task_done` anyway, log the minor limitation in the task note, and proceed.

## Constraints
- Implementation: Pure Go (standard library only).
- Parallel Architecture:
  - Phase 1: Shared contract for `Chunk`, `SnapshotManifest`, and `BlobStore`.
  - Phase 2 (Parallel Workers):
    - Worker 1 (`pkg/chunker`): Streams files into fixed-size chunks and manages hash-based deduplication.
    - Worker 2 (`pkg/manifest`): Serializes/deserializes directory metadata and snapshot file trees.
    - Worker 3 (`pkg/compress`): Gzip streaming compression pipeline for chunk storage.
    - Worker 4 (`pkg/restore`): Reconstructs original directory structure and file contents from a manifest.
  - Phase 3: Integration tests verifying that backing up a folder twice where only one file changed stores zero duplicate chunks.
- Verification: `go test -v ./...` verifying backup, modification, incremental snapshot, and full directory recovery to a temp folder.
