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

The rest of the rules were settled while building M2:

- **The search backtracks.** Preferring the literal child at each step is not enough on its
  own. With `/a/b/c` and `/{x}/b/d` both defined, a request for `/a/b/d` descends into the
  literal `a`, runs out of trie, and has to come back up and try the parameter branch. A
  matcher that committed to the literal branch would 404 a route that exists.
- **A parameter is a whole segment.** `/v{n}` is refused at definition time rather than
  quietly matching the literal text `v{n}`.
- **Parameter names belong to the route, not to the trie node.** `/users/{id}` and
  `/users/{login}` walk through the same node; the walk collects values and the route that
  answered names them.
- **Slashes are normalised on both sides.** `/users`, `/users/` and `//users` are one route,
  and the stored pattern is the normalised form — which is what makes the unique index on
  (project, method, path) mean what it says.
- **Matching is on the escaped path.** `r.URL.Path` has already turned `%2F` into a slash, so
  `/users/a%2Fb` would split into three segments and hand the endpoint a parameter the client
  never sent. Segments are split first and decoded afterwards.
- **A method mismatch is 405 with `Allow`**, listing every verb the path answers across all
  the patterns it matches. `OPTIONS` on a path nothing claims is answered `204` with the same
  header rather than refused: that is the question `OPTIONS` asks.
- **An exact verb beats `*`, and `HEAD` falls back to `GET`** — net/http discards the body of
  a `HEAD` response, so the headers are right and the body is absent without a second route.
- **Delay is applied before anything is written**, with the response's write deadline pushed
  out to cover it. The alternative — raising the server's `WriteTimeout` to the 60 s ceiling
  `delay_ms` allows — would weaken the guard on every route to accommodate a handful.

Two patterns that reduce to the same shape and verb — `/users/{id}` and `/users/{name}` for
`GET` — are a pair the database cannot refuse, because it compares the pattern text. The first
by path order wins and the other is logged as shadowed, so the choice is deterministic and the
dead route is not silent.

The table is rebuilt from the database on any change to an endpoint or a project, and on a
30-second timer besides. The timer is not how a new endpoint goes live; it is the answer to a
rebuild that failed and to a second instance whose edits this one never saw.

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

The rest of the rules were settled while building M3:

- **One endpoint row expands into six routes**, done where the table is built rather than
  inside the trie. The trie takes routes one at a time and knows nothing about collections,
  so `/users/me` defined statically still outranks the `/users/{id}` the expansion added,
  and a verb nothing claims still answers 405 with the verbs that are claimed.
- **The endpoint row is stored under the wildcard verb.** It is not a route anyone can send;
  it is the row six routes come from. Storing it that way is also what makes the unique
  index on (project, method, path) refuse a second collection rooted at the same place.
- **The identifier is the server's.** A client that sends one of its own has it overwritten,
  because two clients posting the same fixture would otherwise collide and the second would
  get an error from a server that exists to be predictable. A *seed* may name its own ids —
  that is how a fixture gets to say `/users/1` — and allocation steps over the ones it named.
- **The counter is advanced in the statement that reads it**, so two concurrent creates
  cannot be handed one number: the second waits on the row lock the first holds.
- **A whole-number identifier is written into the document as a number**, a uuid as a string.
  A client mocking a real API expects the type the real one would send.
- **Filters are containment tests**, which is what the `jsonb_path_ops` GIN index can answer;
  comparing `body ->> 'field'` would be a sequential scan. A query string has no types, so a
  value that also reads as a JSON scalar is matched both ways — `?id=1` finds `{"id":1}` and
  `{"id":"1"}` alike — as two containment tests ORed together, both index-backed.
- **An unknown underscore-prefixed parameter is refused**, not ignored. The underscore
  namespace is the server's, so `?_limits=5` is a typo; answering it with the first hundred
  documents would look as though the parameter had worked.
- **A listing has a default limit of 100 and a ceiling of 1000**, with `X-Total-Count` saying
  what was left out. An unlimited listing is a promise that gets harder to keep as somebody's
  fixture grows.
- **Documents carry a `seq` column** (migration `00002`) and it is what an unsorted listing is
  ordered by, and the tie-break under `_sort`. Neither existing column would do: `created_at`
  is identical across a seed, and `id` is a random uuid, so paging could return a document
  twice or not at all.
- **`PATCH` is `jsonb`'s `||` — shallow, one level.** A nested object in the request replaces
  the one in the document. A deep merge would leave no way to remove a nested field, and `PUT`
  is there for callers who want to say what the whole document is.
- **Saving a collection does not apply its seed.** Editing the seed prepares what the next
  reset will restore; throwing away the documents somebody is working with as a side effect of
  saving would be a surprise. Creating a collection *does* apply it, because a collection that
  needs a reset before it answers is a step nobody would guess at.
