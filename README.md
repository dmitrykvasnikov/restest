# restest

A mock REST API server. You define endpoints and datasets through a web interface, and restest
serves them over HTTP so you can develop and test **your own** clients against a predictable,
controllable API.

Requests flow inbound — from your client to restest. It does not call your API; that is the
deliberately deferred phase 2 (`DESIGN.md` §10).

Single Go binary, PostgreSQL, server-rendered HTML. No npm in the toolchain, no ORM, no Redis.

---

## Project status

**Milestones M0 through M5 are done.** The mock server works, it holds state, and it writes down
what passed through it: define an endpoint or a collection in the web interface and it answers
`curl` immediately, with no restart and no deploy. `POST` a record and the next `GET` returns it.
Every mock request lands in a per-project log that tails live in the browser, so what a client
actually sent is visible as it arrives. And there is something to try before any of that: a
shared demo project at `/m/demo/…` answers with no account at all. Programmatic configuration —
API tokens and the rest of `/api/v1/` — is the next milestone.

```sh
curl localhost:8080/m/demo/users        # eight users, no account, no token
```

| Milestone | Subject | Status |
|---|---|---|
| M0 | Skeleton: config, logging, pgx pool, migrations, Docker, health probes | **done** |
| M1 | Accounts and projects: Argon2id, sessions, CSRF, project CRUD | **done** |
| M2 | Static mocks and the route matcher (`/m/{slug}/…`) | **done** |
| M3 | Stateful collections: CRUD over stored documents, filters, reset to seed | **done** |
| M4 | Request log and inspector: recording, live tail over SSE, retention | **done** |
| M5 | Public datasets and the demo project at `/m/demo/…` | **done** |
| M6 | API tokens and management API | next |
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
- **Stateful collections.** A collection is a named set of JSON documents plus a seed. An
  endpoint of kind *collection* expands into the six REST routes over it, so a `POST` comes back
  on the next `GET`, survives a restart, and can be filtered for.
- **Listing queries**: `_page`, `_limit`, `_sort`, `_order`, and any `field=value` filter, served
  by the GIN index on the document body. `X-Total-Count` says how many matched.
- **Reset to seed**, as a button in the interface and at
  `POST /api/v1/projects/{slug}/collections/{name}/reset` — see the limitation below.
- **The request log.** Every request to `/m/{slug}/…` is recorded — method, path, query, headers,
  bodies, status, duration and remote address — including the ones nothing matched, which are
  usually the interesting ones. Recording happens off the request path: the exchange goes to a
  buffered queue that a batching writer drains, so the client never waits for the log of itself.
- **The inspector** at `/projects/{slug}/log`, **live-tailing over SSE** — send a request and the
  row appears without a refresh — with a detail view of any exchange, paging back through the
  history, and a count of anything the queue had to drop, because a gap in a log that does not
  admit to gaps is worse than no log.
- **Credentials are redacted before they are stored.** `Authorization`, `Proxy-Authorization`,
  `Cookie` and `Set-Cookie` reach the database as `Bearer [redacted]`, keeping the scheme and
  dropping the secret. Bodies above 64 KiB are stored truncated and marked as truncated.
- **Retention by partition.** `exchanges` is partitioned by month; a daily job creates the next
  three months and detaches and drops anything older than the window, so expiry is a file unlink
  rather than a `DELETE` over the busiest table in the schema.
- **Built-in datasets.** `users`, `posts`, `comments` and `todos`, cross-referenced by
  `userId` and `postId`. Tick them when creating a project and it answers the six REST routes
  over each of them straight away. A dataset is stored as the collection's seed, so it can be
  edited, reset and deleted like anything you typed yourself.
- **The demo project** at `/m/demo/…`, provisioned at startup and served to anyone with no
  account, no cookie and no token. Writes to it are real writes — a `POST` comes back on the
  next `GET` — and every collection is restored to its seed hourly, so the next visitor finds
  it as you did. It belongs to an account with a random discarded password, so it is in
  nobody's project list and nobody can edit it.
- Health probes, embedded and content-hashed static assets, structured JSON logs with a request
  id, graceful shutdown, migrations applied at startup.

### What does not work yet

- **The reset route is not scriptable yet.** `/api/v1/` authenticates with the session cookie and
  is guarded by CSRF, so the button in the interface works and a shell script does not. Exempting
  a cookie-authenticated mutating route from CSRF is the hole the guard exists to close; a bearer
  token is not a cookie and needs no exemption, so **M6 is what makes this route scriptable — at
  the same URL.**
