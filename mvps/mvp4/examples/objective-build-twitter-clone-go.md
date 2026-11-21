# Objective
Build a lightweight, in-memory Twitter-like web service in Go featuring passwordless user registration, tweet posting, following relationships, chronological timeline feeds, and a self-contained, browser-based single-page web UI served directly by the Go backend without external frontend tooling. A person must be able to start the server, open it in a browser, and use every feature without errors.

## Constraints
- Implementation & Dependencies:
  - Backend: Written strictly in Go (1.22+, so `http.ServeMux` method patterns like `"POST /users"` are available) using only the Go standard library (`net/http`, `encoding/json`, `sync`, `time`, `sort`, `embed`, `regexp`, `unicode/utf8`). No external modules (`go get` forbidden), no database drivers.
  - Frontend: Pure vanilla HTML, CSS, and JavaScript in a single file, `web/index.html`, embedded via `embed.FS`. Strictly NO npm, Node, React, Vite, bundlers, or third-party frontend dependencies.
- Architecture:
  - In-memory thread-safe state protected by `sync.RWMutex`. Data is lost when the server restarts; that is expected.
  - Directory layout:
    - `internal/store`: domain models and concurrent storage.
    - `internal/api`: REST handlers and routing.
    - `web/`: `index.html`, the embed declaration, and its test.
    - `cmd/server/main.go`: server initialization and entry point. Listens on `:8080` by default (`-addr` flag to override) and logs `listening on http://localhost:8080`.
- API conventions (the frontend relies on these exactly):
  - Every response body is JSON with `Content-Type: application/json`, except `GET /`.
  - Every error response is `{"error": "<human-readable message>"}`.
  - A user is `{"username": "alice"}`. A tweet is `{"id": 1, "username": "alice", "body": "hello", "created_at": "<RFC 3339 time>"}`.
  - Usernames: 1–20 characters, letters, digits and `_` only. Tweet bodies: 1–280 characters, counted as Unicode characters (`utf8.RuneCountInString`), not bytes.
- API Endpoints (JSON REST):
  1. `POST /users`: Register with `{"username": "alice"}`. `201` with the user; `400` if invalid; `409` if taken.
  2. `GET /users`: `200` with a JSON array of all usernames, sorted alphabetically (`[]` when there are none, never `null`).
  3. `POST /tweets`: `{"username": "alice", "body": "hello world"}`. `201` with the tweet; `400` if the body is invalid; `404` if the user doesn't exist.
  4. `POST /follow`: `{"follower": "alice", "target": "bob"}`. `200` with `{"following": true}`; `400` for self-follow; `404` if either user doesn't exist. Following twice is not an error.
  5. `POST /unfollow`: `{"follower": "alice", "target": "bob"}`. `200` with `{"following": false}`; `404` if either user doesn't exist. Unfollowing someone not followed is not an error.
  6. `GET /following?username=alice`: `200` with the alphabetically sorted array of usernames alice follows; `404` if the user doesn't exist.
  7. `GET /timeline?username=alice&limit=20`: `200` with a JSON array (never `null`) of tweets by alice and the accounts she follows, newest first. Sort by a per-server increasing tweet `id`, descending, so tweets created within the same clock tick keep their order. `limit` defaults to 20, max 100; `400` if not a positive integer; `404` if the user doesn't exist.
  8. Unknown routes return `404`; a wrong method on a known route returns `405`.
