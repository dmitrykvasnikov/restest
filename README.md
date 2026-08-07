# restest

A mock REST API server. You define endpoints and datasets through a web interface, and restest
serves them over HTTP so you can develop and test **your own** clients against a predictable,
controllable API.

Requests flow inbound — from your client to restest. It does not call your API; that is the
deliberately deferred phase 2 (`DESIGN.md` §10).

Single Go binary, PostgreSQL, server-rendered HTML. No npm in the toolchain, no ORM, no Redis.

---

## Project status

**Milestones M0 through M7 are done — the MVP is complete.** The mock server works, it holds
state, and it writes down what passed through it: define an endpoint or a collection in the web
interface and it answers `curl` immediately, with no restart and no deploy. `POST` a record and
the next `GET` returns it. Every mock request lands in a per-project log that tails live in the
browser, so what a client actually sent is visible as it arrives. **And none of it needs the
browser**: an API token reaches the whole of `/api/v1/`, so a CI job can create a project, define
its endpoints, read the log and reset state between runs. There is also something to try before
any of that: a shared demo project at `/m/demo/…` answers with no account at all.

M7 made it something you can leave running: rate limits on the traffic that is unauthenticated by
design, caps and timeouts on what one request may cost, a strict Content-Security-Policy on the
interface and a sandbox on everything a project serves, Prometheus metrics at `/metrics`, and a
backup and restore procedure that has been used to restore.

```sh
curl localhost:8080/m/demo/users        # eight users, no account, no token

# and with a token, the same mocks are configurable from a script
curl -H "Authorization: Bearer $RESTEST_TOKEN" localhost:8080/api/v1/

curl localhost:8080/metrics             # request rate, match rate, latency, queue depth
```

| Milestone | Subject | Status |
|---|---|---|
| M0 | Skeleton: config, logging, pgx pool, migrations, Docker, health probes | **done** |
| M1 | Accounts and projects: Argon2id, sessions, CSRF, project CRUD | **done** |
| M2 | Static mocks and the route matcher (`/m/{slug}/…`) | **done** |
| M3 | Stateful collections: CRUD over stored documents, filters, reset to seed | **done** |
| M4 | Request log and inspector: recording, live tail over SSE, retention | **done** |
| M5 | Public datasets and the demo project at `/m/demo/…` | **done** |
| M6 | API tokens and the management API at `/api/v1/` | **done** |
| M7 | Hardening: rate limits, body caps, CSP, metrics, backup and restore | **done** |

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
  `POST /api/v1/projects/{slug}/collections/{name}/reset`, which a test suite can call with a
  token between runs.
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
- **API tokens**, created at `/tokens`. The plaintext is shown once and never again — the row
  holds its SHA-256 and a short prefix, so a lost token is revoked and replaced rather than
  looked up. Optional expiry in days; revocation takes effect on the next request. The page
  shows when each was last used, which is how a token nobody needs any more becomes visible.
- **The management API at `/api/v1/`**, taking either the session cookie or
  `Authorization: Bearer …`: projects, endpoints, collections, reset and the request log. It is
  the same code the interface runs, so validation and ownership scoping are identical, and an
  endpoint defined by a script answers the next request with no restart.
  `GET /api/v1/` lists the routes and names the account a token resolved to, which is the
  cheapest way to check a credential.
- **A request that presents a token is authenticated by that token alone**, never falling back
  to the session — which is what makes those requests safe to exempt from CSRF while the cookie
  stays guarded.
- **Rate limits.** Mock traffic is counted per client address and per project, and the management
  API per credential — the token, or the account behind the session. A refusal is a 429 with
  `Retry-After` and a body naming the limit it hit, in the same JSON shape as every other mock
  error. Each limit is a setting and zero turns it off. The interface itself is not limited.
- **Caps and timeouts.** Every request body is capped before any handler reads one, headers are
  capped, and the server has read, write and idle timeouts — all settings. A delayed endpoint
  still works: it lifts its own write deadline rather than the guard being relaxed for everybody.