- **Reset is one transaction**, so a client reading the collection sees the old contents or
  the new ones and never an empty collection halfway through being refilled. That matters
  because reset is what a test suite calls between runs, and the run after it starts at once.

### 5.1 What the reset route can and cannot do today

`/api/v1/` is authenticated by the session cookie and guarded by CSRF, which makes the reset
route usable from the interface and **not yet from a shell script**. The alternative was
exempting a cookie-authenticated mutating route from CSRF, which is the hole the guard exists
to close.

A token sent as `Authorization: Bearer` is not a cookie and needs no such exemption, so M6 is
what makes this route scriptable — at this URL, unchanged. Building bearer auth early to close
the gap would be building M6 inside M3.

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

`Secure` follows the scheme of `RESTEST_BASE_URL`, the address users actually reach the
instance on. A browser never returns a `Secure` cookie over plain HTTP, so hard-coding it on
would break every local login and hard-coding it off would ship that mistake to production.
One setting decides it, because two settings for one question can disagree. The same value
also decides whether the CSRF middleware treats requests as arriving over TLS, which is what
makes its `Origin`/`Referer` check correct behind a proxy that terminates TLS.

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
| JSON editor | CodeMirror 5, vendored | §9.3 |
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

### 9.3 Why CodeMirror 5 and not 6

Decided during M2, when the editor was actually needed. The original choice was CodeMirror 6;
it was changed because it cannot be met without breaking a harder constraint.

CodeMirror 6 is published only as ES modules split across a dozen packages — `@codemirror/state`,
`view`, `language`, `lang-json` and the rest — which have to be bundled into something a browser
can load, and which break outright if two copies of `@codemirror/state` end up in the page.
Bundling means a bundler, and a bundler means npm. "No npm in the toolchain" is a constraint
this project committed to (`CONTEXT.md` §6), and it is the more valuable of the two: it is what
keeps `go build` the whole build.

The alternatives to dropping to version 5 were both worse. Vendoring a prebuilt bundle from a
third-party build service would put an opaque 400 kB artefact in the repository that nobody can
regenerate or audit. Shipping CodeMirror 6 without `lang-json` would give an editor with no JSON
highlighting, which is most of what the editor is for.

CodeMirror 5 ships plain script files — `codemirror.js`, `codemirror.css` and the modes and
addons — that `curl` fetches and `go:embed` serves. `make vendor-codemirror` does the fetching,
and is run only when the version changes. It is in maintenance rather than active development,
which for a JSON textarea is not a problem worth solving today. Version 6 becomes available
again the day a bundler is acceptable, and the change is one file: `static/js/editor.js`.

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
    web/           # handlers, middleware
      templates/   # Go templates, embedded via go:embed
      static/      # generated CSS and vendored JS, embedded via go:embed
      tailwind.css # build input, not embedded and not served
    runner/        # phase 2, outbound — depends only on core/
    integration/   # tests needing a real Postgres, behind a build tag
  migrations/      # goose migrations, embedded via go:embed
  DESIGN.md
  task.md
```

Templates and assets sit next to the handlers that serve them rather than in a top-level
`web/`, so that one package owns the whole browser-facing side and its `go:embed` directives
name paths inside itself.

`internal/web/static/css/app.css` is generated by `make assets` from the Tailwind standalone
binary and is **committed**, so `go build` needs no Node, no Tailwind and no network. Its URL
carries a hash of the embedded assets, which is what lets it be cached forever and still
change the moment a deployment changes it.

`internal/core/dbgen` is generated by `make sqlc` from `migrations/` and
`internal/core/queries/`. The migrations are the only schema definition: there
is no second copy for the generator to drift from.

## 12. Open questions

Genuinely undecided; none block starting work.

1. **State isolation for stateful collections.** With shared per-project state, two
   parallel CI runs interfere with each other. The alternative is per-run state keyed by a
   client-supplied header. Suggested path: ship shared state, add optional isolation later
   — it is additive. **M3 shipped shared state**, as suggested. A collection rooted below a
   parameter — `/tenants/{tenant}/users` — matches and collects the parameter but reads the
   same documents whatever its value: state is per collection, not per parameter. Isolation
   is still the nullable scope column on `documents` plus a client-supplied header, and it is
   still additive.
2. **Demo project reset policy** — how often, and whether anonymous writes are persisted at
   all or only echoed.
3. **Log retention window** and body size cap.
4. **Anonymous throwaway projects** — whether a visitor can create a temporary project
   without registering.
5. **Deployment model** — self-hosted single instance vs public service. Still open; the
   design above is compatible with both, which is why Postgres and path-prefix addressing
   were chosen.