- Frontend Requirements (`GET /`):
  - The Go server serves `web/index.html` at `GET /` with `Content-Type: text/html; charset=utf-8`.
  - The UI must allow a user in any standard browser to:
    1. Register a username via a simple input form.
    2. Switch the active user, choosing from the server's user list (`GET /users`), loaded when the page opens and after each registration. The active user may be remembered in `localStorage`, but the list of users comes from the server, so it is correct after a server restart or in a second browser.
    3. Post a new tweet as the active user, with a live character count up to 280.
    4. View the active user's timeline, refreshed after posting, following or unfollowing, on switching user, and every 5 seconds.
    5. Follow or unfollow other users, picked from the user list, and see whom the active user follows (`GET /following`).
  - Robustness:
    - Every function the script calls must be defined in the script, and every `getElementById` id must exist in the markup.
    - Error messages shown to the user must include the real cause: the server's `error` field for a non-2xx response, or the exception's message (`err.message`) for anything thrown. Never show a fixed "network error" text that hides a JavaScript bug. Also log caught errors with `console.error`.
    - If the remembered active user no longer exists on the server (`404`, e.g. after a restart), clear it and ask the user to register or pick another.
    - Render tweet bodies and usernames with `textContent`, never `innerHTML`, so a tweet containing HTML can't break the page.
    - Use relative URLs (`fetch('/users')`), so the page works from whatever address serves it.
- Automated Testing & Verification:
  - `internal/api/api_test.go`, using `net/http/httptest` against the same handler `main.go` serves. It must simulate:
    1. Registering `alice` and `bob`; `GET /users` returns `["alice","bob"]`.
    2. `bob` posts two tweets.
    3. `alice` follows `bob`; `GET /following?username=alice` returns `["bob"]`.
    4. `alice` posts one tweet.
    5. `GET /timeline?username=alice` returns all three tweets, newest first.
    6. After `alice` unfollows `bob`, her timeline has only her own tweet.
    7. Error cases return the right status and an `{"error": ...}` body: duplicate username (`409`), invalid username (`400`), empty and 281-character tweets (`400`), self-follow (`400`), unknown user (`404`).
    8. `GET /` returns `200` with `text/html` content containing the app markup.
  - `web/index_test.go`: a static check of `index.html` in plain Go (read the embedded file, use `regexp`), so mistakes in the page fail `go test` without a browser:
    1. Every id passed to `getElementById(...)` exists as an `id="..."` attribute in the markup.
    2. Every bare function call in the script (`name(` not preceded by `.`) is either defined in the script (`function name`, `const name =`, `let name =`) or on a short allowlist of JavaScript keywords and built-ins (`if`, `for`, `while`, `switch`, `catch`, `function`, `return`, `typeof`, `fetch`, `encodeURIComponent`, `setInterval`, `setTimeout`, `parseInt`, `String`, `Number`, `Date`, `Error`, `Array`, `JSON`, `console`, `alert`, ...). Keep the allowlist to built-ins; never add the page's own function names to it.
    3. Every path passed to `fetch(...)` is one of the API routes above.
  - All automated tests must pass cleanly under `go test -race ./...`, and `go vet ./...` must be clean.
- Documentation: `README.md` says how to run it (`go run ./cmd/server`, then open http://localhost:8080 in a browser, not `index.html` from disk) and how to run the tests.

## Quality bar
- Functional over polished. A task is done when `go build ./...` and `go vet ./...` succeed and its tests pass. Do not block on visual styling, refactoring, or subjective design patterns.
- Frontend: the static checks in `web/index_test.go` plus a careful read of the script are the verification. Do not ask for browser automation, cross-browser testing, responsive CSS or animations. Do check, by reading the code, that each button's click handler calls only defined functions, sends the JSON the API expects, and reads the fields the API returns.
- Fix Threshold & Fallback:
  - Fix compiler errors, panics, failing tests, and anything that would make a feature fail in the browser.
  - After at most 5 worker turns on a task, if the core functionality works but secondary checks still fail, call `task_done` anyway, record the remaining issues in the note, and move on.
- Concurrency: basic `sync.RWMutex` protection is required, and tests run with `-race`; no load testing.

## Done when
- `go vet ./...` and `go test -race ./...` pass, including `web/index_test.go`.
- Every endpoint above exists with the stated status codes and JSON shapes.
- The director has read `web/index.html` and traced each user action (register, switch user, post, follow, unfollow, timeline refresh) from its click or change handler through `fetch` to the UI update, and found no call to an undefined function, wrong field name or wrong route.
- `README.md` has the run instructions.