- **A strict Content-Security-Policy** on every page: `default-src 'none'`, scripts and styles
  from this origin only, no `unsafe-inline` anywhere. That is affordable because no template
  carries an inline script, an inline style or an `on…` attribute — verified in a real browser
  against the CodeMirror editor and the live log tail, with zero violations.
- **Everything a project serves is sandboxed.** A mock response carries
  `Content-Security-Policy: sandbox`, so an endpoint returning a page with a script in it renders
  in an opaque origin with scripts disabled — it cannot read the session cookie or make a
  same-origin request. An endpoint cannot override this with response headers of its own; the
  header is written after the handler has had its turn. Browser clients fetching mocks
  cross-origin are unaffected, because the header that would refuse them is deliberately absent.
- **Who a request is from.** `X-Forwarded-For` is read only from peers named in
  `RESTEST_TRUSTED_PROXIES`, and then right to left, stopping at the first address no trusted hop
  wrote. With nothing named — the default — the header is ignored entirely, because believing it
  otherwise lets any caller claim any address.
- **Prometheus metrics at `/metrics`**: request rate and latency by surface, the matcher's four
  outcomes so the match rate is computable, rate-limit refusals, the exchange queue's depth and
  capacity, the size of the route table, and the Go and process collectors. Labels are bounded
  sets on purpose — nothing is labelled by project, path or address. Optionally behind a bearer
  token of its own.
- **Backup and restore.** `make backup` dumps through `pg_dump` inside the database container;
  `make restore FILE=…` stops the application, recreates the database, restores and waits for
  `/readyz`. Both need nothing on the host but Docker.
- Health probes, embedded and content-hashed static assets, structured JSON logs with a request
  id, graceful shutdown, migrations applied at startup.

### What does not work yet

- **The request log has no search and no filters.** It is a list newest-first with a detail view,
  in the browser and over the API alike; finding one request among a thousand means paging.
  Filtering by status, method or path is the obvious next thing and is not built.
- **A token is exactly as powerful as the account that made it.** There is no per-project scope
  and no read-only token, so a token leaked from a CI job can reach every project that account
  owns. Revoking it is immediate, which is the mitigation there is.
- **No route mints a token.** Tokens are created and revoked in the interface only — deliberately,
  so that a leaked token cannot be used to make a permanent replacement for itself.
- **Requests to an unknown project slug are not recorded** — there is no project whose log they
  would belong to. They are in the process log, not the inspector.
- **Datasets can only be chosen when a project is created.** Adding one to a project that
  already exists means creating the collection and its endpoint by hand — the seed of a
  built-in dataset is not offered from the collection form.
- **The demo project has no page.** It is offered on the login page and reached with `curl`;
  there is no browsable view of what is in it, because it belongs to no account that can log
  in and open it.
- **No state isolation.** Collections hold one set of documents per project, so two parallel CI
  runs against one project interfere. `DESIGN.md` §12.1 has the additive path.
- **The rate limits are per process.** They are in-memory token buckets, so two instances behind
  a load balancer enforce each limit twice — once each — and a caller spread across both gets
  twice the rate. A shared counter means Redis, which this project does not have and does not
  want until there is a second instance to share with (`DESIGN.md` §9.1).
- **Rate-limited requests are not in the inspector.** The limiter wraps the recorder rather than
  sitting inside it, so a refused request costs neither a captured body nor a row. The refusal is
  the 429 the client is holding, and the count is in `restest_rate_limited_total`.
- **No CORS headers by default** — though an endpoint can set its own response headers, including
  `Access-Control-Allow-Origin`, and they apply to collection responses too.
- No password reset and no account deletion in the UI. Reset needs a decision about mail that has
  not been made.

---

## Requirements

**To run it, all you need is Docker.**

| Tool | Needed for | Notes |
|---|---|---|
| **Docker** with the Compose plugin | Running it, and `make backup` / `make restore` | `docker compose version` should print something. Everything else runs inside the containers, including `pg_dump` and `pg_restore` — there is no need for a Postgres client on the host. |
| **`make`** | The commands below | Everything a target does is one or two commands; `make help` lists them, and you can run them by hand if you would rather not install `make`. |
| **`bash` and `curl`** | `scripts/backup.sh`, `scripts/restore.sh`, and the examples in this file | Both are present on any Linux or macOS system as it ships. |
| **Go 1.26** | Running the server on the host, running the tests, or working on the code | Not needed to run the stack. |

