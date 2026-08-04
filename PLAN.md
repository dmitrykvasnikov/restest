# restest — MVP implementation plan

Companion to `DESIGN.md`, which holds the decisions. This document holds the order of work.
Written 2026-08-02, before any code.

Milestones are sequenced so that each one ends at a state that can be demonstrated and, from
M2 onward, is genuinely usable. Nothing here is a deadline; the ordering is the point.

**Status (2026-08-04):** M0–M3 merged to master. M4 (request log and inspector) is next;
see `notes/notes_04_1.md` § "Next step" for where to pick up.

---

## M0 — Skeleton ✅ done (feature/01-skeleton)

Nothing user-facing. Establishes the shape everything else drops into.

- [x] `go.mod`, `cmd/restest/main.go`, graceful shutdown on SIGTERM.
- [x] Config from environment; fail loudly at startup on anything missing.
- [x] `log/slog` JSON handler, request id in context, logged on every request.
- [x] pgx connection pool; goose migrations embedded via `go:embed` and applied at startup.
- [x] `docker-compose.yml` with Postgres 17 and the app; multi-stage Dockerfile to distroless.
- [x] `GET /healthz` (process alive) and `GET /readyz` (database reachable).
- [x] `Makefile`: `run`, `test`, `migrate`, `sqlc`, `lint`.

**Done when:** `docker compose up` yields a running binary against a migrated database.

## M1 — Accounts and projects ✅ done (feature/02-accounts-projects)

- [x] Register, log in, log out. Argon2id hashing with tuned parameters.
- [x] `scs` sessions in Postgres; `HttpOnly`, `Secure`, `SameSite=Lax`.
- [x] CSRF via `nosurf` on every mutating form.
- [x] Project CRUD. Slug validation against the format constraint, plus a reserved-word list
      (`api`, `admin`, `demo`, `m`, `static`, `healthz`).
- [x] Base layout: Tailwind, HTMX, page shell, flash messages.

**Done when:** a new user can register, log in and create a project.

## M2 — Static mocks — *first useful milestone* ✅ done (feature/03-static-mocks)

- [x] Endpoint CRUD in the UI, with CodeMirror for the response body.
- [x] **The route matcher.** In-memory radix trie built from the database, held behind an
      `RWMutex`, rebuilt on any endpoint change. Literal segments outrank parameter segments, so
      `/users/me` beats `/users/{id}`.
- [x] `/m/{slug}/…` handler: match, apply `delay_ms`, write status, headers and body.
- [x] Unmatched requests return 404 with a JSON body listing the nearest defined routes.
- [x] Method fallthrough: a path that exists under a different verb returns 405, not 404.

**Done when:** an endpoint defined in the UI answers correctly to `curl`.

**Risk:** the matcher is the trickiest code in the project. Table-driven tests for precedence,
trailing slashes, encoded path segments and parameter extraction should be written alongside it,
not after.

## M3 — Stateful collections — *satisfies the core of task.md* ✅ done (feature/04-collections)

- [x] Collection CRUD; seed edited as a JSON array.
- [x] One `collection` endpoint expands to six routes rooted at its path.
- [x] List: `_page`, `_limit`, `_sort`, `_order`, plus `field=value` filters served by the GIN index.
- [x] Create allocates ids via `UPDATE collections SET next_serial = next_serial + 1 RETURNING`,
      or generates a uuid, per `id_strategy`.
- [x] `PUT` replaces, `PATCH` shallow-merges, `DELETE` returns 204.
- [x] Reset to seed, in the UI and at
      `POST /api/v1/projects/{slug}/collections/{name}/reset`.

**Done when:** `POST` a record, `GET` it back, filter for it, delete it, reset, and it is gone.

**Known gap carried forward:** the reset route is session-authenticated and CSRF-guarded only —
not yet scriptable without a browser session. M6 adds bearer-token auth at the same URL.

## M4 — Request log and inspector ⬅ next up (feature 05, not started)

- [ ] Recording middleware wrapping the mock handler: captures request and response, truncating
      bodies above a cap.
- [ ] Writes go to a buffered channel drained by a batching writer, so logging never sits in the
      request path. **The buffer must report drops rather than discard silently** — a log that
      quietly loses entries is worse than no log.
- [ ] Per-project list view, live-tailing over SSE; detail view of a single exchange.
- [ ] Monthly partition creation job, and retention by detaching old partitions.

**Done when:** requests appear in the UI as they arrive, without a refresh.

## M5 — Public datasets (not started)

- [ ] Seed templates: `users`, `posts`, `comments`, `todos`.
- [ ] New projects can be created pre-seeded from a template.
- [ ] Demo project provisioned at startup, reachable at `/m/demo/…` with no account.
- [ ] Scheduled reset of the demo project so one visitor cannot spoil it for the next.

**Done when:** `curl /m/demo/users` works from a logged-out browser.

## M6 — API tokens and management API (not started)

- [ ] Token CRUD; plaintext shown once, SHA-256 stored, prefix retained for identification.
- [ ] `Authorization: Bearer` middleware, updating `last_used_at`.
- [ ] `/api/v1/` covering projects, endpoints, collections and reset, so a CI job can configure
      mocks without the UI.

**Done when:** a shell script can create a project, define endpoints and reset state.

## M7 — Hardening (not started)

- [ ] Per-IP and per-project rate limiting on mock traffic; request body size caps; server
      read/write timeouts.
- [ ] Security headers, and a strict CSP that the vendored CodeMirror actually satisfies.
- [ ] Backup and restore procedure, written down and tested by restoring.
- [ ] Prometheus metrics: request rate, match rate, latency, exchange write queue depth.

---

## Testing

- Unit tests for the matcher, filter parsing and id allocation.
- Integration tests against real Postgres via testcontainers-go. The heavy use of `jsonb` means
  a mocked database would test nothing worth testing.
- HTTP-level tests through `httptest` exercising the mock server end to end.
- Golden files for response fixtures.

## Deliberately deferred

Scriptable responses, OpenAPI import, subdomain addressing, the outbound runner (`DESIGN.md`
§10), and any form of team access control. Each is additive on the schema above.

## Open questions that touch this plan

Carried from `DESIGN.md` §12. None block M0–M2.

- **State isolation (§12.1)** lands in M3 if it is wanted early. The additive path is a nullable
  scope column on `documents` plus a client-supplied header; deciding late costs a migration but
  not a redesign.
- Demo reset policy (§12.2) is needed by M5, retention window (§12.3) by M4.
