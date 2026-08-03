# Session notes 03-1 — M2 static mocks and the route matcher

Date: 2026-08-03
Feature: 03 — M2 static mocks and the route matcher
Branch: `feature/03-static-mocks`
Outcome: the milestone's "done when" is met — an endpoint defined in the UI answers correctly to
`curl`. Verified against the compose stack, not only in tests.

---

## Starting point

M1 left accounts and projects. The `endpoints` table and its constraints were already in
migration `00001` from feature 00, so **no migration was written this session** and none was
needed. The project page said, truthfully, that nothing answered under `/m/{slug}/`.

## What was built

| Path | Role |
|---|---|
| `internal/core/endpoint.go` | `Endpoint`, `EndpointInput`, `Headers`, store CRUD, `MockData` |
| `internal/core/queries/endpoints.sql` | Endpoint CRUD plus the two statements the route table reads |
| `internal/core/validate.go` | Method, path, status, delay, body and header rules; `NormalizePath`, `SplitPath`, `PathParams` |
| `internal/mock/mock.go` | `Route`, `Ref`, `Outcome`, `Result` — the vocabulary, no HTTP |
| `internal/mock/trie.go` | The radix trie: insert, depth-first search with backtracking, method selection, `Allow` |
| `internal/mock/table.go` | An immutable snapshot of every project's routes |
| `internal/mock/router.go` | The table behind an `RWMutex`, `Reload`, `Refresh` |
| `internal/mock/nearest.go` | Suggestion ranking: common prefix, then Levenshtein |
| `internal/web/mock.go` | The `/m/{slug}/` handler: delay, status, headers, body; the JSON 404 and 405 |
| `internal/web/endpoints.go` | Endpoint CRUD handlers and the form |
| `internal/web/templates/pages/endpoint_form.html` | The form, with the CodeMirror body editor |
| `internal/web/static/vendor/codemirror/`, `static/js/editor.js` | The vendored editor and its five lines of glue |

No new Go dependencies. The only new third-party code is the vendored CodeMirror.

## Decisions made while building

The two that changed `DESIGN.md` are marked.

- **CodeMirror 5, not 6** *(DESIGN.md §9.3 added, §9 table updated)*. CodeMirror 6 is published
  only as ES modules across a dozen packages that have to be bundled, and that breaks outright if
  two copies of `@codemirror/state` reach the page. Bundling means npm, and "no npm in the
  toolchain" (`CONTEXT.md` §6) is the harder constraint — it is what keeps `go build` the whole
  build. The alternatives were an opaque prebuilt bundle nobody can regenerate, or version 6
  without `lang-json` and therefore without JSON highlighting, which is most of what the editor is
  for. Version 5 is five files `curl` fetches and `go:embed` serves. Reversible in one file the day
  a bundler is acceptable.
- **The matcher backtracks** *(DESIGN.md §4 extended)*. Preferring the literal child at each step
  is not enough on its own. With `/a/b/c` and `/{x}/b/d` defined, a request for `/a/b/d` descends
  into the literal `a`, runs out of trie at `d`, and the answer is up the parameter branch. This
  was the one part of the design that could have been got quietly wrong, and it has a test named
  after it.
- **Parameter names live on the route, not on the trie node.** `/users/{id}` and `/users/{login}`
  walk through the same node and disagree about what it is called. The walk collects values in
  order; the route that answered zips them with its own names. A node holding the name would have
  had to pick one and be wrong for the other.
- **Matching is on `r.URL.EscapedPath()`, not `r.URL.Path`.** `Path` has already turned `%2F` into
  a slash, so `/users/a%2Fb` would split into three segments and hand the endpoint a parameter the
  client never sent. Segments are split first and decoded afterwards.
- **Paths are normalised on both sides and stored normalised.** `/users`, `/users/` and `//users`
  are one route. This is what makes the unique index on (project, method, path) mean what it says:
  without it, two rows could match the same requests and which answered would be a coin toss.
- **A parameter must be a whole segment.** `/v{n}` is a plausible thing to type; it is refused with
  a message saying so rather than silently matching the literal text `v{n}`.
- **The table is immutable and swapped wholesale.** The `RWMutex` is held for a pointer read, not
  for the length of a match, so a request that starts matching finishes against the snapshot it
  began with even if a rebuild lands mid-way.
- **A failed reload keeps the old table.** A rebuild fails because the database is unreachable, and
  answering "no such project" to every mock request until it comes back would turn a blip into a
  wrong answer where a slightly stale one was available.
