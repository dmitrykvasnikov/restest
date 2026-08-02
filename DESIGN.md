# restest — Design

Status: **decisions agreed, no code written yet.** Written 2026-08-02.
Companion to `task.md`, which stays as the original problem statement. This document holds
decisions and the reasoning behind them, and grows with the project.

---

## 1. Goal

**restest is a mock REST API server.** Users define endpoints and datasets through a web
interface; restest serves them over HTTP so that users can develop and test *their own
clients* against a predictable, controllable API.

This disambiguates the phrasing in `task.md`: requests flow **inbound**, from the user's
client to us. restest does not send requests to the user's API — that is a deliberately
deferred phase 2 (§10).

## 2. Non-goals

Explicitly out of scope. Listed so that they can be reconsidered deliberately rather than
drifted into:

- GraphQL, gRPC, WebSocket or SSE mocking. HTTP/REST only.
- Recording or proxying real upstream traffic (Beeceptor's pass-through mode).
- Scriptable responses — templating, conditional branching, fake-data generation.
  Considered and postponed: it amounts to a mini-language inside the product.
- OpenAPI/Swagger import and export. Plausible later, not now.
- Team collaboration, shared projects, role-based access control. One owner per project.
- Billing, quotas, plans.
- Chaos features (random failures, bandwidth throttling) beyond a fixed response delay.

## 3. Core concepts

```
User ──owns──> Project ──contains──> Endpoint
                  │                     │
                  ├──contains──> Collection ──contains──> Document
                  └──accumulates──> Exchange (request log)
```

**User** — email, Argon2id password hash. Owns projects and API tokens.

**Project** — the unit of isolation and the thing that appears in mock URLs. Has a unique
`slug`. Endpoints, collections and logs all belong to a project.

Ownership deliberately runs user → project → endpoint rather than user → endpoint, so that
phase 2 test suites can attach to a project without a data migration.

**Endpoint** — a route the mock server answers. Two kinds:
- `static` — fixed status, headers and body.
- `collection` — bound to a Collection, giving conventional REST semantics (§5).

Fields: method, path pattern, kind, response spec, `delay_ms`, `is_enabled`.

**Collection** — a named set of JSON documents plus a `seed` (a JSON array). Schema-less.

**Document** — one JSON object in a collection, stored as `jsonb`.

**Exchange** — one request/response pair with timings. Carries a `direction` field
(`inbound` today, `outbound` in phase 2) so the runner reuses this table rather than
introducing a parallel one. This is the main abstraction shared between the two phases.

**ApiToken** — for CI and scripted management. Random 32 bytes, displayed once, stored as
a SHA-256 hash alongside a short non-secret prefix for identification in the UI.

## 4. URL layout and mock addressing

| Space | Path | Auth |
|---|---|---|
| Web UI | `/` … | session cookie |
| Management API | `/api/v1/…` | session cookie or API token |
| Mock traffic | `/m/{project-slug}/…` | none by default |

**Decision: mock endpoints live under a path prefix, not a subdomain.**

Subdomains (`myproject.restest.dev/users`) look nicer but require wildcard DNS and a
wildcard TLS certificate via DNS-01 challenge, and they do not work on `localhost` without
per-developer setup. The path prefix works identically in local development, on a
self-hosted instance, and in a public deployment.

Subdomains can be added later as an *alias* that rewrites onto the same handler, so
existing path URLs never break. The reverse — starting with subdomains and retrofitting
paths — is the painful direction.

### Matching rules

Path patterns support named parameters: `/users/{id}/posts`. Matching is
method + path against an in-memory radix trie, ordered so that literal segments beat
parameter segments (`/users/me` wins over `/users/{id}`). First match after ordering wins.

An unmatched request returns `404` with a JSON body naming the project and listing the
closest defined routes — the common case is a typo, and a bare 404 wastes the user's time.

## 5. Stateful collections

Endpoints of kind `collection` implement real CRUD against stored documents, so a `POST`
followed by a `GET` returns the new record. This is what makes the "list of users" scenario
in `task.md` actually useful for testing a client.

| Request | Behaviour |
|---|---|
| `GET /items` | list; supports `_page`, `_limit`, `_sort`, `_order`, and `field=value` filters |
| `GET /items/{id}` | one document, or 404 |
| `POST /items` | create, server-assigned id, returns 201 + `Location` |
| `PUT /items/{id}` | full replace |
| `PATCH /items/{id}` | shallow merge |
| `DELETE /items/{id}` | delete, returns 204 |

**Reset to seed** restores the collection to its seed array. Available as a UI button and
as `POST /api/v1/projects/{slug}/collections/{name}/reset`, so test suites can reset
between runs.

Documents are `jsonb` with GIN indexes, which is what makes arbitrary field filtering
possible without a per-collection schema.

## 6. Public datasets

Every project is seeded with optional built-in templates — `users`, `posts`, `comments`,
`todos` — so a new user has something working immediately.

A shared demo project is reachable without any account at `/m/demo/…`, covering the
`task.md` requirement for "simple sets of data to test without authorisation". Anonymous
writes to the demo project are accepted and echoed back but reset on a fixed schedule, so
one user's experiment cannot spoil the demo for everyone else.

## 7. Request log and inspector

Every mock request is recorded as an Exchange: method, path, query, request headers and
body, response status/headers/body, duration, remote address.

The UI shows a live-tailing list per project via SSE, with a detail view of any exchange.
This is not in `task.md` but is half the value of a tool like this — seeing exactly what a
client actually sent is usually the reason someone reaches for a mock server.

Bodies are truncated above a size cap. Retention is time-based, implemented with monthly
table partitions so that expiry is a partition detach rather than a long-running `DELETE`.

## 8. Authentication

**Web UI — server-side sessions.** Cookie holding an opaque id, session state in Postgres
via `alexedwards/scs`. Cookies are `HttpOnly`, `Secure`, `SameSite=Lax`. CSRF tokens on all
mutating forms.

Not JWT. Browser sessions need instant revocation on logout and password change, which
stateless tokens cannot give without a server-side denylist — at which point the session
table is back, only more complicated.

**Passwords — Argon2id**, per current OWASP guidance.

**Programmatic access — API tokens**, scoped to a user, sent as `Authorization: Bearer`.
Only the management API accepts them; mock traffic is unauthenticated.

## 9. Stack

| Layer | Choice | Note |
|---|---|---|
| Language | Go 1.26 | single static binary |
| App routing | stdlib `net/http` + `ServeMux` | Go 1.22+ patterns are sufficient |
| Mock routing | hand-written radix trie | routes come from the DB, rebuilt on change, guarded by `RWMutex` |
| Database | PostgreSQL 17 | §9.1 |
| DB access | pgx v5 + sqlc | type-safe Go generated from plain SQL, no ORM |
| Migrations | goose | embedded via `go:embed` |
| Sessions | `alexedwards/scs` v2, Postgres store | |
| CSRF | `justinas/nosurf` | |
| Frontend | Go templates + HTMX + Alpine.js + Tailwind | §9.2 |
| JSON editor | CodeMirror 6, vendored | |
| Logging | `log/slog`, JSON | |
| Testing | stdlib `testing`, `httptest`, testcontainers-go | real Postgres in integration tests |
| Deploy | multi-stage Docker → distroless, compose, Caddy for TLS | |

### 9.1 Why PostgreSQL and not SQLite

SQLite would work and would be operationally simpler. Postgres is chosen because the
deployment model is still open, and this is the asymmetric bet — going Postgres → SQLite is
never needed, while SQLite → Postgres under load is real work.

- Mock bodies and collection documents are JSON. `jsonb` + GIN indexes give filtering and
  search directly, with no per-collection schema.
- The exchange log is the write-heavy table. SQLite serialises writes to a single writer;
  fine for one developer, a ceiling for anything public.
- Time-based partitioning for log retention.
- **Postgres removes the need for a queue broker.** Phase 2 needs background workers;
  `SELECT … FOR UPDATE SKIP LOCKED` is a sound job queue. No Redis, no RabbitMQ, ever.

Redis is not in the stack. It becomes relevant only for rate limiting across multiple app
instances in a public deployment; until then an in-process cache of endpoint definitions is
enough.

### 9.2 Why HTMX and not a SPA

The UI is CRUD forms plus a live log. HTMX covers both, and SSE gives the live inspector
without WebSockets. Templates and assets embed into the binary via `go:embed`, preserving
single-artifact deployment. Tailwind runs from its standalone binary, so **npm never enters
the toolchain**.

If the UI later grows genuinely rich, a SPA becomes reasonable — and the migration only
rewrites the view layer, since all logic stays server-side. The reverse direction would be
more expensive.

## 10. Phase 2 — outbound test runner (not built)

Reserved so that today's design does not preclude it. **No code for this now.**

The eventual shape: users define request suites against their own API, restest executes
them on a schedule or on demand and reports results.

What today's design already provides: the Exchange type and table with its `direction`
field; project-scoped ownership; API tokens; business logic kept out of HTTP handlers so it
can be driven by a worker; and Postgres as the job queue.

Constraints known in advance:

- **SSRF is the central risk.** A service that fetches arbitrary user-supplied URLs will be
  asked to fetch `169.254.169.254` and internal hosts. The outbound client needs a custom
  `DialContext` rejecting private and link-local ranges, redirect limits, response size
  caps, and hard timeouts. Eventually it belongs in its own process and network zone.
- **Assertions should reuse an existing expression language** — CEL (`google/cel-go`) or
  JSONPath — rather than inventing a DSL.

`internal/mock/` and `internal/runner/` must never import each other. Both depend only on
`internal/core/`.

What must *not* be built ahead of time: plugin systems, a shared interface implemented by
both mock and runner, worker infrastructure, or an assertion language. These are opposite
directions of data flow; a premature common abstraction would be a fiction that costs more
than a later refactor.

## 11. Repository layout

```
restest/
  cmd/restest/main.go   # wiring and shutdown, nothing else
  internal/
    config/        # environment configuration, validated at startup
    logging/       # slog handler construction
    database/      # pgx pool, migration run
    core/          # users, projects, tokens, Exchange type, storage
      queries/     # hand-written SQL, input to sqlc
      dbgen/       # sqlc output — generated, never edited
    mock/          # inbound: matcher, collections, seeding
    web/           # handlers, middleware, templates, static assets
    runner/        # phase 2, outbound — depends only on core/
    integration/   # tests needing a real Postgres, behind a build tag
  migrations/      # goose migrations, embedded via go:embed
  web/
    templates/
    static/
  DESIGN.md
  task.md
```

`internal/core/dbgen` is generated by `make sqlc` from `migrations/` and
`internal/core/queries/`. The migrations are the only schema definition: there
is no second copy for the generator to drift from.

## 12. Open questions

Genuinely undecided; none block starting work.

1. **State isolation for stateful collections.** With shared per-project state, two
   parallel CI runs interfere with each other. The alternative is per-run state keyed by a
   client-supplied header. Suggested path: ship shared state, add optional isolation later
   — it is additive.
2. **Demo project reset policy** — how often, and whether anonymous writes are persisted at
   all or only echoed.
3. **Log retention window** and body size cap.
4. **Anonymous throwaway projects** — whether a visitor can create a temporary project
   without registering.
5. **Deployment model** — self-hosted single instance vs public service. Still open; the
   design above is compatible with both, which is why Postgres and path-prefix addressing
   were chosen.
