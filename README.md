# restest

A mock REST API server. You define endpoints and datasets through a web interface, and restest
serves them over HTTP so you can develop and test **your own** clients against a predictable,
controllable API.

Requests flow inbound — from your client to restest. It does not call your API; that is the
deliberately deferred phase 2 (`DESIGN.md` §10).

Single Go binary, PostgreSQL, server-rendered HTML. No npm in the toolchain, no ORM, no Redis.

---

## Project status

**Milestones M0, M1 and M2 are done.** The mock server works: define an endpoint in the web
interface and it answers `curl` immediately, with no restart and no deploy. State — collections
that survive a `POST` and come back on a `GET` — is the next milestone.

| Milestone | Subject | Status |
|---|---|---|
| M0 | Skeleton: config, logging, pgx pool, migrations, Docker, health probes | **done** |
| M1 | Accounts and projects: Argon2id, sessions, CSRF, project CRUD | **done** |
| M2 | Static mocks and the route matcher (`/m/{slug}/…`) | **done** |
| M3 | Stateful collections | next |
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
- **Static mock endpoints.** Method, path pattern with named parameters, status code, response
  headers, response body and a delay, edited in the browser with a CodeMirror editor for the body.
- **The route matcher.** An in-memory radix trie per project, rebuilt from the database whenever
  an endpoint or a project changes. A literal segment outranks a parameter, the search backtracks,
  and a match costs tens of microseconds.
- **`/m/{slug}/…` serves them**, unauthenticated and without a CSRF token, because a test client
  is not a browser. An unmatched path answers 404 with the nearest defined routes listed; a path
  defined for another verb answers 405 with `Allow`.
- Health probes, embedded and content-hashed static assets, structured JSON logs with a request
  id, graceful shutdown, migrations applied at startup.

### What does not work yet

- **No state.** Endpoints are static: the same request always gets the same answer. A `POST`
  followed by a `GET` does not return the new record. That is M3, and it is what makes the
  "list of users" scenario actually useful.
- No collections, documents, request log, API tokens or management API. The schema for them
  exists in migration `00001`; the code does not.
- No demo project — `/m/demo/…` is M5, and `demo` is a reserved slug held for it.
- No password reset, no account deletion in the UI, no rate limiting, no CSP, no CORS headers on
  mock responses. Reset needs a mail decision; the rest is M7.

---

## Requirements

- **Docker** with Compose — enough on its own for the quick start.
- **Go 1.26** if you want to run the server on the host, run the tests, or work on the code.

Nothing else needs installing. `goose`, `sqlc` and `golangci-lint` are pinned in the `Makefile`
and fetched by `go run` when a target needs them; Tailwind is a single downloaded binary and its
output stylesheet is committed, and CodeMirror is vendored as plain script files, so `go build`
needs neither Node nor the network.

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
3. The project page shows its mock base URL — `http://localhost:8080/m/{slug}/` — and, below it,
   the endpoints it serves.
4. **New endpoint.** The form opens on a working example: `GET /hello` returning
   `{"message": "hello"}` with a 200. Save it and it answers at once.
   - **Path** takes named parameters in braces: `/users/{id}/posts`. A parameter is a whole
     segment — `/v{n}` is refused rather than quietly matched as literal text.
   - **Response headers** are one `Name: value` per line. Leave `Content-Type` out and it is
     guessed from the body: parsed as JSON if it parses, sniffed otherwise. Framing headers
     (`Content-Length`, `Transfer-Encoding`, `Connection`, …) are refused — they belong to the
     server.
   - **Delay** holds the response for up to 60 seconds, for testing spinners and client timeouts.
   - **Enabled** off makes the endpoint invisible to the matcher without deleting it.
5. Rename the project. Every route moves with it: the new slug answers immediately and the old one
   stops. The flash message says so.
6. Delete the endpoint, or the project. The buttons are real forms that work without JavaScript,
   with `hx-confirm` layered on top by HTMX.
7. Log out, log back in — with a differently-cased address, which works, because the email column
   is `citext`.

Wrong password and unknown address return the same message and take the same time: the login path
verifies against a decoy hash when the address is not found, so timing does not leak which
accounts exist.

**With curl** — the mock server, which needs no account, no cookie and no CSRF token. Define
`GET /users/{id}` returning `{"id":1,"name":"Sam"}` and `GET /users/me` returning something else,
in a project called `checkout`:

