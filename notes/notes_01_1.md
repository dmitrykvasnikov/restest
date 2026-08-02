# Session notes 01-1 — M0 skeleton

Date: 2026-08-02
Feature: 01 — M0 skeleton
Branch: `feature/01-skeleton`
Outcome: the milestone's "done when" is met — `docker compose up` yields a running binary
against a migrated database. Nothing user-facing yet, by design.

---

## Starting point

Feature 00 left the schema (`migrations/00001_init.sql`, verified against Postgres 17) and the
plan. No Go code existed. This session wrote the shape everything else drops into.

## What was built

| Path | Role |
|---|---|
| `cmd/restest/main.go` | Wiring and shutdown, nothing else |
| `internal/config/` | Environment configuration, validated at startup |
| `internal/logging/` | slog handler construction |
| `internal/database/` | pgx pool, migration run |
| `internal/core/` | `Store` over the sqlc-generated layer |
| `internal/core/queries/`, `internal/core/dbgen/` | SQL input and sqlc output |
| `internal/web/` | Routing, middleware, health handlers |
| `internal/integration/` | Tests needing real Postgres, behind a build tag |
| `migrations/embed.go` | `go:embed` of the migration files |
| `Dockerfile`, `docker-compose.yml`, `Makefile`, `sqlc.yaml`, `.golangci.yml` | Toolchain |

Go 1.26, module `github.com/dmitrykvasnikov/restest`. Direct dependencies: pgx v5 and goose v3,
plus testcontainers-go for tests. Nothing else — the dependency list stays short deliberately.

## Decisions made while building

These were not settled in `DESIGN.md` and were decided here.

- **Configuration reports every problem at once.** `config.Load` accumulates errors and joins
  them, rather than failing on the first. A misconfigured deployment is then fixed in one pass
  instead of one restart per mistake. `RESTEST_` prefix; an empty variable counts as unset,
  because `RESTEST_DATABASE_URL=` in a compose file is the same mistake as omitting it.
- **`Config` implements `slog.LogValuer`** so the startup line can log the whole configuration
  with the database password redacted. Logging config is too useful to give up over one field.
- **Migrations run at startup, under a Postgres advisory lock** (key `0x72657374657374`,
  "restest" in ASCII). Two instances starting together would otherwise both try to apply the
  same migration; the loser waits and finds nothing to do. The migration run opens its own
  `database/sql` connection with `MaxOpenConns(1)`, because the lock is session-scoped and has
  to live on the same connection as the migrations — borrowing one from the application pool
  would also deadlock a small pool.
- **An incoming `X-Request-Id` is honoured, but only if it is short (≤64) and alphanumeric with
  `-_.`**, otherwise it is replaced by a generated one. This lets a request be followed across a
  proxy or a CI job without letting a caller write arbitrary text into our logs. Ids come from
  `crypto/rand.Text()`.
- **`/healthz` deliberately does not touch the database.** A liveness probe that fails when the
  database is down would have the orchestrator restart a healthy process, which cannot help and
  drops the in-flight requests. `/readyz` is the one that checks the database, and returns 503
  with `{"status":"unavailable","detail":"database unreachable"}` — the driver's error goes to
  the log, not to an unauthenticated caller.
- **`/readyz` runs a real query through the sqlc layer** (`select 1 as ok`) rather than a
  protocol-level ping, so it fails when the pool is exhausted or the server has stopped
  planning statements. It also means the generated database layer is exercised from M0, so M1
  does not meet sqlc wiring problems for the first time.
- **Access log level depends on the response**: 5xx error, 4xx warn, successful health probes
  debug, everything else info. The container runtime probes every few seconds; at info those
  lines would drown everything worth reading.
- **The panic recoverer sits inside the access-log middleware**, so a recovered panic is still
  reported in the access log as the 500 it became. `http.ErrAbortHandler` is re-panicked rather
  than swallowed, since that is how a handler says the client has gone.
- **`recorder` implements `Unwrap()`** so `http.ResponseController` reaches the real writer —
  the live log tail in M4 needs to flush.
- **Compose publishes both services on 127.0.0.1 by default** (`RESTEST_BIND` to override).
  There is no authentication until M1, and a development instance should not land on the LAN by
  accident.
- **Tools are pinned in the Makefile and fetched with `go run`** — goose, sqlc, golangci-lint —
  so a checkout needs only Go and Docker, and everyone runs the same versions. Nothing extra
  enters `go.mod`.
- **The image build cannot see `.git`**, so `make up` passes the commit in as a build argument
  and it is linked into the binary; a local `go build` picks it up automatically, adding
  `-dirty` when the tree is not clean. The startup line says which build is running.

## Verification

Everything below was run, not reasoned about.

- `make test` — unit tests, race detector on, all pass. Config parsing and defaults, request id
  generation and rejection of hostile values, access log fields and levels, panic handling,
  `ResponseController` reaching through the wrapper, health handler behaviour including a
  database that never answers.
- `make test-integration` — Postgres 17 in a container via testcontainers-go. Migration from
  empty creates the expected eight tables plus `exchanges_default` and `goose_db_version`;
  migration is idempotent across three runs; **down removes everything** (`CONTEXT.md` §7); the
  full startup sequence ends with `/readyz` answering 200; `/readyz` turns 503 once the database
  is gone.
- `make lint` — `go vet` plus golangci-lint with `bodyclose`, `errorlint`, `misspell`, `nilerr`
  on top of the default set. Clean.
- `make up` from a removed volume: `version_before: 0, version_after: 1`, both probes 200, ten
  tables in `public`. **This is the milestone's "done when".**
- `docker compose stop app` → `shutting down` then `stopped` in 0.2s, well inside the grace
  period. SIGTERM reaches the binary because it is PID 1 in exec form.
- `make run` on the host against the compose database, and `make migrate` / `make migrate-status`
  through the goose CLI.

## Deliberately not done

- **No container `HEALTHCHECK` for the app.** distroless has no shell and no curl, so it would
  need a self-check mode in the binary. `/readyz` is there for an orchestrator that can reach it;
  revisit if compose needs to gate on the app being ready.
- **No README.** `make help` lists every command, and `.env.example` documents the settings.
- **No graceful-shutdown test in Go.** It was verified against the real container, where SIGTERM
  and PID 1 actually mean something; a test would have exercised the mock rather than the thing.

## State of the repository

Branch `feature/01-skeleton`, **not committed** — `CONTEXT.md` §2 says commits happen when the
owner asks. `internal/core/dbgen/` is generated output and is meant to be committed with the
rest. Compose stack left running.

## Next step

Feature 02 / milestone M1: accounts and projects. Register, log in, log out with Argon2id;
`scs` sessions in the `sessions` table the schema already has; `nosurf` CSRF; project CRUD with
slug validation and the reserved-word list; and the first templates, which brings in the
Tailwind standalone binary and HTMX. The first real queries go in `internal/core/queries/`, so
`make sqlc` starts earning its place.