- **A 30-second background refresh, as well as reload-on-change.** Not how a new endpoint goes
  live — every mutating handler reloads. It is the answer to a rebuild that failed and to a second
  instance whose edits this one never saw, and the deployment model is still open (`DESIGN.md`
  §12.5).
- **Projects are loaded separately from endpoints**, so a project with nothing defined is still
  recognised. "Nothing is defined here" and "there is no such project" are different answers, and
  the endpoint rows alone cannot tell them apart. The two statements are not in a transaction: a
  torn read can only produce an endpoint whose project has just gone, which the table drops on the
  way in.
- **Mock traffic skips the session and CSRF middleware**, listed in `isUnsessioned` alongside the
  probes. Stronger reason than for the probes: a test client POSTing to a mock carries no CSRF
  token, and routing it through nosurf would answer every write with 400.
- **`OPTIONS` on a path that defines none answers 204 with `Allow`**, not 405. That is the question
  `OPTIONS` asks and the header is already the answer. Every other verb gets the 405 `PLAN.md`
  asks for.
- **`HEAD` falls back to `GET`, and an exact verb beats `*`.** net/http discards the body of a
  `HEAD` response, so this costs nothing and gives correct headers. `Allow` lists `HEAD` wherever
  `GET` appears, because that is what the server will actually do.
- **The response's write deadline is extended before a delay.** `delay_ms` allows up to 60 s and
  the server's `WriteTimeout` is 30 s. `http.NewResponseController(w).SetWriteDeadline` pushes out
  this one response rather than weakening the guard on every route. Verified with a real 40-second
  delay against the running container.
- **Content-Type is sniffed by parsing when the endpoint sets none.** `http.DetectContentType`
  calls a JSON object `text/plain`, which is true and unhelpful for a mock REST API. JSON is tried
  first, by `json.Valid`; anything else falls through to net/http's sniffing.
- **Framing headers are refused at validation** — `Content-Length`, `Transfer-Encoding`,
  `Connection`, `Upgrade` and the rest. A mock definition that could desynchronise its own framing
  is a broken response at best.
- **A 204 or 304 with a body configured is served without the body.** The user is allowed to make
  that definition; net/http would log a complaint rather than send it, and a status the user chose
  should not look like a fault in the server.
- **Two patterns of the same shape and verb** — `/users/{id}` and `/users/{name}` for `GET` — are a
  pair the database cannot refuse, because it compares the pattern text. The first by path order
  wins, which makes it deterministic, and the other is logged as shadowed rather than disappearing
  without a word.
- **Headers are edited as `Name: value` lines**, not as JSON. `Lines()` sorts them, so editing an
  endpoint twice without touching the field does not reorder it, and the round trip through
  `ParseHeaderLines` is tested.
- **The body form field has its CRLFs stripped.** Browsers submit textarea newlines as CRLF, and a
  body served with a stray `\r` on every line is not the body that was typed.
- **`EndpointInput.Validate` is exported.** The store calls the same rules on the way in, so it is
  not a gate anyone has to remember; it exists so that a test standing in for the store cannot
  become a second, laxer copy of the rules — which the handler tests use it for.

## Verification

Everything below was run, not reasoned about.

- `make test` — race detector on. The matcher's table-driven suite: precedence, backtracking out of
  a dead literal branch, trailing and doubled slashes, an empty segment being dropped rather than
  matching a parameter, `%2F`, `%C3%A9`, `%20`, `%6De` decoding to a literal, parameter extraction
  including two names at one position, the wildcard verb, exact-beats-wildcard, `HEAD`→`GET`,
  `Allow` as the union across every pattern a path matches, project isolation, an endpoint whose
  project is gone, and shadowing. Suggestion ranking including stability across 20 calls, and
  Levenshtein checked for symmetry. The router: nothing served before the first reload, a rebuild
  that removes as well as adds, a failed reload keeping the old table, `Refresh` stopping on
  cancellation, and 800 lookups against 200 rebuilds under `-race`.
- `make test` also covers the handler: the response verbatim with its headers, five Content-Type
  cases, a POST with no CSRF token and no cookie set on the way back, 405 with `Allow` in both the
  header and the body, `OPTIONS` → 204, the 404 body's `nearest` list, an unknown project, `%2F`,
  a measured delay, 204 with no body, `HEAD` with no body, the project root reached both with and
  without its trailing slash, and the application's own pages still answering.
