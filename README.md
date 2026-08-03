# restest

A mock REST API server. You define endpoints and datasets through a web interface, and restest
serves them over HTTP so you can develop and test **your own** clients against a predictable,
controllable API.

Requests flow inbound — from your client to restest. It does not call your API; that is the
deliberately deferred phase 2 (`DESIGN.md` §10).

Single Go binary, PostgreSQL, server-rendered HTML. No npm in the toolchain, no ORM, no Redis.

---

## Project status

**Milestones M0 and M1 are done.** The application runs, migrates its own database, and carries
accounts and projects end to end. The mock server itself — the thing the product is named for —
is the next milestone.

| Milestone | Subject | Status |
|---|---|---|
| M0 | Skeleton: config, logging, pgx pool, migrations, Docker, health probes | **done** |
| M1 | Accounts and projects: Argon2id, sessions, CSRF, project CRUD | **done** |
| M2 | Static mocks and the route matcher (`/m/{slug}/…`) | next |
| M3 | Stateful collections | planned |
| M4 | Request log and inspector | planned |
| M5 | Public datasets and the demo project | planned |
| M6 | API tokens and management API | planned |
| M7 | Hardening: rate limits, CSP, metrics, backups | planned |

`PLAN.md` holds the full milestone list and each one's "done when" condition.

### What works today

- Registration, login, logout. Passwords hashed with Argon2id (m=19 MiB, t=2, p=1).
- Server-side sessions in Postgres, `HttpOnly` + `SameSite=Lax` cookies, `Secure` when the base
  URL is `https`. CSRF tokens on every mutating form.
- Project CRUD — create, list, view, rename, delete — scoped to the owner in SQL, so another
  account's project is indistinguishable from one that never existed.
- Slug validation matching the database constraint exactly, plus a reserved-word list.
- Health probes, embedded and content-hashed static assets, structured JSON logs with a request
  id, graceful shutdown, migrations applied at startup.

### What does not work yet

- **No mock traffic.** `/m/{slug}/…` is not routed yet; a project page shows its future mock base
  URL and nothing answers under it. That is M2.
- No endpoints, collections, documents, request log, API tokens or management API. The schema for
  them exists in migration `00001`; the code does not.
- No password reset, no account deletion in the UI, no rate limiting, no CSP. Reset needs a mail
  decision; the rest is M7.

---

## Requirements

- **Docker** with Compose — enough on its own for the quick start.
- **Go 1.26** if you want to run the server on the host, run the tests, or work on the code.

Nothing else needs installing. `goose`, `sqlc` and `golangci-lint` are pinned in the `Makefile`
and fetched by `go run` when a target needs them; Tailwind is a single downloaded binary and its
output stylesheet is committed, so `go build` needs neither Node nor the network.

## Quick start

```sh
git clone https://github.com/dmitrykvasnikov/restest.git
cd restest
make up          # docker compose up --build -d
```

Then open <http://localhost:8080>. It redirects to `/login`; follow "Create an account" and
register. The database is created and migrated by the application on startup — there is no
separate setup step.

```sh
make logs        # follow the application log
make down        # stop, keeping the data volume
```

Settings are optional: compose has working defaults for everything. To change them, copy
`.env.example` to `.env` (git-ignored, since it holds the database password) and edit it. Both
ports are published on `127.0.0.1` only; set `RESTEST_BIND=0.0.0.0` when you mean to expose the
instance to the network.

## Running the server on the host

Useful while working on the code: only Postgres runs in Docker, the binary runs under your Go
toolchain.

```sh
docker compose up -d db      # just the database
make run                     # go run ./cmd/restest
```

`make run` defaults to `postgres://restest:restest@localhost:5432/restest?sslmode=disable`.
Override `RESTEST_DATABASE_URL` in the environment to point somewhere else. Note that `make run`
does **not** read `.env` — compose does, the Makefile does not.

## What you can do right now

A complete pass through everything M0 and M1 deliver.

**In a browser** at <http://localhost:8080>:

1. `/register` — create an account. Email must be a bare address, password at least 8 characters.
2. You land on `/projects`, empty. Create one: a **name** (up to 80 characters) and a **slug**.
   The slug is what will appear in mock URLs. It is trimmed and lower-cased before validation, so
   `"  MyAPI "` becomes `myapi`, and then must be lower-case letters, digits and hyphens, starting
   and ending alphanumeric, 1–40 characters. `api`, `admin`, `demo`, `healthz`, `m` and `static`
   are reserved and refused.
3. The project page shows its future mock base URL — `http://localhost:8080/m/{slug}` — and says
   plainly that nothing answers there yet.
4. Rename it. A rename is allowed, and the flash message warns that the old mock URL will stop
   matching once M2 lands.
5. Delete it. The button is a real form that works without JavaScript, with `hx-confirm` layered
   on top by HTMX.
6. Log out, log back in — with a differently-cased address, which works, because the email column
   is `citext`.

Wrong password and unknown address return the same message and take the same time: the login path
verifies against a decoy hash when the address is not found, so timing does not leak which
accounts exist.

**With curl** — the probes, which are the only endpoints that need no session:

```sh
curl -i localhost:8080/healthz   # 200 {"status":"ok"} — the process is alive
curl -i localhost:8080/readyz    # 200, or 503 {"status":"unavailable","detail":"database unreachable"}
```

`/healthz` deliberately touches nothing else: a liveness probe that fails when the database is
down would have an orchestrator restart a perfectly healthy process.

