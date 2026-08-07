# restest — MVP implementation plan

Companion to `DESIGN.md`, which holds the decisions. This document holds the order of work.
Written 2026-08-02, before any code.

Milestones are sequenced so that each one ends at a state that can be demonstrated and, from
M2 onward, is genuinely usable. Nothing here is a deadline; the ordering is the point.

**Status (2026-08-07):** M0–M6 merged to master. M7 (hardening) is next; see
`notes/notes_07_1.md` § "Next step" for where to pick up.

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
not yet scriptable without a browser session. **Closed by M6**, which added bearer-token auth at
the same URL, unchanged.

## M4 — Request log and inspector ✅ done (feature/05-request-log)

- [x] Recording middleware wrapping the mock handler: captures request and response, truncating
      bodies above a cap.
- [x] Writes go to a buffered channel drained by a batching writer, so logging never sits in the
      request path. **The buffer reports drops rather than discarding silently** — counted,
      warned about in the process log, and shown on the inspector page.
- [x] Per-project list view, live-tailing over SSE; detail view of a single exchange.
- [x] Monthly partition creation job, and retention by detaching old partitions.
- [x] Credentials redacted at capture rather than at display, so nothing that reaches the table
      can be un-redacted later. Not on the original list; it belonged here.

**Done when:** requests appear in the UI as they arrive, without a refresh.

**Decisions that closed `DESIGN.md` §12.3:** 64 KiB per recorded body, three months of retention,
both configurable. See §7.1 there for the reasoning.

**Carried forward:** the log has no search or filters, and is not reachable over `/api/v1/`. The
second **landed with M6**, which routed the rest of the API anyway; the first is still open.

## M5 — Public datasets ✅ done (feature/06-datasets)

- [x] Seed templates: `users`, `posts`, `comments`, `todos`. Embedded JSON, stored as a
      collection's seed, so a dataset is the same object a user could have typed.
- [x] New projects can be created pre-seeded from a template — the project and every dataset
      it was created with in one transaction.
- [x] Demo project provisioned at startup, reachable at `/m/demo/…` with no account. An
      ordinary project with an owner nobody can log in as.
- [x] Scheduled reset of the demo project so one visitor cannot spoil it for the next: hourly
      by default, and once at startup.

**Done when:** `curl /m/demo/users` works from a logged-out browser.

**Decisions that closed `DESIGN.md` §12.2:** anonymous writes are persisted, and the demo is
restored to its seeds every hour. See §6.1 there for the reasoning.

**Carried forward:** a dataset can only be chosen when a project is created; adding one to an
existing project means creating a collection by hand. The demo has no page of its own — it is
offered on the login page and reached with `curl`.

## M6 — API tokens and management API ✅ done (feature/07-api-tokens)

- [x] Token CRUD; plaintext shown once, SHA-256 stored, prefix retained for identification.
      Optional expiry, revocation immediate. Minted in the interface only.
- [x] `Authorization: Bearer` middleware, updating `last_used_at` in the statement that
      authenticates — so an expired or revoked token is refused and not marked used.
- [x] `/api/v1/` covering projects, endpoints, collections and reset, so a CI job can configure
      mocks without the UI. The request log came with it, read-only, closing M4's gap.
- [x] A bearer-authenticated request is exempt from CSRF, and **never falls back to the
      session** — which is what makes that exemption sound rather than convenient.

**Done when:** a shell script can create a project, define endpoints and reset state.

**Decisions that closed `DESIGN.md` §5.1:** see §8.1 there — the no-fallback rule behind the
CSRF exemption, hashing without a KDF, expiry enforced in SQL, and why no route mints a token.

**Carried forward:** tokens are account-wide — there is no per-project scope and no read-only
token, so a token is exactly as powerful as the account that made it. The log is still without
search or filters, over the API as in the browser. There is no rate limiting on `/api/v1/`;
that is M7's.

## M7 — Hardening ⬅ next up (feature 08, not started)

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
- Nothing here is open any longer. The demo reset policy (§12.2) was settled by M5 — writes
  persist, the seeds come back hourly — and the retention window and body cap (§12.3) by M4:
  three months and 64 KiB. All four settings are configurable.