- `make test-integration` — real Postgres 17 via testcontainers. Endpoint round trip including the
  `jsonb` header column, paths stored normalised, the unique index refusing the second spelling of
  the same route, cross-owner isolation for create, read, update, delete and list, the cascade from
  project to endpoints, `MockData` excluding disabled endpoints while still listing the project,
  and `TestSchemaRefusesWhatTheGoRulesRefuse`, which inserts invalid rows **directly** past the Go
  validation to prove the constraints agree (`CONTEXT.md` §7). `TestTheM2Milestone` is the "done
  when", end to end over HTTP; `TestRenamingAProjectMovesItsMockURL` and
  `TestDisabledEndpointDoesNotAnswer` cover the two rebuild triggers that are easy to forget.
- `make lint` — clean.
- `make up`, then the whole thing by curl against the container. Four endpoints defined through the
  form; `GET /users/7` → 200 with `X-Mock: yes`; `GET /users/me` → the literal route, not the
  parameter; `POST /users` → 201 with the `Location` header, no CSRF token anywhere; `DELETE
  /users/7` → 405 `Allow: GET, HEAD`; `OPTIONS /users` → 204 `Allow: POST`; `HEAD /users/7` → 200
  with headers and no body; `/users/7/` → 200; `//users//7` → 307 to the clean path; `/users/a%2Fb`
  → 200 while `/users/a/b` → 404; `/userz/7` → 404 listing four nearby routes; an unknown slug →
  404 naming it. A 700 ms delay measured at 0.701 s. A **40-second** delay against the 30-second
  `WriteTimeout` completed at 40.0 s with a 200 — the write-deadline extension does what it is for.
  Renaming the project moved every route and the old slug stopped answering.
- The application log across all of it: **no `ERROR` lines**. Matching costs 0.026–0.126 ms per
  request.

## Deliberately not done

- **No CORS headers on mock responses.** A browser-based client hitting a mock from another origin
  will be blocked, and this is a real gap for one of the tool's plausible uses. It belongs with the
  security headers in M7 rather than as an improvisation here, and it needs a decision — permissive
  by default, or per-project.
- **No shadowed-route warning in the UI.** `BuildTable` logs it; the project page does not show it.
- **No enable/disable toggle in the endpoint list** — the checkbox on the edit form is the only way.
- **No request logging.** Every mock request is a candidate `Exchange` and none is recorded. M4.
- **No body size cap on incoming mock requests, and no rate limiting.** M7.
- **No `Content-Length` sanity beyond refusing the header** — the mock body is written as-is and
  net/http computes the length.

## Notes for whoever picks this up

- `internal/mock` must not grow an HTTP import. The matcher takes a method and a path and returns a
  decision; that is what lets it be tested as a table. `internal/web/mock.go` is where the wire is.
- The route table is keyed by **slug**, so anything that changes a project or an endpoint has to
  call `s.reloadRoutes`. There are five call sites: the three project handlers, `endpointSaved`
  (create and update) and the endpoint delete. A sixth place that forgets will be stale for up to
  30 seconds and then correct, which is exactly the kind of bug that is hard to see.
- `core.SplitPath` is used by both the stored pattern and the incoming request. They have to agree,
  and that is why it lives in `core` rather than in `mock` — `core` validates, `mock` matches, and a
  disagreement would be a route that can be defined and never matched.
- M3 adds `kind = 'collection'`, where one endpoint row expands to six routes. `BuildTable` already
  takes a flat list of routes and inserts them one at a time, so the expansion is a loop in
  `BuildTable` and not a change to the trie.
- The `endpoints_kind_fields` check constraint requires `status_code` non-null for `static` and
  null for `collection`. `CreateEndpoint` hard-codes `'static'`; M3 will need a second statement or
  a nullable parameter rather than an edit to that one.

## State of the repository

Branch `feature/03-static-mocks`. `internal/core/dbgen/` is regenerated output and is committed.
`internal/web/static/css/app.css` is regenerated by `make assets` and committed;
`static/vendor/codemirror/` is vendored by `make vendor-codemirror` and committed. Compose stack
left running with one account (`sam@example.com`), one project (`checkout`) and three endpoints:
`GET /users/{id}`, `GET /users/me` and `POST /users`.

## Next step

Feature 04 / milestone M3: stateful collections — collection CRUD with the seed as a JSON array,
one `collection` endpoint expanding to six routes, `_page`/`_limit`/`_sort`/`_order` and
`field=value` filters over the GIN index, serial and uuid id allocation, and reset to seed. It is
the milestone that satisfies the core of `task.md`. `DESIGN.md` §12.1 — per-run state isolation —
is the open question that lands with it, and the suggested path is still to ship shared state and
add isolation later, since it is additive.