Scripting the forms with curl needs care: `nosurf` requires `Origin`, `Referer` **or**
`Sec-Fetch-Site` on every unsafe request, plus the CSRF token from the form. Browsers send those
headers; a script has to be told to. Note also that a logged-in page carries two CSRF fields — the
logout form and the page's own form — so a scraper wants the right one.

### URL map

| Path | Method | Status |
|---|---|---|
| `/` | GET | redirects to `/projects` or `/login` |
| `/register`, `/login` | GET, POST | working |
| `/logout` | POST | working |
| `/projects` | GET, POST | working, requires an account |
| `/projects/new`, `/projects/{slug}`, `/projects/{slug}/edit`, `/projects/{slug}/delete` | GET, POST | working, requires an account |
| `/healthz`, `/readyz` | GET | working, no session |
| `/static/…` | GET | embedded assets, content-hashed, cached immutably |
| `/m/{slug}/…` | any | **M2 — not routed yet** |
| `/api/v1/…` | any | **M6 — not routed yet** |

Anything unmatched renders the application's own 404 page, and a path that exists under a
different verb returns 405 with an `Allow` header rather than a misleading 404.

## Configuration

Every variable is read once at startup and validated together, so a misconfigured process refuses
to start and reports every problem in one pass rather than one restart per mistake.

| Variable | Default | Meaning |
|---|---|---|
| `RESTEST_DATABASE_URL` | — | **Required.** PostgreSQL connection string. |
| `RESTEST_HTTP_ADDR` | `:8080` | Listen address. |
| `RESTEST_BASE_URL` | `http://localhost:8080` | The address users actually reach this instance on, no trailing slash. |
| `RESTEST_DATABASE_MAX_CONNS` | `10` | pgx pool size, 1–1000. |
| `RESTEST_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `RESTEST_LOG_FORMAT` | `json` | `json` for deployments, `text` for a readable terminal. |
| `RESTEST_SHUTDOWN_TIMEOUT` | `15s` | How long a graceful shutdown waits for in-flight requests. |

`RESTEST_BASE_URL` does two jobs: it is what the UI shows as the root of every mock URL, and its
**scheme decides whether cookies are marked `Secure`**. A browser never returns a `Secure` cookie
over plain HTTP, so an instance behind TLS must set this to its `https` address or nobody will be
able to log in. Compose-level settings — ports, the database password, the bind interface — live
in `.env`; see `.env.example`.

The database password is never logged: `Config` implements `slog.LogValuer` and redacts it.

## Make targets

`make` with no argument lists them.

| Target | What it does |
|---|---|
| `make up` / `down` / `logs` | The compose stack, with the git revision stamped into the image |
| `make run` | Run the server on the host against the database in compose |
| `make build` | Build into `bin/restest` |
| `make test` | Unit tests, race detector on — no Docker needed |
| `make test-integration` | Everything, including tests that start a real Postgres |
| `make lint` | `go vet` plus golangci-lint |
| `make fmt`, `make tidy` | Format the source, tidy the module |
| `make migrate`, `migrate-down`, `migrate-status`, `migrate-new NAME=…` | goose, for working on migrations |
| `make sqlc` | Regenerate `internal/core/dbgen` from the migrations and query files |
| `make assets` | Rebuild the stylesheet with the Tailwind standalone binary |
| `make vendor-htmx` | Re-download vendored HTMX, only when changing versions |

The application applies migrations itself at startup, so the `migrate` targets are for developing
migrations, not for deployment.

## Tests

```sh
make test              # fast, no Docker
make test-integration  # starts Postgres 17 in a container via testcontainers-go
```

The unit tests drive a real `httptest` server through a cookie jar: registration, login, logout,
session renewal, cookie attributes in both the plain and TLS configurations, a stale session for a
deleted account, project CRUD, the HTMX delete, CSRF rejection, and the 404 and 405 pages.

The integration tests run against real Postgres — the heavy use of `jsonb` means a mocked database
would test nothing worth testing. They cover `citext` case-insensitivity, the duplicate-address
and duplicate-slug paths through the actual unique indexes, cross-owner isolation, and the cascade
from users to projects. One of them inserts deliberately invalid slugs *past* the Go validation to
prove the database constraint and the Go rule agree in both directions.

## Repository layout

```
cmd/restest/main.go   wiring and shutdown, nothing else
internal/
  config/             environment configuration, validated at startup
  logging/            slog handler construction
  database/           pgx pool, migration run
  core/               domain logic: users, projects, hashing, validation
    queries/          hand-written SQL, input to sqlc
    dbgen/            sqlc output — generated, never edited
  web/                handlers, middleware, sessions, CSRF
    templates/        Go templates, embedded
    static/           generated CSS and vendored JS, embedded
  integration/        tests needing a real Postgres, behind a build tag
migrations/           goose migrations, embedded
notes/                session history, append-only
```

Business logic stays out of the HTTP handlers, so the phase 2 runner can drive the same logic from
a background worker with no request in sight. Validation lives in `core` and returns errors keyed
by form field, so the management API in M6 enforces the same rules as the browser forms rather
than a second copy of them.

## Documents

| File | Role |
|---|---|
| `task.md` | The original problem statement. Frozen, never edited. |
| `DESIGN.md` | Decisions and the reasoning behind them. |
| `PLAN.md` | Milestones and their order. |
| `CONTEXT.md` | How this project is worked on: branches, notes, definition of done. |
| `notes/` | One note per working session, recording what was decided and what was actually verified. |

Start with `DESIGN.md` if you want to know why something is the way it is, `PLAN.md` if you want to
know what comes next, and the most recent file in `notes/` if you are picking the work up.