- **The request log has no search and no filters.** It is a list newest-first with a detail view;
  finding one request among a thousand means paging. Filtering by status, method or path is the
  obvious next thing and is not built.
- **The log is not exposed over the API**, so it can be read in a browser and not from a script.
  It joins the rest of `/api/v1/` in M6.
- **Requests to an unknown project slug are not recorded** — there is no project whose log they
  would belong to. They are in the process log, not the inspector.
- No API tokens and nothing else under `/api/v1/`. The schema exists in migration `00001`; the
  code does not.
- **Datasets can only be chosen when a project is created.** Adding one to a project that
  already exists means creating the collection and its endpoint by hand — the seed of a
  built-in dataset is not offered from the collection form.
- **The demo project has no page.** It is offered on the login page and reached with `curl`;
  there is no browsable view of what is in it, because it belongs to no account that can log
  in and open it.
- **No state isolation.** Collections hold one set of documents per project, so two parallel CI
  runs against one project interfere. `DESIGN.md` §12.1 has the additive path.
- No password reset, no account deletion in the UI, no rate limiting, no CSP, and no CORS headers
  by default — though an endpoint can set its own response headers, including
  `Access-Control-Allow-Origin`, and they apply to collection responses too. Reset needs a mail
  decision; the rest is M7.

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

A complete pass through everything M0 to M5 deliver.

**Before anything else**, with the stack up and no account at all:

```sh
curl localhost:8080/m/demo/users            # the demo project's users dataset
curl localhost:8080/m/demo/users/1          # {"id":1,"name":"Ada Lovelace",…}
curl 'localhost:8080/m/demo/posts?userId=1' # posts reference users by userId
curl -X POST localhost:8080/m/demo/todos -d '{"title":"Try restest","completed":false}'
```

The demo holds all four built-in datasets — `users`, `posts`, `comments`, `todos` — and your
writes are real: the record you just created comes back on the next `GET`, survives a restart,
and is cleared when the demo is restored to its seeds, which happens hourly and at startup.
Set `RESTEST_DEMO_ENABLED=false` on an instance that should not serve it.

**In a browser** at <http://localhost:8080>:

1. `/register` — create an account. Email must be a bare address, password at least 8 characters.
2. You land on `/projects`, empty. Create one: a **name** (up to 80 characters) and a **slug**.
   The slug is what will appear in mock URLs. It is trimmed and lower-cased before validation, so
   `"  MyAPI "` becomes `myapi`, and then must be lower-case letters, digits and hyphens, starting
   and ending alphanumeric, 1–40 characters. `api`, `admin`, `demo`, `healthz`, `m` and `static`
   are reserved and refused — `demo` because the shared demo project answers there.
   The same form offers the four **built-in datasets**. Tick any of them and the project is
   created with a collection holding that dataset's seed and the endpoint serving it, so
   `/m/{slug}/users` answers before you have defined anything yourself. The project and every
   dataset are one transaction: if any part is refused, no project is created.
3. The project page shows its mock base URL — `http://localhost:8080/m/{slug}/` — and, below it,
   the endpoints it serves.
4. **New endpoint**, kind **static**. The form opens on a working example: `GET /hello` returning
   `{"message": "hello"}` with a 200. Save it and it answers at once.
   - **Path** takes named parameters in braces: `/users/{id}/posts`. A parameter is a whole
     segment — `/v{n}` is refused rather than quietly matched as literal text.
   - **Response headers** are one `Name: value` per line. Leave `Content-Type` out and it is
     guessed from the body: parsed as JSON if it parses, sniffed otherwise. Framing headers
     (`Content-Length`, `Transfer-Encoding`, `Connection`, …) are refused — they belong to the
     server.
   - **Delay** holds the response for up to 60 seconds, for testing spinners and client timeouts.
   - **Enabled** off makes the endpoint invisible to the matcher without deleting it.
5. **New collection**, for state. Give it a name, say which field carries the identifier (`id` by
   default), choose `serial` or `uuid` for new identifiers, and paste a **seed** — a JSON array of
   objects, edited in the same CodeMirror. The seed is applied as soon as the collection is
   created.
6. **New endpoint**, kind **collection**, pointed at it. One row, six routes: `GET` and `POST` on
   the path you gave, and `GET`, `PUT`, `PATCH`, `DELETE` on `/{id}` below it.
7. Rename the project. Every route moves with it: the new slug answers immediately and the old one
   stops. The flash message says so.
8. **Reset** the collection from the project page. Everything written since the last reset goes,
   the seed comes back, and the identifier counter goes back with it.
9. Delete the endpoint, the collection, or the project. The buttons are real forms that work
   without JavaScript, with `hx-confirm` layered on top by HTMX. Deleting a collection takes its
   documents and the endpoint serving it.
