# Session notes 00-1 — Project idea and stack selection

Date: 2026-08-02
Outcome: scope disambiguated, stack chosen, `DESIGN.md` written. No code yet.

---

## Starting point

The repository contained a single file, `task.md`, with a five-line description: a tool for
testing REST API services, able to respond to GET/POST and other standard requests, with a web
interface for managing a database of endpoints, some auth-free sample datasets, and a user system
for editing endpoints and test data.

Request for this session: analyse `task.md` and pick a stack — languages, database, and anything
else needed. Implementation planning explicitly out of scope.

## The central ambiguity

"Tool for testing REST API services" has two opposite readings:

1. **Mock server** — the app responds to requests; the user tests *their client* against it.
   (JSONPlaceholder, Beeceptor, httpbin.)
2. **Test client / runner** — the app sends requests to *the user's API* and asserts on responses.
   (Postman, Newman.)

The rest of `task.md` pointed clearly at reading 1 — "should be able to respond on GET/POST",
"sets of data to test without authorisation, like getting list of users". But the two readings
imply completely different architectures, so it was raised rather than assumed.

## Gaps identified in task.md

- **No request log / inspector.** Roughly half the value of such a tool is seeing exactly what a
  client actually sent.
- **No namespacing.** With multiple users, whose `/users` is served at `/users`?
- **No programmatic access.** Web login only; CI needs API tokens.
- **No non-goals.** Nothing stating what is deliberately not built.

A technical observation that shaped the language choice: **routes in this application are defined
at runtime from the database, not at compile time.** That negates the main advantage of
type-level routers (Servant in Haskell, tRPC in TypeScript) — routing will be dynamic matching
regardless.

## Key exchange — starting with mocks while keeping the runner possible

Question raised: can we build scenario 1 first and still lay a foundation for the runner, or is
that too complex?

Answer: yes, and it costs roughly 5–10% extra effort, almost all of it in schema design. What the
two phases genuinely share is users/projects/auth, the HTTP-exchange representation, the request
log, and the UI shell. What they do not share is direction of data flow — mocking is an inbound
server, the runner is an outbound client with schedulers, retries and SSRF exposure.

Worth doing now (cheap):

1. One domain type and one table for an HTTP exchange, with a `direction` field. This is the
   single most valuable shared abstraction and it is free.
2. Ownership as user → project → endpoints, not user → endpoints, so test suites attach to
   projects later without a data migration.
3. Module boundaries from day one: `core/` ← `mock/`, later `core/` ← `runner/`, never between.
4. Business logic outside HTTP handlers, so a worker can drive it later.

Worth *not* doing (this is where "too complex" actually lives): plugin systems, a shared interface
implemented by both mock and runner, queues and workers, an assertion DSL. Premature
generalisation here costs more than an honest later refactor.

Two things cheap to decide now and painful to change later: the mock addressing scheme (changing
it breaks every saved URL), and awareness that the runner is an SSRF surface, which eventually
shapes process and network topology.

## Decisions

| Question | Answer |
|---|---|
| Direction | Mock server first; outbound runner as an explicitly reserved phase 2 |
| Backend language | Go |
| Mock sophistication | Stateful CRUD from v1 — POST really creates, GET returns it, reset-to-seed |
| Deployment model | Undecided; design must keep both self-hosted and SaaS viable |

Derived from those:

- **PostgreSQL, not SQLite.** The undecided deployment model settles it — Postgres → SQLite is
  never needed, SQLite → Postgres under load is real work. `jsonb` + GIN suits schema-less
  collection documents; the exchange log is write-heavy and SQLite serialises writes; time
  partitioning handles retention. Critically, `SELECT … FOR UPDATE SKIP LOCKED` makes Postgres a
  sound job queue, so phase 2 needs no Redis or RabbitMQ.
- **Path-prefix mock addressing `/m/{project-slug}/…`, not subdomains.** No wildcard DNS or
  DNS-01 TLS challenge, works on localhost unchanged, viable for both deployment models.
  Subdomains can be added later as an alias without breaking existing URLs; the reverse is the
  painful direction.
- **HTMX + Go templates + Alpine + Tailwind, not a SPA.** The UI is CRUD forms plus a live log;
  SSE covers live tailing without WebSockets; `go:embed` preserves single-binary deployment;
  Tailwind's standalone binary keeps npm out of the toolchain entirely. A later move to a SPA
  would rewrite only the view layer.
- **Server-side sessions, not JWT.** Browser sessions need instant revocation, which stateless
  tokens only achieve by reintroducing a server-side table, more complicated. API tokens
  separately, for CI.
- Supporting choices: pgx v5 + sqlc (no ORM), goose migrations, Argon2id, `scs` sessions,
  `nosurf` CSRF, `log/slog`, testcontainers-go, distroless Docker + Caddy.

## Deliverables

- `DESIGN.md` — full design document, 12 sections. Holds decisions and reasoning.
- `task.md` — **intentionally left untouched** as the original problem statement, so that problem
  and solution stay visibly distinct.

## Open questions carried forward

1. **State isolation for stateful collections** — the important one. Shared per-project state
   means two parallel CI runs interfere. Suggested path: ship shared, add optional per-run
   isolation via a client header later, since it is additive.
2. Demo project reset policy, and whether anonymous writes persist or are only echoed.
3. Log retention window and body size cap.
4. Whether anonymous visitors can create throwaway projects.
5. Deployment model — still open by design.

## Next step

Database schema and an MVP implementation plan.