Nothing else needs installing. `goose`, `sqlc` and `golangci-lint` are pinned in the `Makefile`
and fetched by `go run` when a target needs them; Tailwind is a single downloaded binary and its
output stylesheet is committed; CodeMirror and HTMX are vendored as plain script files. So
**`go build` needs neither Node nor the network**, and **there is no npm in the toolchain** —
that is a constraint this project keeps rather than a fact about it today (`DESIGN.md` §9.2).

Optional, and only if you want them:

- **A Prometheus server**, to scrape `/metrics`. The endpoint works without one; `curl` reads it
  perfectly well.
- **A reverse proxy** such as Caddy or nginx, for TLS on a public instance. See
  [Running it behind a proxy](#running-it-behind-a-proxy), which has the two settings that
  matter.

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

A complete pass through everything M0 to M6 deliver.

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
11. **API tokens**, from the link in the header. Name one — `ci`, say — and optionally give it a
    number of days to live. The plaintext is shown once, on the page that created it, with a
    `curl` line ready to paste; reload and it is gone for good, because only its hash was kept.
    The table lists each token by its prefix with when it was created, when it was last used and
    when it expires. **Revoke** takes effect on the next request that carries it.
12. Log out, log back in — with a differently-cased address, which works, because the email column
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

**With curl — the management API.** Everything the interface does to a project, a script can do
too. Create a token at `/tokens` in the browser, copy it the once it is shown, and export it:

```sh
export T=rst_…                                   # shown once, at /tokens
export B=http://localhost:8080
A() { curl -sH "Authorization: Bearer $T" -H 'Content-Type: application/json' "$@"; }

A $B/api/v1/                                      # who you are, and what there is to call
# {"user":"sam@example.com","authenticated_by":"token","routes":[…]}

A -X POST $B/api/v1/projects -d '{"slug":"ci","name":"CI fixtures"}'
A -X POST $B/api/v1/projects/ci/collections \
     -d '{"name":"users","seed":[{"id":1,"name":"Ada"}]}'
A -X POST $B/api/v1/projects/ci/endpoints \
     -d '{"path":"/users","collection":"users"}'   # kind is inferred from `collection`
A -X POST $B/api/v1/projects/ci/endpoints \
     -d '{"method":"GET","path":"/health","status_code":200,"body":"{\"ok\":true}"}'

curl $B/m/ci/users                                # already answering, no restart
curl -X POST $B/m/ci/users -d '{"name":"Grace"}'  # a test run writes to it

A -X POST $B/api/v1/projects/ci/collections/users/reset
# {"project":"ci","collection":"users","documents":1}

A $B/api/v1/projects/ci/log                       # what the client actually sent
A -X DELETE $B/api/v1/projects/ci                 # 204, and /m/ci/ stops answering
```

Worth knowing:

- **A token is not a cookie**, so these calls need no CSRF token and no session. A request that
  presents a token is authenticated by it alone and never falls back to a session, even if the
  caller has one — which is what makes the exemption safe.
- **Projects and collections take `PATCH`**, and an absent field keeps its current value.
  **Endpoints take `PUT`** with the whole definition, because `kind` decides which of the other
  fields mean anything.
- **Unknown JSON fields are refused**, not ignored: `{"slugg":"ci"}` is a 400 naming the field,
  because a misspelling that is silently dropped looks exactly like one that worked.
- A rejected definition is a `422` carrying the same per-field messages the forms show:
  `{"error":"…","fields":{"slug":"That slug is taken. Pick another one."}}`.
- A project is addressed by slug, a collection by name, an endpoint by the id its creation
  returned. Everything under `/api/v1/` is scoped to the account the credential belongs to, so
  another account's project is the same 404 as one that never existed.
- The log is read-only, pages with `?before=<cursor>` following the `next` field, and takes
  `?limit=` up to 500. A body that is not valid UTF-8 comes back as
  `{"text":null,"bytes":…,"binary":true}` rather than mangled.

**The probes**, which are the only other endpoints that need no session:

```sh
curl -i localhost:8080/healthz   # 200 {"status":"ok"} — the process is alive
curl -i localhost:8080/readyz    # 200, or 503 {"status":"unavailable","detail":"database unreachable"}
```

`/healthz` deliberately touches nothing else: a liveness probe that fails when the database is
down would have an orchestrator restart a perfectly healthy process.

**And what the instance itself is doing**, which needs no session either:

```sh
curl -s localhost:8080/metrics | grep restest_mock_requests_total
# restest_mock_requests_total{outcome="matched"} 41
# restest_mock_requests_total{outcome="no_route"} 3

# a burst past the per-address limit, to see the guard answer
seq 1 200 | xargs -P 30 -I{} curl -s -o /dev/null -w '%{http_code}\n' \
  localhost:8080/m/demo/users | sort | uniq -c
#     122 200
#      78 429
```

[Operating it](#operating-it) has the rest: what each limit is, how to back the database up and
restore it, and the two settings a proxied instance must change.

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
| `/tokens` | GET, POST | **working** — API tokens; the plaintext is shown once, on the page that created it |
| `/tokens/{id}/delete` | POST | **working** — revoke, effective on the next request |
| `/healthz`, `/readyz` | GET | working, no session |
| `/metrics` | GET | **working** — Prometheus exposition; 404 when `RESTEST_METRICS_ENABLED=false`, behind `RESTEST_METRICS_TOKEN` when one is set |
| `/static/…` | GET | embedded assets, content-hashed, cached immutably |
| `/m/{slug}/…` | any | **working** — mock traffic, no session, no CSRF, no token |
| `/m/demo/…` | any | **working** — the shared demo project, the same handler as any other slug |

Everything below takes **either** the session cookie **or** `Authorization: Bearer …`, and a
request that presents a token needs no CSRF token:

| Path | Method | Status |
|---|---|---|
| `/api/v1/` | GET | **working** — the account, how it was proved, and the route list |
| `/api/v1/projects` | GET, POST | **working** — list, create (with `datasets` to pre-seed) |
| `/api/v1/projects/{slug}` | GET, PATCH, DELETE | **working** |
| `/api/v1/projects/{slug}/endpoints` | GET, POST | **working** |
| `/api/v1/projects/{slug}/endpoints/{id}` | GET, PUT, DELETE | **working** — `PUT` is the whole definition |
| `/api/v1/projects/{slug}/collections` | GET, POST | **working** |
| `/api/v1/projects/{slug}/collections/{name}` | GET, PATCH, DELETE | **working** |
| `/api/v1/projects/{slug}/collections/{name}/reset` | POST | **working** — the URL M3 shipped, now scriptable |
| `/api/v1/projects/{slug}/log` | GET | **working** — `?before=` to page, `?limit=` up to 500 |
| `/api/v1/projects/{slug}/log/{id}` | GET | **working** — headers and bodies as recorded |

Tokens themselves are not in the API: they are minted and revoked at `/tokens` only, so that a
leaked token cannot be used to make a permanent replacement for itself. A refusal from any
`/api/v1/` route is JSON, including the 404 and the 405 — a script that has been parsing JSON
should not be handed a page of HTML.

Anything unmatched renders the application's own 404 page, and a path that exists under a
different verb returns 405 with an `Allow` header rather than a misleading 404.

---

## Operating it

Everything in this section is on by default with settings that no honest client meets. It is here
because the defaults are decisions, and an operator should be able to see what they are.

### Rate limits

Three limits, each in requests per second per key, each turned off by setting it to zero:

| Setting | Default | Counted per | Why |
|---|---|---|---|
| `RESTEST_RATE_LIMIT_IP` | `50` | client address, across every project | One runaway loop should not be able to use the whole instance. |
| `RESTEST_RATE_LIMIT_PROJECT` | `200` | project, across every client | A project a whole CI fleet points at is one the per-address limit would never notice. |
| `RESTEST_RATE_LIMIT_API` | `20` | credential — the token, or the account behind the session | The credentialed surface. A script with a mistake in it can otherwise create projects until the disk fills. |

The bucket depth is twice the rate, with a floor of 20, so a client that has been idle for a
second can spend two seconds' worth at once. The interface is deliberately not limited: it is
behind a session, and a limit there is mostly a way to lock somebody out of their own project
while they are looking at it.

A refusal looks like this, and says which limit it hit:

```sh
$ curl -i localhost:8080/m/demo/users
HTTP/1.1 429 Too Many Requests
Retry-After: 1
Content-Type: application/json; charset=utf-8

{"error":"too many requests from this address; this instance serves 50 mock requests a second per client","project":"demo","method":"GET","path":"/users"}
```

Refusals are counted in `restest_rate_limited_total{scope=…}`. They are **not** in the project's
request log: the limiter wraps the recorder rather than sitting inside it, so shedding load
actually sheds it.

### Running it behind a proxy

Two settings matter, and both are wrong by default for a proxied instance — deliberately, because
the safe default for one reached directly is the unsafe one for one behind a proxy, and guessing
would be worse than asking.

```sh
RESTEST_BASE_URL=https://restest.example.com   # the address users actually type
RESTEST_TRUSTED_PROXIES=172.18.0.0/16          # the proxy, as this process sees it
```

`RESTEST_BASE_URL` decides whether cookies are marked `Secure`. A browser never returns a
`Secure` cookie over plain HTTP, so an instance behind TLS that leaves this at its default will
find that **nobody can log in**.

`RESTEST_TRUSTED_PROXIES` is a comma-separated list of addresses and CIDR blocks. Only requests
arriving from one of them have their `X-Forwarded-For` read, and it is read right to left,
stopping at the first address no trusted hop wrote — which is the only entry a caller could not
have forged. With the list empty, the header is ignored and every request is attributed to the
address it arrived from. Leave it empty and a proxied instance will count the whole internet as
one client for the rate limit, and record the proxy's address for every request in every log.

The value is what *this process* sees as the peer, which behind compose is the proxy's address on
the Docker network rather than its public one. `docker compose logs app` shows it in the
`remote_ip` field of any request that arrived through the proxy.

### Metrics

`GET /metrics` serves the Prometheus text exposition. `RESTEST_METRICS_ENABLED=false` unregisters
it, and the path then answers 404.

| Metric | Type | What it answers |
|---|---|---|
| `restest_http_requests_total{surface,method,status}` | counter | Request rate, by `mock` / `app` / `api` / `ops` |
| `restest_http_request_duration_seconds{surface}` | histogram | Latency, and the count for a rate |
| `restest_mock_requests_total{outcome}` | counter | The match rate: `matched` over the sum of all four outcomes |
| `restest_rate_limited_total{scope}` | counter | Refusals, by which limit refused them |
| `restest_exchange_queue_depth` / `_capacity` | gauge | Whether the request log is keeping up |
| `restest_exchanges_dropped_total` | counter | Whether it already failed to |
| `restest_route_table_projects` / `_routes` | gauge | A route table that has quietly emptied itself |
| `restest_rate_limiter_keys{scope}` / `_cleared_total` | gauge, counter | How many clients are being counted, and whether the table ever hit its ceiling |
| `restest_build_info{revision}` | gauge | Which build this is |
| `go_*`, `process_*` | — | Heap, goroutines, file descriptors |

A useful pair to start from:

```promql
rate(restest_mock_requests_total{outcome="matched"}[5m])
  / rate(restest_mock_requests_total[5m])                    # match rate
histogram_quantile(0.99,
  rate(restest_http_request_duration_seconds_bucket{surface="mock"}[5m]))
```

**The scrape is not public information**: it names every project's traffic volume and this
process's memory layout. Compose publishes the port on `127.0.0.1` only, so by default nothing on
the network can reach it. On an instance that is published as it stands, either stop the proxy
routing `/metrics` or set `RESTEST_METRICS_TOKEN`, which makes the endpoint require that token as
`Authorization: Bearer …` — and it is never satisfied by a logged-in session.

### Backup and restore

`pg_dump` and `pg_restore` run inside the database container, so the host needs nothing but
Docker.

```sh
make backup                                        # → backups/restest-20260807T130906Z.dump
make restore FILE=backups/restest-20260807T130906Z.dump
```

The dump is taken in the custom format, compressed, in one transaction — so a backup taken under
traffic is one moment rather than a smear across several — and the application keeps running.
`backups/` is git-ignored: a dump holds every account's data.

**Restoring replaces the database.** The script says so and asks for the database name before it
does anything; `RESTEST_RESTORE_YES=1` skips the prompt for a scripted drill. It stops the
application first, because restoring underneath a running one would leave the route table it
holds in memory answering for endpoints that no longer exist. The database is dropped and
recreated rather than restored over, so nothing the dump does not mention survives. Then the
application starts, applies any migrations the backup predates, and the script waits for
`/readyz` before reporting success.

**A backup you have not restored is not a backup.** The drill:

```sh
make backup                                     # note the filename it prints
docker compose exec -T db psql -U restest -d restest \
  -c "delete from users where email = 'you@example.com';"
make restore FILE=backups/restest-….dump
docker compose exec -T db psql -U restest -d restest -c "select email from users;"
```

One thing to know before reading the result: the **demo project is reset to its seeds on every
startup**, and a restore restarts the application. Documents written to `/m/demo/…` will not come
back, whatever the backup holds — that is the demo working as designed, not the restore failing.
Use an ordinary account and project as the canary, as above.

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
| `RESTEST_RATE_LIMIT_IP` | `50` | Mock requests a second per client address, 0–1000000. Zero turns the limit off. |
| `RESTEST_RATE_LIMIT_PROJECT` | `200` | Mock requests a second per project, 0–1000000. Zero turns the limit off. |
| `RESTEST_RATE_LIMIT_API` | `20` | Management API requests a second per credential, 0–1000000. Zero turns the limit off. |
| `RESTEST_TRUSTED_PROXIES` | *(empty)* | Comma-separated addresses and CIDR blocks whose `X-Forwarded-For` is believed. Empty means the header is never read. |
| `RESTEST_MAX_REQUEST_BODY` | `1048576` | Bytes of request body accepted, 4096–67108864. Applied before any handler reads one. |
| `RESTEST_MAX_HEADER_BYTES` | `65536` | Bytes of request line and headers, 4096–1048576. |
| `RESTEST_READ_HEADER_TIMEOUT` | `10s` | How long a client may take to send its headers, `1s`–`1h`. |
| `RESTEST_READ_TIMEOUT` | `30s` | How long a client may take to send its whole request, `1s`–`1h`. |
| `RESTEST_WRITE_TIMEOUT` | `30s` | How long the response may take, `1s`–`1h`. A delayed endpoint and the log tail lift this for themselves. |
| `RESTEST_IDLE_TIMEOUT` | `120s` | How long a kept-alive connection may sit between requests, `1s`–`1h`. |
| `RESTEST_METRICS_ENABLED` | `true` | Serve the Prometheus exposition at `/metrics`. `false` leaves the path answering 404. |
| `RESTEST_METRICS_TOKEN` | *(empty)* | When set, `/metrics` requires it as `Authorization: Bearer …`. Never logged. |

The rate limits and `RESTEST_TRUSTED_PROXIES` are explained in
[Operating it](#operating-it), which has the reasoning and the failure modes.

`RESTEST_BASE_URL` does two jobs: it is what the UI shows as the root of every mock URL, and its
**scheme decides whether cookies are marked `Secure`**. A browser never returns a `Secure` cookie
over plain HTTP, so an instance behind TLS must set this to its `https` address or nobody will be
able to log in. Compose-level settings — ports, the database password, the bind interface — live
in `.env`; see `.env.example`.

The database password is never logged: `Config` implements `slog.LogValuer` and redacts it. Nor is
`RESTEST_METRICS_TOKEN` — the startup line says whether one is set, not what it is.

## Make targets

`make` with no argument lists them.

| Target | What it does |
|---|---|
| `make up` / `down` / `logs` | The compose stack, with the git revision stamped into the image |
| `make backup` | Dump the database into `backups/`, through `pg_dump` in the container |
| `make restore FILE=…` | Stop the application, recreate the database, restore, wait for `/readyz` |
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

API tokens are tested at both levels too. Unit tests cover the token format — the `rst_` mark,
32 random bytes of raw URL-safe base64, no character that needs quoting in a shell — that the
prefix identifies a token without giving much of it away, that hashing is stable and carries no
plaintext, and that a malformed credential is refused before the database is asked at all.
Handler tests mint a token through the real form and then present it: a mutating call with a
token and no CSRF token is accepted, the same call with a *bad* token is refused rather than
falling back to the logged-in session, the session on its own is still guarded, and a revoked
token stops working. Against real Postgres: the hash in the row is the hash of the token that
was issued, `last_used_at` is written by the statement that authenticates, an expired token is
refused *and* not marked used, and a token reaches its own account's projects and nothing else.

The hardening is tested at the level it is written. The rate limiter has unit tests of its own for
the burst being spent and refilling at the rate, keys not spending each other, the sweep dropping
buckets nothing has touched, the table being emptied rather than grown past its ceiling, and eight
goroutines sharing one bucket under the race detector. Above it, handler tests prove that a mock
request over the per-address limit and one over the per-project limit are both refused with the
limit named in the body, that a slug no project has never becomes a key, that a refused request is
**not** written to the request log, that a limit of zero serves everything, and that the interface
is not limited at all. The API's key is a hash of the presented token and never the token itself,
which is a test rather than a comment.

The client address resolver has a table covering the header being ignored without a trusted proxy
and from an untrusted peer, a forged prefix, repeated headers, ports, IPv6 in both forms, a
mapped IPv4 peer, an unreadable hop, and a chain padded to five thousand entries. The security
headers are tested on the interface, on static assets, on a mock response and on a mock 404 — and
one test proves an endpoint's own headers **cannot** replace the sandbox, because that is the
whole of why the header is written where it is. The body cap is tested through a mock write, the
management API and a browser form, which is the point of applying it once above all three.
`/metrics` is tested off, on, behind a token, with the wrong token, and with a logged-in session
that must not be enough.

`TestTheM2Milestone` through `TestTheM6Milestone` walk each milestone end to end: define it in
the form, `curl` it, and — for M3 — post, fetch, filter, delete and reset; for M4, open the SSE
stream, send a request, and read the row off the wire; for M5, read and write the demo from a
client with no cookies at all, reset it, and watch the visitor's document disappear and the
identifier counter go back with it. M6 is the whole of it from a script: register, mint a token,
then create a project, a collection and two endpoints over `/api/v1/` with no cookie jar at all,
call the mock URLs they created, write to one, reset it, read the request log back through the
API, and delete the project — after which `/m/ci/` stops answering.

## Repository layout

```
cmd/restest/main.go   wiring and shutdown, nothing else
internal/
  config/             environment configuration, validated at startup
  logging/            slog handler construction
  database/           pgx pool, migration run
  core/               domain logic: users, API tokens, projects, endpoints, collections,
                      documents, the built-in datasets and the demo project, the exchange
                      recorder and log partition maintenance
    queries/          hand-written SQL, input to sqlc
    datasets/         the built-in dataset seeds, embedded JSON
    dbgen/            sqlc output — generated, never edited
  mock/               inbound: the radix trie, the router, route expansion, suggestions
  ratelimit/          a keyed token bucket, in-process, swept and bounded
  metrics/            the Prometheus registry and collectors
  web/                handlers, middleware, sessions, CSRF, bearer authentication,
                      the mock and collection handlers, /api/v1/ (api*.go), the
                      security headers, the rate limits and the client address
    templates/        Go templates, embedded
    static/           generated CSS and vendored JS and CodeMirror, embedded
  integration/        tests needing a real Postgres, behind a build tag
migrations/           goose migrations, embedded
scripts/              backup and restore, through pg_dump in the container
notes/                session history, append-only
```

Business logic stays out of the HTTP handlers, so the phase 2 runner can drive the same logic from
a background worker with no request in sight. Validation lives in `core` and returns errors keyed
by form field, so the management API enforces the same rules as the browser forms rather
than a second copy of them — every `/api/v1/` handler calls the core method its page calls. `internal/mock` speaks no HTTP at all — it takes a method and a path
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