```sh
curl localhost:8080/m/checkout/users/7      # {"id":1,"name":"Sam"}   — the parameter route
curl localhost:8080/m/checkout/users/me     # the literal route: it outranks the parameter
curl localhost:8080/m/checkout/users/7/     # same answer; a trailing slash is not a new route
curl -X POST localhost:8080/m/checkout/users/7   # 405, with Allow: GET, HEAD
curl localhost:8080/m/checkout/userz/7      # 404 — see below
```

A 404 says what is nearby rather than only that nothing matched:

```json
{
  "error": "no endpoint matches GET /userz/7 in project \"checkout\"",
  "project": "checkout",
  "method": "GET",
  "path": "/userz/7",
  "nearest": [
    { "method": "GET", "path": "/users/me" },
    { "method": "GET", "path": "/users/{id}" }
  ]
}
```

A few more rules worth knowing:

- `HEAD` is answered from the `GET` route, headers and all, with no body.
- `OPTIONS` on a path that defines no `OPTIONS` endpoint answers `204` with `Allow`, rather than
  the 405 every other verb would get.
- A method of `*` matches any verb, and an exact verb defined alongside it wins.
- `%2F` in a path stays inside its segment: `/users/a%2Fb` matches `/users/{id}` with
  `id = "a/b"`, and does **not** match `/users/{id}/{other}`.

**The probes**, which are the only other endpoints that need no session:

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
| `/projects/{slug}/endpoints`, `/endpoints/new`, `/endpoints/{id}/edit`, `/endpoints/{id}`, `/endpoints/{id}/delete` | GET, POST | working, requires an account |
| `/healthz`, `/readyz` | GET | working, no session |
| `/static/…` | GET | embedded assets, content-hashed, cached immutably |
| `/m/{slug}/…` | any | **working** — mock traffic, no session, no CSRF |
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
| `make vendor-htmx`, `make vendor-codemirror` | Re-download the vendored front-end files, only when changing versions |

The application applies migrations itself at startup, so the `migrate` targets are for developing
migrations, not for deployment.

## Tests

```sh
make test              # fast, no Docker
make test-integration  # starts Postgres 17 in a container via testcontainers-go
```

The unit tests drive a real `httptest` server through a cookie jar: registration, login, logout,
session renewal, cookie attributes in both the plain and TLS configurations, a stale session for a
deleted account, project and endpoint CRUD, the HTMX delete, CSRF rejection, and the 404 and 405
pages. The mock server is exercised through a plain client with no cookies, which is what a test
client actually is.

The matcher has a table-driven suite of its own covering precedence, backtracking, trailing and
doubled slashes, percent-encoded segments, parameter extraction, the wildcard verb, the `HEAD`
fallback, `Allow` across every pattern a path matches, and suggestion ranking. The router is
exercised under the race detector with readers and a rebuild running at once.

The integration tests run against real Postgres — the heavy use of `jsonb` means a mocked database
would test nothing worth testing. They cover `citext` case-insensitivity, the duplicate-address,
duplicate-slug and duplicate-route paths through the actual unique indexes, cross-owner isolation,
the cascade from users through projects to endpoints, and the `jsonb` round trip for response
headers. Two of them insert deliberately invalid rows *past* the Go validation to prove the
database constraints and the Go rules agree in both directions. `TestTheM2Milestone` walks the
whole thing: define an endpoint in the form, `curl` it, mistype it, add a literal that outranks it,
delete it.

## Repository layout

```
cmd/restest/main.go   wiring and shutdown, nothing else
internal/
  config/             environment configuration, validated at startup
  logging/            slog handler construction
  database/           pgx pool, migration run
  core/               domain logic: users, projects, endpoints, hashing, validation
    queries/          hand-written SQL, input to sqlc
    dbgen/            sqlc output — generated, never edited
  mock/               inbound: the radix trie, the router, route suggestions
  web/                handlers, middleware, sessions, CSRF, the mock handler
    templates/        Go templates, embedded
    static/           generated CSS and vendored JS and CodeMirror, embedded
  integration/        tests needing a real Postgres, behind a build tag
migrations/           goose migrations, embedded
notes/                session history, append-only
```

Business logic stays out of the HTTP handlers, so the phase 2 runner can drive the same logic from
a background worker with no request in sight. Validation lives in `core` and returns errors keyed
by form field, so the management API in M6 enforces the same rules as the browser forms rather
than a second copy of them. `internal/mock` speaks no HTTP at all — it takes a method and a path
and returns a decision, which is what lets it be tested as a table rather than through a server.

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