10. **Request log**, from the button on the project page. Leave it open and `curl` the project in
    another terminal: the row appears as the request lands, with no refresh — the page holds an
    SSE connection to `/projects/{slug}/log/stream`. Click a row for the whole exchange: request
    headers and body as they arrived, response headers and body as they left, status, duration
    and remote address. Requests nothing matched are there too, marked as unmatched, which is
    usually what you opened the log to find out. Bodies over 64 KiB are shown truncated and
    labelled; `Authorization` and `Cookie` values were redacted before they were stored.
11. Log out, log back in — with a differently-cased address, which works, because the email column
    is `citext`.

The endpoint form shows the fields for the kind you chose and hides the other set. With JavaScript
off it shows both, and the server reads only the fields belonging to the chosen kind, so the form
still works.

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

**With curl — a collection.** Define a collection called `users` seeded with two records and an
endpoint of kind *collection* at `/users`, in a project called `checkout`:

```sh
curl localhost:8080/m/checkout/users
# [{"id":1,"name":"Ada","role":"admin"},{"id":2,"name":"Alan","role":"engineer"}]

curl -i -X POST localhost:8080/m/checkout/users -d '{"name":"Grace","role":"admin"}'
# 201, Location: /m/checkout/users/3, body {"id":3,"name":"Grace","role":"admin"}

curl localhost:8080/m/checkout/users/3            # the record that was just created
curl -X PATCH localhost:8080/m/checkout/users/1 -d '{"role":"pioneer"}'   # shallow merge
curl -X PUT   localhost:8080/m/checkout/users/2 -d '{"name":"Alan Turing"}'  # full replace
curl -i -X DELETE localhost:8080/m/checkout/users/3    # 204, no body
curl -X DELETE localhost:8080/m/checkout/users         # 405, Allow: GET, HEAD, POST
```

Listing takes four parameters and any number of filters:

```sh
curl 'localhost:8080/m/checkout/users?role=admin&_sort=name'
curl 'localhost:8080/m/checkout/users?_limit=1&_page=2'
curl -sD- 'localhost:8080/m/checkout/users' -o /dev/null | grep -i x-total-count
```

- `_page` counts from 1, `_limit` defaults to **100** and may not exceed 1000, `_sort` names a
  document field, `_order` is `asc` or `desc`. `X-Total-Count` reports how many matched in total,
  not how many are in the page.
- Anything without a leading underscore is a **field filter**, so a collection with a field called
  `page` is still filterable on it. Repeating a field asks for either value.
- A query string has no types, so `?id=1` matches both `{"id": 1}` and `{"id": "1"}`. A value with
  a leading zero — `?code=007` — is not a JSON number and matches only the string.
- An unknown underscore parameter is a 400 rather than being ignored: `?_limits=5` is a typo, and
  quietly returning the first hundred documents would look as though it had worked.
- Identifiers are the server's. A `POST` that supplies its own `id` has it overwritten; a `PUT` or
  `PATCH` cannot rename the document it addressed. A **seed** may state its own identifiers, and
  allocation steps over the ones it named.
- The unsorted listing order is insertion order, and it is the tie-break under `_sort`, so paging
  through equal values returns each document exactly once.
- Request bodies are capped at 1 MiB; a body that is not a JSON object is a 400.

Reset from the interface, or — once M6 lands and this route takes bearer tokens — from a test
suite:

```sh
curl -X POST localhost:8080/api/v1/projects/checkout/collections/users/reset
# {"project":"checkout","collection":"users","documents":2}
```

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
| `/projects/{slug}/collections`, `/collections/new`, `/collections/{id}/edit`, `/collections/{id}`, `/collections/{id}/delete` | GET, POST | working, requires an account |
| `/projects/{slug}/log` | GET | **working** — the request log, newest first, `?before=` to page back |
| `/projects/{slug}/log/stream` | GET | **working** — the live tail, `text/event-stream`, `?after=` to resume |
| `/projects/{slug}/log/{id}` | GET | **working** — one exchange in full |
| `/healthz`, `/readyz` | GET | working, no session |
| `/static/…` | GET | embedded assets, content-hashed, cached immutably |
| `/m/{slug}/…` | any | **working** — mock traffic, no session, no CSRF |
| `/m/demo/…` | any | **working** — the shared demo project, the same handler as any other slug |
| `/api/v1/projects/{slug}/collections/{name}/reset` | POST | **working** — session cookie and CSRF today, bearer tokens in M6 |
| the rest of `/api/v1/…` | any | **M6 — not routed yet** |

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
| `RESTEST_LOG_BODY_LIMIT` | `65536` | Bytes of each recorded body kept, 0–1048576. Above it the body is stored truncated and marked. |
| `RESTEST_LOG_BUFFER` | `1000` | Exchanges that may wait to be written, 1–1000000. Beyond it they are dropped and counted, never queued in front of a response. |
| `RESTEST_LOG_RETENTION_MONTHS` | `3` | Months of request log kept, counting the current one, 1–120. Expiry detaches and drops that month's partition. |
| `RESTEST_DEMO_ENABLED` | `true` | Provision the shared demo project at startup and offer it on the login page. `false` leaves an existing demo in the database alone rather than deleting it. |
| `RESTEST_DEMO_RESET_INTERVAL` | `1h` | How often the demo is restored to its seeds, `1m`–`168h`. It also runs once at startup. |

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
deleted account, project, endpoint and collection CRUD, the HTMX delete, CSRF rejection, and the
404 and 405 pages. The mock server is exercised through a plain client with no cookies, which is
what a test client actually is — including the collection routes, over an in-memory document store
so that checking a `Location` header does not need Docker.

