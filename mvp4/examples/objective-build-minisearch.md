# Objective
Build a fast, in-memory full-text search engine in Go ("minisearch") that indexes collections of text documents, supports Boolean multi-term queries, and ranks results using TF-IDF (Term Frequency–Inverse Document Frequency) scoring.

## Constraints
- Implementation: Written strictly in Go (1.21+) using only the Go standard library (`strings`, `unicode`, `sync`, `math`, `sort`, `os`, `flag`). No external modules (`go get` forbidden), no external NLP libraries.
- Workflow & Agent Role Separation:
  - Strict Separation of Concerns: The agent that writes the implementation code for any component must NOT write its test suite or perform verification.
  - Test Independence: For every component or integration milestone, testing and verification tasks must be assigned to an independent, dedicated verifier agent to guarantee objective validation.
  - Task Structure: Plan all modules with distinct, alternating task handoffs:
    1. Implement component/feature.
    2. Independently verify component with unit tests and assertions.
- Architecture & Parallel Execution Structure:
  - Phase 1 (Contract Definition):
    - Establish core domain models and interfaces in `pkg/types/types.go` (`Document`, `Token`, `Posting`, `Index`, `QueryNode`, `SearchResult`).
  - Phase 2 (Parallel Component Tracks - 4 Isolated Packages):
    - Component 1 (`pkg/analysis`): Text analyzer handling case-folding (lowercasing), punctuation removal, stop-word filtering (standard minimal English stop words list), and simple prefix/suffix normalization (stemming or trimming).
    - Component 2 (`pkg/index`): Thread-safe inverted index storing postings maps (`term -> []Posting{DocID, TermFrequency, Positions}`) with concurrent read/write protection (`sync.RWMutex`).
    - Component 3 (`pkg/ranking`): Scoring engine computing TF-IDF relevance scores for matched documents across multi-word queries, returning sorted matches descending by score.
    - Component 4 (`pkg/query`): Query parser supporting single-word terms and boolean operators (`AND`, `OR`, `NOT`, plus optional grouping parentheses).
    (Note: Within each component track / feature, implementer and tester responsibilities must be dispatched separately).
  - Phase 3 (Integration & CLI):
    - Wire components together in an engine facade (`pkg/engine`).
    - Build a runnable CLI binary at `cmd/minisearch/main.go` that can ingest a folder of `.txt` files and enter an interactive search loop or run single-shot CLI queries: `minisearch -dir ./docs "golang AND concurrency"`.
    - End-to-end integration and verification performed by an independent verifier agent.
- Documentation & Usability:
  - Must generate a comprehensive `README.md` at the project root explaining:
    1. Project overview and architecture breakdown.
    2. Query syntax guide with examples (single terms, `AND`, `OR`, `NOT`).
    3. How to build the binary and run tests (`go test ./...`).
    4. Practical usage examples showing how to index sample text files and inspect ranked results.

## Quality bar
- Deterministic ranking over linguistic edge-case perfection. A task is considered done when `go build ./...` and `go vet ./...` succeed and independent verification tests pass.
- Verification Standards:
  - Scoring & Ranking Invariant: When searching for `"concurrency"`, a document containing the word 5 times must score higher than a document containing it once (higher Term Frequency). When searching for `"go AND concurrency"`, documents containing both terms must rank higher than documents containing only one.
  - Boolean Precision: Queries with `NOT <term>` must strictly exclude documents containing that term. `AND` queries must only return documents containing all required terms.
  - Test Fixtures: Use small string slices or `t.TempDir()` with 3 to 6 short text documents (e.g., 2–5 sentences each). Do not load multi-megabyte corpora in automated unit tests.
- Out of Scope (Do Not Block On):
  - Do not require complex linguistic Porter Stemmer algorithms if a lightweight algorithmic suffix stripper or basic normalization passes test fixtures.
  - Do not require persistent disk storage, B-Trees, or WAL logs; the index is strictly in-memory.
  - Do not require fuzzy/Levenshtein matching, typo tolerance, or phrase distance/proximity scoring.
- Parallel Task Isolation:
  - When evaluating individual component packages (`pkg/analysis`, `pkg/index`, `pkg/ranking`, `pkg/query`), verify strictly against that package's local unit tests (`_test.go`). Do not block or fail an isolated component task because a sibling track is not yet merged or integrated.
- Fix Threshold & Fallback:
  - Fix compiler errors, panics, broken boolean logic, or incorrect sort order.
  - After at most 5 iterations on a task, if the core algorithms work and pass their main tests, call `task_done` anyway, record minor limitations in the task note, and proceed to integration.