The matcher has a table-driven suite of its own covering precedence, backtracking, trailing and
doubled slashes, percent-encoded segments, parameter extraction, the wildcard verb, the `HEAD`
fallback, `Allow` across every pattern a path matches, suggestion ranking, and the expansion of a
collection endpoint into its six routes. The router is exercised under the race detector with
readers and a rebuild running at once.

The integration tests run against real Postgres — the heavy use of `jsonb` means a mocked database
would test nothing worth testing. They cover `citext` case-insensitivity, the duplicate-address,
duplicate-slug, duplicate-route and duplicate-collection paths through the actual unique indexes,
cross-owner isolation, the cascades from users through projects to endpoints, collections and
documents, the `jsonb` round trip for response headers, identifier allocation under twelve
concurrent creates, containment filtering against the GIN index, sorting and paging including a
page past the end and twenty documents with an identical sort key, and reset. Several of them
insert deliberately invalid rows *past* the Go validation to prove the database constraints and
the Go rules agree in both directions.

The request log is tested at every level it has. The recorder's own tests make a write fail on
demand and prove it batches, drops rather than blocks when the queue is full, counts drops
separately from failed writes, warns about them, and flushes what is queued on shutdown. The
middleware's tests prove that a body it recorded is still the body the handler reads, that a
body of exactly the limit is not reported as truncated, that the UI's own traffic is not
recorded, and that a delayed response still works through the capturing writer. The partition
tests, against real Postgres, check that rows land in their month's partition, that uncovered
months fall into the default, that retention drops what has expired and never the current month,
and that maintenance is safe to run twice.

The datasets and the demo are tested at both levels. Unit tests prove every built-in dataset is
valid input to the same validation a typed seed goes through, that it seeds to the number of
documents the form promises with the identifiers its summary implies, and that an unknown
dataset name is refused rather than skipped. Against real Postgres: a project created with two
datasets serves them immediately and filters over them; a project asked for a dataset that does
not exist creates nothing at all, because it is one transaction; provisioning the demo twice
produces one project, one set of collections and one account; and provisioning refuses to adopt
a project holding the demo slug that is not the demo.

`TestTheM2Milestone` through `TestTheM5Milestone` walk each milestone end to end: define it in
the form, `curl` it, and — for M3 — post, fetch, filter, delete and reset; for M4, open the SSE
stream, send a request, and read the row off the wire; for M5, read and write the demo from a
client with no cookies at all, reset it, and watch the visitor's document disappear and the
identifier counter go back with it.

## Repository layout

```
cmd/restest/main.go   wiring and shutdown, nothing else
internal/
  config/             environment configuration, validated at startup
  logging/            slog handler construction
  database/           pgx pool, migration run
  core/               domain logic: users, projects, endpoints, collections, documents,
                      the built-in datasets and the demo project, the exchange recorder
                      and log partition maintenance
    queries/          hand-written SQL, input to sqlc
    datasets/         the built-in dataset seeds, embedded JSON
    dbgen/            sqlc output — generated, never edited
  mock/               inbound: the radix trie, the router, route expansion, suggestions
  web/                handlers, middleware, sessions, CSRF, the mock and collection handlers
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

Most SQL is generated by sqlc from `internal/core/queries/`. The document statements are the
exception and are assembled by hand in `internal/core/document.go`, because the number of filters
and the sort key come from the request. Every value still travels as a bind parameter, including
the sort key, which is applied as `body -> $n` rather than pasted into the `ORDER BY`.

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
